package settings

import (
	"context"
	"errors"
	"testing"
)

func mapLookup(m map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	s := FromEnv(mapLookup(nil))
	if s.Stage != "dev" || s.AWSRegion != "us-west-2" || s.AuthMetadataMode != "direct" {
		t.Fatalf("unexpected defaults: %+v", s)
	}
	if s.RequireOriginSecret || s.BreakGlassEnabled {
		t.Fatalf("enforcement must default to off: %+v", s)
	}
	if s.PublicBaseURL != "http://127.0.0.1:8000" {
		t.Fatalf("base url: %q", s.PublicBaseURL)
	}
	if len(s.BreakGlassScopes) != 1 || s.BreakGlassScopes[0] != "msg/read" {
		t.Fatalf("break-glass scopes: %v", s.BreakGlassScopes)
	}
}

func TestFromEnvReadsEverything(t *testing.T) {
	s := FromEnv(mapLookup(map[string]string{
		"STAGE":                 "prod",
		"AWS_REGION":            "us-east-1",
		"MCP_RESOURCE_URL":      "https://mcp.example.com/mcp/",
		"COGNITO_ISSUER":        "https://cognito-idp.us-west-2.amazonaws.com/pool",
		"COGNITO_DOMAIN":        "https://auth.example.com",
		"ALLOWED_CLIENT_IDS":    "client-a, client-b",
		"AUTH_METADATA_MODE":    "fronted",
		"REQUIRE_ORIGIN_SECRET": "TRUE",
		"ORIGIN_SECRET":         "shh",
		"BREAK_GLASS_ENABLED":   "true",
		"BREAK_GLASS_SHA256":    "abc123",
		"BREAK_GLASS_SCOPES":    "msg/read,msg/email:send",
	}))
	if s.PublicBaseURL != "https://mcp.example.com" {
		t.Fatalf("base url derived wrongly: %q", s.PublicBaseURL)
	}
	if len(s.AllowedClientIDs) != 2 || s.AllowedClientIDs[1] != "client-b" {
		t.Fatalf("client ids: %v", s.AllowedClientIDs)
	}
	if !s.RequireOriginSecret || s.OriginSecret != "shh" || !s.BreakGlassEnabled {
		t.Fatalf("flags: %+v", s)
	}
	if s.JWKSURL() != "https://cognito-idp.us-west-2.amazonaws.com/pool/.well-known/jwks.json" {
		t.Fatalf("jwks: %q", s.JWKSURL())
	}
	if len(s.BreakGlassScopes) != 2 {
		t.Fatalf("scopes: %v", s.BreakGlassScopes)
	}
}

func TestPublicBaseURLOverrideAndUnparsable(t *testing.T) {
	s := FromEnv(mapLookup(map[string]string{"MCP_RESOURCE_URL": "https://x/mcp/", "PUBLIC_BASE_URL": "https://y"}))
	if s.PublicBaseURL != "https://y" {
		t.Fatalf("override ignored: %q", s.PublicBaseURL)
	}
	s = FromEnv(mapLookup(map[string]string{"MCP_RESOURCE_URL": "not a url"}))
	if s.PublicBaseURL != "not a url" {
		t.Fatalf("unparsable url should pass through: %q", s.PublicBaseURL)
	}
}

func TestResolveOriginSecret(t *testing.T) {
	ctx := context.Background()
	base := Settings{}
	got, err := ResolveOriginSecret(ctx, base, mapLookup(nil), nil)
	if err != nil || got.OriginSecret != "" {
		t.Fatalf("no param should be a no-op: %v %+v", err, got)
	}
	literal := Settings{OriginSecret: "literal"}
	got, err = ResolveOriginSecret(ctx, literal, mapLookup(map[string]string{"ORIGIN_SECRET_PARAM": "/x"}), nil)
	if err != nil || got.OriginSecret != "literal" {
		t.Fatalf("literal must win: %v %+v", err, got)
	}
	var asked string
	fetch := func(_ context.Context, name string) (string, error) { asked = name; return "from-ssm", nil }
	got, err = ResolveOriginSecret(ctx, base, mapLookup(map[string]string{"ORIGIN_SECRET_PARAM": "/p"}), fetch)
	if err != nil || got.OriginSecret != "from-ssm" || asked != "/p" {
		t.Fatalf("fetch path: %v %+v asked=%q", err, got, asked)
	}
	failing := func(context.Context, string) (string, error) { return "", errors.New("denied") }
	if _, err = ResolveOriginSecret(ctx, base, mapLookup(map[string]string{"ORIGIN_SECRET_PARAM": "/p"}), failing); err == nil {
		t.Fatal("fetch error must propagate")
	}
}

func TestIntOr(t *testing.T) {
	s := FromEnv(mapLookup(map[string]string{"MAX_RECIPIENTS": "5", "RATE_LIMIT_PER_HOUR": "junk", "EMAIL_MAX_RAW_BYTES": ""}))
	if s.MaxRecipients != 5 {
		t.Fatalf("parsed int: %d", s.MaxRecipients)
	}
	if s.RateLimitPerHour != 20 {
		t.Fatalf("junk must fall back: %d", s.RateLimitPerHour)
	}
	if s.EmailMaxRawBytes != 10*1024*1024 {
		t.Fatalf("empty must fall back: %d", s.EmailMaxRawBytes)
	}
}

// The API is called with the ARN so authorization resolves the phone-number
// resource the SendSms policy names; an E.164 string does not, and every send
// was denied for the action rather than for the resource.
func TestSendingIdentityPrefersTheARN(t *testing.T) {
	arn := "arn:aws:sms-voice:us-west-2:1:phone-number/phone-abc"
	if got := (Settings{OriginationIdentity: "+18885550000", OriginationIdentityARN: arn}).SendingIdentity(); got != arn {
		t.Fatalf("SendingIdentity() = %q, want the ARN", got)
	}
	if got := (Settings{OriginationIdentity: "+18885550000"}).SendingIdentity(); got != "+18885550000" {
		t.Fatalf("with no ARN configured it falls back to the number: %q", got)
	}
}
