package mosh

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// A same-size resize is what the client sends after re-attaching a DataChannel.
// It must produce a full repaint from state 0, otherwise a client that missed
// the earlier (lost) diff stays frozen on its stale screen.
func TestSameSizeResizeForcesFullRepaint(t *testing.T) {
	srv, err := NewServer("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	rw := struct {
		io.Reader
		io.Writer
		io.Closer
	}{outR, inW, outR}

	var mu sync.Mutex
	go srv.ServeRW(&rw, func(c, r uint16) { mu.Lock(); defer mu.Unlock() })
	defer func() {
		outR.Close()
		outW.Close()
		inR.Close()
		inW.Close()
		srv.conn.Close()
	}()

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const cols, rows = 100, 30
	client.Resize(cols, rows)
	go outW.Write([]byte("resync-marker\r\n"))

	drain := func(want string, timeout time.Duration) bool {
		var all string
		deadline := time.After(timeout)
		for {
			select {
			case <-deadline:
				return strings.Contains(clientStripANSI(all), want)
			default:
			}
			if out := client.Recv(200 * time.Millisecond); out != nil {
				all += string(out)
				if strings.Contains(clientStripANSI(all), want) {
					return true
				}
			}
		}
	}

	if !drain("resync-marker", 5*time.Second) {
		t.Fatal("initial output never arrived")
	}
	// Let the server run dry so the repaint below cannot be a repeat.
	dry := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(dry) {
		client.Recv(50 * time.Millisecond)
	}

	client.Resize(cols, rows) // same size: a resync request, not a real resize
	if !drain("resync-marker", 5*time.Second) {
		t.Fatal("same-size resize did not trigger a full repaint")
	}
}

func TestResetDiffBaseClearsStale(t *testing.T) {
	ocb, err := NewOCB(make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTransport(ocb, true)
	tr.SetPending([]byte("diff"))
	tr.Tick()
	if !tr.Stale(0) {
		t.Fatal("expected unacked diff to be stale")
	}
	tr.ResetDiffBase()
	if tr.Stale(0) {
		t.Fatal("ResetDiffBase must clear the stale marker")
	}
}
