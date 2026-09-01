package guardrails

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
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

// InlineAttachment is the caller-supplied part of one attachment as the inline
// checks see it: the strings that end up in a header, plus the decoded size.
// TransferEncoding and Bytes are carried so one struct describes an attachment
// for both this ladder and mimebuild.Attachment; the encoding itself is
// validated by mimebuild, which has to refuse an unusable one rather than emit
// a corrupt message.
type InlineAttachment struct {
	FileName         string
	ContentType      string
	Disposition      string
	ContentID        string
	TransferEncoding string
	Bytes            int
}

// Bounds SES applied on the Simple path (PRD §5.3): ContentId 1..78 comes from
// the Attachment API reference, and the other two are the documented limits
// for the fields they cover.
const (
	maxContentIDLength   = 78
	maxFileNameLength    = 255
	maxContentTypeLength = 78
)

// cidReference matches a cid: URI in an HTML body. The charset is deliberately
// the one the Content-ID check enforces, so a reference can only match an ID
// that would itself be accepted, and the word boundary keeps a word ending in
// "cid" (acid:, plastid:) from inventing a reference nothing declares. The scan
// only ever runs when at least one attachment is inline, so prose containing
// the literal "cid:" cannot make an ordinary send fail either.
var cidReference = regexp.MustCompile(`(?i)\bcid:([A-Za-z0-9._@+-]+)`)

// InlineAttachments checks everything that has to be true before the server
// assembles a multipart/related itself (docs/plans/email-inline-mime.md §4).
// It returns no decisions at all when nothing is inline, so a plain attachment
// send is evaluated exactly as it is today, and it stops at the first refusal
// like the raw ladder does, so a refusal names the stage that failed.
func InlineAttachments(atts []InlineAttachment, htmlBody string) []Decision {
	first := -1
	for i, a := range atts {
		if a.isInline() {
			first = i
			break
		}
	}
	if first < 0 {
		return nil
	}
	var decisions []Decision
	for _, check := range []func() Decision{
		func() Decision { return attachmentFields(atts) },
		func() Decision { return inlineContentIDs(atts) },
		func() Decision { return inlineNeedsHTML(atts[first], first, htmlBody) },
		func() Decision { return inlineCIDRefs(atts, htmlBody) },
	} {
		d := check()
		decisions = append(decisions, d)
		if !d.Allowed {
			break
		}
	}
	return decisions
}

// isInline reports whether the attachment belongs in the multipart/related
// group. INLINE is the caller's explicit signal; a ContentId with no
// disposition at all is the same intent expressed less completely, and an
// explicit ATTACHMENT wins. mimebuild.isInline applies the identical rule, so
// the checks here cover exactly the parts the assembler will place inline.
func (a InlineAttachment) isInline() bool {
	disposition := strings.TrimSpace(a.Disposition)
	return strings.EqualFold(disposition, "INLINE") ||
		(disposition == "" && strings.TrimSpace(a.ContentID) != "")
}

// attachmentFields re-checks what SES used to check for us. On the Simple path
// SES validated FileName, ContentType, ContentId, and ContentTransferEncoding
// and refused the call itself; once the server assembles the message and sends
// Content.Raw, SES sees opaque bytes and never looks, so these become our
// checks.
func attachmentFields(atts []InlineAttachment) Decision {
	const name = "attachment_fields"
	for i, a := range atts {
		switch {
		case a.FileName == "":
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d has no FileName", i)}
		case len(a.FileName) > maxFileNameLength:
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) has a FileName of %d characters, the maximum is %d",
					i, a.FileName, len(a.FileName), maxFileNameLength)}
		case len(a.ContentType) > maxContentTypeLength:
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) has a ContentType of %d characters, the maximum is %d",
					i, a.FileName, len(a.ContentType), maxContentTypeLength)}
		}
	}
	return Decision{Name: name, Allowed: true}
}

// inlineContentIDs checks the identifier the HTML has to match: every inline
// attachment needs one, and every ContentId in the message has to be usable
// and unique, because two parts answering to the same cid: is a message whose
// rendering depends on the client.
func inlineContentIDs(atts []InlineAttachment) Decision {
	const name = "inline_content_id"
	seen := map[string]int{}
	for i, a := range atts {
		id := bareContentID(a.ContentID)
		if id == "" {
			if a.isInline() {
				return Decision{Name: name, Allowed: false,
					Reason: fmt.Sprintf("attachment %d (%q) is inline but its ContentId %q is empty, so the HTML's cid: reference has nothing to resolve to",
						i, a.FileName, a.ContentID)}
			}
			continue
		}
		if len(id) > maxContentIDLength {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) has a ContentId of %d characters, the maximum is %d",
					i, a.FileName, len(id), maxContentIDLength)}
		}
		if r, bad := invalidContentIDRune(id); bad {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) has a ContentId %q containing %q, which is outside [A-Za-z0-9._@+-]",
					i, a.FileName, id, r)}
		}
		if previous, duplicate := seen[id]; duplicate {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("attachment %d (%q) reuses the ContentId %q of attachment %d",
					i, a.FileName, id, previous)}
		}
		seen[id] = i
	}
	return Decision{Name: name, Allowed: true}
}

// inlineNeedsHTML refuses an inline attachment in a message with no HTML body:
// there is nothing that could carry the cid: reference, so the part could only
// ever arrive as an ordinary attachment.
func inlineNeedsHTML(a InlineAttachment, i int, htmlBody string) Decision {
	const name = "inline_needs_html"
	if htmlBody != "" {
		return Decision{Name: name, Allowed: true}
	}
	return Decision{Name: name, Allowed: false,
		Reason: fmt.Sprintf("attachment %d (%q) is inline but the message has no Html body, so nothing can reference a cid:", i, a.FileName)}
}

// inlineCIDRefs matches the HTML's references against the attachments both
// ways. A reference to an identifier no attachment declares is a refusal - the
// image would be a broken placeholder in the delivered mail. An inline
// attachment nothing references is allowed *with a reason*, the shape
// RecipientsAllowed uses for "allow-list disabled": the send is valid, and the
// part simply arrives the way it does today.
func inlineCIDRefs(atts []InlineAttachment, htmlBody string) Decision {
	const name = "inline_cid_refs"
	declared := map[string]bool{}
	for _, a := range atts {
		if id := bareContentID(a.ContentID); id != "" {
			declared[id] = true
		}
	}
	referenced := map[string]bool{}
	for _, match := range cidReference.FindAllStringSubmatch(htmlBody, -1) {
		id := match[1]
		if !declared[id] {
			return Decision{Name: name, Allowed: false,
				Reason: fmt.Sprintf("the HTML references cid:%s, which no attachment declares as its ContentId", id)}
		}
		referenced[id] = true
	}
	for i, a := range atts {
		if id := bareContentID(a.ContentID); a.isInline() && !referenced[id] {
			return Decision{Name: name, Allowed: true,
				Reason: fmt.Sprintf("attachment %d (%q) is inline but nothing in the HTML references cid:%s, so it will arrive as an ordinary attachment",
					i, a.FileName, id)}
		}
	}
	return Decision{Name: name, Allowed: true,
		Reason: fmt.Sprintf("%d cid: references resolved", len(referenced))}
}

// bareContentID strips the angle brackets a caller may have written, since
// both spellings are accepted and the cid: reference matches the bare form.
// mimebuild does the same normalisation when it writes the header; here it
// makes a violation a decision the model can read instead of an error.
func bareContentID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > 1 && strings.HasPrefix(id, "<") && strings.HasSuffix(id, ">") {
		return id[1 : len(id)-1]
	}
	return id
}

// invalidContentIDRune returns the first rune that may not appear in a
// Content-ID. The charset is restrictive because the value is written into a
// header the server assembles itself.
func invalidContentIDRune(id string) (rune, bool) {
	for _, r := range id {
		if !isContentIDRune(r) {
			return r, true
		}
	}
	return 0, false
}

func isContentIDRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("._@+-", r)
	}
}
