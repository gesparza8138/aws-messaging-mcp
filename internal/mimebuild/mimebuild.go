// Package mimebuild assembles a complete MIME message whose HTML part and
// inline image parts are siblings inside a multipart/related, and walks an
// assembled message back into a flat description of its parts.
//
// It exists because SES's Content.Simple shape puts every attachment under a
// multipart/mixed root, and a cid: reference only resolves when the HTML part
// and the referenced part are siblings inside a multipart/related (RFC 2387):
// INLINE plus a ContentId is accepted and delivered today, but the image
// arrives as an ordinary attachment (docs/plans/email-inline-mime.md).
// Building the message here and sending it as Content.Raw is the fix.
//
// Only the standard library is imported, and nothing here reaches AWS. The
// package also does not re-check the caller's string lengths:
// guardrails.InlineAttachments owns the FileName, ContentType, and ContentId
// rules SES used to enforce on the Simple path, and it runs first. What is
// enforced here is only what would otherwise produce a *broken* message -
// header injection, an unusable transfer encoding, an unusable content type.
package mimebuild

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"strings"
	"time"
)

// Transfer encodings, in the SES spelling on the way in (BASE64,
// QUOTED_PRINTABLE, SEVEN_BIT) and the MIME header spelling on the way out.
const (
	encodingBase64 = "base64"
	encodingQP     = "quoted-printable"
	encoding7Bit   = "7bit"
)

const (
	dispositionInline     = "inline"
	dispositionAttachment = "attachment"
)

// defaultCharset is used for a text part whose charset the caller left empty.
const defaultCharset = "utf-8"

// Part is one node of a MIME tree, flat with a dotted path ("1" is the whole
// message, "1.2" its second child, "1.2.1" that child's first).
//
// Flat and non-recursive on purpose: jsonschema.For returns an error on a
// named recursive type, and the server turns that error into a panic at tool
// registration, so a self-referential Parts []Part field would take down every
// tool for the sake of one diagnostic field (docs/plans/email-inline-mime.md
// §6). A reader that wants a tree rebuilds it from Path.
type Part struct {
	Path        string `json:"path"`
	Depth       int    `json:"depth"`
	ContentType string `json:"content_type"`
	Disposition string `json:"disposition,omitempty"`
	ContentID   string `json:"content_id,omitempty"` // bare, brackets stripped
	FileName    string `json:"filename,omitempty"`
	Bytes       int    `json:"bytes"`
}

// Attachment is one file to place in the message. Content is the decoded
// bytes (the guardrails already decoded them), and an attachment is inline -
// a member of the multipart/related group rather than of the multipart/mixed
// one - by the rule in isInline.
type Attachment struct {
	FileName         string
	ContentType      string // defaults to application/octet-stream
	ContentID        string // with or without angle brackets; emitted as <id>
	Disposition      string // INLINE or ATTACHMENT (SES spelling)
	TransferEncoding string // BASE64 (default), QUOTED_PRINTABLE, or SEVEN_BIT
	Description      string
	Content          []byte
}

// Message is everything Assemble needs to build a message.
//
// There is deliberately no Bcc field. Blind recipients ride on the SES API's
// Destination.BccAddresses, which SES honours for a raw message without
// disclosing them; a Bcc header inside the message we build would be delivered
// to everybody and would leak every hidden recipient to every recipient. To
// and Cc do become headers, because a message with no To displays as
// undisclosed-recipients.
type Message struct {
	From    string
	To      []string
	Cc      []string
	ReplyTo []string
	Subject string

	Text        string
	HTML        string
	TextCharset string // defaults to utf-8
	HTMLCharset string // defaults to utf-8

	Attachments []Attachment

	// Date is written as the Date header. It is injected rather than read
	// from the clock so a test can pin it; a zero value falls back to
	// time.Now, because a message without a plausible Date reads as spam.
	Date time.Time

	// Boundary returns one multipart boundary per call. Injected so a test can
	// pin them; nil means random boundaries from crypto/rand.
	Boundary func() string
}

// node is one node of the tree Assemble builds before it writes anything.
// A multipart node's boundary has to be known when its *parent* writes the
// Content-Type header that names it, and multipart.Writer only accepts
// SetBoundary before the first part is created - so boundaries are assigned in
// a pass over the finished tree, not discovered while writing.
type node struct {
	mediaType string            // media type with no parameters
	params    map[string]string // charset, name, boundary, type
	children  []*node

	// Leaf fields; ignored for a multipart node.
	disposition string // inline, attachment, or empty for a body part
	contentID   string // bare, no angle brackets
	fileName    string
	description string
	encoding    string // Content-Transfer-Encoding value
	body        []byte
}

func (n *node) isMultipart() bool { return strings.HasPrefix(n.mediaType, "multipart/") }

// Assemble builds the complete MIME message and returns it with a flat
// description of the tree it built, in the same shape Walk produces, so the
// caller learns the structure without parsing the bytes back.
//
// Every value that reaches a header is encoded or parsed and re-emitted, never
// interpolated: multipart.Writer.CreatePart formats headers with a bare
// fmt.Fprintf("%s: %s\r\n") and sanitises nothing, so a CR or LF in any
// caller-supplied string would otherwise inject headers of its own. Today SES
// writes every header for us and none of this is reachable; the moment we
// assemble, all of it is.
func Assemble(m Message) ([]byte, []Part, error) {
	root, err := buildTree(m)
	if err != nil {
		return nil, nil, err
	}
	boundary := m.Boundary
	if boundary == nil {
		boundary = defaultBoundary
	}
	assignBoundaries(root, boundary)

	var buf bytes.Buffer
	if err := writeTopHeaders(&buf, m, root); err != nil {
		return nil, nil, err
	}
	var parts []Part
	if err := writeNode(&buf, root, "1", 0, &parts); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), parts, nil
}

// buildTree chooses the containers. The target is alternative-outer:
//
//	multipart/mixed                       (only when ordinary attachments exist)
//	├── multipart/alternative             (only when both Text and HTML are set)
//	│   ├── text/plain
//	│   └── multipart/related; type="text/html"
//	│       ├── text/html                 (the root part of the related group)
//	│       └── inline parts, each with a Content-ID
//	└── attachment parts
//
// Every outer layer collapses when it would hold one child, so HTML plus one
// inline image is a bare multipart/related.
//
// Alternative-outer rather than related-outer because RFC 2387's type=
// parameter must name the media type of the *root part* of the related group.
// Related-outer makes that root the multipart/alternative, so the only correct
// header would be type="multipart/alternative" - and getting that parameter
// wrong is itself a documented cause of the exact "the image arrives as an
// attachment" symptom this package exists to fix. Here the related group's
// root is a plain text/html and type="text/html" is trivially right, which is
// also the shape mainstream senders emit and client heuristics are tuned for.
func buildTree(m Message) (*node, error) {
	var inline, files []*node
	for i, a := range m.Attachments {
		n, err := attachmentNode(a, i)
		if err != nil {
			return nil, err
		}
		// With no HTML nothing can carry a cid: reference, so an inline part
		// has no related group to join and travels as an ordinary attachment
		// in the caller's original order. guardrails' inline_needs_html
		// refuses that shape before it reaches us; assembling something valid
		// anyway keeps Assemble total.
		if m.HTML != "" && isInline(a.Disposition, a.ContentID) {
			inline = append(inline, n)
			continue
		}
		files = append(files, n)
	}

	var text, html *node
	if m.Text != "" {
		text = textNode("text/plain", m.Text, m.TextCharset)
	}
	if m.HTML != "" {
		html = textNode("text/html", m.HTML, m.HTMLCharset)
	}
	// The related group stands in for the HTML part wherever that part would
	// have gone: as the alternative's second child, or as the whole body.
	htmlBranch := html
	if html != nil && len(inline) > 0 {
		htmlBranch = &node{
			mediaType: "multipart/related",
			params:    map[string]string{"type": "text/html"},
			children:  append([]*node{html}, inline...),
		}
	}

	var body *node
	switch {
	case text != nil && htmlBranch != nil:
		body = &node{mediaType: "multipart/alternative", params: map[string]string{}, children: []*node{text, htmlBranch}}
	case htmlBranch != nil:
		body = htmlBranch
	case text != nil:
		body = text
	default:
		// Neither body: an empty text/plain, so the message is still a valid
		// one part message rather than a header block with nothing under it.
		body = textNode("text/plain", "", m.TextCharset)
	}
	if len(files) > 0 {
		body = &node{mediaType: "multipart/mixed", params: map[string]string{}, children: append([]*node{body}, files...)}
	}
	return body, nil
}

// textNode builds a body part. Text goes out quoted-printable, which wraps
// its own lines, so a long HTML body cannot produce a line over RFC 5321's
// 1000 octet limit.
func textNode(mediaType, data, charset string) *node {
	if charset == "" {
		charset = defaultCharset
	}
	return &node{
		mediaType: mediaType,
		params:    map[string]string{"charset": charset},
		encoding:  encodingQP,
		body:      []byte(data),
	}
}

// attachmentNode validates the caller's strings and builds one file part. The
// content type is parsed rather than interpolated so that a type carrying
// parameters ("text/calendar; method=REQUEST") survives and a type carrying a
// CRLF is refused here instead of splitting the header block later.
func attachmentNode(a Attachment, i int) (*node, error) {
	declared := a.ContentType
	if declared == "" {
		declared = "application/octet-stream"
	}
	mediaType, params, err := mime.ParseMediaType(declared)
	if err != nil {
		return nil, fmt.Errorf("attachment %d (%q): invalid ContentType %q: %w", i, a.FileName, a.ContentType, err)
	}
	if a.FileName != "" {
		params["name"] = a.FileName
	}
	encoding, err := transferEncoding(a, i)
	if err != nil {
		return nil, err
	}
	var contentID string
	if a.ContentID != "" {
		contentID, err = normalizeContentID(a.ContentID)
		if err != nil {
			return nil, fmt.Errorf("attachment %d (%q): %w", i, a.FileName, err)
		}
	}
	disposition := dispositionAttachment
	if isInline(a.Disposition, a.ContentID) {
		disposition = dispositionInline
	}
	return &node{
		mediaType:   mediaType,
		params:      params,
		disposition: disposition,
		contentID:   contentID,
		fileName:    a.FileName,
		description: a.Description,
		encoding:    encoding,
		body:        a.Content,
	}, nil
}

// transferEncoding maps the SES spelling onto the MIME header value. SEVEN_BIT
// on bytes that are not 7 bit clean is refused by name rather than emitted:
// the message would be silently corrupted in transit, and an error the caller
// can read is worth more than a delivery that looks like it worked.
func transferEncoding(a Attachment, i int) (string, error) {
	switch a.TransferEncoding {
	case "", "BASE64":
		return encodingBase64, nil
	case "QUOTED_PRINTABLE":
		return encodingQP, nil
	case "SEVEN_BIT":
		for offset, b := range a.Content {
			if b == 0 || b > 127 {
				return "", fmt.Errorf("attachment %d (%q): SEVEN_BIT requested but byte %d is %#02x, which is not 7-bit clean; use BASE64", i, a.FileName, offset, b)
			}
		}
		return encoding7Bit, nil
	default:
		return "", fmt.Errorf("attachment %d (%q): unknown ContentTransferEncoding %q; use BASE64, QUOTED_PRINTABLE, or SEVEN_BIT", i, a.FileName, a.TransferEncoding)
	}
}

// assignBoundaries fills in every multipart node's boundary, depth first, so
// the order the injected generator is called in is fixed.
func assignBoundaries(n *node, boundary func() string) {
	if !n.isMultipart() {
		return
	}
	n.params["boundary"] = boundary()
	for _, c := range n.children {
		assignBoundaries(c, boundary)
	}
}

// writeNode writes one node's body (its parent already wrote its headers) and
// appends its description, depth first, so the returned parts are in the same
// order Walk produces from the finished bytes.
func writeNode(w io.Writer, n *node, path string, depth int, parts *[]Part) error {
	p := Part{
		Path:        path,
		Depth:       depth,
		ContentType: n.mediaType,
		Disposition: n.disposition,
		ContentID:   n.contentID,
		FileName:    n.fileName,
	}
	if !n.isMultipart() {
		written, err := writeBody(w, n)
		if err != nil {
			return err
		}
		p.Bytes = written
		*parts = append(*parts, p)
		return nil
	}
	// A container reports no bytes of its own; its children carry them.
	*parts = append(*parts, p)

	mw := multipart.NewWriter(w)
	// The only legal moment to set the boundary: it is already in the
	// Content-Type header the parent wrote, and after the first CreatePart the
	// writer refuses to change it.
	if err := mw.SetBoundary(n.params["boundary"]); err != nil {
		return fmt.Errorf("part %s: %w", path, err)
	}
	for i, c := range n.children {
		header, err := partHeader(c)
		if err != nil {
			return err
		}
		pw, err := mw.CreatePart(header)
		if err != nil {
			return err
		}
		if err := writeNode(pw, c, fmt.Sprintf("%s.%d", path, i+1), depth+1, parts); err != nil {
			return err
		}
	}
	return mw.Close()
}

// writeBody encodes a leaf's bytes and reports how many bytes it wrote, which
// is what Walk counts when it reads the same part back.
func writeBody(w io.Writer, n *node) (int, error) {
	c := &countingWriter{w: w}
	switch n.encoding {
	case encodingBase64:
		enc := base64.NewEncoder(base64.StdEncoding, &lineWrapper{w: c})
		if _, err := enc.Write(n.body); err != nil {
			return c.n, err
		}
		if err := enc.Close(); err != nil {
			return c.n, err
		}
	case encodingQP:
		qp := quotedprintable.NewWriter(c)
		if _, err := qp.Write(n.body); err != nil {
			return c.n, err
		}
		if err := qp.Close(); err != nil {
			return c.n, err
		}
	default: // 7bit, already checked to be 7-bit clean
		if _, err := c.Write(n.body); err != nil {
			return c.n, err
		}
	}
	return c.n, nil
}

// countingWriter counts what it passes through, so Assemble can report each
// part's encoded size without measuring offsets into the buffer.
type countingWriter struct {
	w io.Writer
	n int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += n
	return n, err
}

// base64LineLen is the classic base64 body line length. RFC 5321 caps a line
// at 1000 octets including CRLF, and base64.NewEncoder wraps nothing at all -
// a 75 KB image would otherwise be one 100,000 character line, accepted here
// and mangled or rejected in transit.
const base64LineLen = 76

// lineWrapper breaks the base64 stream into base64LineLen character lines
// separated by CRLF.
type lineWrapper struct {
	w io.Writer
	n int // characters already on the current line
}

func (l *lineWrapper) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if l.n == base64LineLen {
			if _, err := l.w.Write([]byte("\r\n")); err != nil {
				return written, err
			}
			l.n = 0
		}
		chunk := p
		if room := base64LineLen - l.n; len(chunk) > room {
			chunk = chunk[:room]
		}
		n, err := l.w.Write(chunk)
		written += n
		l.n += n
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}
