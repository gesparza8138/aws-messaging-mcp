package lambdaadapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestMuxDispatch(t *testing.T) {
	ran := false
	m := &Mux{
		HTTP: New(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})),
		Tasks: map[string]TaskFunc{
			"files-cleanup": func(context.Context) error { ran = true; return nil },
			"broken":        func(context.Context) error { return errors.New("boom") },
		},
	}
	ctx := context.Background()

	out, err := m.Invoke(ctx, json.RawMessage(`{"task":"files-cleanup"}`))
	if err != nil || !ran {
		t.Fatalf("task dispatch: out=%v err=%v ran=%v", out, err, ran)
	}
	if _, err := m.Invoke(ctx, json.RawMessage(`{"task":"unknown"}`)); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("unknown task: %v", err)
	}
	if _, err := m.Invoke(ctx, json.RawMessage(`{"task":"broken"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("task error not propagated: %v", err)
	}

	event := events.LambdaFunctionURLRequest{
		RawPath: "/healthz",
		RequestContext: events.LambdaFunctionURLRequestContext{
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{Method: "GET", Path: "/healthz"},
		},
	}
	raw, _ := json.Marshal(event)
	out, err = m.Invoke(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp, ok := out.(events.LambdaFunctionURLResponse); !ok || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("http fallthrough: %+v", out)
	}

	if _, err := m.Invoke(ctx, json.RawMessage(`not json`)); err == nil {
		t.Fatal("garbage payload accepted")
	}
}
