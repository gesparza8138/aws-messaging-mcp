// Package httpapi wires the HTTP surface: health, OAuth metadata, the auth
// middleware chain, and the MCP endpoint (PRD 4.1).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
)

// TokenVerifier verifies a bearer token; satisfied by *auth.Verifier.
type TokenVerifier interface {
	Verify(ctx context.Context, token string) (auth.Principal, error)
}

// Config assembles the server.
type Config struct {
	Settings settings.Settings
	Verifier TokenVerifier
	// MCP is the Streamable HTTP handler; it is mounted at /mcp/ and /mcp.
	MCP http.Handler
}

type principalKey struct{}

// PrincipalFrom returns the authenticated caller attached by the middleware.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(auth.Principal)
	return p, ok
}

// WithPrincipal attaches p to ctx; the middleware uses it, and tool tests use
// it to simulate an authenticated request.
func WithPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// NewHandler builds the complete http.Handler. No redirects are ever issued:
// the Host header at the Lambda is the raw Function URL, so a redirect would
// leak it past CloudFront.
func NewHandler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "stage": cfg.Settings.Stage})
	})
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, ProtectedResourceDocument(cfg.Settings))
		})
	}
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/oauth"} {
		mux.HandleFunc("GET "+p, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, AuthorizationServerDocument(cfg.Settings))
		})
	}
	// Both spellings dispatch to the same handler; the MCP handler ignores the path.
	mux.Handle("/mcp", cfg.MCP)
	mux.Handle("/mcp/", cfg.MCP)
	// Public informational pages (edge allow-list exempts them): the landing
	// page and the SMS opt-in disclosure that the toll-free verification form
	// references (PRD M3-5).
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		writeHTML(w, indexPage())
	})
	mux.HandleFunc("GET /opt-in", func(w http.ResponseWriter, _ *http.Request) {
		writeHTML(w, optInPage(cfg.Settings.OptInPhoneNumber))
	})
	return authMiddleware(cfg, mux)
}

func writeHTML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

func indexPage() string {
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Gabriel Esparza — personal messaging service</title></head><body>
<h1>Personal messaging service</h1>
<p>This is a private, single-owner notification service operated by Gabriel Esparza.
It sends one-off transactional email and text messages on the owner's behalf —
reminders, confirmations, and links to documents the recipient asked for.
There is no marketing, no recurring campaigns, and no public sign-up.</p>
<p><a href="/opt-in">SMS opt-in and opt-out details</a></p>
</body></html>`
}

func optInPage(phoneNumber string) string {
	from := "our toll-free number"
	if phoneNumber != "" {
		from = phoneNumber
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>SMS opt-in — Gabriel Esparza</title></head><body>
<h1>SMS opt-in</h1>
<p>By asking Gabriel Esparza — in person, by phone, or in writing — to send you
information by text, you agree to receive a one-time message from ` + from + `.
Message and data rates may apply. Message frequency varies with your requests;
there are no recurring messages.</p>
<p>Reply <strong>STOP</strong> to opt out at any time, or <strong>HELP</strong> for help.</p>
</body></html>`
}

func authMiddleware(cfg Config, next http.Handler) http.Handler {
	s := cfg.Settings
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.OriginSecretOK(r.Header.Get("X-Origin-Secret"), s.OriginSecret, s.RequireOriginSecret) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		path := r.URL.Path
		if path == "/healthz" || path == "/" || path == "/opt-in" || strings.HasPrefix(path, "/.well-known/") {
			next.ServeHTTP(w, r)
			return
		}
		principal, ok := authenticate(r.Context(), cfg, r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+s.PublicBaseURL+`/.well-known/oauth-protected-resource"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

// authenticate resolves the bearer token: break-glass first (if enabled),
// then the Cognito verifier.
func authenticate(ctx context.Context, cfg Config, authorization string) (auth.Principal, bool) {
	const prefix = "bearer "
	if len(authorization) <= len(prefix) || !strings.EqualFold(authorization[:len(prefix)], prefix) {
		return auth.Principal{}, false
	}
	token := strings.TrimSpace(authorization[len(prefix):])
	if token == "" {
		return auth.Principal{}, false
	}
	s := cfg.Settings
	if s.BreakGlassEnabled && s.BreakGlassSHA256 != "" {
		if p, ok := auth.VerifyBreakGlass(token, s.BreakGlassSHA256, s.BreakGlassScopes); ok {
			return p, true
		}
	}
	p, err := cfg.Verifier.Verify(ctx, token)
	if err != nil {
		return auth.Principal{}, false
	}
	return p, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
