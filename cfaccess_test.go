package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestCFAccessMiddlewareDev(t *testing.T) {
	os.Setenv("MODE", "development")
	defer os.Unsetenv("MODE")

	val := newCFAccessValidator()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	var secured http.Handler = handler
	if production() {
		secured = val.middleware(handler)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/config", nil)
	secured.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dev mode should pass without token, got %d", rec.Code)
	}
}

func TestCFAccessMiddlewareProd(t *testing.T) {
	os.Setenv("MODE", "production")
	defer os.Unsetenv("MODE")

	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kid := "test-key-id"
	teamDomain := "example.cloudflareaccess.com"
	aud := "test-aud"

	jwksSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nStr := base64.RawURLEncoding.EncodeToString(privKey.N.Bytes())
		eStr := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privKey.E)).Bytes())
		json.NewEncoder(w).Encode(map[string]interface{}{
			"keys": []map[string]string{
				{
					"kid": kid,
					"kty": "RSA",
					"n":   nStr,
					"e":   eStr,
				},
			},
		})
	}))
	defer jwksSrv.Close()

	os.Setenv("CF_TEAM_DOMAIN", teamDomain)
	os.Setenv("CF_ACCESS_AUD", aud)
	defer os.Unsetenv("CF_TEAM_DOMAIN")
	defer os.Unsetenv("CF_ACCESS_AUD")

	validator := newCFAccessValidator()
	validator.jwksURL = jwksSrv.URL

	handler := validator.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. Missing header -> 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/config", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("prod mode without header should 401, got %d", rec.Code)
	}

	// 2. Valid token -> 200
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"aud":   []string{aud},
		"iss":   "https://" + teamDomain,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"sub":   "user@example.com",
		"email": "user@example.com",
	})
	token.Header["kid"] = kid
	tokenStr, err := token.SignedString(privKey)
	if err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/config", nil)
	req.Header.Set("Cf-Access-Jwt-Assertion", tokenStr)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prod mode with valid token should 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
