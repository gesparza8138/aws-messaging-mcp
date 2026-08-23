package guardrails

import (
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
)

// RawEmail validates a base64 MIME message (PRD §5.3 rules for Content.Raw):
// the decoded size must not exceed maxBytes and the MIME From header must be
// in the sender allow-list. It returns the parsed From address for logging.
func RawEmail(dataBase64 string, maxBytes int, allow []string) (string, []Decision) {
	sizeName, fromName := "raw_size", "sender_allow_list"
	decoded, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return "", []Decision{{Name: sizeName, Allowed: false, Reason: "Raw.Data is not valid base64"}}
	}
	if len(decoded) > maxBytes {
		return "", []Decision{{Name: sizeName, Allowed: false,
			Reason: fmt.Sprintf("decoded message is %d bytes, maximum is %d", len(decoded), maxBytes)}}
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(decoded)))
	if err != nil {
		return "", []Decision{
			{Name: sizeName, Allowed: true},
			{Name: fromName, Allowed: false, Reason: "cannot parse MIME headers: " + err.Error()},
		}
	}
	addr, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return "", []Decision{
			{Name: sizeName, Allowed: true},
			{Name: fromName, Allowed: false, Reason: "cannot parse the From header"},
		}
	}
	return addr.Address, []Decision{
		{Name: sizeName, Allowed: true},
		SenderAllowed(addr.Address, allow),
	}
}
