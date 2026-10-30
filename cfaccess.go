package main

import (
	"crypto/rsa"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// production reports whether the gateway runs behind Cloudflare Access and must
// therefore authenticate every request. Anything else is development, where
// Cloudflare is not in front and devtools talks to the port directly.
func production() bool {
	return strings.EqualFold(os.Getenv("MODE"), "production")
}

// cfAccessValidator checks Cloudflare Access JWTs (and an optional static bearer
// token for scripts) against the team's published JWKS.
type cfAccessValidator struct {
	teamDomain string
	aud        string
	jwksURL    string
	staticTok  string

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

func newCFAccessValidator() *cfAccessValidator {
	team := strings.TrimPrefix(getEnv("CF_TEAM_DOMAIN", ""), "https://")
	team = strings.TrimSuffix(team, "/")
	return &cfAccessValidator{
		teamDomain: team,
		aud:        getEnv("CF_ACCESS_AUD", ""),
		jwksURL:    fmt.Sprintf("https://%s/cdn-cgi/access/certs", team),
		staticTok:  getEnv("GATEWAY_TOKEN", ""),
	}
}

// withAuth wraps a handler with Cloudflare Access checks when MODE=production.
// In development it is a no-op so devtools can hit the port directly.
func withAuth(next http.Handler) http.Handler {
	if !production() {
		return next
	}
	v := newCFAccessValidator()
	if v.teamDomain == "" || v.aud == "" {
		log.Fatal("[FATAL] MODE=production requires CF_TEAM_DOMAIN and CF_ACCESS_AUD")
	}
	return v.middleware(next)
}

// middleware rejects unauthenticated requests when MODE=production.
func (v *cfAccessValidator) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v.staticTok != "" && v.checkStatic(r) {
			next.ServeHTTP(w, r)
			return
		}
		raw := r.Header.Get("Cf-Access-Jwt-Assertion")
		if raw == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if err := v.verify(raw); err != nil {
			log.Printf("[WARN] Rejected request from %s: %v", r.RemoteAddr, err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (v *cfAccessValidator) checkStatic(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(got) == len(v.staticTok) &&
		subtle.ConstantTimeCompare([]byte(got), []byte(v.staticTok)) == 1
}

func (v *cfAccessValidator) verify(raw string) error {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(v.aud),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("https://"+v.teamDomain),
	)
	_, err := parser.ParseWithClaims(raw, jwt.MapClaims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(kid)
	})
	return err
}

func (v *cfAccessValidator) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	k := v.keys[kid]
	stale := time.Since(v.fetchedAt) > time.Hour
	v.mu.Unlock()

	if k != nil && !stale {
		return k, nil
	}
	if err := v.refresh(); err != nil {
		if k != nil {
			return k, nil // key rotation server unreachable: keep the known key
		}
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	return nil, errors.New("unknown signing key id")
}

func (v *cfAccessValidator) refresh() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(v.jwksURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS fetch: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}

	keys := make(map[string]*rsa.PublicKey, len(body.Keys))
	for _, k := range body.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(keys) == 0 {
		return errors.New("JWKS contained no RSA keys")
	}

	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}
