package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

type Session struct {
	ID             string
	Port           int
	Key            string
	Cmd            *exec.Cmd
	PeerConn       *webrtc.PeerConnection
	DataChannel    *webrtc.DataChannel
	UDPConn        *net.UDPConn
	CreatedAt      time.Time
	LastSeen       time.Time
	DisconnectedAt time.Time
	mu             sync.Mutex
}

var (
	sessionsMu sync.Mutex
	sessions   = make(map[string]*Session)
)

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

func startMoshServer() (int, string, *exec.Cmd, error) {
	// login (setuid root) asks for username + password; -p keeps our LANG/TERM env
	cmd := exec.Command("mosh-server", "new", "-p", "0", "-c", "256", "--", "/usr/bin/login", "-p")
	cmd.Env = append(os.Environ(),
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		"TERM=xterm-256color",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, "", nil, err
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return 0, "", nil, fmt.Errorf("failed to start mosh-server: %w", err)
	}

	re := regexp.MustCompile(`MOSH CONNECT (\d+) ([\w+/=]+)`)
	buf := make([]byte, 2048)
	lineBuf := ""
	timeout := time.After(5 * time.Second)

	for {
		select {
		case <-timeout:
			cmd.Process.Kill()
			return 0, "", nil, fmt.Errorf("timeout waiting for MOSH CONNECT")
		default:
			n, err := stdout.Read(buf)
			if n > 0 {
				lineBuf += string(buf[:n])
				matches := re.FindStringSubmatch(lineBuf)
				if len(matches) == 3 {
					port, _ := strconv.Atoi(matches[1])
					key := matches[2]
					log.Printf("[INFO] Spawned mosh-server on UDP port %d (PID %d)", port, cmd.Process.Pid)
					return port, key, cmd, nil
				}
			}
			if err != nil {
				cmd.Process.Kill()
				return 0, "", nil, fmt.Errorf("error reading mosh-server stdout: %w", err)
			}
		}
	}
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

	iceServers, err := fetchCloudflareICEServers()
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
			log.Printf("[INFO] Reconnecting client to existing session %s (port %d)", req.SessionID, sess.Port)
		}
	}
	sessionsMu.Unlock()

	if !isReconnect {
		port, key, moshCmd, err := startMoshServer()
		if err != nil {
			peerConnection.Close()
			http.Error(w, "Failed to start mosh-server: "+err.Error(), http.StatusInternalServerError)
			return
		}

		serverUDPAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			moshCmd.Process.Kill()
			peerConnection.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		localUDP, err := net.DialUDP("udp4", nil, serverUDPAddr)
		if err != nil {
			moshCmd.Process.Kill()
			peerConnection.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		sessionID := fmt.Sprintf("%d-%d", port, time.Now().UnixNano())
		sess = &Session{
			ID:        sessionID,
			Port:      port,
			Key:       key,
			Cmd:       moshCmd,
			PeerConn:  peerConnection,
			UDPConn:   localUDP,
			CreatedAt: time.Now(),
			LastSeen:  time.Now(),
		}

		sessionsMu.Lock()
		sessions[sessionID] = sess
		sessionsMu.Unlock()

		// Server UDP reader goroutine (bridges mosh-server -> current PeerConnection DataChannel)
		go func(s *Session) {
			buf := make([]byte, 4096)
			for {
				n, err := s.UDPConn.Read(buf)
				if err != nil {
					if !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
						endSession(s, "mosh-server exited")
					}
					return
				}
				s.mu.Lock()
				dc := s.DataChannel
				s.mu.Unlock()

				if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen && n > 0 {
					dc.Send(buf[:n])
				}
			}
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
				if _, err := sess.UDPConn.Write(msg.Data); err != nil {
					go endSession(sess, "mosh-server exited")
					return
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
		"port":       sess.Port,
		"iceServers": iceServers,
	})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	iceServers, err := fetchCloudflareICEServers()
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
	s.UDPConn.Close()
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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
					sess.UDPConn.Close()
					if sess.PeerConn != nil {
						sess.PeerConn.Close()
					}
					if sess.Cmd != nil && sess.Cmd.Process != nil {
						sess.Cmd.Process.Kill()
					}
					delete(sessions, id)
				}
			}
			sessionsMu.Unlock()
		}
	}()
}

func main() {
	host := getEnv("HOST", "127.0.0.1")
	port := getEnv("PORT", "8090")
	addr := net.JoinHostPort(host, port)
	fs := http.FileServer(http.Dir("web/static"))

	startReaper()

	http.Handle("/", fs)
	http.HandleFunc("/api/config", handleConfig)
	http.HandleFunc("/api/offer", handleOffer)

	log.Printf("[INFO] Mosh WebRTC server listening on http://%s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server exited: %v", err)
	}
}
