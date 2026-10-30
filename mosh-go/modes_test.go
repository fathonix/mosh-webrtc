package mosh

import (
	"os"
	"testing"
)

func TestCaptureModesRaw(t *testing.T) {
	data, err := os.ReadFile("/tmp/herdr.raw")
	if err != nil {
		t.Skip(err)
	}
	s := &Server{}
	s.captureModes(data)
	t.Logf("pending modes len=%d %q", len(s.pendingModes), s.pendingModes)
	if len(s.pendingModes) == 0 {
		t.Fatal("no modes captured")
	}
}
