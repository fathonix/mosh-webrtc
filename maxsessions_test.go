package main

import "testing"

func TestMaxSessions(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", 8},
		{"3", 3},
		{"0", 8},
		{"nope", 8},
	}
	for _, c := range cases {
		t.Setenv("MAX_SESSIONS", c.env)
		if got := maxSessions(); got != c.want {
			t.Fatalf("env %q: got %d want %d", c.env, got, c.want)
		}
	}
}
