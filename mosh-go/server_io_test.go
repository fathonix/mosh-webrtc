//go:build darwin || linux

package mosh

import (
	"testing"
	"time"
)

// TestServerIO runs a server with no UDP socket: datagrams only cross channels.
func TestServerIO(t *testing.T) {
	srv, err := NewServerIO("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if srv.conn != nil {
		t.Fatal("NewServerIO must not bind a UDP socket")
	}
	if srv.Port() != 0 {
		t.Fatalf("Port() = %d, want 0", srv.Port())
	}

	inbound := make(chan []byte, 4)
	var sent int
	srv.AttachDatagrams(inbound, func([]byte) { sent++ })

	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()

	select {
	case <-srv.started:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
	}

	// Closing the inbound channel ends the datagram loop and the session.
	close(inbound)
	if err := srv.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down after inbound closed")
	}
}
