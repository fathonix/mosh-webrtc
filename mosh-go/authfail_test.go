//go:build !js

package mosh

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakePS puts a ps(1) on PATH that reports name as the process name, so the
// test can simulate "still at the login prompt" versus "logged in shell"
// without needing a real login(1).
func fakePS(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// fakePgrep reports one child pid (exit 0) or none (exit 1), like pgrep -P.
func fakePgrep(t *testing.T, hasChild bool) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if hasChild {
		script = "#!/bin/sh\necho 4242\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "pgrep"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func runningProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

func TestCountAuthFailuresAcrossChunks(t *testing.T) {
	fakePS(t, "login")
	fakePgrep(t, false)
	s := &Server{cmd: runningProcess(t)}
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

// Shell output is free text: a diff, a log line or this very source file can
// contain "Login incorrect". Only login(1) itself may count as a failure.
func TestCountAuthFailuresIgnoresShellOutput(t *testing.T) {
	fakePS(t, "zsh")
	s := &Server{cmd: runningProcess(t)}
	for i := 0; i < maxLoginFailures*2; i++ {
		if s.countAuthFailures([]byte("Login incorrect\r\n")) {
			t.Fatal("tripped after login: shell output must not count as a failed login")
		}
	}
	if s.authFailures != 0 {
		t.Fatalf("authFailures = %d, want 0", s.authFailures)
	}
}

// After authentication login(1) keeps its own name and waits on the shell, so
// the authenticated state must be read from the process tree: a child exists.
func TestCountAuthFailuresIgnoresOutputAfterLoginForksShell(t *testing.T) {
	fakePS(t, "login")
	fakePgrep(t, true)
	s := &Server{cmd: runningProcess(t)}
	for i := 0; i < maxLoginFailures*2; i++ {
		if s.countAuthFailures([]byte("Login incorrect\r\n")) {
			t.Fatal("tripped after the shell was forked: only a real login prompt may count")
		}
	}
}
