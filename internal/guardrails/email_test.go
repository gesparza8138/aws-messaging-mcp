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
