package guardrails

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSenderAllowed(t *testing.T) {
	allow := []string{"mcp-dev@example.com", " MCP@Example.com "}
	if d := SenderAllowed("MCP-DEV@example.com", allow); !d.Allowed {
		t.Fatalf("case-insensitive match failed: %+v", d)
	}
	if d := SenderAllowed("mcp@example.com", allow); !d.Allowed {
		t.Fatalf("trimmed match failed: %+v", d)
	}
	if d := SenderAllowed("evil@example.com", allow); d.Allowed || d.Reason == "" {
		t.Fatalf("disallowed sender passed: %+v", d)
	}
	if d := SenderAllowed("anyone@example.com", nil); d.Allowed {
		t.Fatal("empty allow-list must block")
	}
}

func TestRecipientsAllowed(t *testing.T) {
	if d := RecipientsAllowed([]string{"a@x.com"}, nil); !d.Allowed {
		t.Fatal("empty list disables the check")
	}
	allow := []string{"Owner@Example.com"}
	if d := RecipientsAllowed([]string{"owner@example.com"}, allow); !d.Allowed {
		t.Fatalf("allowed recipient blocked: %+v", d)
	}
	if d := RecipientsAllowed([]string{"owner@example.com", "other@x.com"}, allow); d.Allowed || !strings.Contains(d.Reason, "other@x.com") {
		t.Fatalf("disallowed recipient passed: %+v", d)
	}
}

func TestMaxRecipients(t *testing.T) {
	if d := MaxRecipients(0, 10); d.Allowed {
		t.Fatal("zero recipients must block")
	}
	if d := MaxRecipients(10, 10); !d.Allowed {
		t.Fatal("at the limit is allowed")
	}
	if d := MaxRecipients(11, 10); d.Allowed {
		t.Fatal("over the limit must block")
	}
}

func TestResultBlocked(t *testing.T) {
	var r Result
	if !r.Add(Decision{Name: "a", Allowed: true}) {
		t.Fatal("allowed add must return true")
	}
	if r.Add(Decision{Name: "b", Allowed: false, Reason: "no"}) {
		t.Fatal("blocked add must return false")
	}
	blocked, is := r.Blocked()
	if !is || blocked.Name != "b" {
		t.Fatalf("blocked: %+v %v", blocked, is)
	}
	var clean Result
	clean.Add(Decision{Name: "a", Allowed: true})
	if _, is := clean.Blocked(); is {
		t.Fatal("clean result must not be blocked")
	}
}

type fakeStore struct {
	counts map[string]int
	err    error
}

func (f *fakeStore) IncrementWindow(_ context.Context, key string, _ time.Time, _ time.Duration) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[key]++
	return f.counts[key], nil
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 8, 23, 10, 30, 0, 0, time.UTC)
	store := &fakeStore{}
	l := &Limiter{Store: store, PerHour: 2, PerDay: 3, Now: func() time.Time { return now }}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if d := l.Check(ctx, "tool"); !d.Allowed {
			t.Fatalf("call %d blocked: %+v", i, d)
		}
	}
	if d := l.Check(ctx, "tool"); d.Allowed || !strings.Contains(d.Reason, "hourly limit") {
		t.Fatalf("third call in the hour must block: %+v", d)
	}
	// New hour: hourly resets, daily continues counting.
	now = now.Add(time.Hour)
	if d := l.Check(ctx, "tool"); d.Allowed || !strings.Contains(d.Reason, "daily limit") {
		t.Fatalf("fourth call today must hit the daily limit: %+v", d)
	}
	// New day resets both.
	now = now.Add(24 * time.Hour)
	if d := l.Check(ctx, "tool"); !d.Allowed {
		t.Fatalf("new day must allow: %+v", d)
	}
}

func TestLimiterFailsClosed(t *testing.T) {
	l := &Limiter{Store: &fakeStore{err: errors.New("ddb down")}, PerHour: 1, PerDay: 1}
	if d := l.Check(context.Background(), "tool"); d.Allowed || !strings.Contains(d.Reason, "unavailable") {
		t.Fatalf("store failure must block: %+v", d)
	}
}

func TestLimiterDayStoreFailure(t *testing.T) {
	store := &fakeStore{}
	calls := 0
	l := &Limiter{PerHour: 5, PerDay: 5, Store: storeFunc(func(ctx context.Context, key string, w time.Time, ttl time.Duration) (int, error) {
		calls++
		if calls == 2 {
			return 0, errors.New("ddb down")
		}
		return store.IncrementWindow(ctx, key, w, ttl)
	})}
	if d := l.Check(context.Background(), "tool"); d.Allowed {
		t.Fatalf("day-window failure must block: %+v", d)
	}
}

type storeFunc func(ctx context.Context, key string, w time.Time, ttl time.Duration) (int, error)

func (f storeFunc) IncrementWindow(ctx context.Context, key string, w time.Time, ttl time.Duration) (int, error) {
	return f(ctx, key, w, ttl)
}

func mime(from string) string {
	return base64.StdEncoding.EncodeToString([]byte("From: " + from + "\r\nTo: o@x.com\r\nSubject: hi\r\n\r\nbody\r\n"))
}

// decisionByName finds a decision by its guardrail name, so the assertions
// survive the ladder growing another rung.
func decisionByName(t *testing.T, decisions []Decision, name string) Decision {
	t.Helper()
	for _, d := range decisions {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no %q decision in %+v", name, decisions)
	return Decision{}
}

func TestRawEmail(t *testing.T) {
	allow := []string{"mcp@example.com"}
	garbage := base64.StdEncoding.EncodeToString([]byte("\x00\x01 not a mime message without headers"))
	noFrom := base64.StdEncoding.EncodeToString([]byte("To: o@x.com\r\nSubject: hi\r\n\r\nbody\r\n"))
	cases := []struct {
		name    string
		data    string
		maximum int
		blocked string
	}{
		{"bad base64", "!!!not-base64", 1024, "raw_base64"},
		{"oversize", mime("mcp@example.com"), 10, "raw_size"},
		{"unparsable MIME", garbage, 1024, "raw_mime"},
		{"missing From", noFrom, 1024, "sender_allow_list"},
		{"disallowed sender", mime("evil@x.com"), 1024, "sender_allow_list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, decisions := RawEmail(tc.data, tc.maximum, allow)
			if d := decisionByName(t, decisions, tc.blocked); d.Allowed || d.Reason == "" {
				t.Fatalf("%s must block with a reason: %+v", tc.blocked, d)
			}
			for _, d := range decisions {
				if !d.Allowed && d.Name != tc.blocked {
					t.Fatalf("only %s may block: %+v", tc.blocked, d)
				}
			}
		})
	}
	message := "From: Gabe <mcp@example.com>\r\nTo: o@x.com\r\nSubject: hi\r\n\r\nbody\r\n"
	decoded, decisions := RawEmail(base64.StdEncoding.EncodeToString([]byte(message)), 1024, allow)
	if string(decoded) != message {
		t.Fatalf("decoded bytes: %q", decoded)
	}
	for _, name := range []string{"raw_base64", "raw_size", "raw_mime", "sender_allow_list"} {
		if d := decisionByName(t, decisions, name); !d.Allowed {
			t.Fatalf("valid raw blocked: %+v", d)
		}
	}
}
