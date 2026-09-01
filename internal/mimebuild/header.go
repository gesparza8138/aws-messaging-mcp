package mimebuild

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"maps"
	"mime"
	"net/mail"
	"net/textproto"
	"regexp"
	"slices"
	"strings"
	"time"
)

// maxHeaderLine is where a header line is folded. RFC 5322 asks for 78
// characters and permits 998; 76 leaves room for the CRLF.
const maxHeaderLine = 76

// writeTopHeaders writes the message's own header block, in a fixed order so
// the same message always assembles to the same bytes, followed by the blank
// line that ends it. The root part's Content-* headers belong to this block
// too - the root has no parent to write them.
func writeTopHeaders(buf *bytes.Buffer, m Message, root *node) error {
	from, err := formatAddress(m.From)
	if err != nil {
		return fmt.Errorf("invalid From address: %w", err)
	}
	lines := [][2]string{{"From", from}}
	// To and Cc become headers; Cc and Reply-To are omitted when empty, and
	// Bcc has no field to come from (see Message).
	for _, list := range []struct {
		name  string
		addrs []string
	}{{"To", m.To}, {"Cc", m.Cc}, {"Reply-To", m.ReplyTo}} {
		if len(list.addrs) == 0 {
			continue
		}
		value, err := formatAddressList(list.name, list.addrs)
		if err != nil {
			return err
		}
		lines = append(lines, [2]string{list.name, value})
	}
	// SES writes a Date for a Simple message; a raw message without one reads
	// as spam, so a caller who forgot to inject a clock still gets a plausible
	// header rather than the year 1.
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	lines = append(lines,
		[2]string{"Subject", encodeHeaderText(m.Subject)},
		[2]string{"Date", date.Format(time.RFC1123Z)},
		[2]string{"MIME-Version", "1.0"})

	header, err := partHeader(root)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(header)) {
		lines = append(lines, [2]string{name, header.Get(name)})
	}
	for _, line := range lines {
		fmt.Fprintf(buf, "%s: %s\r\n", line[0], line[1])
	}
	buf.WriteString("\r\n")
	return nil
}

// partHeader builds one part's Content-* headers. multipart.Writer.CreatePart
// writes them with a bare fmt.Fprintf and sanitises nothing, so every value
// here is either a package constant, a mime.FormatMediaType result (which
// quotes or RFC 2231 encodes its parameters), an encoded word, or a Content-ID
// whose charset was checked when the attachment was built.
func partHeader(n *node) (textproto.MIMEHeader, error) {
	contentType := mime.FormatMediaType(n.mediaType, n.params)
	if contentType == "" {
		return nil, fmt.Errorf("cannot format content type %q", n.mediaType)
	}
	header := textproto.MIMEHeader{"Content-Type": {contentType}}
	if n.encoding != "" {
		header.Set("Content-Transfer-Encoding", n.encoding)
	}
	if n.disposition != "" {
		params := map[string]string{}
		if n.fileName != "" {
			params["filename"] = n.fileName
		}
		header.Set("Content-Disposition", mime.FormatMediaType(n.disposition, params))
	}
	if n.contentID != "" {
		// Assigned rather than Set: textproto canonicalizes to "Content-Id",
		// and while RFC 5322 makes field names case-insensitive, every other
		// mailer emits "Content-ID". This is the header a client must match to
		// resolve a cid:, so it spells it the way the rest of the world does
		// instead of relying on every client's parser being case-correct.
		header["Content-ID"] = []string{"<" + n.contentID + ">"}
	}
	if n.description != "" {
		header.Set("Content-Description", encodeHeaderText(n.description))
	}
	return header, nil
}

// encodeHeaderText makes a caller-supplied string safe to place in an
// unstructured header (Subject, Content-Description).
//
// mime.QEncoding.Encode is the whole defence and it is applied
// unconditionally: its needsEncoding treats every rune below space except tab
// as needing encoding, so a CR or LF comes back as =0D=0A and cannot split the
// header block. Skipping the encode because the input "looks like ASCII" is
// precisely the hole - injected text looks like ASCII too.
//
// A value too long for one line is re-encoded as a run of encoded words joined
// by a fold. Whitespace between adjacent encoded words is dropped when they
// are decoded, so the original string is rebuilt exactly; folding *unencoded*
// text would insert a space at every fold point, and a long value with no
// spaces in it could not be folded at all.
func encodeHeaderText(s string) string {
	if encoded := mime.QEncoding.Encode(defaultCharset, s); len(encoded) <= maxHeaderLine {
		return encoded
	}
	// Room for "=?utf-8?q?" + "?=" and the space a fold begins with.
	maxPayload := maxHeaderLine - len("=?"+defaultCharset+"?q??=") - 1
	var out, word strings.Builder
	payload := 0
	flush := func() {
		if out.Len() > 0 {
			out.WriteString("\r\n ")
		}
		fmt.Fprintf(&out, "=?%s?q?%s?=", defaultCharset, word.String())
		word.Reset()
		payload = 0
	}
	for _, r := range s {
		q := qEncodeRune(r)
		if payload+len(q) > maxPayload {
			flush()
		}
		word.WriteString(q)
		payload += len(q)
	}
	flush()
	return out.String()
}

// qEncodeRune renders one rune in RFC 2047 Q form: printable ASCII stays
// itself, a space becomes an underscore, and everything else - a CR or an LF
// included - becomes =XX per UTF-8 byte.
func qEncodeRune(r rune) string {
	if r == ' ' {
		return "_"
	}
	if r > ' ' && r < 0x7f && r != '=' && r != '?' && r != '_' {
		return string(r)
	}
	var b strings.Builder
	for _, c := range []byte(string(r)) {
		fmt.Fprintf(&b, "=%02X", c)
	}
	return b.String()
}

// formatAddress parses one address and re-emits it from the parsed struct
// rather than from the caller's string. net/mail refuses a CR or LF anywhere
// in an address, and Address.String quotes or encodes the display name, so
// nothing the caller wrote reaches the header uninspected.
func formatAddress(addr string) (string, error) {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return "", fmt.Errorf("cannot parse address %q: %w", addr, err)
	}
	return parsed.String(), nil
}

// formatAddressList formats every address in one header value, folding onto a
// continuation line before an address that would take the line past
// maxHeaderLine. A single address longer than that is left whole: there is no
// legal place to fold inside one.
func formatAddressList(name string, addrs []string) (string, error) {
	var b strings.Builder
	length := len(name) + len(": ")
	for i, addr := range addrs {
		formatted, err := formatAddress(addr)
		if err != nil {
			return "", fmt.Errorf("invalid %s address: %w", name, err)
		}
		if i > 0 {
			b.WriteString(",")
			length++
			if length+1+len(formatted) > maxHeaderLine {
				b.WriteString("\r\n ")
				length = 1
			} else {
				b.WriteString(" ")
				length++
			}
		}
		b.WriteString(formatted)
		length += len(formatted)
	}
	return b.String(), nil
}

// normalizeContentID accepts either spelling the caller may have used -
// "chart" or "<chart>" - and returns the bare form, which is what a cid:
// reference matches and what the Content-ID header wraps in angle brackets
// again. The charset is restricted so nothing can escape the header; the same
// rule lives in guardrails.InlineAttachments, which reports a violation as a
// decision rather than as an error.
func normalizeContentID(id string) (string, error) {
	bare := id
	if len(bare) > 1 && strings.HasPrefix(bare, "<") && strings.HasSuffix(bare, ">") {
		bare = bare[1 : len(bare)-1]
	}
	if bare == "" {
		return "", fmt.Errorf("ContentId %q is empty", id)
	}
	for _, r := range bare {
		if !isContentIDRune(r) {
			return "", fmt.Errorf("ContentId %q contains %q, which is outside [A-Za-z0-9._@+-]", id, r)
		}
	}
	return bare, nil
}

// isContentIDRune reports whether r may appear in a Content-ID.
func isContentIDRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return strings.ContainsRune("._@+-", r)
	}
}

// isInline decides which attachments join the multipart/related group. INLINE
// is the caller's explicit signal; a ContentId with no disposition at all is
// the same intent expressed less completely - the part exists to be referenced
// by a cid: - so it is treated the same way, which is the difference between
// rendering and not rendering for a caller who sets only ContentId. An
// explicit ATTACHMENT wins, leaving a way to attach a file that merely carries
// a Content-ID. guardrails.InlineAttachments applies the identical rule, so
// the two packages agree on which attachments the inline checks cover.
func isInline(disposition, contentID string) bool {
	trimmed := strings.TrimSpace(disposition)
	return strings.EqualFold(trimmed, "INLINE") || (trimmed == "" && contentID != "")
}

// defaultBoundary returns a random boundary. crypto/rand.Text is 26 base32
// characters, every one of them legal in a boundary, and the result stays well
// inside multipart.Writer's 70 character limit.
func defaultBoundary() string { return "----=_Part_" + rand.Text() }

// qualifyContentIDs turns every bare Content-ID into the msg-id form the
// header grammar actually requires. RFC 2045 defines Content-ID as a msg-id,
// whose grammar is id-left "@" id-right - the "@" is not optional - and Gmail
// enforces it: a message carrying Content-ID: <chart> is accepted and
// delivered, but the cid: reference silently degrades to an ordinary
// attachment, while <chart@example.com> renders inline. Reported from the
// field with a controlled A/B (docs/plans/email-inline-mime.md).
//
// A bare id is qualified with the sender's domain, and every cid: reference
// to it in the HTML is rewritten to match, because RFC 2392 resolves a cid:
// URL against the full Content-ID minus its brackets - qualifying only the
// header would orphan the references. Ids that already carry an "@" are left
// exactly as given, as are their references.
func qualifyContentIDs(m *Message) error {
	domain := ""
	for i, a := range m.Attachments {
		if a.ContentID == "" {
			continue
		}
		bare, err := normalizeContentID(a.ContentID)
		if err != nil {
			return err
		}
		if strings.Contains(bare, "@") {
			m.Attachments[i].ContentID = bare
			continue
		}
		if domain == "" {
			parsed, err := mail.ParseAddress(m.From)
			if err != nil {
				return fmt.Errorf("cannot parse From address %q: %w", m.From, err)
			}
			if at := strings.LastIndex(parsed.Address, "@"); at >= 0 {
				domain = parsed.Address[at+1:]
			}
			if domain == "" {
				return fmt.Errorf("cannot derive a Content-ID domain from From address %q", m.From)
			}
		}
		qualified := bare + "@" + domain
		m.Attachments[i].ContentID = qualified
		// Word-boundary on the id charset so "chart" never rewrites inside
		// "chart2"; the (…|$) group is restored by the $2 in the replacement.
		re := regexp.MustCompile(`cid:` + regexp.QuoteMeta(bare) + `([^A-Za-z0-9._@+-]|$)`)
		m.HTML = re.ReplaceAllString(m.HTML, "cid:"+qualified+"$1")
	}
	return nil
}
