//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fetch GETs a URL with redirects disabled and returns status + body.
func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// awaitStatus polls until the URL returns want (CloudFront behavior and
// object propagation take a little while right after a deploy).
func awaitStatus(t *testing.T, url string, want int, within time.Duration) (int, string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, body := fetch(t, url)
		if code == want || time.Now().After(deadline) {
			return code, body
		}
		time.Sleep(10 * time.Second)
	}
}

// TestFilesTools exercises the M4b exit criteria live: signed download,
// tampered/expired/deleted → 403, guardrail block, presigned PUT round trip.
func TestFilesTools(t *testing.T) {
	e := load(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	session := e.session(ctx, t)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	registered := map[string]bool{}
	for _, tool := range tools.Tools {
		registered[tool.Name] = true
	}
	if !registered["files_put_object"] {
		t.Skip("files tools not registered; stage has no signing key wired yet")
	}
	for _, want := range []string{"files_create_upload_url", "files_create_signed_url", "files_list_objects", "files_delete_object"} {
		if !registered[want] {
			t.Fatalf("tool %q missing", want)
		}
	}

	stamp := time.Now().UTC().Format(time.RFC3339)
	content := "e2e files " + stamp

	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_put_object", Arguments: map[string]any{
		"FileName": "x.html", "ContentType": "text/html", "Body": "<html>", "ExpiresIn": "P1D",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.IsError || !strings.Contains(text(blocked), "blocked by guardrail") {
		t.Fatalf("text/html must hit the deny-list: %s", text(blocked))
	}

	put, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_put_object", Arguments: map[string]any{
		"FileName": "e2e.txt", "ContentType": "text/plain", "Body": content, "ExpiresIn": "P1D",
	}})
	if err != nil || put.IsError {
		t.Fatalf("put: err=%v result=%s", err, text(put))
	}
	out := structured(t, put)
	signedURL, _ := out["SignedUrl"].(string)
	key, _ := out["Key"].(string)
	if signedURL == "" || !strings.HasPrefix(key, "shared/") {
		t.Fatalf("put output: %v", out)
	}

	code, body := awaitStatus(t, signedURL, http.StatusOK, 3*time.Minute)
	if code != http.StatusOK || body != content {
		t.Fatalf("signed download: %d %q", code, body)
	}

	// The replacement must differ from what is already there: overwriting a
	// signature character with a constant "X" is a no-op ~1/64 of the time
	// (whenever that character already is an X), and the "tampered" URL is
	// then byte-identical to the valid one and rightly returns 200 - which
	// this test once reported as a broken key group. Both cases stay inside
	// CloudFront's base64 alphabet, so the request reaches the verifier.
	tampered := signedURL
	if i := strings.Index(tampered, "Signature="); i > 0 {
		flip := byte('X')
		if tampered[i+15] == flip {
			flip = 'x'
		}
		tampered = tampered[:i+15] + string(flip) + tampered[i+16:]
	}
	if code, _ := fetch(t, tampered); code != http.StatusForbidden {
		t.Fatalf("tampered signature: %d, want 403", code)
	}

	short, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_create_signed_url", Arguments: map[string]any{
		"Key": key, "DateLessThan": time.Now().Add(5 * time.Second).UTC().Format(time.RFC3339),
	}})
	if err != nil || short.IsError {
		t.Fatalf("short sign: err=%v result=%s", err, text(short))
	}
	shortURL, _ := structured(t, short)["SignedUrl"].(string)
	time.Sleep(7 * time.Second)
	if code, _ := fetch(t, shortURL); code != http.StatusForbidden {
		t.Fatalf("expired link: %d, want 403", code)
	}

	upload, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_create_upload_url", Arguments: map[string]any{
		"FileName": "big.bin", "ContentType": "application/octet-stream", "ContentLength": int64(len(content)),
	}})
	if err != nil || upload.IsError {
		t.Fatalf("upload url: err=%v result=%s", err, text(upload))
	}
	up := structured(t, upload)
	uploadURL, _ := up["UploadUrl"].(string)
	uploadKey, _ := up["Key"].(string)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader([]byte(content)))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(content))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("presigned PUT: %v", err)
	}
	putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT: %d", putResp.StatusCode)
	}
	signed2, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_create_signed_url", Arguments: map[string]any{
		"Key": uploadKey, "ExpiresIn": "PT1H",
	}})
	if err != nil || signed2.IsError {
		t.Fatalf("sign uploaded: err=%v result=%s", err, text(signed2))
	}
	url2, _ := structured(t, signed2)["SignedUrl"].(string)
	if code, body := awaitStatus(t, url2, http.StatusOK, 2*time.Minute); code != http.StatusOK || body != content {
		t.Fatalf("uploaded download: %d %q", code, body)
	}

	list, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_list_objects", Arguments: map[string]any{}})
	if err != nil || list.IsError || !strings.Contains(toJSON(t, structured(t, list)), key) {
		t.Fatalf("list: err=%v result=%s", err, text(list))
	}

	for _, k := range []string{key, uploadKey} {
		del, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files_delete_object", Arguments: map[string]any{"Key": k}})
		if err != nil || del.IsError {
			t.Fatalf("delete %s: err=%v result=%s", k, err, text(del))
		}
	}
	if code, _ := awaitStatus(t, signedURL, http.StatusForbidden, 2*time.Minute); code != http.StatusForbidden {
		t.Fatalf("deleted object still serves: %d", code)
	}
}
