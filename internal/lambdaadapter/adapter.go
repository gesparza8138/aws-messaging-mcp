// Package lambdaadapter serves a standard http.Handler behind a Lambda
// Function URL in BUFFERED invoke mode (PRD 9.2). The official lambdaurl
// package supports only RESPONSE_STREAM, and R3 was verified buffered, so
// this ~100-line adapter keeps the design and avoids a third-party proxy.
package lambdaadapter

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
)

// Handler converts a Function URL event into an http.Request, runs next, and
// converts the recorded response back into a Function URL response.
type Handler struct{ next http.Handler }

// New wraps next.
func New(next http.Handler) *Handler { return &Handler{next: next} }

// Invoke is the Lambda handler signature for Function URL events.
func (h *Handler) Invoke(ctx context.Context, event events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	req, err := toRequest(ctx, event)
	if err != nil {
		return events.LambdaFunctionURLResponse{
			StatusCode: http.StatusBadRequest,
			Headers:    map[string]string{"Content-Type": "application/json"},
			Body:       `{"error":"bad request"}`,
		}, nil
	}
	rec := httptest.NewRecorder()
	h.next.ServeHTTP(rec, req)
	return toResponse(rec), nil
}

func toRequest(ctx context.Context, event events.LambdaFunctionURLRequest) (*http.Request, error) {
	body := event.Body
	if event.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(event.Body)
		if err != nil {
			return nil, err
		}
		body = string(decoded)
	}
	target := event.RawPath
	if target == "" {
		target = "/"
	}
	if event.RawQueryString != "" {
		target += "?" + event.RawQueryString
	}
	if _, err := url.ParseRequestURI(target); err != nil {
		return nil, err
	}
	method := event.RequestContext.HTTP.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, value := range event.Headers {
		req.Header.Set(name, value)
	}
	if len(event.Cookies) > 0 {
		req.Header.Set("Cookie", strings.Join(event.Cookies, "; "))
	}
	if host := event.Headers["host"]; host != "" {
		req.Host = host
	}
	req.RemoteAddr = event.RequestContext.HTTP.SourceIP
	return req, nil
}

func toResponse(rec *httptest.ResponseRecorder) events.LambdaFunctionURLResponse {
	headers := make(map[string]string, len(rec.Header()))
	var cookies []string
	for name, values := range rec.Header() {
		if strings.EqualFold(name, "Set-Cookie") {
			cookies = append(cookies, values...)
			continue
		}
		headers[name] = strings.Join(values, ", ")
	}
	raw := rec.Body.Bytes()
	resp := events.LambdaFunctionURLResponse{
		StatusCode: rec.Code,
		Headers:    headers,
		Cookies:    cookies,
	}
	if utf8.Valid(raw) {
		resp.Body = string(raw)
	} else {
		resp.Body = base64.StdEncoding.EncodeToString(raw)
		resp.IsBase64Encoded = true
	}
	return resp
}
