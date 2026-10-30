package mosh

import (
	"os"
	"os/exec"
	"testing"
)

func TestStillAtLoginOnlyForLoginProcess(t *testing.T) {
	// A running process that is not /usr/bin/login must not look like a
	// pending login.
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skip("sleep unavailable:", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	s := &Server{cmd: cmd}
	if s.StillAtLogin() {
		t.Fatal("sleep was reported as a login prompt")
	}

	self := &Server{cmd: &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}}
	if self.StillAtLogin() {
		t.Fatal("test binary was reported as a login prompt")
	}
}
