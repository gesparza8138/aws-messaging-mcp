package mimebuild

import (
	"fmt"
	"strings"
	"testing"
)

// nested is the four-level message the cap tests walk:
// mixed > alternative > related > (html, image), plus an attachment.
func nested(t *testing.T) []byte {
	t.Helper()
	m := pinned()
	m.Text = "see the chart"
	m.HTML = `<img src="cid:chart">`
	m.Attachments = []Attachment{inlineImage("chart"), fileAttachment()}
	msg, _ := assemble(t, m)
	return msg
}

func paths(parts []Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Path
	}
	return out
}

// TestWalkCaps: the bytes may be a caller's own MIME message, and a 10 MB one
// can carry millions of empty parts, so both limits truncate the list cleanly
// instead of failing or running to the end.
func TestWalkCaps(t *testing.T) {
	msg := nested(t)
	cases := []struct {
		name     string
		depth    int
		parts    int
		expected []string
	}{
		{"no limit reached", 10, 100, []string{"1", "1.1", "1.1.1", "1.1.2", "1.1.2.1", "1.1.2.2", "1.2"}},
		{"depth 0 stops at the root", 0, 100, []string{"1"}},
		{"depth 1 keeps the root's children", 1, 100, []string{"1", "1.1", "1.2"}},
		{"depth 2", 2, 100, []string{"1", "1.1", "1.1.1", "1.1.2", "1.2"}},
		{"parts 1", 10, 1, []string{"1"}},
		{"parts 4", 10, 4, []string{"1", "1.1", "1.1.1", "1.1.2"}},
		{"parts 0", 10, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := Walk(msg, tc.depth, tc.parts)
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			if got := paths(parts); strings.Join(got, ",") != strings.Join(tc.expected, ",") {
				t.Fatalf("paths %v, want %v", got, tc.expected)
			}
		})
	}
}

func TestWalkMalformed(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"not a message", "\x00\x01 no headers here", "headers"},
		{"unparseable Content-Type", "From: a@b.com\r\nContent-Type: text//plain\r\n\r\nbody\r\n", "Content-Type"},
		{"multipart with no boundary", "From: a@b.com\r\nContent-Type: multipart/mixed\r\n\r\nbody\r\n", "boundary"},
		{"truncated multipart", "From: a@b.com\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\n\r\nhi\r\n", "part 1"},
		{"unparseable child Content-Type", "From: a@b.com\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text//plain\r\n\r\nhi\r\n--B--\r\n", "part 1.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := Walk([]byte(tc.msg), 10, 100)
			if err == nil {
				t.Fatalf("malformed input accepted: %+v", parts)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// TestWalkHeaderDefaults covers what a message we did not assemble may look
// like: no Content-Type at all, a filename only on the Content-Type, and a
// Content-Disposition that will not parse.
func TestWalkHeaderDefaults(t *testing.T) {
	msg := strings.ReplaceAll(`From: a@b.com
Content-Type: multipart/mixed; boundary=B

--B

no content type at all
--B
Content-Type: image/png; name=chart.png
Content-Disposition: inline; filename
Content-Id: <chart>

BYTES
--B--
`, "\n", "\r\n")
	parts, err := Walk([]byte(msg), 10, 100)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []Part{
		{Path: "1", Depth: 0, ContentType: "multipart/mixed"},
		{Path: "1.1", Depth: 1, ContentType: "text/plain", Bytes: len("no content type at all")},
		{Path: "1.2", Depth: 1, ContentType: "image/png", ContentID: "chart",
			FileName: "chart.png", Bytes: len("BYTES")},
	}
	if len(parts) != len(want) {
		t.Fatalf("parts %+v", parts)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Errorf("part %d:\n got  %+v\n want %+v", i, parts[i], want[i])
		}
	}
}

// TestWalkContentIDForms: a Content-ID is reported bare whichever way the
// sender wrote it, so it matches the cid: reference in the HTML.
func TestWalkContentIDForms(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"<chart>", "chart"},
		{" <chart> ", "chart"},
		{"chart", "chart"},
		{"<", "<"},
	} {
		msg := fmt.Sprintf("From: a@b.com\r\nContent-Type: image/png\r\nContent-Id: %s\r\n\r\nBYTES\r\n", tc.header)
		parts, err := Walk([]byte(msg), 10, 100)
		if err != nil {
			t.Fatalf("Walk %q: %v", tc.header, err)
		}
		if parts[0].ContentID != tc.want {
			t.Errorf("Content-Id %q -> %q, want %q", tc.header, parts[0].ContentID, tc.want)
		}
	}
}
