//go:build e2e

// Package e2e exercises the deployed dev stack end to end through the public
// edge: CloudFront allow-list → origin secret injection → Cognito
// client_credentials token → every M2 tool, including one real email and its
// delivery event in the SES event trail (PRD §11.3).
//
// Required environment: E2E_BASE_URL, E2E_TOKEN_URL, E2E_CLIENT_ID,
// E2E_CLIENT_SECRET, E2E_SENDER, E2E_RECIPIENT. Optional: E2E_CONFIG_SET
// (asserted when set), E2E_EVENTS_LOG_GROUP (delivery check skipped when
// unset or unreadable). The suite skips entirely when E2E_BASE_URL is unset
// so a stray `go test -tags e2e` stays harmless.
package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type env struct {
	baseURL, tokenURL, clientID, clientSecret string
	sender, recipient, configSet, logGroup    string
}

func load(t *testing.T) env {
	t.Helper()
	if os.Getenv("E2E_BASE_URL") == "" {
		t.Skip("E2E_BASE_URL unset; e2e suite needs a deployed dev stack")
	}
	req := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s must be set when E2E_BASE_URL is", name)
		}
		return v
	}
	return env{
		baseURL:      strings.TrimSuffix(req("E2E_BASE_URL"), "/"),
		tokenURL:     req("E2E_TOKEN_URL"),
		clientID:     req("E2E_CLIENT_ID"),
		clientSecret: req("E2E_CLIENT_SECRET"),
		sender:       req("E2E_SENDER"),
		recipient:    req("E2E_RECIPIENT"),
		configSet:    os.Getenv("E2E_CONFIG_SET"),
		logGroup:     os.Getenv("E2E_EVENTS_LOG_GROUP"),
	}
}

func (e env) token(t *testing.T) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.tokenURL,
		strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(e.clientID, e.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		t.Fatalf("token response: status %d, error %q, decode %v", resp.StatusCode, body.Error, err)
	}
	return body.AccessToken
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (e env) session(ctx context.Context, t *testing.T) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   e.baseURL + "/mcp/",
		HTTPClient: &http.Client{Transport: bearer{e.token(t)}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func structured(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	if r == nil {
		t.Fatal("no result to read structured content from (the call itself failed)")
	}
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

// text is called from t.Fatalf argument lists, including ones reporting a
// transport error, where CallTool returns a nil result. Dereferencing that
// panicked and buried the actual error under a SIGSEGV stack, so a nil result
// reports itself instead.
func text(r *mcp.CallToolResult) string {
	if r == nil {
		return "<no result>"
	}
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

// TestAuthContract: the public edge admits this runner, unauthenticated MCP
// calls get the 401 contract, and the discovery documents resolve.
func TestAuthContract(t *testing.T) {
	e := load(t)
	resp, err := http.Get(e.baseURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d (runner IP not in the edge allow-list?)", resp.StatusCode)
	}

	resp, err = http.Post(e.baseURL+"/mcp/", "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /mcp/: %d, want 401", resp.StatusCode)
	}
	// The Function URL remaps WWW-Authenticate (PRD A4 deviation); accept it
	// under either name, pointing at the protected-resource document.
	challenge := resp.Header.Get("WWW-Authenticate") + resp.Header.Get("x-amzn-Remapped-WWW-Authenticate")
	if !strings.Contains(challenge, "resource_metadata") {
		t.Errorf("401 challenge lacks resource_metadata: %q", challenge)
	}

	resp, err = http.Get(e.baseURL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || len(doc.AuthorizationServers) == 0 {
		t.Fatalf("protected-resource document: status %d, %v", resp.StatusCode, err)
	}
}

// TestTools drives every registered tool through a real OAuth session, ending
// with one real email and, when readable, its Delivery event in the trail.
func TestTools(t *testing.T) {
	e := load(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session := e.session(ctx, t)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"hello", "ses_send_email", "ses_list_email_identities", "ses_get_account"} {
		if !names[want] {
			t.Fatalf("tool %q missing from %v", want, names)
		}
	}

	hello, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "hello", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("hello: %v", err)
	}
	if out := structured(t, hello); out["auth_method"] != "oauth" || out["caller"] != e.clientID {
		t.Fatalf("hello principal: %v", out)
	}

	account, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_get_account", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("ses_get_account: %v", err)
	}
	if out := structured(t, account); out["SendingEnabled"] != true {
		t.Fatalf("ses_get_account: %v", out)
	}

	identities, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_list_email_identities", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("ses_list_email_identities: %v", err)
	}
	if !strings.Contains(text(identities)+fmt.Sprint(structured(t, identities)), strings.SplitN(e.sender, "@", 2)[1]) {
		t.Errorf("sending domain missing from identities: %v", structured(t, identities))
	}

	stamp := time.Now().UTC().Format(time.RFC3339)
	message := func(to, subject string, dryRun bool) map[string]any {
		return map[string]any{
			"DryRun":           dryRun,
			"FromEmailAddress": e.sender,
			"Destination":      map[string]any{"ToAddresses": []string{to}},
			"Content": map[string]any{"Simple": map[string]any{
				"Subject": map[string]any{"Data": subject},
				"Body":    map[string]any{"Text": map[string]any{"Data": "e2e run " + stamp}},
			}},
		}
	}

	dry, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email",
		Arguments: message(e.recipient, "e2e dry run "+stamp, true)})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.IsError {
		t.Fatalf("dry run refused: %s", text(dry))
	}
	out := structured(t, dry)
	would, _ := out["WouldCall"].(map[string]any)
	if would == nil || out["MessageId"] != nil {
		t.Fatalf("dry run must return WouldCall and no MessageId: %v", out)
	}
	if e.configSet != "" && would["ConfigurationSetName"] != e.configSet {
		t.Errorf("ConfigurationSetName not injected: %v", would["ConfigurationSetName"])
	}

	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email",
		Arguments: message("intruder@example.com", "must not send", false)})
	if err != nil {
		t.Fatalf("blocked send: %v", err)
	}
	if !blocked.IsError || !strings.Contains(text(blocked), "blocked by guardrail") {
		t.Fatalf("disallowed recipient must hit a guardrail: %s", text(blocked))
	}

	sent, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email",
		Arguments: message(e.recipient, "e2e "+stamp, false)})
	if err != nil {
		t.Fatalf("real send: %v", err)
	}
	if sent.IsError {
		t.Fatalf("real send refused: %s", text(sent))
	}
	messageID, _ := structured(t, sent)["MessageId"].(string)
	if messageID == "" {
		t.Fatal("real send returned no MessageId")
	}
	t.Logf("sent %s", messageID)
	e.awaitDelivery(ctx, t, messageID)
}

// TestAttachByReference: an object already in the files bucket becomes an
// inline email image by key — no bytes through the model — the dry run proves
// the server fetched exactly those bytes and assembled them into a
// multipart/related, and the send after it puts that message in a real mailbox.
//
// The real send is the point. This test used to stop at the DryRun, which never
// leaves the server, so nothing here could observe that the delivered image was
// not rendering — that is precisely why the defect shipped
// (docs/plans/email-inline-mime.md §8). A green run still only proves the
// message left correctly assembled; the acceptance criterion is the owner
// opening it in Gmail, Apple Mail, and Outlook.
func TestAttachByReference(t *testing.T) {
	e := load(t)
	// Three minutes because the real send below waits on the delivery event.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	session := e.session(ctx, t)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	files := false
	for _, tool := range tools.Tools {
		files = files || tool.Name == "files_put_object"
	}
	if !files {
		t.Skip("files tools not registered on this stage")
	}

	png := []byte("\x89PNG\r\n\x1a\n e2e attach-by-reference pixel")
	put, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_put_object", Arguments: map[string]any{
		"FileName": "e2e-inline.png", "ContentType": "image/png",
		"ContentEncoding": "base64", "Body": base64.StdEncoding.EncodeToString(png),
		"ExpiresIn": "P1D",
	}})
	if err != nil {
		t.Fatalf("files_put_object: %v", err)
	}
	if put.IsError {
		t.Fatalf("files_put_object refused: %s", text(put))
	}
	key, _ := structured(t, put)["Key"].(string)
	if key == "" {
		t.Fatalf("no key in the upload result: %v", structured(t, put))
	}
	t.Cleanup(func() {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "files_delete_object", Arguments: map[string]any{"Key": key}}); err != nil {
			t.Logf("cleanup of %s failed: %v", key, err)
		}
	})

	inline := func(subject string, dryRun bool) map[string]any {
		return map[string]any{
			"DryRun":           dryRun,
			"FromEmailAddress": e.sender,
			"Destination":      map[string]any{"ToAddresses": []string{e.recipient}},
			"Content": map[string]any{"Simple": map[string]any{
				"Subject": map[string]any{"Data": subject},
				"Body": map[string]any{
					"Text": map[string]any{"Data": "the pixel should be visible in the HTML part"},
					"Html": map[string]any{"Data": `<p>inline pixel: <img src="cid:e2e-pixel"></p>`},
				},
				"Attachments": []map[string]any{{
					"FileName": "e2e-inline.png", "ContentType": "image/png",
					"RawContentKey": key, "ContentDisposition": "INLINE", "ContentId": "e2e-pixel",
				}},
			}},
		}
	}

	dry, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email",
		Arguments: inline("e2e attach by reference", true)})
	if err != nil {
		t.Fatalf("ses_send_email: %v", err)
	}
	if dry.IsError {
		t.Fatalf("attach by reference refused: %s", text(dry))
	}
	out := structured(t, dry)

	// An inline attachment makes the server assemble the message itself, so the
	// echo is Content.Raw — the whole MIME message, base64 in JSON — and not
	// Content.Simple.Attachments.
	if dig(out, "WouldCall", "Content")["Simple"] != nil {
		t.Fatalf("an inline send must be assembled into Content.Raw: %v", out["WouldCall"])
	}
	encoded, _ := dig(out, "WouldCall", "Content", "Raw")["Data"].(string)
	message, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(message) == 0 {
		t.Fatalf("assembled message %q: %v", encoded, err)
	}
	if !strings.Contains(string(message), "multipart/related") {
		t.Errorf("no multipart/related container, so a cid: cannot resolve:\n%s", message)
	}
	if !strings.Contains(string(message), "Content-ID: <e2e-pixel>") {
		t.Errorf("no Content-ID header for the HTML to reference:\n%s", message)
	}
	// The fetched bytes, base64 inside the message: the caller never sent them
	// and the server never had to send them back as a separate field.
	if !strings.Contains(string(message), base64.StdEncoding.EncodeToString(png)) {
		t.Errorf("the uploaded object's bytes are not in the assembled message:\n%s", message)
	}

	// The same tree, reported rather than parsed out of the bytes: when an
	// image does not render, mime_structure is what the caller reads instead of
	// mailing a human and asking. Here it has to agree with the message above.
	structure, _ := dig(out, "ServerMetadata")["mime_structure"].([]any)
	related, sibling := "", false
	for _, p := range structure {
		part, _ := p.(map[string]any)
		if part["content_type"] == "multipart/related" {
			related, _ = part["path"].(string)
		}
		if part["content_id"] == "e2e-pixel" {
			path, _ := part["path"].(string)
			sibling = related != "" && strings.HasPrefix(path, related+".")
		}
	}
	if !sibling {
		t.Errorf("mime_structure must place the image inside the related group: %v", structure)
	}

	// One digest for the attachment as it arrived, one for the message it was
	// assembled into.
	digests, _ := dig(out, "ServerMetadata")["content_digests"].([]any)
	if len(digests) != 2 {
		t.Fatalf("content_digests: %v", out["ServerMetadata"])
	}
	want := sha256.Sum256(png)
	if digest := digests[0].(map[string]any); digest["sha256"] != hex.EncodeToString(want[:]) {
		t.Errorf("digest %v does not match the uploaded object", digest)
	}
	whole := sha256.Sum256(message)
	if digest := digests[1].(map[string]any); digest["part"] != "assembled" || digest["sha256"] != hex.EncodeToString(whole[:]) {
		t.Errorf("digest %v does not match the assembled message", digest)
	}

	// And now the half no DryRun can cover: a real inline image in a real
	// mailbox, which is the only place the reported defect was visible.
	stamp := time.Now().UTC().Format(time.RFC3339)
	sent, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ses_send_email",
		Arguments: inline("e2e inline image "+stamp, false)})
	if err != nil {
		t.Fatalf("real inline send: %v", err)
	}
	if sent.IsError {
		t.Fatalf("real inline send refused: %s", text(sent))
	}
	messageID, _ := structured(t, sent)["MessageId"].(string)
	if messageID == "" {
		t.Fatal("real inline send returned no MessageId")
	}
	t.Logf("sent inline image %s — open it and confirm the pixel renders in the body", messageID)
	e.awaitDelivery(ctx, t, messageID)
}

// dig walks nested JSON objects, returning an empty map at the first miss so
// the caller's own assertion reports the failure.
func dig(m map[string]any, path ...string) map[string]any {
	for _, key := range path {
		next, ok := m[key].(map[string]any)
		if !ok {
			return map[string]any{}
		}
		m = next
	}
	return m
}

// awaitDelivery polls the SES event trail for the message's Delivery event.
// Missing permissions or slow mailbox providers skip rather than fail: the
// send itself already proved the pipeline (PRD §11.3).
func (e env) awaitDelivery(ctx context.Context, t *testing.T, messageID string) {
	t.Helper()
	if e.logGroup == "" {
		t.Log("E2E_EVENTS_LOG_GROUP unset; skipping delivery-event check")
		return
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Skipf("no AWS credentials for the delivery check: %v", err)
	}
	logs := cloudwatchlogs.NewFromConfig(cfg)
	start := time.Now().Add(-time.Minute).UnixMilli()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := logs.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(e.logGroup),
			StartTime:     aws.Int64(start),
			FilterPattern: aws.String(fmt.Sprintf("%q", messageID)),
		})
		if err != nil {
			t.Skipf("event trail unreadable (deploy role lacks logs:FilterLogEvents?): %v", err)
		}
		for _, event := range resp.Events {
			if strings.Contains(aws.ToString(event.Message), "Delivery") {
				t.Logf("delivery event observed for %s", messageID)
				return
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Skipf("no Delivery event for %s within 2m (provider delay?); send succeeded", messageID)
}
