//go:build !js

package mosh

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/unixshells/vt-go"
)

var (
	decModeRe = regexp.MustCompile(`\x1b\[\?([0-9;]+)([hl])`)
	// mouseModes are the DEC private modes xterm.js must see to capture pointer
	// input. Everything else (alt screen 1049, cursor 25) would desync xterm
	// from the shadow framebuffer we render into.
	mouseModes = map[string]bool{"9": true, "1000": true, "1002": true, "1003": true, "1005": true, "1006": true, "1015": true, "1016": true}
)

const (
	// Server gives up waiting for client after this.
	associationTimeout = 60 * time.Second

	// Network timeout for idle sessions.
	defaultNetworkTimeout = 24 * time.Hour

	// Default terminal size.
	defaultCols = 80
	defaultRows = 24
)

// Server is a native mosh server. It listens on UDP, runs a shell in a PTY,
// bridges data through the SSP transport, and diffs terminal framebuffers
// to produce HostMessage updates for the client.
type Server struct {
	key  []byte
	ocb  *OCB
	port int
	conn *net.UDPConn
	ptmx *os.File
	cmd  *exec.Cmd

	shell string
	cols  int
	rows  int

	// Transport handles SSP sequencing, fragmentation, and crypto.
	transport *Transport

	// VT emulator and framebuffer state for CUP-based diffing.
	emu        *vt.Emulator
	baseFB     *Framebuffer // what client has (last-acked)
	sentFB     *Framebuffer // what we last sent (pending ack)
	curVisible atomic.Bool

	// pendingModes holds mouse-tracking mode changes seen on the PTY. The diff
	// carries only rendered cells, so these must be replayed to the client.
	pendingModes []byte

	// Remote client address — set on first received datagram.
	mu         sync.Mutex
	clientAddr *net.UDPAddr

	// Optional in-memory datagram path (see AttachDatagrams); nil means UDP.
	inbound <-chan []byte
	sendOut func([]byte)

	// resync asks the main loop to resend the whole screen (after a reconnect).
	resync chan struct{}

	// Login throttle: count failed login prompts seen on the PTY so a session
	// cannot be used to brute-force the host account over the network.
	authFailures int
	scanTail     []byte

	started chan struct{} // closed when PTY is running
	done    chan struct{}
}

// GenerateKey creates a random 128-bit mosh key and returns it as base64.
func GenerateKey() ([]byte, string, error) {
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return nil, "", err
	}
	return key, base64.StdEncoding.EncodeToString(key), nil
}

// NewServer creates a native mosh server.
// It binds a UDP port, generates a key, and is ready to serve.
func NewServer(shell string, portLow, portHigh int) (*Server, error) {
	conn, port, err := BindUDP(portLow, portHigh)
	if err != nil {
		return nil, err
	}
	srv, err := newServer(shell)
	if err != nil {
		conn.Close()
		return nil, err
	}
	srv.conn = conn
	srv.port = port
	return srv, nil
}

// NewServerIO creates a server with no UDP socket. Call AttachDatagrams before
// Serve to carry mosh datagrams over another transport (e.g. a WebRTC channel).
func NewServerIO(shell string) (*Server, error) {
	return newServer(shell)
}

func newServer(shell string) (*Server, error) {
	key, _, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	ocb, err := NewOCB(key)
	if err != nil {
		return nil, err
	}

	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}

	return &Server{
		key:       key,
		ocb:       ocb,
		shell:     shell,
		cols:      defaultCols,
		rows:      defaultRows,
		transport: NewTransport(ocb, true),
		started:   make(chan struct{}),
		done:      make(chan struct{}),
		resync:    make(chan struct{}, 1),
	}, nil
}

// AttachDatagrams moves the server off UDP: inbound yields client datagrams and
// send delivers server datagrams back. Closing inbound shuts the server down.
func (s *Server) AttachDatagrams(inbound <-chan []byte, send func([]byte)) {
	s.mu.Lock()
	s.inbound = inbound
	s.sendOut = send
	s.clientAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} // connectionless: no NAT to learn
	s.mu.Unlock()
}

// Kill terminates the shell process, which ends Serve.
func (s *Server) Kill() error {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Kill()
	}
	return nil
}

// Port returns the UDP port the server is listening on.
func (s *Server) Port() int {
	return s.port
}

// KeyBase64 returns the mosh key as a base64 string (22 chars, no padding).
func (s *Server) KeyBase64() string {
	encoded := base64.StdEncoding.EncodeToString(s.key)
	for len(encoded) > 0 && encoded[len(encoded)-1] == '=' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded
}

// ConnectLine returns the MOSH CONNECT line that clients parse.
func (s *Server) ConnectLine() string {
	return fmt.Sprintf("MOSH CONNECT %d %s", s.port, s.KeyBase64())
}

// Serve starts the shell and event loop. Blocks until the session ends.
func (s *Server) Serve() error {
	s.cmd = exec.Command(s.shell)
	s.cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"MOSH_SERVER_NETWORK_TMOUT=86400",
	)

	var err error
	s.ptmx, err = pty.Start(s.cmd)
	if err != nil {
		if s.conn != nil {
			s.conn.Close()
		}
		return fmt.Errorf("pty: %w", err)
	}
	close(s.started)

	pty.Setsize(s.ptmx, &pty.Winsize{
		Rows: uint16(s.rows),
		Cols: uint16(s.cols),
	})

	s.emu = vt.NewEmulator(s.cols, s.rows)
	s.curVisible.Store(true)
	s.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { s.curVisible.Store(visible) },
	})
	s.baseFB = NewFramebuffer(s.cols, s.rows)
	go s.drainReplies(s.ptmx)

	var wg sync.WaitGroup
	wg.Add(3)

	// PTY reader: reads shell output into pending buffer.
	ptyOutput := make(chan []byte, 64)
	go func() {
		defer wg.Done()
		s.readPTY(ptyOutput)
	}()

	// UDP receiver: decrypts datagrams, feeds to transport.
	userInput := make(chan UserInstruction, 64)
	go func() {
		defer wg.Done()
		s.recvUDP(userInput)
	}()

	// Main loop: process input, diff framebuffer, send via transport.
	go func() {
		defer wg.Done()
		s.mainLoop(ptyOutput, userInput)
	}()

	err = s.cmd.Wait()
	close(s.done)
	s.ptmx.Close()
	if s.conn != nil {
		s.conn.Close()
	}
	wg.Wait()
	return err
}

// TriggerResync forces the next diff to be against an empty screen (oldNum=0),
// replaying the entire current terminal content to the client. Call this when
// a client reconnects or roams to a new DataChannel.
func (s *Server) TriggerResync() {
	select {
	case s.resync <- struct{}{}:
	default:
	}
}

// Done returns a channel that is closed when the server shuts down.
func (s *Server) Done() <-chan struct{} {
	return s.done
}

// drainReplies forwards emulator query responses (DECRQM, DA, OSC palette
// queries) back to the application. vt-go writes them to an unbuffered pipe,
// so without a reader the emulator blocks forever on the first query.
func (s *Server) drainReplies(w io.Writer) {
	if w == nil {
		return
	}
	buf := make([]byte, 1024)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// withModes prepends pending mouse-tracking mode changes to a diff: rendered
// diffs carry cells only, never the raw mode escapes the application emitted.
// Duplicates are harmless (enabling a mode twice is idempotent) and are
// cleared once the client acks the frame.
func (s *Server) withModes(diff []byte) []byte {
	if len(s.pendingModes) == 0 {
		return diff
	}
	out := make([]byte, 0, len(s.pendingModes)+len(diff))
	out = append(out, s.pendingModes...)
	return append(out, diff...)
}

// maxLoginFailures is how many "Login incorrect" replies a single session may
// produce before its shell is killed.
const maxLoginFailures = 5

// countAuthFailures reports true once the session has produced too many failed
// logins. The tail is kept so a message split across two PTY reads still matches.
func (s *Server) countAuthFailures(data []byte) bool {
	s.scanTail = append(s.scanTail, data...)
	for {
		i := bytes.Index(s.scanTail, []byte("Login incorrect"))
		if i < 0 {
			break
		}
		s.scanTail = s.scanTail[i+len("Login incorrect"):]
		s.authFailures++
	}
	if len(s.scanTail) > 32 {
		s.scanTail = s.scanTail[len(s.scanTail)-32:]
	}
	return s.authFailures >= maxLoginFailures
}

// captureModes records mouse-tracking mode changes from raw PTY output.
func (s *Server) captureModes(data []byte) {
	for _, m := range decModeRe.FindAllSubmatch(data, -1) {
		for _, p := range strings.Split(string(m[1]), ";") {
			if mouseModes[p] {
				s.pendingModes = append(s.pendingModes, m[0]...)
				break
			}
		}
	}
}

// ServeRW runs the event loop using an external io.ReadWriteCloser instead
// of spawning a shell in a PTY. The caller provides terminal I/O through rw
// and a resize callback. When rw is closed or reaches EOF, the server shuts down.
func (s *Server) ServeRW(rw io.ReadWriteCloser, resize func(cols, rows uint16)) error {
	close(s.started)

	s.emu = vt.NewEmulator(s.cols, s.rows)
	s.curVisible.Store(true)
	s.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(visible bool) { s.curVisible.Store(visible) },
	})
	s.baseFB = NewFramebuffer(s.cols, s.rows)
	go s.drainReplies(rw)

	var wg sync.WaitGroup
	wg.Add(3)

	ioOutput := make(chan []byte, 64)
	go func() {
		defer wg.Done()
		s.readIO(rw, ioOutput)
	}()

	userInput := make(chan UserInstruction, 64)
	go func() {
		defer wg.Done()
		s.recvUDP(userInput)
	}()

	go func() {
		defer wg.Done()
		s.mainLoopRW(rw, resize, ioOutput, userInput)
	}()

	<-s.done
	rw.Close()
	s.conn.Close()
	wg.Wait()
	return nil
}

func (s *Server) readIO(r io.Reader, out chan<- []byte) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case out <- data:
			case <-s.done:
				return
			}
		}
		if err != nil {
			select {
			case <-s.done:
			default:
				close(s.done)
			}
			return
		}
	}
}

func (s *Server) mainLoopRW(rw io.Writer, resize func(cols, rows uint16), ioOutput <-chan []byte, userInput <-chan UserInstruction) {
	// Wait for the first client datagram. If no client connects within
	// 60 seconds, this was a failed setup — shut down to release resources.
	deadline := time.After(associationTimeout)
	for {
		s.mu.Lock()
		addr := s.clientAddr
		s.mu.Unlock()
		if addr != nil {
			break
		}
		select {
		case <-s.done:
			return
		case <-deadline:
			close(s.done)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	dirty := false

	for {
		select {
		case <-s.done:
			return

		case data, ok := <-ioOutput:
			if !ok {
				return
			}
			s.captureModes(data)
			if s.countAuthFailures(data) && s.cmd != nil && s.cmd.Process != nil {
				s.cmd.Process.Kill()
			}
			s.emu.Write(data)
			dirty = true

		case ui := <-userInput:
			if len(ui.Keys) > 0 {
				rw.Write(ui.Keys)
			}
			if ui.Width > 0 && ui.Height > 0 && (int(ui.Width) != s.cols || int(ui.Height) != s.rows) {
				s.mu.Lock()
				s.cols = int(ui.Width)
				s.rows = int(ui.Height)
				s.mu.Unlock()
				if resize != nil {
					resize(uint16(ui.Width), uint16(ui.Height))
				}
				s.emu.Resize(int(ui.Width), int(ui.Height))
				s.baseFB = NewFramebuffer(int(ui.Width), int(ui.Height))
				s.sentFB = nil
				s.transport.ResetDiffBase()
				dirty = true
			}

		case <-s.resync:
			s.mu.Lock()
			cols, rows := s.cols, s.rows
			s.mu.Unlock()
			s.baseFB = NewFramebuffer(cols, rows)
			s.sentFB = nil
			s.transport.ResetDiffBase()
			dirty = true

		case <-ticker.C:
			if s.sentFB != nil && s.transport.AckedByRemote() >= s.transport.SentNum() {
				s.baseFB = s.sentFB
				s.sentFB = nil
				s.pendingModes = nil
			}

			if dirty {
				currentFB := SnapshotEmulator(s.emu, s.curVisible.Load())
				diffBytes := s.withModes(currentFB.Diff(s.baseFB))
				if len(diffBytes) > 0 {
					hi := HostInstruction{Hoststring: diffBytes, EchoAckNum: -1}
					s.transport.SetPending(marshalHostMessage([]HostInstruction{hi}))
					s.sentFB = currentFB
				}
				dirty = false
			}

			datagrams := s.transport.Tick()
			s.sendDatagrams(datagrams)
		}
	}
}

// Close shuts down the server.
func (s *Server) Close() {
	// If Serve() was never called, just close the socket.
	select {
	case <-s.done:
		return
	case <-s.started:
		// PTY is running — kill it.
	default:
		// Serve() never called. Close socket and unblock any future Serve().
		s.conn.Close()
		return
	}

	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.conn.Close()
	if s.ptmx != nil {
		s.ptmx.Close()
	}
}

// mainLoop is the SSP event loop. It feeds PTY output to a VT emulator,
// snapshots the framebuffer, diffs against the last-acked state, and
// sends CUP-based updates via the transport.
func (s *Server) mainLoop(ptyOutput <-chan []byte, userInput <-chan UserInstruction) {
	// Wait for the first client datagram. If no client connects within
	// 60 seconds, this was a failed setup — shut down to release resources.
	deadline := time.After(associationTimeout)
	for {
		s.mu.Lock()
		addr := s.clientAddr
		s.mu.Unlock()
		if addr != nil {
			break
		}
		select {
		case <-s.done:
			return
		case <-deadline:
			close(s.done)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	dirty := false

	for {
		select {
		case <-s.done:
			return

		case data, ok := <-ptyOutput:
			if !ok {
				return
			}
			s.captureModes(data)
			if s.countAuthFailures(data) && s.cmd != nil && s.cmd.Process != nil {
				s.cmd.Process.Kill()
			}
			s.emu.Write(data)
			dirty = true

		case ui := <-userInput:
			if len(ui.Keys) > 0 {
				s.ptmx.Write(ui.Keys)
			}
			if ui.Width > 0 && ui.Height > 0 && (int(ui.Width) != s.cols || int(ui.Height) != s.rows) {
				s.mu.Lock()
				s.cols = int(ui.Width)
				s.rows = int(ui.Height)
				s.mu.Unlock()
				pty.Setsize(s.ptmx, &pty.Winsize{
					Cols: uint16(ui.Width),
					Rows: uint16(ui.Height),
				})
				s.emu.Resize(int(ui.Width), int(ui.Height))
				s.baseFB = NewFramebuffer(int(ui.Width), int(ui.Height))
				s.sentFB = nil
				s.transport.ResetDiffBase()
				dirty = true
			}

		case <-s.resync:
			s.mu.Lock()
			cols, rows := s.cols, s.rows
			s.mu.Unlock()
			s.baseFB = NewFramebuffer(cols, rows)
			s.sentFB = nil
			s.transport.ResetDiffBase()
			dirty = true

		case <-ticker.C:
			// Advance base when client acks all pending.
			if s.sentFB != nil && s.transport.AckedByRemote() >= s.transport.SentNum() {
				s.baseFB = s.sentFB
				s.sentFB = nil
				s.pendingModes = nil
			}

			if dirty {
				currentFB := SnapshotEmulator(s.emu, s.curVisible.Load())
				diffBytes := s.withModes(currentFB.Diff(s.baseFB))
				if len(diffBytes) > 0 {
					hi := HostInstruction{Hoststring: diffBytes, EchoAckNum: -1}
					s.transport.SetPending(marshalHostMessage([]HostInstruction{hi}))
					s.sentFB = currentFB
				}
				dirty = false
			}

			datagrams := s.transport.Tick()
			s.sendDatagrams(datagrams)
		}
	}
}

// readPTY reads from the PTY and sends data to the channel.
func (s *Server) readPTY(out chan<- []byte) {
	buf := make([]byte, 8192)
	for {
		select {
		case <-s.done:
			return
		default:
		}

		s.ptmx.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case out <- data:
			case <-s.done:
				return
			}
		}
		if err != nil && !os.IsTimeout(err) {
			return
		}
	}
}

// recvUDP reads encrypted datagrams, decrypts via transport, parses UserMessage.
func (s *Server) recvUDP(out chan<- UserInstruction) {
	buf := make([]byte, maxPayload+64)
	s.mu.Lock()
	inbound := s.inbound
	s.mu.Unlock()

	for {
		select {
		case <-s.done:
			return
		default:
		}

		var (
			data []byte
			addr *net.UDPAddr
			err  error
		)

		if inbound != nil {
			select {
			case <-s.done:
				return
			case dg, ok := <-inbound:
				if !ok {
					return
				}
				if len(dg) < minDatagram {
					continue
				}
				data = dg
				addr = nil
			}
		} else {
			s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
			var n int
			n, addr, err = s.conn.ReadFromUDP(buf)
			if err != nil {
				if os.IsTimeout(err) {
					if time.Since(s.transport.LastRecv()) > defaultNetworkTimeout {
						return
					}
					continue
				}
				return
			}
			if n < minDatagram {
				continue
			}
			data = make([]byte, n)
			copy(data, buf[:n])
		}

		// Feed to transport — only update clientAddr on successful decrypt.
		diff := s.transport.Recv(data)

		if addr != nil {
			s.mu.Lock()
			s.clientAddr = addr
			s.mu.Unlock()
		}

		if diff == nil {
			continue
		}

		// Parse UserMessage protobuf.
		instrs, err := unmarshalUserMessage(diff)
		if err != nil {
			continue
		}
		for _, ui := range instrs {
			select {
			case out <- ui:
			case <-s.done:
				return
			}
		}
	}
}

// sendDatagrams sends wire datagrams to the client.
func (s *Server) sendDatagrams(datagrams [][]byte) {
	s.mu.Lock()
	addr := s.clientAddr
	send := s.sendOut
	s.mu.Unlock()
	if send != nil {
		for _, dg := range datagrams {
			send(dg)
		}
		return
	}
	if addr == nil {
		return
	}
	for _, dg := range datagrams {
		s.conn.WriteToUDP(dg, addr)
	}
}

// BindUDP binds a UDP socket on the first available port in [low, high].
func BindUDP(low, high int) (*net.UDPConn, int, error) {
	if low == 0 && high == 0 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
		if err != nil {
			return nil, 0, err
		}
		return conn, conn.LocalAddr().(*net.UDPAddr).Port, nil
	}

	if high < low {
		high = low
	}

	for port := low; port <= high; port++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{
			IP:   net.IPv4zero,
			Port: port,
		})
		if err != nil {
			continue
		}
		return conn, port, nil
	}
	return nil, 0, fmt.Errorf("no available UDP port in range %d-%d", low, high)
}

// WriteTo implements io.WriterTo for streaming output to an SSH channel.
func (s *Server) WriteTo(w io.Writer) (int64, error) {
	line := s.ConnectLine() + "\n"
	n, err := w.Write([]byte(line))
	return int64(n), err
}

// sendToClient encrypts and sends a raw payload datagram to the client.
// Used by legacy tests that don't go through the Transport.
func (s *Server) sendToClient(payload []byte) {
	s.mu.Lock()
	addr := s.clientAddr
	if addr == nil {
		s.mu.Unlock()
		return
	}

	s.transport.mu.Lock()
	s.transport.seqOut++
	seq := s.transport.seqOut
	lastTS := s.transport.lastTS
	s.transport.mu.Unlock()
	s.mu.Unlock()

	dirSeq := dirToClient | (seq & seqMask)
	var dirSeqBytes [8]byte
	binary.BigEndian.PutUint64(dirSeqBytes[:], dirSeq)

	var nonce [12]byte
	copy(nonce[4:], dirSeqBytes[:])

	ts := uint16(time.Now().UnixMilli() & 0xffff)
	plaintext := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint16(plaintext[0:], ts)
	binary.BigEndian.PutUint16(plaintext[2:], lastTS)
	if len(payload) > 0 {
		copy(plaintext[4:], payload)
	}

	tagAndCT := s.ocb.Encrypt(nonce[:], plaintext)

	wire := make([]byte, 8+len(tagAndCT))
	copy(wire[:8], dirSeqBytes[:])
	copy(wire[8:], tagAndCT)

	s.conn.WriteToUDP(wire, addr)
}
