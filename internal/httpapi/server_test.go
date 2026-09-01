package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/mcpserver"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
	"github.com/gesparza8138/aws-messaging-mcp/internal/testkeys"
)

const (
	issuer         = "https://cognito-idp.us-west-2.amazonaws.com/us-west-2_ITPOOL"
	clientID       = "integration-client"
	originSecret   = "integration-origin-secret"
	breakGlassTok  = "integration-break-glass-token"
	cognitoDomain  = "https://auth.test.example.com"
	metadataDirect = "direct"
)

type fixture struct {
	srv  *httptest.Server
	keys *testkeys.Keys
	cfg  settings.Settings
}

func newFixture(t *testing.T, mode string) *fixture {
	t.Helper()
	keys, err := testkeys.New(issuer, clientID)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(breakGlassTok))
	// The base URL is only known after the server starts; it is set below.
	cfg := settings.Settings{
		Stage:               "test",
		CognitoIssuer:       issuer,
		CognitoDomain:       cognitoDomain,
		AllowedClientIDs:    []string{clientID},
		AuthMetadataMode:    mode,
		RequireOriginSecret: true,
		OriginSecret:        originSecret,
		BreakGlassEnabled:   true,
		BreakGlassSHA256:    hex.EncodeToString(sum[:]),
		BreakGlassScopes:    []string{"msg/read"},
	}
	var handler http.Handler
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	srv.Start()
	cfg.MCPResourceURL = srv.URL + "/mcp/"
	cfg.PublicBaseURL = srv.URL
	handler = httpapi.NewHandler(httpapi.Config{
		Settings: cfg,
		Verifier: auth.NewVerifier(issuer, []string{clientID}, keys.Provider()),
		MCP:      mcpserver.NewHandler(mcpserver.Deps{Settings: cfg}),
	})
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, keys: keys, cfg: cfg}
}

// newFixtureWithSES is newFixture plus a fake SES client so the email tools
// register; email guardrail config mirrors the mcpserver unit tests.
func newFixtureWithSES(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, metadataDirect)
	cfg := f.cfg
	cfg.SESConfigurationSet = "cfgset"
	cfg.SESReplyTo = "owner@example.com"
	cfg.SESSenderAddresses = []string{"mcp-dev@example.com"}
	cfg.RecipientAllowList = []string{"owner@example.com"}
	cfg.MaxRecipients = 5
	cfg.EmailMaxRawBytes = 1024
	handler := httpapi.NewHandler(httpapi.Config{
		Settings: cfg,
		Verifier: auth.NewVerifier(issuer, []string{clientID}, f.keys.Provider()),
		MCP:      mcpserver.NewHandler(mcpserver.Deps{Settings: cfg, SES: stubSES{}}),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	cfg.MCPResourceURL = srv.URL + "/mcp/"
	cfg.PublicBaseURL = srv.URL
	return &fixture{srv: srv, keys: f.keys, cfg: cfg}
}

func (f *fixture) do(t *testing.T, method, path, token string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Origin-Secret", originSecret)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestOriginSecretEnforced(t *testing.T) {
	f := newFixture(t, metadataDirect)
	for _, h := range []string{"", "wrong"} {
		req, _ := http.NewRequest(http.MethodGet, f.srv.URL+"/healthz", nil)
		if h != "" {
			req.Header.Set("X-Origin-Secret", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("header %q: status %d", h, resp.StatusCode)
		}
	}
	resp := f.do(t, http.MethodGet, "/healthz", "", "")
	if resp.StatusCode != http.StatusOK || decode(t, resp)["stage"] != "test" {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

// The landing and opt-in pages are public: no bearer token, HTML content,
// and the opt-in text degrades gracefully while no number is configured
// (PRD M3-5). The origin secret is still enforced (TestOriginSecretEnforced
// covers every route through the same middleware).
func TestPublicPages(t *testing.T) {
	f := newFixture(t, metadataDirect)
	for path, want := range map[string]string{"/": "/opt-in", "/opt-in": "our toll-free number"} {
		// The unconfigured fixture proves both contact lines degrade away
		// rather than rendering an empty mailto: or a bare "sent from".
		resp := f.do(t, http.MethodGet, path, "", "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s: status %d type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s: body lacks %q", path, want)
		}
	}
	// Unknown root-adjacent paths still require auth ("/" must not swallow them).
	if resp := f.do(t, http.MethodGet, "/anything", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/anything: %d, want 401", resp.StatusCode)
	}
}

// The landing page carries the contact details the toll-free verification
// reviewers cross-check against the registration: the support email and the
// sending number, both from settings so they cannot drift from what the
// server uses. The number is described as send-only because the toll-free
// has no voice capability.
func TestLandingPageContactDetails(t *testing.T) {
	f := newFixture(t, metadataDirect)
	cfg := f.cfg
	cfg.SESReplyTo = "owner@example.com"
	cfg.OptInPhoneNumber = "+18885550000"
	handler := httpapi.NewHandler(httpapi.Config{
		Settings: cfg,
		Verifier: auth.NewVerifier(issuer, []string{clientID}, f.keys.Provider()),
		MCP:      mcpserver.NewHandler(mcpserver.Deps{Settings: cfg, SES: stubSES{}}),
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
	req.Header.Set("X-Origin-Secret", cfg.OriginSecret)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		"owner@example.com",
		`mailto:owner@example.com`,
		"+18885550000",
		"does not accept voice calls",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("landing page lacks %q:\n%s", want, body)
		}
	}
}

func TestUnauthenticated401Contract(t *testing.T) {
	f := newFixture(t, metadataDirect)
	for _, path := range []string{"/mcp/", "/mcp"} {
		resp := f.do(t, http.MethodPost, path, "", "{}")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		want := `Bearer resource_metadata="` + f.srv.URL + `/.well-known/oauth-protected-resource"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Fatalf("%s: WWW-Authenticate %q", path, got)
		}
	}
	resp := f.do(t, http.MethodPost, "/mcp/", "nonsense", "{}")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("garbage token: %d", resp.StatusCode)
	}
	// Bearer prefix is case-insensitive; an empty token is rejected.
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp/", strings.NewReader("{}"))
	req.Header.Set("X-Origin-Secret", originSecret)
	req.Header.Set("Authorization", "BEARER   ")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty bearer: %d", resp2.StatusCode)
	}
}

func TestNoRedirectsEver(t *testing.T) {
	f := newFixture(t, metadataDirect)
	tok, _ := f.keys.Mint(nil)
	for _, path := range []string{"/mcp", "/mcp/"} {
		resp := f.do(t, http.MethodPost, path, tok, "{}")
		if resp.StatusCode >= 300 && resp.StatusCode < 400 || resp.StatusCode == http.StatusNotFound {
			t.Fatalf("%s with token: unexpected %d", path, resp.StatusCode)
		}
	}
	resp := f.do(t, http.MethodGet, "/.well-known/oauth-protected-resource/", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("trailing slash on an exact route must 404, got %d", resp.StatusCode)
	}
}

func TestMetadataDocuments(t *testing.T) {
	f := newFixture(t, metadataDirect)
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		doc := decode(t, f.do(t, http.MethodGet, path, "", ""))
		if doc["resource"] != f.cfg.MCPResourceURL {
			t.Fatalf("%s resource: %v", path, doc["resource"])
		}
		if as := doc["authorization_servers"].([]any); as[0] != issuer {
			t.Fatalf("%s authorization_servers: %v", path, as)
		}
		if scopes := doc["scopes_supported"].([]any); len(scopes) != len(settings.ScopesSupported) {
			t.Fatalf("%s scopes: %v", path, scopes)
		}
	}
	as := decode(t, f.do(t, http.MethodGet, "/.well-known/oauth-authorization-server", "", ""))
	if as["issuer"] != issuer || as["authorization_endpoint"] != cognitoDomain+"/oauth2/authorize" ||
		as["token_endpoint"] != cognitoDomain+"/oauth2/token" || as["jwks_uri"] != f.cfg.JWKSURL() {
		t.Fatalf("authorization-server doc: %v", as)
	}
	if pkce := as["code_challenge_methods_supported"].([]any); len(pkce) != 1 || pkce[0] != "S256" {
		t.Fatalf("pkce: %v", pkce)
	}
	fronted := newFixture(t, "fronted")
	doc := decode(t, fronted.do(t, http.MethodGet, "/.well-known/oauth-protected-resource", "", ""))
	if as := doc["authorization_servers"].([]any); as[0] != fronted.srv.URL+"/oauth" {
		t.Fatalf("fronted authorization_servers: %v", as)
	}
	suffixed := decode(t, fronted.do(t, http.MethodGet, "/.well-known/oauth-authorization-server/oauth", "", ""))
	if suffixed["issuer"] != issuer {
		t.Fatalf("suffixed AS doc: %v", suffixed)
	}
}

func callHello(t *testing.T, f *fixture, token, path, name string) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   f.srv.URL + path,
		HTTPClient: &http.Client{Transport: headerTransport{token: token}},
	}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "hello" {
		t.Fatalf("tools: %+v", tools.Tools)
	}
	args := map[string]any{}
	if name != "" {
		args["name"] = name
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hello", Arguments: args})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	return result
}

type headerTransport struct{ token string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Origin-Secret", originSecret)
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}

func structured(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOAuthRoundTripBothPaths(t *testing.T) {
	f := newFixture(t, metadataDirect)
	tok, _ := f.keys.Mint(nil)
	for _, path := range []string{"/mcp/", "/mcp"} {
		result := callHello(t, f, tok, path, "Gabe")
		if result.IsError {
			t.Fatalf("%s: tool error: %+v", path, result.Content)
		}
		out := structured(t, result)
		if out["message"] != "Hello, Gabe!" || out["stage"] != "test" || out["caller"] != "integration-user" || out["auth_method"] != "oauth" {
			t.Fatalf("%s: %v", path, out)
		}
	}
}

func TestBreakGlassRoundTrip(t *testing.T) {
	f := newFixture(t, metadataDirect)
	result := callHello(t, f, breakGlassTok, "/mcp/", "")
	out := structured(t, result)
	if result.IsError || out["auth_method"] != "break_glass" || out["caller"] != "break-glass" || out["message"] != "Hello, world!" {
		t.Fatalf("%v %v", result.IsError, out)
	}
}

func TestMissingScopeIsToolErrorNot401(t *testing.T) {
	f := newFixture(t, metadataDirect)
	tok, _ := f.keys.Mint(testkeys.Claims{"scope": "msg/email:send"})
	result := callHello(t, f, tok, "/mcp/", "")
	if !result.IsError {
		t.Fatal("expected isError result")
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "msg/read") {
		t.Fatalf("error text: %q", text)
	}
}

func TestPrincipalFromEmptyContext(t *testing.T) {
	if _, ok := httpapi.PrincipalFrom(context.Background()); ok {
		t.Fatal("no principal expected")
	}
}

var _ = io.EOF

// emailSession connects the real MCP client to a fixture carrying the SES
// tools, over the full HTTP + auth chain.
func emailSession(t *testing.T, f *fixture) *mcp.ClientSession {
	t.Helper()
	tok, _ := f.keys.Mint(testkeys.Claims{"scope": "msg/read msg/email:send"})
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   f.srv.URL + "/mcp/",
		HTTPClient: &http.Client{Transport: headerTransport{token: tok}},
	}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// sesDryRunThroughFullChain exercises an email tool end to end: HTTP, auth
// middleware, scope check, guardrails, DryRun injection.
func TestSESDryRunThroughFullChain(t *testing.T) {
	f := newFixtureWithSES(t)
	ctx := context.Background()
	session := emailSession(t, f)
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 4 {
		t.Fatalf("tools: %v %v", err, tools)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email", Arguments: map[string]any{
		"FromEmailAddress": "mcp-dev@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"owner@example.com"}},
		"Content":          map[string]any{"Simple": map[string]any{"Subject": map[string]any{"Data": "s"}, "Body": map[string]any{"Text": map[string]any{"Data": "b"}}}},
		"DryRun":           true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %+v", err, result)
	}
	out := structured(t, result)
	meta := out["ServerMetadata"].(map[string]any)
	if meta["dry_run"] != true {
		t.Fatalf("metadata: %v", meta)
	}
	if out["WouldCall"] == nil {
		t.Fatalf("WouldCall missing: %v", out)
	}
	if _, ok := meta["content_digests"]; ok {
		t.Fatalf("a text-only email has no binary part to digest: %v", meta)
	}
}

// TestSESContentDigestThroughFullChain proves the digest survives the JSON
// round trip the client actually reads it from. The attachment here is an
// ordinary one, so the call stays on SES's Simple path; the inline shape is
// the test below.
func TestSESContentDigestThroughFullChain(t *testing.T) {
	f := newFixtureWithSES(t)
	png := []byte("\x89PNG not really a png")
	result, err := emailSession(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "ses_send_email", Arguments: map[string]any{
		"FromEmailAddress": "mcp-dev@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"owner@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "s"},
			"Body":    map[string]any{"Html": map[string]any{"Data": `<p>the logo is attached</p>`}},
			"Attachments": []any{map[string]any{
				"FileName": "logo.png", "ContentType": "image/png", "ContentDisposition": "ATTACHMENT",
				"RawContent": base64.StdEncoding.EncodeToString(png),
			}},
		}},
		"DryRun": true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %+v", err, result)
	}
	out := structured(t, result)
	digests := out["ServerMetadata"].(map[string]any)["content_digests"].([]any)
	if len(digests) != 1 {
		t.Fatalf("digests: %v", digests)
	}
	sum := sha256.Sum256(png)
	first := digests[0].(map[string]any)
	if first["part"] != "attachment[0]:logo.png" || first["sha256"] != hex.EncodeToString(sum[:]) || first["bytes"] != float64(len(png)) {
		t.Fatalf("digest: %v", first)
	}
	// The echo carries the decoded bytes back as base64, which only validates
	// against the output schema because of the []byte override in mcpserver.
	att := out["WouldCall"].(map[string]any)["Content"].(map[string]any)["Simple"].(map[string]any)["Attachments"].([]any)[0]
	if att.(map[string]any)["RawContent"] != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("WouldCall attachment bytes: %v", att)
	}
}

// TestSESInlineAssemblyThroughFullChain is the assembled path through the MCP
// client, which is the only place the SDK validates the result against the
// declared output schema: the echo moves from Content.Simple to Content.Raw
// here, and its Data is the third []byte the override has to describe as a
// base64 string (EC-8). It also proves the caller can read the tree back.
func TestSESInlineAssemblyThroughFullChain(t *testing.T) {
	f := newFixtureWithSES(t)
	png := []byte("\x89PNG not really a png")
	result, err := emailSession(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "ses_send_email", Arguments: map[string]any{
		"FromEmailAddress": "mcp-dev@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"owner@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "s"},
			"Body":    map[string]any{"Html": map[string]any{"Data": `<img src="cid:logo">`}},
			"Attachments": []any{map[string]any{
				"FileName": "logo.png", "ContentType": "image/png", "ContentDisposition": "INLINE", "ContentId": "logo",
				"RawContent": base64.StdEncoding.EncodeToString(png),
			}},
		}},
		"DryRun": true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %+v", err, result)
	}
	out := structured(t, result)
	content := out["WouldCall"].(map[string]any)["Content"].(map[string]any)
	if content["Simple"] != nil {
		t.Fatalf("an inline send is assembled, so the echo is Content.Raw: %v", content)
	}
	encoded, ok := content["Raw"].(map[string]any)["Data"].(string)
	if !ok {
		t.Fatalf("Content.Raw.Data must come back as a base64 string: %v", content)
	}
	msg, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("Content.Raw.Data: %v", err)
	}
	if !strings.Contains(string(msg), "multipart/related") || !strings.Contains(string(msg), "Content-ID: <logo>") {
		t.Fatalf("the assembled message must put the image in a related group:\n%s", msg)
	}
	// One digest per attachment, plus one for the message they were assembled
	// into.
	digests := out["ServerMetadata"].(map[string]any)["content_digests"].([]any)
	if len(digests) != 2 {
		t.Fatalf("digests: %v", digests)
	}
	whole := digests[1].(map[string]any)
	sum := sha256.Sum256(msg)
	if whole["part"] != "assembled" || whole["sha256"] != hex.EncodeToString(sum[:]) || whole["bytes"] != float64(len(msg)) {
		t.Fatalf("assembled digest: %v", whole)
	}
}

// TestSESRawDryRunThroughFullChain guards the same output-schema override on
// the Raw path, whose Data field is the other []byte in the DryRun echo.
func TestSESRawDryRunThroughFullChain(t *testing.T) {
	f := newFixtureWithSES(t)
	mime := []byte("From: mcp-dev@example.com\r\nTo: owner@example.com\r\nSubject: s\r\n\r\nb\r\n")
	result, err := emailSession(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "ses_send_email", Arguments: map[string]any{
		"FromEmailAddress": "mcp-dev@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"owner@example.com"}},
		"Content":          map[string]any{"Raw": map[string]any{"Data": base64.StdEncoding.EncodeToString(mime)}},
		"DryRun":           true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("call: %v %+v", err, result)
	}
	out := structured(t, result)
	digests := out["ServerMetadata"].(map[string]any)["content_digests"].([]any)
	sum := sha256.Sum256(mime)
	if len(digests) != 1 || digests[0].(map[string]any)["part"] != "raw" || digests[0].(map[string]any)["sha256"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("raw digest: %v", digests)
	}
}

// TestSESAttachByReferenceThroughFullChain proves an attachment carrying only
// RawContentKey survives the SDK's *input* validation — RawContent stopped
// being a required property when the reference form was added — and that the
// stage-without-a-files-store refusal reaches the client as a readable tool
// error. This fixture registers the SES tools only, which is exactly that
// stage.
func TestSESAttachByReferenceThroughFullChain(t *testing.T) {
	f := newFixtureWithSES(t)
	result, err := emailSession(t, f).CallTool(context.Background(), &mcp.CallToolParams{Name: "ses_send_email", Arguments: map[string]any{
		"FromEmailAddress": "mcp-dev@example.com",
		"Destination":      map[string]any{"ToAddresses": []string{"owner@example.com"}},
		"Content": map[string]any{"Simple": map[string]any{
			"Subject": map[string]any{"Data": "s"},
			"Body":    map[string]any{"Text": map[string]any{"Data": "b"}},
			"Attachments": []any{map[string]any{
				"FileName": "report.pdf", "ContentType": "application/pdf",
				"RawContentKey": "shared/abc/report.pdf",
			}},
		}},
		"DryRun": true,
	}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !result.IsError {
		t.Fatalf("a stage with no files store must refuse the reference: %+v", result)
	}
	if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "not configured") {
		t.Fatalf("error text: %q", text)
	}
}

type stubSES struct{}

func (stubSES) SendEmail(context.Context, *sesv2.SendEmailInput, ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	return &sesv2.SendEmailOutput{}, nil
}
func (stubSES) ListEmailIdentities(context.Context, *sesv2.ListEmailIdentitiesInput, ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error) {
	return &sesv2.ListEmailIdentitiesOutput{}, nil
}
func (stubSES) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	return &sesv2.GetAccountOutput{}, nil
}
