//go:build !js

package mosh

import "testing"

func TestCountAuthFailuresAcrossChunks(t *testing.T) {
	s := &Server{}
	if s.countAuthFailures([]byte("login: Login incor")) {
		t.Fatal("tripped before the message completed")
	}
	if s.countAuthFailures([]byte("rect\r\n")) {
		t.Fatal("tripped on first completed failure")
	}
	for i := 2; i < maxLoginFailures; i++ {
		if s.countAuthFailures([]byte("Login incorrect\r\n")) {
			t.Fatalf("tripped early at attempt %d", i)
		}
	}
	if !s.countAuthFailures([]byte("Login incorrect\r\n")) {
		t.Fatal("expected the limit to trip on attempt 5")
	}
}
