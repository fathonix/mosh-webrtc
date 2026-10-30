package mosh

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// TestResetPeerAllowsFreshClientWithRestartedSequence tests the exact browser
// refresh bug: client 1 sends sequence 1..50. Client 2 (page reload) arrives
// with the same session key and restarts at sequence 1. Without ResetPeer(),
// the server rejects every packet as a replay (seq <= seqInMax).
func TestResetPeerAllowsFreshClientWithRestartedSequence(t *testing.T) {
	key := make([]byte, 16)
	rand.Read(key)
	ocbS, _ := NewOCB(key)
	ocbC1, _ := NewOCB(key)
	ocbC2, _ := NewOCB(key)

	server := NewTransport(ocbS, true)
	client1 := NewTransport(ocbC1, false)

	// Client 1 sends 10 packets, advancing server.seqInMax to 10.
	for i := 0; i < 10; i++ {
		client1.SetPending([]byte("data"))
		dgs := client1.Tick()
		for _, dg := range dgs {
			server.Recv(dg)
		}
	}
	if server.seqInMax != 10 {
		t.Fatalf("expected server seqInMax=10, got %d", server.seqInMax)
	}

	// Browser reloads: client 2 starts from sequence 1 with the same key.
	client2 := NewTransport(ocbC2, false)
	client2.SetPending([]byte("keystroke from fresh tab"))
	freshDgs := client2.Tick()
	if len(freshDgs) == 0 {
		t.Fatal("no datagrams from client2")
	}

	// Without ResetPeer, the server must reject it (proving the bug).
	if d := server.Recv(freshDgs[0]); d != nil {
		t.Fatal("expected rejection as replay without ResetPeer")
	}

	// Now reset the server transport for the fresh peer.
	server.ResetPeer()

	// Client 2 re-sends (or sends next datagram at sequence 1..).
	// With ResetPeer, the server accepts it immediately.
	diff := server.Recv(freshDgs[0])
	if !bytes.Equal(diff, []byte("keystroke from fresh tab")) {
		t.Fatalf("expected payload accepted after ResetPeer, got %q", diff)
	}
}
