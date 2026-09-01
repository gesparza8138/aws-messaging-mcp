package guardrails

import (
	"encoding/base64"
	"strings"
	"testing"
)

func attachment(name, content string) AttachmentInput {
	return AttachmentInput{FileName: name, RawContent: base64.StdEncoding.EncodeToString([]byte(content))}
}

func TestEmailAttachments(t *testing.T) {
	decoded, decisions := EmailAttachments(nil, 100)
	if decoded != nil || decisions != nil {
		t.Fatalf("nil list must decide nothing: %v %+v", decoded, decisions)
	}
	if _, decisions = EmailAttachments([]AttachmentInput{}, 100); decisions != nil {
		t.Fatalf("empty list must decide nothing: %+v", decisions)
	}

	decoded, decisions = EmailAttachments([]AttachmentInput{attachment("a.txt", "hello")}, 100)
	if len(decoded) != 1 || string(decoded[0]) != "hello" {
		t.Fatalf("decoded: %q", decoded)
	}
	if d := decisionByName(t, decisions, "attachment_size"); !d.Allowed || !strings.Contains(d.Reason, "5 bytes") {
		t.Fatalf("small attachment blocked: %+v", d)
	}

	mixed := []AttachmentInput{attachment("ok.txt", "fine"), {FileName: "bad.png", RawContent: "!!!not-base64"}}
	decoded, decisions = EmailAttachments(mixed, 100)
	if string(decoded[0]) != "fine" || decoded[1] != nil {
		t.Fatalf("failed decode must not disturb the others: %q", decoded)
	}
	d := decisionByName(t, decisions, "attachment_base64")
	if d.Allowed || !strings.Contains(d.Reason, "bad.png") || !strings.Contains(d.Reason, "1") {
		t.Fatalf("bad base64 must name the index and file: %+v", d)
	}

	if _, decisions = EmailAttachments([]AttachmentInput{attachment("a.txt", "0123456789")}, 10); !decisionByName(t, decisions, "attachment_size").Allowed {
		t.Fatal("exactly at the limit is allowed")
	}
	_, decisions = EmailAttachments([]AttachmentInput{attachment("a.txt", "0123456789"), attachment("b.txt", "x")}, 10)
	if d := decisionByName(t, decisions, "attachment_size"); d.Allowed || !strings.Contains(d.Reason, "11 bytes") {
		t.Fatalf("one byte over must block: %+v", d)
	}

	decoded, decisions = EmailAttachments([]AttachmentInput{attachment("empty.txt", "")}, 10)
	if len(decoded[0]) != 0 || !decisionByName(t, decisions, "attachment_size").Allowed {
		t.Fatalf("zero-byte attachments are legal: %q %+v", decoded, decisions)
	}
}

func inlineImage(name, id string) InlineAttachment {
	return InlineAttachment{FileName: name, ContentType: "image/png", Disposition: "INLINE", ContentID: id}
}

func plainAttachment(name string) InlineAttachment {
	return InlineAttachment{FileName: name, ContentType: "application/pdf", Disposition: "ATTACHMENT"}
}

// TestInlineAttachmentsEmpty: nothing inline decides nothing, so the plain
// attachment path behaves exactly as it does today.
func TestInlineAttachmentsEmpty(t *testing.T) {
	if d := InlineAttachments(nil, "<p>hi</p>"); d != nil {
		t.Fatalf("nil list must decide nothing: %+v", d)
	}
	if d := InlineAttachments([]InlineAttachment{plainAttachment("notes.pdf")}, "<p>hi</p>"); d != nil {
		t.Fatalf("a plain attachment must decide nothing: %+v", d)
	}
	// Even a body that talks about cid: is untouched when nothing is inline.
	if d := InlineAttachments([]InlineAttachment{plainAttachment("notes.pdf")}, "read about cid:nothing"); d != nil {
		t.Fatalf("the cid: scan must not run without an inline attachment: %+v", d)
	}
}

func TestInlineAttachmentsAllowed(t *testing.T) {
	atts := []InlineAttachment{inlineImage("chart.png", "chart"), plainAttachment("notes.pdf")}
	decisions := InlineAttachments(atts, `<img src="cid:chart"><img src="CID:chart">`)
	for _, name := range []string{"attachment_fields", "inline_content_id", "inline_needs_html", "inline_cid_refs"} {
		if d := decisionByName(t, decisions, name); !d.Allowed {
			t.Fatalf("a valid inline message was refused: %+v", d)
		}
	}
	if d := decisionByName(t, decisions, "inline_cid_refs"); !strings.Contains(d.Reason, "1 cid: references resolved") {
		t.Fatalf("the reference count is reported: %+v", d)
	}
	// A word ending in "cid" is not a reference.
	if d := InlineAttachments(atts, `<img src="cid:chart"> and prose about acid:rain`); !d[3].Allowed {
		t.Fatalf("acid: must not read as a cid: reference: %+v", d[3])
	}
	// The bracketed spelling is the same identifier.
	if d := InlineAttachments([]InlineAttachment{inlineImage("chart.png", "<chart>")}, `<img src="cid:chart">`); !d[3].Allowed {
		t.Fatalf("the angle-bracket spelling must match: %+v", d)
	}
	// A ContentId with no disposition at all is inline too, which is the rule
	// mimebuild uses when it decides what joins the multipart/related.
	implied := InlineAttachment{FileName: "chart.png", ContentType: "image/png", ContentID: "chart"}
	if d := InlineAttachments([]InlineAttachment{implied}, `<img src="cid:chart">`); len(d) != 4 {
		t.Fatalf("a bare ContentId is inline: %+v", d)
	}
}

// TestInlineAttachmentsUnreferenced: an inline attachment nothing references
// is a warning, not a refusal - the send is valid and the part simply arrives
// the way it does today.
func TestInlineAttachmentsUnreferenced(t *testing.T) {
	decisions := InlineAttachments([]InlineAttachment{inlineImage("chart.png", "chart")}, "<p>no images here</p>")
	d := decisionByName(t, decisions, "inline_cid_refs")
	if !d.Allowed || !strings.Contains(d.Reason, "cid:chart") || !strings.Contains(d.Reason, "ordinary attachment") {
		t.Fatalf("an unreferenced inline part must be allowed with a reason: %+v", d)
	}
}

func TestInlineAttachmentsRefusals(t *testing.T) {
	long := strings.Repeat("a", 300)
	cases := []struct {
		name    string
		atts    []InlineAttachment
		html    string
		blocked string
		reason  string
	}{
		{"no FileName", []InlineAttachment{inlineImage("", "chart")}, `<img src="cid:chart">`,
			"attachment_fields", "attachment 0 has no FileName"},
		{"FileName too long", []InlineAttachment{inlineImage(long, "chart")}, `<img src="cid:chart">`,
			"attachment_fields", "the maximum is 255"},
		{"ContentType too long", []InlineAttachment{{FileName: "chart.png", ContentType: "image/" + long,
			Disposition: "INLINE", ContentID: "chart"}}, `<img src="cid:chart">`,
			"attachment_fields", "the maximum is 78"},
		{"inline without a ContentId", []InlineAttachment{inlineImage("chart.png", "")}, `<p>hi</p>`,
			"inline_content_id", "is empty"},
		{"empty angle brackets", []InlineAttachment{inlineImage("chart.png", "<>")}, `<p>hi</p>`,
			"inline_content_id", "is empty"},
		{"ContentId too long", []InlineAttachment{inlineImage("chart.png", strings.Repeat("c", 79))},
			`<p>hi</p>`, "inline_content_id", "the maximum is 78"},
		{"ContentId charset", []InlineAttachment{inlineImage("chart.png", "chart image")}, `<p>hi</p>`,
			"inline_content_id", "[A-Za-z0-9._@+-]"},
		{"ContentId with a CRLF", []InlineAttachment{inlineImage("chart.png", "chart\r\nBcc: a@b.com")},
			`<p>hi</p>`, "inline_content_id", "outside"},
		{"duplicate ContentId", []InlineAttachment{inlineImage("a.png", "chart"), inlineImage("b.png", "<chart>")},
			`<img src="cid:chart">`, "inline_content_id", "reuses the ContentId"},
		{"no Html body", []InlineAttachment{inlineImage("chart.png", "chart")}, "",
			"inline_needs_html", "no Html body"},
		{"dangling cid: reference", []InlineAttachment{inlineImage("chart.png", "chart")},
			`<img src="cid:chart"><img src="cid:missing">`, "inline_cid_refs", "cid:missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decisions := InlineAttachments(tc.atts, tc.html)
			d := decisionByName(t, decisions, tc.blocked)
			if d.Allowed || !strings.Contains(d.Reason, tc.reason) {
				t.Fatalf("%s must block naming %q: %+v", tc.blocked, tc.reason, d)
			}
			if last := decisions[len(decisions)-1]; last.Name != tc.blocked {
				t.Fatalf("the ladder must stop at the refusal: %+v", decisions)
			}
			for _, other := range decisions[:len(decisions)-1] {
				if !other.Allowed {
					t.Fatalf("only %s may block: %+v", tc.blocked, other)
				}
			}
		})
	}
}

func TestInlineAttachmentIsInline(t *testing.T) {
	cases := []struct {
		disposition, contentID string
		want                   bool
	}{
		{"INLINE", "chart", true},
		{"inline", "chart", true},
		{" INLINE ", "", true},
		{"", "chart", true},
		{"ATTACHMENT", "chart", false},
		{"", "", false},
	}
	for _, tc := range cases {
		a := InlineAttachment{Disposition: tc.disposition, ContentID: tc.contentID}
		if got := a.isInline(); got != tc.want {
			t.Errorf("isInline(%q, %q) = %v, want %v", tc.disposition, tc.contentID, got, tc.want)
		}
	}
}

func TestBareContentID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<chart>", "chart"},
		{" <chart> ", "chart"},
		{"chart", "chart"},
		{"<", "<"},
		{"<>", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := bareContentID(tc.in); got != tc.want {
			t.Errorf("bareContentID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Every character class the charset allows, and one it does not.
	if r, bad := invalidContentIDRune("aZ9._@+-"); bad {
		t.Errorf("the allowed charset was rejected at %q", r)
	}
	if r, bad := invalidContentIDRune("a b"); !bad || r != ' ' {
		t.Errorf("a space must be rejected: %q %v", r, bad)
	}
}
