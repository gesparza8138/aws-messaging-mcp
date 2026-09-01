package mimebuild

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

// pinned makes an assembly deterministic: a fixed Date and boundaries that
// count up, so two runs produce identical bytes and a golden comparison means
// something.
func pinned() Message {
	n := 0
	return Message{
		From:    "Gabe <mcp@example.com>",
		To:      []string{"owner@example.com"},
		Subject: "Weekly chart",
		Date:    time.Date(2026, 9, 1, 10, 30, 0, 0, time.FixedZone("UTC", 0)),
		Boundary: func() string {
			n++
			return fmt.Sprintf("BOUNDARY%d", n)
		},
	}
}

func png() []byte { return []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a} }

func inlineImage(id string) Attachment {
	return Attachment{FileName: "chart.png", ContentType: "image/png", ContentID: id,
		Disposition: "INLINE", Content: png()}
}

func fileAttachment() Attachment {
	return Attachment{FileName: "notes.pdf", ContentType: "application/pdf",
		Disposition: "ATTACHMENT", Content: []byte("%PDF-1.7 not really")}
}

// tree renders the hierarchy of an assembled message by parsing the bytes back
// with mime/multipart, deliberately without going through Walk: nothing in
// this repo had ever re-read a message it built, which is how the inline-CID
// defect shipped (docs/plans/email-inline-mime.md §8).
func tree(t *testing.T, msg []byte) string {
	t.Helper()
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("cannot parse the assembled message: %v", err)
	}
	var b strings.Builder
	renderPart(t, &b, textproto.MIMEHeader(parsed.Header), parsed.Body, 0)
	return b.String()
}

func renderPart(t *testing.T, b *strings.Builder, header textproto.MIMEHeader, body io.Reader, depth int) {
	t.Helper()
	value := header.Get("Content-Type")
	if value == "" {
		value = "text/plain"
	}
	mediaType, params, err := mime.ParseMediaType(value)
	if err != nil {
		t.Fatalf("cannot parse Content-Type %q: %v", value, err)
	}
	fmt.Fprintf(b, "%s%s", strings.Repeat("  ", depth), mediaType)
	if params["type"] != "" {
		fmt.Fprintf(b, "; type=%q", params["type"])
	}
	var notes []string
	if cd := header.Get("Content-Disposition"); cd != "" {
		disposition, dparams, err := mime.ParseMediaType(cd)
		if err != nil {
			t.Fatalf("cannot parse Content-Disposition %q: %v", cd, err)
		}
		notes = append(notes, disposition)
		if name := dparams["filename"]; name != "" {
			notes = append(notes, "filename="+name)
		}
	}
	if id := header.Get("Content-Id"); id != "" {
		notes = append(notes, "cid="+id)
	}
	if len(notes) > 0 {
		fmt.Fprintf(b, " [%s]", strings.Join(notes, " "))
	}
	b.WriteString("\n")
	if !strings.HasPrefix(mediaType, "multipart/") {
		return
	}
	reader := multipart.NewReader(body, params["boundary"])
	for {
		part, err := reader.NextRawPart()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("cannot read the next part: %v", err)
		}
		renderPart(t, b, part.Header, part, depth+1)
	}
}

func assemble(t *testing.T, m Message) ([]byte, []Part) {
	t.Helper()
	msg, parts, err := Assemble(m)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	return msg, parts
}

// TestAssembleLayouts is the point of the package: the container hierarchy is
// asserted from the assembled bytes, for every shape the collapsing rules can
// produce.
func TestAssembleLayouts(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Message)
		want string
	}{
		{
			name: "html and inline only is a bare related",
			edit: func(m *Message) {
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart")}
			},
			want: `multipart/related; type="text/html"
  text/html
  image/png [inline filename=chart.png cid=<chart>]
`,
		},
		{
			name: "text and html and inline drops the mixed",
			edit: func(m *Message) {
				m.Text = "see the chart"
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart")}
			},
			want: `multipart/alternative
  text/plain
  multipart/related; type="text/html"
    text/html
    image/png [inline filename=chart.png cid=<chart>]
`,
		},
		{
			name: "an ordinary attachment adds the mixed",
			edit: func(m *Message) {
				m.Text = "see the chart"
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart"), fileAttachment()}
			},
			want: `multipart/mixed
  multipart/alternative
    text/plain
    multipart/related; type="text/html"
      text/html
      image/png [inline filename=chart.png cid=<chart>]
  application/pdf [attachment filename=notes.pdf]
`,
		},
		{
			name: "html and inline and an attachment drops the alternative",
			edit: func(m *Message) {
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart"), fileAttachment()}
			},
			want: `multipart/mixed
  multipart/related; type="text/html"
    text/html
    image/png [inline filename=chart.png cid=<chart>]
  application/pdf [attachment filename=notes.pdf]
`,
		},
		{
			name: "several inline images are siblings of the html",
			edit: func(m *Message) {
				m.HTML = `<img src="cid:a"><img src="cid:b">`
				first, second := inlineImage("a"), inlineImage("b")
				second.FileName = "second.png"
				m.Attachments = []Attachment{first, second}
			},
			want: `multipart/related; type="text/html"
  text/html
  image/png [inline filename=chart.png cid=<a>]
  image/png [inline filename=second.png cid=<b>]
`,
		},
		{
			name: "html alone is a bare text/html",
			edit: func(m *Message) { m.HTML = "<p>hi</p>" },
			want: "text/html\n",
		},
		{
			name: "text and html alone is a bare alternative",
			edit: func(m *Message) {
				m.Text = "hi"
				m.HTML = "<p>hi</p>"
			},
			want: `multipart/alternative
  text/plain
  text/html
`,
		},
		{
			name: "an attachment with no inline part keeps today's mixed",
			edit: func(m *Message) {
				m.Text = "hi"
				m.Attachments = []Attachment{fileAttachment()}
			},
			want: `multipart/mixed
  text/plain
  application/pdf [attachment filename=notes.pdf]
`,
		},
		{
			name: "an inline part with no html travels as an attachment",
			edit: func(m *Message) {
				m.Text = "hi"
				m.Attachments = []Attachment{inlineImage("chart")}
			},
			want: `multipart/mixed
  text/plain
  image/png [inline filename=chart.png cid=<chart>]
`,
		},
		{
			name: "a ContentId with no disposition is inline too",
			edit: func(m *Message) {
				m.HTML = `<img src="cid:chart">`
				a := inlineImage("chart")
				a.Disposition = ""
				m.Attachments = []Attachment{a}
			},
			want: `multipart/related; type="text/html"
  text/html
  image/png [inline filename=chart.png cid=<chart>]
`,
		},
		{
			name: "an explicit ATTACHMENT keeps a Content-ID out of the related group",
			edit: func(m *Message) {
				m.HTML = `<p>hi</p>`
				a := inlineImage("chart")
				a.Disposition = "ATTACHMENT"
				m.Attachments = []Attachment{a}
			},
			want: `multipart/mixed
  text/html
  image/png [attachment filename=chart.png cid=<chart>]
`,
		},
		{
			name: "no body at all is still a valid one-part message",
			edit: func(*Message) {},
			want: "text/plain\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := pinned()
			tc.edit(&m)
			msg, _ := assemble(t, m)
			if got := tree(t, msg); got != tc.want {
				t.Fatalf("tree:\n%s\nwant:\n%s\nmessage:\n%s", got, tc.want, msg)
			}
		})
	}
}

// TestAssembleTopHeaders checks the message's own header block, including the
// two that must not be there.
func TestAssembleTopHeaders(t *testing.T) {
	m := pinned()
	m.Cc = []string{"cc@example.com"}
	m.ReplyTo = []string{"Reply Here <reply@example.com>"}
	m.HTML = "<p>hi</p>"
	msg, _ := assemble(t, m)
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("cannot parse: %v", err)
	}
	want := map[string]string{
		"From":                      `"Gabe" <mcp@example.com>`,
		"To":                        "<owner@example.com>",
		"Cc":                        "<cc@example.com>",
		"Reply-To":                  `"Reply Here" <reply@example.com>`,
		"Subject":                   "Weekly chart",
		"Date":                      "Tue, 01 Sep 2026 10:30:00 +0000",
		"Mime-Version":              "1.0",
		"Content-Transfer-Encoding": "quoted-printable",
		"Content-Type":              "text/html; charset=utf-8",
	}
	for name, value := range want {
		if got := parsed.Header.Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
	if got := parsed.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc must never be written: %q", got)
	}

	// Empty lists are omitted rather than written empty.
	bare := pinned()
	bare.Cc = nil
	bare.ReplyTo = nil
	bare.HTML = "<p>hi</p>"
	msg, _ = assemble(t, bare)
	for _, name := range []string{"Cc:", "Reply-To:"} {
		if bytes.Contains(msg, []byte("\r\n"+name)) {
			t.Errorf("%s must be omitted when empty:\n%s", name, msg)
		}
	}
}

// TestAssembleRoundTrip proves the parts Assemble reports are the parts a
// reader finds in the bytes it produced.
func TestAssembleRoundTrip(t *testing.T) {
	m := pinned()
	m.Text = "see the chart"
	m.HTML = `<img src="cid:chart">`
	m.Attachments = []Attachment{inlineImage("chart"), fileAttachment()}
	msg, parts := assemble(t, m)

	walked, err := Walk(msg, 10, 100)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(walked) != len(parts) {
		t.Fatalf("Assemble reported %d parts, Walk found %d:\n%+v\n%+v", len(parts), len(walked), parts, walked)
	}
	for i := range parts {
		if parts[i] != walked[i] {
			t.Errorf("part %d:\n assembled %+v\n walked    %+v", i, parts[i], walked[i])
		}
	}
	want := []Part{
		{Path: "1", Depth: 0, ContentType: "multipart/mixed"},
		{Path: "1.1", Depth: 1, ContentType: "multipart/alternative"},
		{Path: "1.1.1", Depth: 2, ContentType: "text/plain", Bytes: len("see the chart")},
		{Path: "1.1.2", Depth: 2, ContentType: "multipart/related"},
		{Path: "1.1.2.1", Depth: 3, ContentType: "text/html", Bytes: len(`<img src=3D"cid:chart">`)},
		{Path: "1.1.2.2", Depth: 3, ContentType: "image/png", Disposition: "inline",
			ContentID: "chart", FileName: "chart.png", Bytes: 12},
		{Path: "1.2", Depth: 1, ContentType: "application/pdf", Disposition: "attachment",
			FileName: "notes.pdf", Bytes: 28},
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("part %d:\n got  %+v\n want %+v", i, parts[i], want[i])
		}
	}
}

// TestAssembleHeaderInjection is the new attack surface: multipart.Writer
// formats headers with a bare fmt.Fprintf, so a CRLF in any caller string
// would otherwise write headers of its own.
func TestAssembleHeaderInjection(t *testing.T) {
	const attack = "chart\r\nBcc: attacker@example.com"

	t.Run("encoded away", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(*Message)
		}{
			{"subject", func(m *Message) { m.Subject = attack }},
			{"description", func(m *Message) { m.Attachments[0].Description = attack }},
			{"filename", func(m *Message) { m.Attachments[0].FileName = attack + ".png" }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := pinned()
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart")}
				tc.edit(&m)
				msg, _ := assemble(t, m)
				assertNoInjectedHeader(t, msg)
			})
		}
	})

	t.Run("refused", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(*Message)
		}{
			{"from", func(m *Message) { m.From = attack + " <mcp@example.com>" }},
			{"to", func(m *Message) { m.To = []string{attack + " <owner@example.com>"} }},
			{"cc", func(m *Message) { m.Cc = []string{"a@b.com\r\nBcc: attacker@example.com"} }},
			{"reply-to", func(m *Message) { m.ReplyTo = []string{attack} }},
			{"content id", func(m *Message) { m.Attachments[0].ContentID = attack }},
			{"content type", func(m *Message) { m.Attachments[0].ContentType = "image/png\r\nBcc: a@b.com" }},
			{"content type parameter", func(m *Message) {
				m.Attachments[0].ContentType = `image/png; note="` + attack + `"`
			}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := pinned()
				m.HTML = `<img src="cid:chart">`
				m.Attachments = []Attachment{inlineImage("chart")}
				tc.edit(&m)
				msg, _, err := Assemble(m)
				if err == nil {
					t.Fatalf("injection accepted:\n%s", msg)
				}
			})
		}
	})
}

// assertNoInjectedHeader fails if the attacker's Bcc reached the message as a
// header, at the top level or on any part.
func assertNoInjectedHeader(t *testing.T, msg []byte) {
	t.Helper()
	for _, line := range strings.Split(string(msg), "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "bcc:") {
			t.Fatalf("injected header line %q in:\n%s", line, msg)
		}
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("cannot parse: %v", err)
	}
	if got := parsed.Header.Get("Bcc"); got != "" {
		t.Fatalf("injected Bcc header %q", got)
	}
	// Parsing the whole tree also proves the part headers still line up.
	tree(t, msg)
}

// TestAssembleLineLength covers the encoder that does not wrap itself:
// base64.NewEncoder would turn a 1 MB image into one 1.4 M character line,
// well past RFC 5321's 1000 octet limit.
func TestAssembleLineLength(t *testing.T) {
	blob := make([]byte, 1<<20)
	for i := range blob {
		blob[i] = byte(i % 251)
	}
	m := pinned()
	// Non-ASCII in a long subject exercises the =XX form of the folded
	// encoded words, which is the same escape a CR or LF would take.
	m.Subject = strings.Repeat("a very long subject ", 40) + "é"
	m.To = nil
	for i := 0; i < 40; i++ {
		m.To = append(m.To, fmt.Sprintf("recipient-number-%02d@example.com", i))
	}
	m.Text = strings.Repeat("long text with no newlines at all ", 100)
	m.HTML = `<img src="cid:chart">` + strings.Repeat("<span>padding</span>", 200)
	m.Attachments = []Attachment{
		{FileName: "chart.png", ContentType: "image/png", ContentID: "chart",
			Disposition: "INLINE", Content: blob},
	}
	msg, _ := assemble(t, m)
	for i, line := range strings.Split(string(msg), "\r\n") {
		if len(line) > 998 {
			t.Fatalf("line %d is %d bytes, the maximum is 998: %.80q...", i, len(line), line)
		}
	}
	// The folded headers must still parse back to the values that went in.
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("cannot parse: %v", err)
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatalf("cannot decode the subject: %v", err)
	}
	if decoded != m.Subject {
		t.Fatalf("subject round trip:\n got  %q\n want %q", decoded, m.Subject)
	}
	addrs, err := parsed.Header.AddressList("To")
	if err != nil || len(addrs) != 40 {
		t.Fatalf("To list: %d addresses, %v", len(addrs), err)
	}
}

// TestAssembleDeterministic pins the date and the boundaries, so the same
// message always assembles to the same bytes.
func TestAssembleDeterministic(t *testing.T) {
	build := func() []byte {
		m := pinned()
		m.Text = "see the chart"
		m.HTML = `<img src="cid:chart">`
		m.Attachments = []Attachment{inlineImage("chart")}
		msg, _ := assemble(t, m)
		return msg
	}
	first, second := build(), build()
	if !bytes.Equal(first, second) {
		t.Fatalf("two assemblies differ:\n%s\n---\n%s", first, second)
	}
	golden := strings.ReplaceAll(`From: "Gabe" <mcp@example.com>
To: <owner@example.com>
Subject: Weekly chart
Date: Tue, 01 Sep 2026 10:30:00 +0000
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary=BOUNDARY1

--BOUNDARY1
Content-Transfer-Encoding: quoted-printable
Content-Type: text/plain; charset=utf-8

see the chart
--BOUNDARY1
Content-Type: multipart/related; boundary=BOUNDARY2; type="text/html"

--BOUNDARY2
Content-Transfer-Encoding: quoted-printable
Content-Type: text/html; charset=utf-8

<img src=3D"cid:chart">
--BOUNDARY2
Content-Disposition: inline; filename=chart.png
Content-ID: <chart>
Content-Transfer-Encoding: base64
Content-Type: image/png; name=chart.png

iVBORw0KGgo=
--BOUNDARY2--

--BOUNDARY1--
`, "\n", "\r\n")
	if string(first) != golden {
		t.Fatalf("assembled:\n%q\nwant:\n%q", first, golden)
	}
}

// TestAssembleContentID checks both spellings the caller may send, since a
// client already tried the angle-bracket form on the assumption it might be
// the difference (docs/plans/email-inline-mime.md §1).
func TestAssembleContentID(t *testing.T) {
	build := func(id string) []byte {
		m := pinned()
		m.HTML = `<img src="cid:chart">`
		m.Attachments = []Attachment{inlineImage(id)}
		msg, _ := assemble(t, m)
		return msg
	}
	bare, bracketed := build("chart"), build("<chart>")
	if !bytes.Equal(bare, bracketed) {
		t.Fatalf("the two ContentId spellings must assemble identically:\n%s\n---\n%s", bare, bracketed)
	}
	if !bytes.Contains(bare, []byte("Content-ID: <chart>\r\n")) {
		t.Fatalf("the header is always the bracketed form:\n%s", bare)
	}
	if _, parts := assemble(t, Message{From: "a@b.com", HTML: "<p>x</p>",
		Attachments: []Attachment{inlineImage("<chart>")}}); parts[2].ContentID != "chart" {
		t.Fatalf("the reported ContentID is the bare form: %+v", parts[2])
	}
}

// TestAssembleTransferEncodings covers the three encodings, including the one
// that must fail rather than emit a corrupt message.
func TestAssembleTransferEncodings(t *testing.T) {
	text := []byte("plain ascii bytes")
	cases := []struct {
		name     string
		encoding string
		content  []byte
		want     string
	}{
		{"default is base64", "", png(), "base64"},
		{"explicit base64", "BASE64", png(), "base64"},
		{"quoted printable", "QUOTED_PRINTABLE", text, "quoted-printable"},
		{"seven bit", "SEVEN_BIT", text, "7bit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := pinned()
			m.Text = "hi"
			m.Attachments = []Attachment{{FileName: "f.bin", ContentType: "application/octet-stream",
				TransferEncoding: tc.encoding, Content: tc.content}}
			msg, _ := assemble(t, m)
			if !bytes.Contains(msg, []byte("Content-Transfer-Encoding: "+tc.want+"\r\n")) {
				t.Fatalf("want %q in:\n%s", tc.want, msg)
			}
		})
	}

	m := pinned()
	m.Text = "hi"
	m.Attachments = []Attachment{{FileName: "chart.png", TransferEncoding: "SEVEN_BIT", Content: png()}}
	_, _, err := Assemble(m)
	if err == nil || !strings.Contains(err.Error(), "chart.png") || !strings.Contains(err.Error(), "7-bit") {
		t.Fatalf("SEVEN_BIT on binary must fail by name: %v", err)
	}
	m.Attachments[0].TransferEncoding = "UUENCODE"
	if _, _, err := Assemble(m); err == nil || !strings.Contains(err.Error(), "UUENCODE") {
		t.Fatalf("an unknown encoding must fail by name: %v", err)
	}
}

func TestAssembleErrors(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Message)
		want string
	}{
		{"no From", func(m *Message) { m.From = "" }, "From"},
		{"unparseable From", func(m *Message) { m.From = "not an address" }, "From"},
		{"unparseable To", func(m *Message) { m.To = []string{"nope"} }, "To"},
		{"unparseable Cc", func(m *Message) { m.Cc = []string{"nope"} }, "Cc"},
		{"unparseable Reply-To", func(m *Message) { m.ReplyTo = []string{"nope"} }, "Reply-To"},
		{"unparseable ContentType", func(m *Message) { m.Attachments[0].ContentType = "image//png" }, "ContentType"},
		{"empty ContentId", func(m *Message) { m.Attachments[0].ContentID = "<>" }, "empty"},
		{"ContentId charset", func(m *Message) { m.Attachments[0].ContentID = "chart image" }, "[A-Za-z0-9._@+-]"},
		{"half-bracketed ContentId", func(m *Message) { m.Attachments[0].ContentID = "<chart" }, "[A-Za-z0-9._@+-]"},
		{"unusable boundary", func(m *Message) { m.Boundary = func() string { return "a b\r\nc" } }, "boundary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := pinned()
			m.HTML = `<img src="cid:chart">`
			m.Attachments = []Attachment{inlineImage("chart")}
			tc.edit(&m)
			_, _, err := Assemble(m)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestPartHeaderRejectsUnusableType covers the guard that keeps a media type
// FormatMediaType cannot render out of the header block. Assemble validates
// every caller-supplied type before it builds a node, so this is reachable
// only from inside the package - which is exactly why the guard is there.
func TestPartHeaderRejectsUnusableType(t *testing.T) {
	if _, err := partHeader(&node{mediaType: "image/png; x"}); err == nil {
		t.Fatal("an unformattable content type must be refused")
	}
}

// TestAssembleZeroDate: a message with no Date reads as spam, so a caller who
// forgot to inject a clock still gets a plausible one.
func TestAssembleZeroDate(t *testing.T) {
	m := pinned()
	m.Date = time.Time{}
	m.Text = "hi"
	msg, _ := assemble(t, m)
	parsed, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatalf("cannot parse: %v", err)
	}
	date, err := mail.ParseDate(parsed.Header.Get("Date"))
	if err != nil {
		t.Fatalf("cannot parse the Date header: %v", err)
	}
	if time.Since(date) > time.Minute || time.Since(date) < -time.Minute {
		t.Fatalf("Date %v is not now", date)
	}
}

// TestAssembleDefaultBoundary covers the crypto/rand generator used when the
// caller injects none: distinct per call, and short enough for the writer.
func TestAssembleDefaultBoundary(t *testing.T) {
	m := pinned()
	m.Boundary = nil
	m.Text = "hi"
	m.HTML = "<p>hi</p>"
	first, _ := assemble(t, m)
	second, _ := assemble(t, m)
	if bytes.Equal(first, second) {
		t.Fatal("random boundaries must differ between assemblies")
	}
	if b := defaultBoundary(); len(b) > 70 || b == defaultBoundary() {
		t.Fatalf("boundary %q must be unique and at most 70 characters", b)
	}
	tree(t, first)
}

// TestAssembleCharsets: the caller's charset reaches the part, and an empty
// one defaults to UTF-8.
func TestAssembleCharsets(t *testing.T) {
	m := pinned()
	m.Text = "hi"
	m.HTML = "<p>hi</p>"
	m.TextCharset = "iso-8859-1"
	msg, _ := assemble(t, m)
	if !bytes.Contains(msg, []byte("text/plain; charset=iso-8859-1")) ||
		!bytes.Contains(msg, []byte("text/html; charset=utf-8")) {
		t.Fatalf("charsets:\n%s", msg)
	}
}
