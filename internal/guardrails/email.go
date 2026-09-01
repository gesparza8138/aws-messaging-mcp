package guardrails

import (
	"encoding/base64"
	"fmt"
)

// AttachmentInput is the caller-supplied part of one Simple-content
// attachment (the schemas package's Attachment, without the SES-only fields).
type AttachmentInput struct {
	FileName   string
	RawContent string
}

// EmailAttachments decodes every attachment and caps their combined decoded
// size against the same budget as Content.Raw (PRD §8). The decoded bytes are
// index-aligned with atts (nil where the decode failed) so the caller never
// decodes twice. Zero-byte attachments are legal; an empty list produces no
// decisions at all.
func EmailAttachments(atts []AttachmentInput, maxTotalBytes int) ([][]byte, []Decision) {
	if len(atts) == 0 {
		return nil, nil
	}
	decoded := make([][]byte, len(atts))
	var decisions []Decision
	total := 0
	for i, a := range atts {
		raw, err := base64.StdEncoding.DecodeString(a.RawContent)
		if err != nil {
			decisions = append(decisions, Decision{Name: "attachment_base64", Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) is not valid base64: %v", i, a.FileName, err)})
			continue
		}
		decoded[i] = raw
		total += len(raw)
	}
	if total > maxTotalBytes {
		return decoded, append(decisions, Decision{Name: "attachment_size", Allowed: false,
			Reason: fmt.Sprintf("%d bytes decoded exceeds the maximum of %d", total, maxTotalBytes)})
	}
	return decoded, append(decisions, Decision{Name: "attachment_size", Allowed: true,
		Reason: fmt.Sprintf("%d bytes across %d attachments", total, len(atts))})
}
