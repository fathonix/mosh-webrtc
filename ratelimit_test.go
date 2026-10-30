package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	rl := newRateLimiter(3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4", now) {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if rl.allow("1.2.3.4", now) {
		t.Fatal("4th request in the window should be rejected")
	}
	if !rl.allow("5.6.7.8", now) {
		t.Fatal("a different IP must have its own budget")
	}
	if !rl.allow("1.2.3.4", now.Add(time.Minute+time.Second)) {
		t.Fatal("budget must reset after the window")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := newRateLimiter(0)
	now := time.Now()
	for i := 0; i < 100; i++ {
		if !rl.allow("1.2.3.4", now) {
			t.Fatal("limit 0 must disable the limiter")
		}
	}
}

func TestClientIPPrefersCloudflareHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/offer", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Fatalf("got %q", got)
	}
}
