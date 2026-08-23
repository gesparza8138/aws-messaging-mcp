package lambdaadapter

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func echoHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Method", r.Method)
		w.Header().Set("X-Path", r.URL.Path)
		w.Header().Set("X-Query", r.URL.RawQuery)
		w.Header().Set("X-Host", r.Host)
		w.Header().Set("X-Remote", r.RemoteAddr)
		w.Header().Set("X-Cookie", r.Header.Get("Cookie"))
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		w.Header().Add("Set-Cookie", "s=1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})
}

func event(method, path, query, body string, b64 bool) events.LambdaFunctionURLRequest {
	return events.LambdaFunctionURLRequest{
		Version:        "2.0",
		RawPath:        path,
		RawQueryString: query,
		Headers:        map[string]string{"host": "abc.lambda-url.us-west-2.on.aws", "content-type": "application/json"},
		Cookies:        []string{"c1=v1", "c2=v2"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: method, Path: path, SourceIP: "203.0.113.9"},
		},
		Body:            body,
		IsBase64Encoded: b64,
	}
}

func TestRoundTrip(t *testing.T) {
	h := New(echoHandler(t))
	resp, err := h.Invoke(context.Background(), event("POST", "/mcp", "a=1&b=2", `{"x":1}`, false))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || resp.Body != `{"x":1}` || resp.IsBase64Encoded {
		t.Fatalf("resp: %+v", resp)
	}
	want := map[string]string{"X-Method": "POST", "X-Path": "/mcp", "X-Query": "a=1&b=2",
		"X-Host": "abc.lambda-url.us-west-2.on.aws", "X-Remote": "203.0.113.9", "X-Cookie": "c1=v1; c2=v2", "X-Multi": "a, b"}
	for k, v := range want {
		if resp.Headers[k] != v {
			t.Errorf("%s = %q, want %q", k, resp.Headers[k], v)
		}
	}
	if len(resp.Cookies) != 1 || resp.Cookies[0] != "s=1" {
		t.Errorf("cookies: %v", resp.Cookies)
	}
}

func TestBase64BodyAndBinaryResponse(t *testing.T) {
	binary := []byte{0xff, 0x00, 0xfe}
	h := New(echoHandler(t))
	resp, err := h.Invoke(context.Background(), event("POST", "/", "", base64.StdEncoding.EncodeToString(binary), true))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsBase64Encoded {
		t.Fatalf("binary body must be base64 encoded: %+v", resp)
	}
	got, _ := base64.StdEncoding.DecodeString(resp.Body)
	if string(got) != string(binary) {
		t.Fatalf("body mismatch: %v", got)
	}
}

func TestDefaultsAndBadInput(t *testing.T) {
	h := New(echoHandler(t))
	resp, err := h.Invoke(context.Background(), events.LambdaFunctionURLRequest{})
	if err != nil || resp.Headers["X-Method"] != "GET" || resp.Headers["X-Path"] != "/" {
		t.Fatalf("defaults: %v %+v", err, resp)
	}
	resp, err = h.Invoke(context.Background(), event("POST", "/", "", "%%%not-base64", true))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad base64 must be 400: %v %+v", err, resp)
	}
	resp, err = h.Invoke(context.Background(), event("GET", "no-leading-slash", "", "", false))
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad path must be 400: %v %+v", err, resp)
	}
}
