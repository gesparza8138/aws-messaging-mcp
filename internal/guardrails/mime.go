package guardrails

import (
	"encoding/base64"
	"fmt"
	"net/mail"
	"strings"
)

// RawEmail validates a base64 MIME message (PRD §5.3 rules for Content.Raw):
// it must decode, and then the whole RawEmailBytes ladder runs on the decoded
// bytes. Each stage is its own decision so a refusal names what actually
// failed, and the earlier passing ones stay in the slice so ServerMetadata
// shows the whole ladder. The decoded bytes are returned (nil when the decode
// failed) so the caller never decodes twice.
func RawEmail(dataBase64 string, maxBytes int, allow []string) ([]byte, []Decision) {
	decoded, err := base64.StdEncoding.DecodeString(dataBase64)
	if err != nil {
		return nil, []Decision{{Name: "raw_base64", Allowed: false,
			Reason: "Raw.Data is not valid base64: " + err.Error()}}
	}
	decisions := []Decision{{Name: "raw_base64", Allowed: true}}
	return decoded, append(decisions, RawEmailBytes(decoded, maxBytes, allow)...)
}

// RawEmailBytes is the same ladder from raw_size on, over bytes that are
// already decoded: the decoded size must not exceed maxBytes, the headers must
// parse, and the MIME From header must be in the sender allow-list.
//
// It is exported for Content.Raw.DataKey, where the server reads the complete
// message out of the files bucket. Those bytes never were base64, so the
// raw_base64 rung is legitimately absent from that ladder rather than skipped —
// there is nothing for it to decide.
func RawEmailBytes(decoded []byte, maxBytes int, allow []string) []Decision {
	if len(decoded) > maxBytes {
		return []Decision{{Name: "raw_size", Allowed: false,
			Reason: fmt.Sprintf("decoded message is %d bytes, maximum is %d", len(decoded), maxBytes)}}
	}
	decisions := []Decision{{Name: "raw_size", Allowed: true}}
	msg, err := mail.ReadMessage(strings.NewReader(string(decoded)))
	if err != nil {
		return append(decisions, Decision{Name: "raw_mime", Allowed: false,
			Reason: "cannot parse MIME headers: " + err.Error()})
	}
	decisions = append(decisions, Decision{Name: "raw_mime", Allowed: true})
	addr, err := mail.ParseAddress(msg.Header.Get("From"))
	if err != nil {
		return append(decisions, Decision{Name: "sender_allow_list", Allowed: false,
			Reason: "cannot parse the From header"})
	}
	return append(decisions, SenderAllowed(addr.Address, allow))
}
