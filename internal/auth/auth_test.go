package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	issuer   = "https://cognito-idp.us-west-2.amazonaws.com/us-west-2_TESTPOOL"
	clientID = "test-client-id"
)

// testKeys holds a signing key and the public set a verifier would fetch.
type testKeys struct {
	private jwk.Key
	set     jwk.Set
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private, err := jwk.Import(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := private.Set(jwk.KeyIDKey, "kid-1"); err != nil {
		t.Fatal(err)
	}
	if err := private.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatal(err)
	}
	public, err := jwk.PublicKeyOf(private)
	if err != nil {
		t.Fatal(err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(public); err != nil {
		t.Fatal(err)
	}
	return testKeys{private: private, set: set}
}

type claims map[string]any

func (k testKeys) mint(t *testing.T, overrides claims) string {
	t.Helper()
	values := claims{
		"sub":       "user-sub-123",
		"iss":       issuer,
		"exp":       time.Now().Add(5 * time.Minute).Unix(),
		"token_use": "access",
		"client_id": clientID,
		"scope":     "msg/read msg/email:send",
	}
	for key, v := range overrides {
		if v == nil {
			delete(values, key)
		} else {
			values[key] = v
		}
	}
	tok := jwt.New()
	for key, v := range values {
		if err := tok.Set(key, v); err != nil {
			t.Fatal(err)
		}
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256(), k.private))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

func (k testKeys) verifier() *Verifier {
	return NewVerifier(issuer, []string{clientID}, KeySetFunc(func(context.Context) (jwk.Set, error) { return k.set, nil }))
}

func reason(t *testing.T, err error) string {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *auth.Error, got %v", err)
	}
	return e.Reason
}

func TestValidToken(t *testing.T) {
	k := newTestKeys(t)
	p, err := k.verifier().Verify(context.Background(), k.mint(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "user-sub-123" || p.ClientID != clientID || p.Method != "oauth" {
		t.Fatalf("principal: %+v", p)
	}
	if !p.HasScope("msg/read") || !p.HasScope("msg/email:send") || p.HasScope("msg/sms:send") {
		t.Fatalf("scopes: %v", p.Scopes)
	}
}

func TestVerificationMatrix(t *testing.T) {
	k := newTestKeys(t)
	other := newTestKeys(t)
	cases := []struct {
		name   string
		token  string
		reason string
	}{
		{"bad signature", other.mint(t, nil), "invalid_token"},
		{"expired", k.mint(t, claims{"exp": time.Now().Add(-time.Minute).Unix()}), "expired"},
		{"wrong issuer", k.mint(t, claims{"iss": "https://evil.example.com"}), "wrong_issuer"},
		{"missing sub", k.mint(t, claims{"sub": nil}), "missing_claim"},
		{"id token", k.mint(t, claims{"token_use": "id"}), "wrong_token_use"},
		{"no token_use", k.mint(t, claims{"token_use": nil}), "wrong_token_use"},
		{"unknown client", k.mint(t, claims{"client_id": "someone-else"}), "unknown_client"},
		{"no client_id", k.mint(t, claims{"client_id": nil}), "unknown_client"},
		{"garbage", "not.a.jwt", "invalid_token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := k.verifier().Verify(context.Background(), tc.token)
			if got := reason(t, err); got != tc.reason {
				t.Fatalf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestMissingScopeClaimYieldsNoScopes(t *testing.T) {
	k := newTestKeys(t)
	p, err := k.verifier().Verify(context.Background(), k.mint(t, claims{"scope": nil}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scopes) != 0 {
		t.Fatalf("scopes should be empty: %v", p.Scopes)
	}
}

func TestKeySetFailure(t *testing.T) {
	k := newTestKeys(t)
	v := NewVerifier(issuer, []string{clientID}, KeySetFunc(func(context.Context) (jwk.Set, error) {
		return nil, errors.New("jwks unreachable")
	}))
	_, err := v.Verify(context.Background(), k.mint(t, nil))
	if got := reason(t, err); got != "unresolvable_signing_key" {
		t.Fatalf("reason = %q", got)
	}
	if err.Error() == "" {
		t.Fatal("error message must not be empty")
	}
}

func TestJWKSProviderFetchesCachesAndRefreshes(t *testing.T) {
	k := newTestKeys(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		if hits == 3 {
			http.Error(w, "blip", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(k.set)
	}))
	defer srv.Close()

	now := time.Now()
	provider := NewJWKSProvider(srv.URL).(*jwksCache)
	provider.now = func() time.Time { return now }

	v := NewVerifier(issuer, []string{clientID}, provider)
	if _, err := v.Verify(context.Background(), k.mint(t, nil)); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	if _, err := v.Verify(context.Background(), k.mint(t, nil)); err != nil || hits != 1 {
		t.Fatalf("second verify must use the cache: err=%v hits=%d", err, hits)
	}
	now = now.Add(2 * time.Hour)
	if _, err := v.Verify(context.Background(), k.mint(t, nil)); err != nil || hits != 2 {
		t.Fatalf("expired cache must refetch: err=%v hits=%d", err, hits)
	}
	now = now.Add(2 * time.Hour)
	if _, err := v.Verify(context.Background(), k.mint(t, nil)); err != nil || hits != 3 {
		t.Fatalf("fetch blip must serve the stale set: err=%v hits=%d", err, hits)
	}
}

func TestJWKSProviderUnreachable(t *testing.T) {
	provider := NewJWKSProvider("http://127.0.0.1:9/.well-known/jwks.json")
	if _, err := provider.KeySet(context.Background()); err == nil {
		t.Fatal("unreachable JWKS must error when nothing is cached")
	}
}

func TestOriginSecret(t *testing.T) {
	if !OriginSecretOK("", "", false) || !OriginSecretOK("x", "", false) {
		t.Fatal("not required must pass")
	}
	if !OriginSecretOK("s3cret", "s3cret", true) {
		t.Fatal("match must pass")
	}
	if OriginSecretOK("wrong", "s3cret", true) || OriginSecretOK("", "s3cret", true) || OriginSecretOK("x", "", true) {
		t.Fatal("mismatch, missing header, and unconfigured secret must fail closed")
	}
}

func TestBreakGlass(t *testing.T) {
	token := "correct-horse-battery-staple"
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	p, ok := VerifyBreakGlass(token, digest, []string{"msg/read", "msg/email:send"})
	if !ok || p.Subject != "break-glass" || p.ClientID != "" || p.Method != "break_glass" || !p.HasScope("msg/email:send") {
		t.Fatalf("principal: %+v ok=%v", p, ok)
	}
	if _, ok := VerifyBreakGlass(token, "ABCDEF"+digest[6:], nil); ok {
		t.Fatal("wrong digest must not match")
	}
	if _, ok := VerifyBreakGlass("wrong", digest, nil); ok {
		t.Fatal("wrong token must not match")
	}
	if _, ok := VerifyBreakGlass(token, hexUpper(digest), nil); !ok {
		t.Fatal("digest comparison must be case-insensitive")
	}
}

func hexUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 32
		}
	}
	return string(b)
}

func TestRequireScope(t *testing.T) {
	p := Principal{Scopes: scopeSet([]string{"msg/read", ""})}
	if err := RequireScope(p, "msg/read"); err != nil {
		t.Fatal(err)
	}
	err := RequireScope(p, "msg/email:send")
	var se *ScopeError
	if !errors.As(err, &se) || se.Scope != "msg/email:send" || err.Error() != "caller lacks required scope 'msg/email:send'" {
		t.Fatalf("unexpected: %v", err)
	}
}
