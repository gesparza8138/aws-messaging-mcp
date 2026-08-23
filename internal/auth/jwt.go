package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// jwksRefresh bounds how long a cached JWKS is used (PRD A8: <= 1 h).
const jwksRefresh = time.Hour

// KeySetProvider resolves the signing keys for token verification.
// Production uses a refreshing JWKS cache; tests supply a static set.
type KeySetProvider interface {
	KeySet(ctx context.Context) (jwk.Set, error)
}

// KeySetFunc adapts a function to KeySetProvider.
type KeySetFunc func(ctx context.Context) (jwk.Set, error)

// KeySet implements KeySetProvider.
func (f KeySetFunc) KeySet(ctx context.Context) (jwk.Set, error) { return f(ctx) }

// Error is a bearer-token verification failure with a structured reason for
// logs. Only a generic 401 reaches the caller.
type Error struct{ Reason string }

func (e *Error) Error() string { return "token verification failed: " + e.Reason }

// Verifier validates Cognito access tokens (PRD A8).
type Verifier struct {
	issuer  string
	allowed map[string]struct{}
	keys    KeySetProvider
}

// NewVerifier configures a Verifier for issuer and the allow-listed client ids.
func NewVerifier(issuer string, allowedClientIDs []string, keys KeySetProvider) *Verifier {
	return &Verifier{issuer: issuer, allowed: scopeSet(allowedClientIDs), keys: keys}
}

// NewJWKSProvider returns a KeySetProvider that fetches the JWKS at jwksURL
// on first use and re-fetches once the cached set is older than jwksRefresh.
// Fetching is lazy so a cold start never blocks on Cognito before the first
// token arrives, and a transient fetch failure surfaces as a 401 rather than
// a crashed function.
func NewJWKSProvider(jwksURL string) KeySetProvider {
	return &jwksCache{url: jwksURL, ttl: jwksRefresh, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}
}

type jwksCache struct {
	url    string
	ttl    time.Duration
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	set       jwk.Set
	fetchedAt time.Time
}

// KeySet implements KeySetProvider.
func (c *jwksCache) KeySet(ctx context.Context) (jwk.Set, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set != nil && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.set, nil
	}
	set, err := jwk.Fetch(ctx, c.url, jwk.WithHTTPClient(c.client))
	if err != nil {
		if c.set != nil {
			return c.set, nil // serve the stale set rather than fail closed on a blip
		}
		return nil, fmt.Errorf("fetch jwks %s: %w", c.url, err)
	}
	c.set, c.fetchedAt = set, c.now()
	return set, nil
}

// Verify checks signature (RS256 against the key set), iss, exp,
// token_use == "access", and an allow-listed client_id, then returns the
// caller with its scopes parsed from the space-separated scope claim.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	set, err := v.keys.KeySet(ctx)
	if err != nil {
		return Principal{}, &Error{Reason: "unresolvable_signing_key"}
	}
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(set, jws.WithInferAlgorithmFromKey(true)),
		jwt.WithIssuer(v.issuer),
		jwt.WithRequiredClaim("sub"),
		jwt.WithValidate(true),
	)
	if err != nil {
		return Principal{}, &Error{Reason: classify(err)}
	}
	var tokenUse string
	if err := tok.Get("token_use", &tokenUse); err != nil || tokenUse != "access" {
		return Principal{}, &Error{Reason: "wrong_token_use"}
	}
	var clientID string
	if err := tok.Get("client_id", &clientID); err != nil {
		return Principal{}, &Error{Reason: "unknown_client"}
	}
	if _, ok := v.allowed[clientID]; !ok {
		return Principal{}, &Error{Reason: "unknown_client"}
	}
	var scope string
	_ = tok.Get("scope", &scope) // absent scope claim means no scopes
	sub, _ := tok.Subject()
	return Principal{
		Subject:  sub,
		ClientID: clientID,
		Scopes:   scopeSet(strings.Fields(scope)),
		Method:   "oauth",
	}, nil
}

func classify(err error) string {
	switch {
	case errors.Is(err, jwt.TokenExpiredError()):
		return "expired"
	case errors.Is(err, jwt.InvalidIssuerError()):
		return "wrong_issuer"
	case errors.Is(err, jwt.MissingRequiredClaimError()):
		return "missing_claim"
	default:
		return "invalid_token"
	}
}
