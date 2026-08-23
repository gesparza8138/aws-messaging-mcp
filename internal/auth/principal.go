// Package auth implements the request authentication chain (PRD 4.1 step 5):
// origin-secret check, then bearer verification (break-glass hash or Cognito
// JWT), then per-tool scope enforcement.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// Principal is an authenticated caller.
type Principal struct {
	// Subject is the stable caller id: the Cognito sub, or "break-glass".
	Subject string
	// ClientID is the OAuth client the token was issued to; empty for break-glass.
	ClientID string
	// Scopes the caller holds, e.g. msg/read.
	Scopes map[string]struct{}
	// Method is "oauth" or "break_glass".
	Method string
}

// HasScope reports whether the principal holds scope.
func (p Principal) HasScope(scope string) bool {
	_, ok := p.Scopes[scope]
	return ok
}

// ScopeError is returned when a caller lacks a required scope. Its message
// is safe to return to the model so it can explain the refusal.
type ScopeError struct{ Scope string }

func (e *ScopeError) Error() string {
	return fmt.Sprintf("caller lacks required scope '%s'", e.Scope)
}

// RequireScope returns a *ScopeError unless p holds scope (PRD A6).
func RequireScope(p Principal, scope string) error {
	if !p.HasScope(scope) {
		return &ScopeError{Scope: scope}
	}
	return nil
}

// OriginSecretOK checks the CloudFront origin-secret header in constant time
// (PRD A9). When enforcement is on, a missing configured secret fails closed.
func OriginSecretOK(provided, expected string, required bool) bool {
	if !required {
		return true
	}
	if expected == "" || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// VerifyBreakGlass checks token against the stored SHA-256 hex digest in
// constant time (PRD A7). It returns (principal, true) on a match.
func VerifyBreakGlass(token, expectedSHA256Hex string, scopes []string) (Principal, bool) {
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(digest), []byte(strings.ToLower(expectedSHA256Hex))) != 1 {
		return Principal{}, false
	}
	return Principal{
		Subject: "break-glass",
		Scopes:  scopeSet(scopes),
		Method:  "break_glass",
	}, true
}

func scopeSet(scopes []string) map[string]struct{} {
	set := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}
