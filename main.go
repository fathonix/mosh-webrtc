package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	mosh "github.com/unixshells/mosh-go"
)

type Session struct {
	ID             string
	Key            string
	Server         *mosh.Server
	Inbound        chan []byte // DataChannel -> mosh server
	Outbound       chan []byte // mosh server -> DataChannel
	PeerConn       *webrtc.PeerConnection
	DataChannel    *webrtc.DataChannel
	CreatedAt      time.Time
	LastSeen       time.Time
	DisconnectedAt time.Time
	mu             sync.Mutex
}

var (
	sessionsMu sync.Mutex
	sessions   = make(map[string]*Session)

	// TURN credentials are valid for 24h; cache them so every connect does not
	// cost a Cloudflare API round trip (and quota).
	iceMu     sync.Mutex
	iceCached []webrtc.ICEServer
	iceExpiry time.Time
)

// newSessionID returns an unguessable session identifier: it is the only thing
// needed to re-attach to a live shell, so it must not be predictable.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func cachedICEServers() ([]webrtc.ICEServer, error) {
	iceMu.Lock()
	defer iceMu.Unlock()

	if len(iceCached) > 0 && time.Now().Before(iceExpiry) {
		return iceCached, nil
	}
	servers, err := fetchCloudflareICEServers()
	if err != nil {
		if len(iceCached) > 0 {
			return iceCached, nil // keep serving the last known good set
		}
		return nil, err
	}
	iceCached, iceExpiry = servers, time.Now().Add(time.Hour)
	return servers, nil
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func fetchCloudflareICEServers() ([]webrtc.ICEServer, error) {
	keyID := os.Getenv("CF_TURN_KEY_ID")
	apiToken := os.Getenv("CF_TURN_KEY_API_TOKEN")

	servers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.cloudflare.com:3478", "stun:stun.l.google.com:19302"}},
	}

	if keyID == "" || apiToken == "" {
		log.Println("[WARN] No CF_TURN_KEY_ID / CF_TURN_KEY_API_TOKEN set; using STUN only")
		return servers, nil
	}

	url := fmt.Sprintf("https://rtc.live.cloudflare.com/v1/turn/keys/%s/credentials/generate-ice-servers", keyID)
	reqBody, _ := json.Marshal(map[string]int{"ttl": 86400})
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return servers, err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[WARN] Cloudflare TURN API request failed: %v", err)
		return servers, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[WARN] Cloudflare TURN API error (%d): %s", resp.StatusCode, string(body))
		return servers, nil
	}

	var cfResp struct {
		IceServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfResp); err != nil {
		return servers, err
	}

	for _, entry := range cfResp.IceServers {
		var udpURLs []string
		for _, u := range entry.URLs {
			if strings.HasPrefix(u, "stun:") || strings.Contains(u, "?transport=udp") || !strings.Contains(u, "transport=") {
				udpURLs = append(udpURLs, u)
			}
		}
		if len(udpURLs) > 0 {
			s := webrtc.ICEServer{URLs: udpURLs}
			if entry.Username != "" {
				s.Username = entry.Username
				s.Credential = entry.Credential
				s.CredentialType = webrtc.ICECredentialTypePassword
			}
			servers = append(servers, s)
		}
	}
	log.Printf("[INFO] Configured %d ICE servers (including Cloudflare TURN)", len(servers))
	return servers, nil
}

// startMoshServer starts an in-process mosh server that has no UDP socket:
// SSP datagrams flow through channels and into the WebRTC DataChannel.
func startMoshServer() (*mosh.Server, chan []byte, chan []byte, error) {
	// login (setuid root) asks for username + password.
	srv, err := mosh.NewServerIO(getEnv("MOSH_LOGIN", "/usr/bin/login"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create mosh server: %w", err)
	}

	inbound := make(chan []byte, 64)
	outbound := make(chan []byte, 64)
	srv.AttachDatagrams(inbound, func(dg []byte) {
		cp := make([]byte, len(dg))
		copy(cp, dg)
		select {
		case outbound <- cp:
		default: // drop rather than block; SSP resends
		}
	})

	go func() {
		if err := srv.Serve(); err != nil {
			log.Printf("[INFO] mosh server exited: %v", err)
		}
	}()

	log.Printf("[INFO] Started in-process mosh server (no UDP port)")
	return srv, inbound, outbound, nil
}

func handleOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		SDP       string `json:"sdp"`
		SessionID string `json:"sessionId"` // if reconnecting/roaming
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	iceServers, err := cachedICEServers()
	if err != nil {
		log.Printf("[WARN] Error getting ICE servers: %v", err)
	}

	mediaEngine := &webrtc.MediaEngine{}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
	peerConnection, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var sess *Session
	isReconnect := false

	sessionsMu.Lock()
	if req.SessionID != "" {
		if existing, ok := sessions[req.SessionID]; ok {
			sess = existing
			isReconnect = true
			log.Printf("[INFO] Reconnecting client to existing session %s", req.SessionID)
		}
	}
	sessionsMu.Unlock()

	if !isReconnect {
		moshSrv, inbound, outbound, err := startMoshServer()
		if err != nil {
			peerConnection.Close()
			http.Error(w, "Failed to start mosh-server: "+err.Error(), http.StatusInternalServerError)
			return
		}

		sessionID, err := newSessionID()
		if err != nil {
			peerConnection.Close()
			http.Error(w, "Failed to generate session id", http.StatusInternalServerError)
			return
		}
		sess = &Session{
			ID:        sessionID,
			Key:       moshSrv.KeyBase64(),
			Server:    moshSrv,
			Inbound:   inbound,
			Outbound:  outbound,
			PeerConn:  peerConnection,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}

		sessionsMu.Lock()
		sessions[sessionID] = sess
		sessionsMu.Unlock()

		// Bridge mosh datagrams -> current DataChannel (survives reconnects).
		go func(s *Session) {
			for {
				select {
				case <-s.Server.Done():
					return
				case dg, ok := <-s.Outbound:
					if !ok {
						return
					}
					s.mu.Lock()
					dc := s.DataChannel
					s.mu.Unlock()
					if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
						if err := dc.Send(dg); err != nil {
							log.Printf("[WARN] DataChannel send failed: %v", err)
						}
					}
				}
			}
		}(sess)

		// login/shell exit ends the session: close the DataChannel so the
		// client stops showing a live-looking terminal and can reconnect.
		go func(s *Session) {
			<-s.Server.Done()
			endSession(s, "shell exited")
		}(sess)

	} else {
		// Update the session's active PeerConnection
		sess.mu.Lock()
		if sess.PeerConn != nil {
			sess.PeerConn.Close()
		}
		sess.PeerConn = peerConnection
		sess.LastSeen = time.Now()
		sess.mu.Unlock()

		// Full repaint over the new channel: the client may have missed diffs
		// while the old transport was dead.
		sess.Server.TriggerResync()
	}

	peerConnection.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[INFO] Session %s WebRTC peer state: %s", sess.ID, state.String())
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if state == webrtc.PeerConnectionStateConnected {
			sess.DisconnectedAt = time.Time{}
			sess.LastSeen = time.Now()
		} else if state == webrtc.PeerConnectionStateDisconnected ||
			state == webrtc.PeerConnectionStateFailed ||
			state == webrtc.PeerConnectionStateClosed {
			if sess.DisconnectedAt.IsZero() {
				sess.DisconnectedAt = time.Now()
			}
		}
	})

	peerConnection.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("[INFO] Session %s got DataChannel '%s'", sess.ID, dc.Label())
		if dc.Label() != "mosh" {
			return
		}

		sess.mu.Lock()
		sess.DataChannel = dc
		sess.mu.Unlock()

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if len(msg.Data) > 0 {
				cp := make([]byte, len(msg.Data))
				copy(cp, msg.Data)
				select {
				case sess.Inbound <- cp:
				default:
				}
				sess.mu.Lock()
				sess.LastSeen = time.Now()
				sess.mu.Unlock()
			}
		})
	})

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  req.SDP,
	}
	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	if err := peerConnection.SetLocalDescription(answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	select {
	case <-gatherComplete:
	case <-time.After(400 * time.Millisecond):
	}

	localDesc := peerConnection.LocalDescription()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sdp":        localDesc.SDP,
		"sessionId":  sess.ID,
		"key":        sess.Key,
		"iceServers": iceServers,
	})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	iceServers, err := cachedICEServers()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"iceServers": iceServers,
	})
}

// endSession tears a session down once its shell is gone and tells the client.
func endSession(s *Session, reason string) {
	log.Printf("[INFO] Session %s ended: %s", s.ID, reason)

	sessionsMu.Lock()
	delete(sessions, s.ID)
	sessionsMu.Unlock()

	s.mu.Lock()
	dc := s.DataChannel
	pc := s.PeerConn
	s.mu.Unlock()

	if dc != nil {
		dc.Close()
	}
	if pc != nil {
		pc.Close()
	}
	_ = s.Server.Kill()
}

// Session reaper: prune disconnected sessions older than 1 hour or inactive > 24h
func startReaper() {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for range ticker.C {
			sessionsMu.Lock()
			now := time.Now()
			for id, sess := range sessions {
				sess.mu.Lock()
				idle := now.Sub(sess.LastSeen)
				isDisconnected := !sess.DisconnectedAt.IsZero()
				disconnectedDuration := time.Duration(0)
				if isDisconnected {
					disconnectedDuration = now.Sub(sess.DisconnectedAt)
				}
				sess.mu.Unlock()

				// Prune if disconnected for > 1 hour, or overall idle for > 24 hours
				if (isDisconnected && disconnectedDuration > 1*time.Hour) || idle > 24*time.Hour {
					log.Printf("[INFO] Reaping mosh session %s (disconnected: %v, idle: %v)", id, disconnectedDuration.Round(time.Second), idle.Round(time.Second))
					if sess.PeerConn != nil {
						sess.PeerConn.Close()
					}
					_ = sess.Server.Kill()
					delete(sessions, id)
				}
			}
			sessionsMu.Unlock()
		}
	}()
}

func handleSession(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/session/")
	sessionsMu.Lock()
	_, ok := sessions[id]
	sessionsMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	host := getEnv("HOST", "127.0.0.1")
	port := getEnv("PORT", "8090")
	addr := net.JoinHostPort(host, port)
	fs := http.FileServer(http.Dir("web/static"))

	startReaper()

	mux := http.NewServeMux()
	mux.Handle("/", fs)
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/offer", handleOffer)
	mux.HandleFunc("/api/session/", handleSession)

	if production() {
		log.Println("[INFO] APP_ENV=production: Cloudflare Access JWT required")
	} else {
		log.Println("[INFO] APP_ENV=development: authentication disabled")
	}

	log.Printf("[INFO] Mosh WebRTC server listening on http://%s", addr)
	if err := http.ListenAndServe(addr, withAuth(mux)); err != nil {
		log.Fatalf("Server exited: %v", err)
	}
}
