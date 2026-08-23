// Package guardrails implements the server-side send controls (PRD §8).
// Every decision is returned to the model in ServerMetadata so a refusal can
// be explained instead of retried blindly.
package guardrails

import (
	"fmt"
	"strings"
)

// Decision records one guardrail evaluation.
type Decision struct {
	Name    string `json:"name"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Result aggregates decisions for a call.
type Result struct {
	Decisions []Decision `json:"guardrails"`
}

// Add appends d and returns whether the call may proceed so far.
func (r *Result) Add(d Decision) bool {
	r.Decisions = append(r.Decisions, d)
	return d.Allowed
}

// Blocked returns the first blocking decision, if any.
func (r *Result) Blocked() (Decision, bool) {
	for _, d := range r.Decisions {
		if !d.Allowed {
			return d, true
		}
	}
	return Decision{}, false
}

// SenderAllowed checks from against the configured sender allow-list
// (case-insensitive exact match). An empty allow-list blocks everything:
// senders are always explicit configuration (PRD §8).
func SenderAllowed(from string, allow []string) Decision {
	name := "sender_allow_list"
	for _, a := range allow {
		if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(a)) {
			return Decision{Name: name, Allowed: true}
		}
	}
	return Decision{Name: name, Allowed: false,
		Reason: fmt.Sprintf("sender %q is not in the allow-list %v", from, allow)}
}

// RecipientsAllowed checks every recipient against the recipient allow-list
// ("test mode", PRD §8). An empty list disables the check.
func RecipientsAllowed(recipients, allow []string) Decision {
	name := "recipient_allow_list"
	if len(allow) == 0 {
		return Decision{Name: name, Allowed: true, Reason: "allow-list disabled"}
	}
	allowed := make(map[string]struct{}, len(allow))
	for _, a := range allow {
		allowed[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}
	for _, r := range recipients {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(r))]; !ok {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("recipient %q is not in the allow-list", r)}
		}
	}
	return Decision{Name: name, Allowed: true}
}

// MaxRecipients caps the total recipient count (PRD §8).
func MaxRecipients(count, maximum int) Decision {
	name := "max_recipients"
	if count == 0 {
		return Decision{Name: name, Allowed: false, Reason: "no recipients"}
	}
	if count > maximum {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("%d recipients exceeds the maximum of %d", count, maximum)}
	}
	return Decision{Name: name, Allowed: true}
}
