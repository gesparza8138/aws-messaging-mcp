// Package settings resolves the server configuration from environment
// variables so the same binary runs unchanged on Lambda (variables set by
// CloudFormation) and locally (variables set by the shell or `make dev`).
package settings

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// ScopesSupported lists the resource-server scopes (PRD A6) in the order
// advertised in the protected-resource metadata.
var ScopesSupported = []string{
	"msg/read",
	"msg/email:send",
	"msg/sms:send",
	"msg/rcs:send",
	"msg/files:write",
}

const (
	defaultStage       = "dev"
	defaultRegion      = "us-west-2"
	defaultResourceURL = "http://127.0.0.1:8000/mcp/"
)

// Settings is the immutable server configuration.
type Settings struct {
	// Stage is the deployment stage, dev or prod.
	Stage string
	// AWSRegion is the region the server operates in.
	AWSRegion string
	// MCPResourceURL is the canonical RFC 9728 resource identifier.
	MCPResourceURL string
	// PublicBaseURL is the origin clients reach the server on.
	PublicBaseURL string
	// CognitoIssuer is the user-pool issuer URL (the JWT iss claim).
	CognitoIssuer string
	// CognitoDomain is the hosted-UI base URL (authorize/token endpoints).
	CognitoDomain string
	// AllowedClientIDs are the app-client ids accepted in the client_id claim.
	AllowedClientIDs []string
	// AuthMetadataMode is "direct" (advertise Cognito) or "fronted"
	// (advertise this host, whose RFC 8414 document adds
	// code_challenge_methods_supported - PRD R2).
	AuthMetadataMode string
	// RequireOriginSecret enforces the CloudFront origin-secret header.
	RequireOriginSecret bool
	// OriginSecret is the expected X-Origin-Secret value; empty means unset.
	OriginSecret string
	// BreakGlassEnabled activates the static bearer fallback (PRD A7).
	BreakGlassEnabled bool
	// BreakGlassSHA256 is the hex digest of the break-glass token.
	BreakGlassSHA256 string
	// BreakGlassScopes are granted to a break-glass caller.
	BreakGlassScopes []string
	// SESConfigurationSet is injected into every send (never caller-set).
	SESConfigurationSet string
	// SESReplyTo is the default Reply-To when the caller omits one.
	SESReplyTo string
	// SESSenderAddresses is the sender allow-list (PRD §8).
	SESSenderAddresses []string
	// RecipientAllowList restricts recipients when non-empty ("test mode").
	RecipientAllowList []string
	// MaxRecipients caps recipients per email.
	MaxRecipients int
	// RateLimitPerHour / RateLimitPerDay bound sends per tool (0 = off).
	RateLimitPerHour int
	RateLimitPerDay  int
	// EmailMaxRawBytes caps the decoded size of Content.Raw.
	EmailMaxRawBytes int
	// RateLimitTable is the DynamoDB counters table name.
	RateLimitTable string
}

// JWKSURL returns the Cognito JWKS endpoint for the configured issuer.
func (s Settings) JWKSURL() string {
	return s.CognitoIssuer + "/.well-known/jwks.json"
}

// Lookup reads one variable; it matches the signature of os.LookupEnv.
type Lookup func(key string) (string, bool)

// FromEnv builds Settings from environment variables via lookup.
func FromEnv(lookup Lookup) Settings {
	get := func(key, fallback string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return fallback
	}
	resourceURL := get("MCP_RESOURCE_URL", defaultResourceURL)
	return Settings{
		Stage:               get("STAGE", defaultStage),
		AWSRegion:           get("AWS_REGION", defaultRegion),
		MCPResourceURL:      resourceURL,
		PublicBaseURL:       get("PUBLIC_BASE_URL", baseOf(resourceURL)),
		CognitoIssuer:       get("COGNITO_ISSUER", ""),
		CognitoDomain:       get("COGNITO_DOMAIN", ""),
		AllowedClientIDs:    splitCSV(get("ALLOWED_CLIENT_IDS", "")),
		AuthMetadataMode:    get("AUTH_METADATA_MODE", "direct"),
		RequireOriginSecret: strings.EqualFold(get("REQUIRE_ORIGIN_SECRET", "false"), "true"),
		OriginSecret:        get("ORIGIN_SECRET", ""),
		BreakGlassEnabled:   strings.EqualFold(get("BREAK_GLASS_ENABLED", "false"), "true"),
		BreakGlassSHA256:    get("BREAK_GLASS_SHA256", ""),
		BreakGlassScopes:    splitCSV(get("BREAK_GLASS_SCOPES", "msg/read")),
		SESConfigurationSet: get("SES_CONFIGURATION_SET", ""),
		SESReplyTo:          get("SES_REPLY_TO", ""),
		SESSenderAddresses:  splitCSV(get("SES_SENDER_ADDRESSES", "")),
		RecipientAllowList:  splitCSV(get("RECIPIENT_ALLOW_LIST", "")),
		MaxRecipients:       intOr(get("MAX_RECIPIENTS", ""), 10),
		RateLimitPerHour:    intOr(get("RATE_LIMIT_PER_HOUR", ""), 20),
		RateLimitPerDay:     intOr(get("RATE_LIMIT_PER_DAY", ""), 300),
		EmailMaxRawBytes:    intOr(get("EMAIL_MAX_RAW_BYTES", ""), 10*1024*1024),
		RateLimitTable:      get("RATE_LIMIT_TABLE", ""),
	}
}

func intOr(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	n := 0
	for _, c := range value {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ParameterFetcher fetches a decrypted SSM parameter by name.
type ParameterFetcher func(ctx context.Context, name string) (string, error)

// ResolveOriginSecret fills OriginSecret from the SSM parameter named by
// ORIGIN_SECRET_PARAM when no literal secret is set (cold-start resolution,
// PRD S1/S2). A missing parameter name leaves the settings unchanged.
func ResolveOriginSecret(ctx context.Context, s Settings, lookup Lookup, fetch ParameterFetcher) (Settings, error) {
	name, _ := lookup("ORIGIN_SECRET_PARAM")
	if s.OriginSecret != "" || name == "" {
		return s, nil
	}
	value, err := fetch(ctx, name)
	if err != nil {
		return s, fmt.Errorf("resolve origin secret %q: %w", name, err)
	}
	s.OriginSecret = value
	return s, nil
}

func baseOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

func splitCSV(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
