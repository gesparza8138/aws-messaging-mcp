// Package guardrails implements the server-side send controls (PRD §8).
// Every decision is returned to the model in ServerMetadata so a refusal can
// be explained instead of retried blindly.
package guardrails

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
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

// maxDisplayNameLength caps the display name in a sender address, counted in
// characters. It labels the message for its recipient ("Daily Report"), so
// anything longer is more likely a mistake than a label.
const maxDisplayNameLength = 64

// SenderAllowed checks from against the configured sender allow-list
// (case-insensitive match on the address). from is an RFC 5322 mailbox, so it
// may carry a display name ("Daily Report" <mcp@example.com>); only the address
// is matched, and the name is checked by senderDecision. An empty allow-list
// blocks everything: senders are always explicit configuration (PRD §8).
func SenderAllowed(from string, allow []string) Decision {
	addr, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil {
		return Decision{Name: "sender_allow_list", Allowed: false,
			Reason: fmt.Sprintf("sender %q is not a valid address: %v", from, err)}
	}
	return senderDecision(addr, allow)
}

// senderDecision is the sender check on an address already parsed, shared with
// the raw ladder, which parses the From header itself. A display name may not
// contain "@": a name like "ceo@bank.example" shows in most clients in place of
// the real address and reads as a different sender.
func senderDecision(addr *mail.Address, allow []string) Decision {
	name := "sender_allow_list"
	if strings.Contains(addr.Name, "@") {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("display name %q must not contain @", addr.Name)}
	}
	if n := utf8.RuneCountInString(addr.Name); n > maxDisplayNameLength {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("display name is %d characters, the maximum is %d", n, maxDisplayNameLength)}
	}
	for _, a := range allow {
		if strings.EqualFold(addr.Address, strings.TrimSpace(a)) {
			return Decision{Name: name, Allowed: true}
		}
	}
	return Decision{Name: name, Allowed: false,
		Reason: fmt.Sprintf("sender %q is not in the allow-list %v", addr.Address, allow)}
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
