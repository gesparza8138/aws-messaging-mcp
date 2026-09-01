package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/smithy-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/mimebuild"
	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
)

type fakeSES struct {
	sendIn   *sesv2.SendEmailInput
	sendErr  error
	listErr  error
	getErr   error
	prodMode bool
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.sendIn = in
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &sesv2.SendEmailOutput{MessageId: aws.String("msg-123")}, nil
}

func (f *fakeSES) ListEmailIdentities(context.Context, *sesv2.ListEmailIdentitiesInput, ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &sesv2.ListEmailIdentitiesOutput{}, nil
}

func (f *fakeSES) GetAccount(context.Context, *sesv2.GetAccountInput, ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &sesv2.GetAccountOutput{ProductionAccessEnabled: f.prodMode, SendingEnabled: true,
		SendQuota: &sestypes.SendQuota{Max24HourSend: 200, MaxSendRate: 1, SentLast24Hours: 3}}, nil
}

func testDeps(ses *fakeSES) Deps {
	return Deps{
		Settings: settings.Settings{
			Stage:               "test",
			SESConfigurationSet: "cfgset",
			SESReplyTo:          "owner@example.com",
			SESSenderAddresses:  []string{"mcp-dev@example.com"},
			RecipientAllowList:  []string{"owner@example.com"},
			MaxRecipients:       2,
			EmailMaxRawBytes:    1024,
		},
		SES: ses,
	}
}

func authedCtx(scopes ...string) context.Context {
	set := map[string]struct{}{}
	for _, s := range scopes {
		set[s] = struct{}{}
	}
	return httpapi.WithPrincipal(context.Background(), auth.Principal{Subject: "u", Scopes: set, Method: "oauth"})
}

func simpleInput(dryRun bool) schemas.SendEmailInput {
	return schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content: &schemas.EmailContent{Simple: &schemas.Message{
			Subject: &schemas.Content{Data: "hi"},
			Body:    &schemas.Body{Text: &schemas.Content{Data: "body"}},
		}},
		DryRun: dryRun,
	}
}

func TestSendEmailDryRunInjectsServerFields(t *testing.T) {
	ses := &fakeSES{}
	res, out, err := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, simpleInput(true))
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if ses.sendIn != nil {
		t.Fatal("DryRun must not call SES")
	}
	if out.WouldCall == nil || aws.ToString(out.WouldCall.ConfigurationSetName) != "cfgset" {
		t.Fatalf("configuration set not injected: %+v", out.WouldCall)
	}
	if got := out.WouldCall.ReplyToAddresses; len(got) != 1 || got[0] != "owner@example.com" {
		t.Fatalf("default reply-to not injected: %v", got)
	}
	if !out.ServerMetadata.DryRun || len(out.ServerMetadata.Guardrails) == 0 {
		t.Fatalf("metadata: %+v", out.ServerMetadata)
	}
}

func TestSendEmailRealSend(t *testing.T) {
	ses := &fakeSES{}
	in := simpleInput(false)
	in.ReplyToAddresses = []string{"custom@example.com"}
	in.EmailTags = []schemas.MessageTag{{Name: "k", Value: "v"}}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
	if out.MessageID != "msg-123" {
		t.Fatalf("message id: %+v", out)
	}
	if got := ses.sendIn.ReplyToAddresses; len(got) != 1 || got[0] != "custom@example.com" {
		t.Fatalf("caller reply-to must win: %v", got)
	}
	if len(ses.sendIn.EmailTags) != 1 || aws.ToString(ses.sendIn.EmailTags[0].Name) != "k" {
		t.Fatalf("tags: %+v", ses.sendIn.EmailTags)
	}
}

func TestSendEmailGuardrailBlocks(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*schemas.SendEmailInput)
		reason string
	}{
		{"bad sender", func(in *schemas.SendEmailInput) { in.FromEmailAddress = "evil@x.com" }, "sender_allow_list"},
		{"bad recipient", func(in *schemas.SendEmailInput) { in.Destination.ToAddresses = []string{"stranger@x.com"} }, "recipient_allow_list"},
		{"too many recipients", func(in *schemas.SendEmailInput) {
			in.Destination.ToAddresses = []string{"owner@example.com", "owner@example.com", "owner@example.com"}
		}, "max_recipients"},
		{"no recipients", func(in *schemas.SendEmailInput) { in.Destination = nil }, "max_recipients"},
		{"bad attachment base64", func(in *schemas.SendEmailInput) {
			in.Content.Simple.Attachments = []schemas.Attachment{{FileName: "a.png", RawContent: "!!!not-base64"}}
		}, "attachment_base64"},
		{"oversize attachments", func(in *schemas.SendEmailInput) {
			in.Content.Simple.Attachments = []schemas.Attachment{{
				FileName:   "big.bin",
				RawContent: base64.StdEncoding.EncodeToString(make([]byte, 2048)), // EmailMaxRawBytes is 1024 in tests
			}}
		}, "attachment_size"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			in := simpleInput(false)
			tc.mutate(&in)
			res, _, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
			if res == nil || !res.IsError {
				t.Fatalf("expected guardrail error, got %+v", res)
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(text, tc.reason) {
				t.Fatalf("reason %q not in %q", tc.reason, text)
			}
			if ses.sendIn != nil {
				t.Fatal("blocked call must not reach SES")
			}
		})
	}
}

func TestSendEmailShapeAndScope(t *testing.T) {
	d := testDeps(&fakeSES{})
	in := simpleInput(false)
	in.Content = &schemas.EmailContent{}
	if res, _, _ := d.sendEmail()(authedCtx("msg/email:send"), nil, in); res == nil || !res.IsError {
		t.Fatal("neither Simple nor Raw must error")
	}
	both := simpleInput(false)
	both.Content.Raw = &schemas.RawMessage{Data: "aGk="}
	if res, _, _ := d.sendEmail()(authedCtx("msg/email:send"), nil, both); res == nil || !res.IsError {
		t.Fatal("both Simple and Raw must error")
	}
	if res, _, _ := d.sendEmail()(authedCtx("msg/read"), nil, simpleInput(false)); res == nil || !res.IsError {
		t.Fatal("missing scope must error")
	}
	if res, _, _ := d.sendEmail()(context.Background(), nil, simpleInput(false)); res == nil || !res.IsError {
		t.Fatal("missing principal must error")
	}
}

func TestSendEmailRawPath(t *testing.T) {
	ses := &fakeSES{}
	raw := base64.StdEncoding.EncodeToString([]byte("From: mcp-dev@example.com\r\nTo: owner@example.com\r\nSubject: s\r\n\r\nb\r\n"))
	in := schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content:          &schemas.EmailContent{Raw: &schemas.RawMessage{Data: raw}},
	}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
	if out.MessageID != "msg-123" || ses.sendIn.Content.Raw == nil {
		t.Fatalf("raw send failed: %+v", out)
	}
	if !strings.Contains(string(ses.sendIn.Content.Raw.Data), "Subject: s") {
		t.Fatalf("guardrail-decoded MIME must reach the call: %q", ses.sendIn.Content.Raw.Data)
	}
}

// sha256Hex is the expectation side of the digest tests: the same hash the
// server should report, computed here straight from crypto/sha256.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func attached(dryRun bool, atts ...schemas.Attachment) schemas.SendEmailInput {
	in := simpleInput(dryRun)
	in.Content.Simple.Attachments = atts
	return in
}

func attachment(name string, body []byte) schemas.Attachment {
	return schemas.Attachment{
		FileName:    name,
		ContentType: "application/octet-stream",
		RawContent:  base64.StdEncoding.EncodeToString(body),
	}
}

func TestSendEmailContentDigests(t *testing.T) {
	png := []byte("\x89PNG not really a png")
	pdf := []byte("%PDF-1.7 tiny")
	mime := []byte("From: mcp-dev@example.com\r\nTo: owner@example.com\r\nSubject: s\r\n\r\nb\r\n")
	rawIn := schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content:          &schemas.EmailContent{Raw: &schemas.RawMessage{Data: base64.StdEncoding.EncodeToString(mime)}},
		DryRun:           true,
	}
	cases := []struct {
		name string
		in   schemas.SendEmailInput
		want []ContentDigest
	}{
		{"text only has no binary part", simpleInput(true), nil},
		{"one attachment", attached(true, attachment("logo.png", png)), []ContentDigest{
			{Part: "attachment[0]:logo.png", Bytes: len(png), SHA256: sha256Hex(png)},
		}},
		{"shared file name stays distinct", attached(true, attachment("logo.png", png), attachment("logo.png", pdf)), []ContentDigest{
			{Part: "attachment[0]:logo.png", Bytes: len(png), SHA256: sha256Hex(png)},
			{Part: "attachment[1]:logo.png", Bytes: len(pdf), SHA256: sha256Hex(pdf)},
		}},
		{"raw message", rawIn, []ContentDigest{{Part: "raw", Bytes: len(mime), SHA256: sha256Hex(mime)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, out, _ := testDeps(&fakeSES{}).sendEmail()(authedCtx("msg/email:send"), nil, tc.in)
			if out.WouldCall == nil {
				t.Fatalf("dry run must echo the call: %+v", out)
			}
			if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, tc.want) {
				t.Fatalf("digests: %+v want %+v", out.ServerMetadata.ContentDigests, tc.want)
			}
		})
	}
}

func TestSendEmailDigestsOnRealSendNotOnBlocked(t *testing.T) {
	body := []byte("report bytes")
	ses := &fakeSES{}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, attached(false, attachment("report.pdf", body)))
	if out.MessageID != "msg-123" || ses.sendIn == nil {
		t.Fatalf("send failed: %+v", out)
	}
	want := []ContentDigest{{Part: "attachment[0]:report.pdf", Bytes: len(body), SHA256: sha256Hex(body)}}
	if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, want) {
		t.Fatalf("real send digests: %+v want %+v", out.ServerMetadata.ContentDigests, want)
	}

	// Blocked calls stop before the digest step, so the error metadata carries
	// decisions only.
	blocked := attached(false, attachment("report.pdf", body))
	blocked.FromEmailAddress = "evil@x.com"
	res, _, _ := testDeps(&fakeSES{}).sendEmail()(authedCtx("msg/email:send"), nil, blocked)
	if res == nil || !res.IsError {
		t.Fatalf("expected a guardrail error: %+v", res)
	}
	if meta := res.StructuredContent.(SendEmailOutput).ServerMetadata; meta.ContentDigests != nil {
		t.Fatalf("blocked call must not report digests: %+v", meta)
	}
}

// A failed decode blocks before the digest step, so this defensive skip is
// only reachable by calling the helper directly.
func TestContentDigestsSkipUndecodedAttachments(t *testing.T) {
	atts := []schemas.Attachment{{FileName: "bad.png"}, {FileName: "ok.txt"}}
	want := []ContentDigest{{Part: "attachment[1]:ok.txt", Bytes: 2, SHA256: sha256Hex([]byte("ok"))}}
	if got := contentDigests(nil, atts, [][]byte{nil, []byte("ok")}, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("digests: %+v want %+v", got, want)
	}
	if got := contentDigests(nil, atts, nil, nil); got != nil {
		t.Fatalf("no decoded bytes must digest nothing: %+v", got)
	}
	// "raw" is the caller's own Content.Raw, so it wins the early return; the
	// two are never both set, and reserving the name is what keeps a server
	// assembled message from erasing the per-attachment digests.
	if got := contentDigests([]byte("mime"), atts, [][]byte{nil, []byte("ok")}, []byte("assembled")); len(got) != 1 || got[0].Part != "raw" {
		t.Fatalf("a caller-supplied raw message digests as one part: %+v", got)
	}
}

type apiError struct{ code, msg string }

func (e *apiError) Error() string                 { return e.code + ": " + e.msg }
func (e *apiError) ErrorCode() string             { return e.code }
func (e *apiError) ErrorMessage() string          { return e.msg }
func (e *apiError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestSendEmailAPIErrorMapped(t *testing.T) {
	ses := &fakeSES{sendErr: &apiError{code: "MessageRejected", msg: "Email address is not verified."}}
	res, _, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, simpleInput(false))
	if res == nil || !res.IsError {
		t.Fatal("API error must be a tool error")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "MessageRejected") || !strings.Contains(text, "not verified") {
		t.Fatalf("error text: %q", text)
	}
}

func TestSendEmailRateLimiterWired(t *testing.T) {
	d := testDeps(&fakeSES{})
	d.Limiter = &guardrails.Limiter{Store: blockedStore{}, PerHour: 1, PerDay: 1}
	res, _, _ := d.sendEmail()(authedCtx("msg/email:send"), nil, simpleInput(false))
	if res == nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "rate_limit") {
		t.Fatalf("limiter must block: %+v", res)
	}
}

type blockedStore struct{}

func (blockedStore) IncrementWindow(context.Context, string, time.Time, time.Duration) (int, error) {
	return 0, errors.New("counters down")
}

func TestListIdentitiesAndGetAccount(t *testing.T) {
	d := testDeps(&fakeSES{prodMode: true})
	_, lo, _ := d.listIdentities()(authedCtx("msg/read"), nil, schemas.ListEmailIdentitiesInput{PageSize: 500})
	if lo.EmailIdentities == nil {
		t.Fatalf("identities: %+v", lo)
	}
	if res, _, _ := d.listIdentities()(authedCtx("msg/email:send"), nil, schemas.ListEmailIdentitiesInput{}); res == nil || !res.IsError {
		t.Fatal("list requires msg/read")
	}
	_, ao, _ := d.getAccount()(authedCtx("msg/read"), nil, schemas.GetAccountInput{})
	if !ao.ProductionAccessEnabled || ao.Max24HourSend != 200 || ao.SentLast24Hours != 3 {
		t.Fatalf("account: %+v", ao)
	}
	failing := testDeps(&fakeSES{listErr: &apiError{code: "Throttling", msg: "slow down"}, getErr: &apiError{code: "X", msg: "y"}})
	if res, _, _ := failing.listIdentities()(authedCtx("msg/read"), nil, schemas.ListEmailIdentitiesInput{}); res == nil || !res.IsError {
		t.Fatal("list API error must map")
	}
	if res, _, _ := failing.getAccount()(authedCtx("msg/read"), nil, schemas.GetAccountInput{}); res == nil || !res.IsError {
		t.Fatal("account API error must map")
	}
}

// The Simple path, which is every send with no inline attachment: SES still
// assembles the message from the fields below. The attachment here declares an
// ATTACHMENT disposition *and* a ContentId, the one shape that carries a
// Content-ID without joining the related group; the inline case is a
// handler-level test, because assembling is the handler's decision.
func TestBuildSendEmailCharsetsAndAttachments(t *testing.T) {
	in := simpleInput(false)
	in.Content.Simple.Subject.Charset = "UTF-8"
	in.Content.Simple.Body.HTML = &schemas.Content{Data: `<p>report</p>`, Charset: "UTF-8"}
	in.Content.Simple.Attachments = []schemas.Attachment{{
		FileName:                "logo.png",
		ContentType:             "image/png",
		RawContent:              base64.StdEncoding.EncodeToString([]byte("png")),
		ContentDescription:      "the logo",
		ContentDisposition:      "ATTACHMENT",
		ContentId:               "logo",
		ContentTransferEncoding: "BASE64",
	}}
	call := buildSendEmail(sendEmailParams{in: in, attDecoded: [][]byte{[]byte("png")}})
	if aws.ToString(call.Content.Simple.Subject.Charset) != "UTF-8" || call.Content.Simple.Body.Html == nil {
		t.Fatalf("charset/html: %+v", call.Content.Simple)
	}
	if len(call.Content.Simple.Attachments) != 1 {
		t.Fatalf("attachments: %+v", call.Content.Simple.Attachments)
	}
	att := call.Content.Simple.Attachments[0]
	if aws.ToString(att.ContentType) != "image/png" || string(att.RawContent) != "png" {
		t.Fatalf("attachment bytes/type: %+v", att)
	}
	if att.ContentDisposition != sestypes.AttachmentContentDispositionAttachment || aws.ToString(att.ContentId) != "logo" {
		t.Fatalf("disposition/cid: %+v", att)
	}
	if att.ContentTransferEncoding != sestypes.AttachmentContentTransferEncodingBase64 || aws.ToString(att.ContentDescription) != "the logo" {
		t.Fatalf("transfer encoding/description: %+v", att)
	}
	if call.ConfigurationSetName != nil || call.ReplyToAddresses != nil {
		t.Fatalf("empty injections must stay unset: %+v", call)
	}
}

// refFake is a files store holding one live shared object of the given bytes.
func refFake(body []byte) *fakeFiles {
	return &fakeFiles{
		headMeta: map[string]string{expiresAtMetaKey: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		headSize: int64(len(body)),
		getBody:  body,
	}
}

// refDeps is testDeps plus the files store ses_send_email reads references
// from; a nil store is the stage where the files tools never registered.
func refDeps(ses *fakeSES, files *fakeFiles) Deps {
	d := testDeps(ses)
	d.Settings.FilesBucket = "files-bucket"
	if files != nil {
		d.Files = files
	}
	return d
}

func reference(dryRun bool, key string) schemas.SendEmailInput {
	return attached(dryRun, schemas.Attachment{FileName: "logo.png", ContentType: "image/png", RawContentKey: key})
}

func TestSendEmailAttachByReference(t *testing.T) {
	png := []byte("\x89PNG already in the files bucket")
	ses, files := &fakeSES{}, refFake(png)
	_, out, _ := refDeps(ses, files).sendEmail()(authedCtx("msg/email:send", "msg/read"), nil,
		reference(false, "shared/abc/logo.png"))
	if out.MessageID != "msg-123" || ses.sendIn == nil {
		t.Fatalf("send failed: %+v", out)
	}
	if got := aws.ToString(files.getIn.Key); got != "files/shared/abc/logo.png" {
		t.Fatalf("bucket key must carry the files/ prefix: %q", got)
	}
	if got := ses.sendIn.Content.Simple.Attachments[0].RawContent; string(got) != string(png) {
		t.Fatalf("fetched bytes must reach SES: %q", got)
	}
	want := []ContentDigest{{Part: "attachment[0]:logo.png", Bytes: len(png), SHA256: sha256Hex(png)}}
	if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, want) {
		t.Fatalf("digests: %+v want %+v", out.ServerMetadata.ContentDigests, want)
	}
	var sized bool
	for _, d := range out.ServerMetadata.Guardrails {
		sized = sized || (d.Name == "attachment_size" && d.Allowed && strings.Contains(d.Reason, fmt.Sprint(len(png))))
	}
	if !sized {
		t.Fatalf("fetched bytes must be counted by attachment_size: %+v", out.ServerMetadata.Guardrails)
	}
}

// A refusal that is free to decide must be decided before a referenced
// attachment costs an S3 read, or a throttled caller could keep reading the
// files bucket through ses_send_email.
func TestSendEmailBlockedBeforeFetchingReferences(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutit func(*Deps, *schemas.SendEmailInput)
		want  string
	}{
		{"rate limit", func(d *Deps, _ *schemas.SendEmailInput) {
			d.Limiter = &guardrails.Limiter{Store: blockedStore{}, PerHour: 1, PerDay: 1}
		}, "rate_limit"},
		{"recipient", func(_ *Deps, in *schemas.SendEmailInput) {
			in.Destination.ToAddresses = []string{"stranger@example.com"}
		}, "recipient_allow_list"},
		{"sender", func(_ *Deps, in *schemas.SendEmailInput) {
			in.FromEmailAddress = "spoofed@example.com"
		}, "sender_allow_list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses, files := &fakeSES{}, refFake([]byte("never read"))
			d := refDeps(ses, files)
			in := reference(false, "shared/abc/logo.png")
			tc.mutit(&d, &in)
			res, _, _ := d.sendEmail()(authedCtx("msg/email:send", "msg/read"), nil, in)
			if res == nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, tc.want) {
				t.Fatalf("want %s block, got %+v", tc.want, res)
			}
			if files.headCalls != 0 || files.getCalls != 0 {
				t.Fatalf("blocked send must not touch S3: heads=%d gets=%d", files.headCalls, files.getCalls)
			}
			if ses.sendIn != nil {
				t.Fatal("blocked send must not reach SES")
			}
		})
	}
}

func TestSendEmailAttachByReferenceDryRun(t *testing.T) {
	pdf := []byte("%PDF-1.7 from the files bucket")
	ses, files := &fakeSES{}, refFake(pdf)
	_, out, _ := refDeps(ses, files).sendEmail()(authedCtx("msg/email:send", "msg/read"), nil,
		reference(true, "shared/abc/report.pdf"))
	if ses.sendIn != nil {
		t.Fatal("DryRun must not call SES")
	}
	if files.getCalls != 1 {
		t.Fatalf("DryRun must still fetch the object: %d GetObject calls", files.getCalls)
	}
	if out.WouldCall == nil || string(out.WouldCall.Content.Simple.Attachments[0].RawContent) != string(pdf) {
		t.Fatalf("would-call must carry the resolved bytes: %+v", out.WouldCall)
	}
	want := []ContentDigest{{Part: "attachment[0]:logo.png", Bytes: len(pdf), SHA256: sha256Hex(pdf)}}
	if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, want) {
		t.Fatalf("digests: %+v want %+v", out.ServerMetadata.ContentDigests, want)
	}
}

// Referenced and inline attachments spend one budget: 600 + 600 fetched bytes
// exceed the 1024-byte test ceiling.
func TestSendEmailReferencedBytesShareTheAttachmentBudget(t *testing.T) {
	ses := &fakeSES{}
	in := attached(false, attachment("inline.bin", make([]byte, 600)),
		schemas.Attachment{FileName: "ref.bin", RawContentKey: "shared/abc/ref.bin"})
	res, _, _ := refDeps(ses, refFake(make([]byte, 600))).sendEmail()(authedCtx("msg/email:send", "msg/read"), nil, in)
	if res == nil || !res.IsError {
		t.Fatalf("combined size must block: %+v", res)
	}
	if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, "attachment_size") {
		t.Fatalf("error text: %q", text)
	}
	if ses.sendIn != nil {
		t.Fatal("blocked call must not reach SES")
	}
}

func TestSendEmailAttachByReferenceRefusals(t *testing.T) {
	both := attached(false, schemas.Attachment{FileName: "a.bin", RawContent: "aGk=", RawContentKey: "shared/abc/a.bin"})
	neither := attached(false, schemas.Attachment{FileName: "a.bin"})
	expired := refFake([]byte("bytes"))
	expired.headMeta = map[string]string{expiresAtMetaKey: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	oversize := refFake([]byte("bytes"))
	oversize.headSize = 2048 // EmailMaxRawBytes is 1024 in these tests
	gone := refFake(nil)
	gone.headErr = &s3types.NotFound{}
	headFailed := refFake(nil)
	headFailed.headErr = &apiError{code: "AccessDenied", msg: "no bucket read"}
	raced := refFake(nil)
	raced.getErr = &s3types.NoSuchKey{}
	getFailed := refFake(nil)
	getFailed.getErr = &apiError{code: "SlowDown", msg: "please retry"}
	torn := refFake(nil)
	torn.getReadErr = errors.New("connection reset")

	cases := []struct {
		name    string
		files   *fakeFiles
		scopes  []string
		in      schemas.SendEmailInput
		want    string
		wantGet int
	}{
		{"both content and key", refFake(nil), []string{"msg/email:send", "msg/read"}, both,
			"attachment 0 must set exactly one of RawContent or RawContentKey", 0},
		{"neither content nor key", refFake(nil), []string{"msg/email:send", "msg/read"}, neither,
			"attachment 0 must set exactly one of RawContent or RawContentKey", 0},
		{"key outside shared/", refFake(nil), []string{"msg/email:send", "msg/read"},
			reference(false, "files/shared/abc/a.bin"), "attachment 0: Key must be under shared/", 0},
		{"no files store on this stage", nil, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "not configured", 0},
		{"email scope alone cannot read the bucket", refFake([]byte("bytes")), []string{"msg/email:send"},
			reference(false, "shared/abc/a.bin"), "msg/read", 0},
		{"object not found", gone, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "no object shared/abc/a.bin", 0},
		{"head fails for another reason", headFailed, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "AccessDenied", 0},
		{"object past its expiry", expired, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "awaiting cleanup", 0},
		{"object over the budget", oversize, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "2048 bytes, over the 1024-byte email budget", 0},
		{"deleted between head and get", raced, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "disappeared between the size check and the read", 1},
		{"get fails for another reason", getFailed, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "SlowDown", 1},
		{"body dies mid-read", torn, []string{"msg/email:send", "msg/read"},
			reference(false, "shared/abc/a.bin"), "connection reset", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			res, _, _ := refDeps(ses, tc.files).sendEmail()(authedCtx(tc.scopes...), nil, tc.in)
			if res == nil || !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, tc.want) {
				t.Fatalf("error text %q lacks %q", text, tc.want)
			}
			if ses.sendIn != nil {
				t.Fatal("refused call must not reach SES")
			}
			if tc.files != nil && tc.files.getCalls != tc.wantGet {
				t.Fatalf("GetObject calls: %d want %d", tc.files.getCalls, tc.wantGet)
			}
		})
	}
}

// pngBytes stands in for an inline image: not a real PNG, but binary enough
// that base64 is the only encoding that can carry it.
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 'p', 'i', 'x'}

// inlineInput is a Text + Html message with one image the HTML references, the
// shape that makes the server assemble the MIME itself.
func inlineInput(dryRun bool) schemas.SendEmailInput {
	in := simpleInput(dryRun)
	in.Content.Simple.Body.HTML = &schemas.Content{Data: `<p>chart: <img src="cid:logo"></p>`}
	in.Content.Simple.Attachments = []schemas.Attachment{{
		FileName:           "logo.png",
		ContentType:        "image/png",
		RawContent:         base64.StdEncoding.EncodeToString(pngBytes),
		ContentDisposition: "INLINE",
		ContentId:          "logo",
	}}
	return in
}

// assembledFrom returns the message the server built, having first insisted the
// call carries it as Content.Raw and nothing else. Reading assembled bytes back
// is the point of these tests: everything in this package used to assert on the
// request we would send, which is exactly how the inline-CID defect shipped
// (docs/plans/email-inline-mime.md §8).
func assembledFrom(t *testing.T, call *sesv2.SendEmailInput) []byte {
	t.Helper()
	if call == nil || call.Content == nil || call.Content.Raw == nil {
		t.Fatalf("an inline send must be assembled into Content.Raw: %+v", call)
	}
	if call.Content.Simple != nil {
		t.Fatalf("an assembled send must not also carry Content.Simple: %+v", call.Content.Simple)
	}
	return call.Content.Raw.Data
}

// parts walks the assembled message into mimebuild's flat part list, where a
// dotted path says who is whose sibling.
func parts(t *testing.T, msg []byte) []mimebuild.Part {
	t.Helper()
	walked, err := mimebuild.Walk(msg, 10, 100)
	if err != nil {
		t.Fatalf("cannot walk the assembled message: %v\n%s", err, msg)
	}
	return walked
}

// TestSendEmailInlineAttachmentIsAssembled is the defect's regression test: an
// INLINE attachment with a ContentId now leaves as a multipart/related whose
// children are the HTML part and the image, which is the tree a cid: reference
// needs to resolve.
func TestSendEmailInlineAttachmentIsAssembled(t *testing.T) {
	ses := &fakeSES{}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, inlineInput(true))
	msg := assembledFrom(t, out.WouldCall)

	// Text and Html both set, so the alternative is the root and the related
	// group stands where the HTML part alone would have.
	related := ""
	byPath := map[string]mimebuild.Part{}
	for _, p := range parts(t, msg) {
		byPath[p.Path] = p
		if p.ContentType == "multipart/related" {
			related = p.Path
		}
	}
	if related == "" {
		t.Fatalf("no multipart/related in the assembled message:\n%s", msg)
	}
	html, image := byPath[related+".1"], byPath[related+".2"]
	if html.ContentType != "text/html" {
		t.Fatalf("the related group's root part must be the HTML: %+v", html)
	}
	if image.ContentType != "image/png" || image.ContentID != "logo" || image.Disposition != "inline" {
		t.Fatalf("the image must be the HTML's sibling, inline, with the cid: %+v", image)
	}
	// The header a client matches a cid: against, spelled the way every other
	// mailer spells it.
	if !strings.Contains(string(msg), "Content-ID: <logo>") {
		t.Fatalf("Content-ID header missing:\n%s", msg)
	}
	// The HTML body travels quoted-printable, which is what keeps a long line
	// under RFC 5321's limit, so its "=" arrives as "=3D".
	if !strings.Contains(string(msg), `<img src=3D"cid:logo">`) {
		t.Fatalf("the HTML body must survive into the message:\n%s", msg)
	}

	decisions := map[string]guardrails.Decision{}
	for _, d := range out.ServerMetadata.Guardrails {
		decisions[d.Name] = d
	}
	for _, name := range []string{"attachment_size", "attachment_fields", "inline_content_id", "inline_needs_html", "inline_cid_refs", "assembled_size"} {
		if d, ok := decisions[name]; !ok || !d.Allowed {
			t.Fatalf("%s decision missing or blocking: %+v", name, out.ServerMetadata.Guardrails)
		}
	}
	if want := fmt.Sprintf("%d bytes assembled", len(msg)); decisions["assembled_size"].Reason != want {
		t.Fatalf("assembled_size reason %q want %q", decisions["assembled_size"].Reason, want)
	}
	// PR A's interim warning was removed with the defect it described.
	if _, ok := decisions["inline_not_rendered"]; ok {
		t.Fatalf("inline_not_rendered must be gone now that the image renders: %+v", out.ServerMetadata.Guardrails)
	}
}

// The sentinel for the blast radius: only inline sends change shape. Anything
// else still goes to SES as Content.Simple, which SES assembles as it always
// has.
func TestSendEmailOnlyInlineSendsBecomeRaw(t *testing.T) {
	ordinary := attached(false, attachment("report.pdf", []byte("%PDF-1.7 tiny")))
	ordinary.Content.Simple.Body.HTML = &schemas.Content{Data: "<p>report attached</p>"}
	// An explicit ATTACHMENT wins over the ContentId, so this part keeps a
	// Content-ID without pulling the message onto the assembled path.
	labelled := attached(false, schemas.Attachment{
		FileName: "logo.png", ContentType: "image/png", ContentId: "logo",
		ContentDisposition: "ATTACHMENT", RawContent: base64.StdEncoding.EncodeToString(pngBytes),
	})
	for _, tc := range []struct {
		name string
		in   schemas.SendEmailInput
		raw  bool
	}{
		{"inline attachment", inlineInput(false), true},
		{"lower-case disposition is the same rule", func() schemas.SendEmailInput {
			in := inlineInput(false)
			in.Content.Simple.Attachments[0].ContentDisposition = "inline"
			return in
		}(), true},
		{"a ContentId with no disposition", func() schemas.SendEmailInput {
			in := inlineInput(false)
			in.Content.Simple.Attachments[0].ContentDisposition = ""
			return in
		}(), true},
		{"no attachments at all", simpleInput(false), false},
		{"an ordinary attachment", ordinary, false},
		{"an attachment that merely carries a ContentId", labelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, tc.in)
			if out.MessageID != "msg-123" {
				t.Fatalf("send failed: %+v", out)
			}
			if tc.raw {
				assembledFrom(t, ses.sendIn)
				return
			}
			if ses.sendIn.Content.Simple == nil || ses.sendIn.Content.Raw != nil {
				t.Fatalf("this send must stay on SES's Simple path: %+v", ses.sendIn.Content)
			}
		})
	}
}

// Bcc is the one recipient list that must not become a header: SES honours
// Destination.BccAddresses for a raw message without disclosing it, and a Bcc
// header would be delivered to everybody.
func TestSendEmailAssembledKeepsBccOutOfTheMessage(t *testing.T) {
	ses := &fakeSES{}
	in := inlineInput(false)
	in.Destination.BccAddresses = []string{"owner@example.com"}
	if _, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in); out.MessageID != "msg-123" {
		t.Fatalf("send failed: %+v", out)
	}
	if msg := string(assembledFrom(t, ses.sendIn)); strings.Contains(msg, "Bcc:") {
		t.Fatalf("a Bcc header would leak the hidden recipient:\n%s", msg)
	}
	if got := ses.sendIn.Destination.BccAddresses; len(got) != 1 || got[0] != "owner@example.com" {
		t.Fatalf("Bcc must still ride on the API parameter: %v", got)
	}
	if got := ses.sendIn.Destination.ToAddresses; len(got) != 1 {
		t.Fatalf("To must still ride on the API parameter too: %v", got)
	}
}

// Reply-To is the mirror image: it becomes a header, so the API parameter has
// to be dropped or SES adds a second one and clients disagree about which wins.
func TestSendEmailAssembledReplyToIsOneHeader(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replyTo []string
		want    string
	}{
		{"server default", nil, "owner@example.com"},
		{"caller's own wins", []string{"custom@example.com"}, "custom@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			in := inlineInput(false)
			in.ReplyToAddresses = tc.replyTo
			testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
			msg := string(assembledFrom(t, ses.sendIn))
			if n := strings.Count(msg, "Reply-To:"); n != 1 {
				t.Fatalf("%d Reply-To headers, want exactly 1:\n%s", n, msg)
			}
			// Angle brackets because every address is re-emitted from a parsed
			// net/mail.Address rather than from the caller's string.
			if !strings.Contains(msg, "Reply-To: <"+tc.want+">") {
				t.Fatalf("Reply-To must carry %s:\n%s", tc.want, msg)
			}
			if ses.sendIn.ReplyToAddresses != nil {
				t.Fatalf("the API parameter must stay unset: %v", ses.sendIn.ReplyToAddresses)
			}
		})
	}
}

// An assembled send digests both halves: each attachment as it arrived, and the
// whole message as it will leave.
func TestSendEmailAssembledDigests(t *testing.T) {
	ses := &fakeSES{}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, inlineInput(true))
	msg := assembledFrom(t, out.WouldCall)
	want := []ContentDigest{
		{Part: "attachment[0]:logo.png", Bytes: len(pngBytes), SHA256: sha256Hex(pngBytes)},
		{Part: "assembled", Bytes: len(msg), SHA256: sha256Hex(msg)},
	}
	if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, want) {
		t.Fatalf("digests: %+v want %+v", out.ServerMetadata.ContentDigests, want)
	}
}

func TestSendEmailInlineRefusals(t *testing.T) {
	danglingRef := inlineInput(false)
	danglingRef.Content.Simple.Body.HTML = &schemas.Content{Data: `<img src="cid:missing">`}
	noHTML := inlineInput(false)
	noHTML.Content.Simple.Body.HTML = nil
	noID := inlineInput(false)
	noID.Content.Simple.Attachments[0].ContentId = ""
	// Not a guardrail: nothing in the string checks knows whether the bytes are
	// 7-bit clean, and a corrupt delivery is worse than a refusal the caller
	// can read.
	sevenBit := inlineInput(false)
	sevenBit.Content.Simple.Attachments[0].ContentTransferEncoding = "SEVEN_BIT"

	for _, tc := range []struct {
		name string
		in   schemas.SendEmailInput
		want string
	}{
		{"the HTML references a cid nothing declares", danglingRef, "inline_cid_refs"},
		{"inline with no HTML to reference it", noHTML, "inline_needs_html"},
		{"inline with no ContentId", noID, "inline_content_id"},
		{"SEVEN_BIT on bytes that are not 7-bit clean", sevenBit, "cannot assemble the inline message"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			res, _, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, tc.in)
			if res == nil || !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, tc.want) {
				t.Fatalf("error text %q lacks %q", text, tc.want)
			}
			if ses.sendIn != nil {
				t.Fatal("refused call must not reach SES")
			}
		})
	}
}

// TestSendEmailOutputSchemaStaysInferable is the tripwire for the hazard in
// docs/plans/email-inline-mime.md §6: jsonschema.For returns an error on any
// named recursive type, sendEmailOutputSchema turns that error into a panic,
// and the panic happens inside NewServer — so a mime_structure that grew a
// Parts []Part field would take down every tool registration, cmd/gendocs, and
// the Lambda cold start rather than failing anything a handler test calls. Here
// it is one failing test.
func TestSendEmailOutputSchemaStaysInferable(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the output schema must stay inferable; mime_structure has to be flat: %v", r)
		}
	}()
	meta := sendEmailOutputSchema().Properties["ServerMetadata"]
	if meta == nil || meta.Properties["mime_structure"] == nil {
		t.Fatalf("mime_structure must be in the declared output schema: %+v", meta)
	}
	part := meta.Properties["mime_structure"].Items
	if part == nil || part.Properties["path"] == nil || part.Properties["depth"] == nil {
		t.Fatalf("each part carries its own path and depth, which is how a reader rebuilds the tree: %+v", part)
	}
}

// The assembled tree is reported without re-reading the message: Assemble
// already returned it, so mime_structure and the bytes cannot disagree.
func TestSendEmailInlineReportsMimeStructure(t *testing.T) {
	ses := &fakeSES{}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, inlineInput(true))
	msg := assembledFrom(t, out.WouldCall)
	if got := out.ServerMetadata.MimeStructure; !reflect.DeepEqual(got, parts(t, msg)) {
		t.Fatalf("mime_structure must equal the walk of the assembled bytes: %+v", got)
	}
	byPath := map[string]mimebuild.Part{}
	for _, p := range out.ServerMetadata.MimeStructure {
		byPath[p.Path] = p
	}
	// Text + Html + one inline image: alternative outside, related inside, the
	// image a sibling of the HTML. That is the whole diagnostic — a caller can
	// see the container the cid: reference needs before anyone opens a mailbox.
	for path, want := range map[string]string{
		"1": "multipart/alternative", "1.1": "text/plain",
		"1.2": "multipart/related", "1.2.1": "text/html", "1.2.2": "image/png",
	} {
		if byPath[path].ContentType != want {
			t.Fatalf("part %s is %q, want %q: %+v", path, byPath[path].ContentType, want, out.ServerMetadata.MimeStructure)
		}
	}
	image := byPath["1.2.2"]
	if image.ContentID != "logo" || image.Disposition != "inline" || image.FileName != "logo.png" || image.Depth != 2 {
		t.Fatalf("the inline part must carry its cid, disposition, filename, and depth: %+v", image)
	}
	if image.Bytes == 0 {
		t.Fatal("a leaf reports its encoded byte count")
	}
	// A real send reports the same structure as its dry run.
	_, sent, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, inlineInput(false))
	if !reflect.DeepEqual(sent.ServerMetadata.MimeStructure, out.ServerMetadata.MimeStructure) {
		t.Fatalf("real send structure: %+v", sent.ServerMetadata.MimeStructure)
	}
}

// A caller-supplied Content.Raw is walked, so a message the caller built by
// hand is just as observable as one the server assembled.
func TestSendEmailRawReportsMimeStructure(t *testing.T) {
	msg := "From: mcp-dev@example.com\r\nTo: owner@example.com\r\nSubject: s\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/related; boundary=\"b\"; type=\"text/html\"\r\n\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<img src=\"cid:logo\">\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-ID: <logo>\r\n\r\nnot-a-png\r\n--b--\r\n"
	in := schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content:          &schemas.EmailContent{Raw: &schemas.RawMessage{Data: base64.StdEncoding.EncodeToString([]byte(msg))}},
		DryRun:           true,
	}
	_, out, _ := testDeps(&fakeSES{}).sendEmail()(authedCtx("msg/email:send"), nil, in)
	got := out.ServerMetadata.MimeStructure
	if len(got) != 3 || got[0].ContentType != "multipart/related" || got[2].ContentID != "logo" {
		t.Fatalf("a caller-supplied raw message must be walked: %+v", got)
	}
}

// A structure that cannot be produced is not a reason to refuse a send: the raw
// ladder has already checked everything that decides deliverability, and the
// walk is a diagnostic. mail.ReadMessage accepts these headers, so raw_mime
// passes and only the walk of the body fails.
func TestSendEmailRawWithUnwalkableBodyStillSends(t *testing.T) {
	msg := "From: mcp-dev@example.com\r\nTo: owner@example.com\r\n" +
		"Content-Type: multipart/mixed\r\n\r\nno boundary parameter, so the walk cannot descend\r\n"
	in := schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content:          &schemas.EmailContent{Raw: &schemas.RawMessage{Data: base64.StdEncoding.EncodeToString([]byte(msg))}},
	}
	ses := &fakeSES{}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
	if out.MessageID != "msg-123" || ses.sendIn == nil {
		t.Fatalf("the send must go through: %+v", out)
	}
	if out.ServerMetadata.MimeStructure != nil {
		t.Fatalf("an unwalkable message reports no structure: %+v", out.ServerMetadata.MimeStructure)
	}
	// The digest still proves what arrived, which is the diagnostic that does
	// not depend on the message parsing.
	if len(out.ServerMetadata.ContentDigests) != 1 {
		t.Fatalf("digests: %+v", out.ServerMetadata.ContentDigests)
	}
}

// A Simple send has no message of ours to describe: SES assembles it, so
// mime_structure is absent rather than guessed at.
func TestSendEmailSimpleReportsNoMimeStructure(t *testing.T) {
	_, out, _ := testDeps(&fakeSES{}).sendEmail()(authedCtx("msg/email:send"), nil,
		attached(true, attachment("report.pdf", []byte("%PDF-1.7 tiny"))))
	if out.ServerMetadata.MimeStructure != nil {
		t.Fatalf("the Simple path has no assembled message: %+v", out.ServerMetadata.MimeStructure)
	}
}

// rawKeyInput sends the whole MIME message by files-bucket key instead of
// inlining ~1.78× its size as base64 in a single string parameter.
func rawKeyInput(dryRun bool, key string) schemas.SendEmailInput {
	return schemas.SendEmailInput{
		FromEmailAddress: "mcp-dev@example.com",
		Destination:      &schemas.Destination{ToAddresses: []string{"owner@example.com"}},
		Content:          &schemas.EmailContent{Raw: &schemas.RawMessage{DataKey: key}},
		DryRun:           dryRun,
	}
}

// rawMessage is a minimal but real message: headers that parse, a From in the
// allow-list, one part.
var rawMessage = []byte("From: mcp-dev@example.com\r\nTo: owner@example.com\r\nSubject: s\r\n\r\nb\r\n")

func TestSendEmailRawByKey(t *testing.T) {
	ses, files := &fakeSES{}, refFake(rawMessage)
	_, out, _ := refDeps(ses, files).sendEmail()(authedCtx("msg/email:send", "msg/read"), nil,
		rawKeyInput(false, "shared/abc/message.eml"))
	if out.MessageID != "msg-123" || ses.sendIn == nil {
		t.Fatalf("send failed: %+v", out)
	}
	if got := aws.ToString(files.getIn.Key); got != "files/shared/abc/message.eml" {
		t.Fatalf("bucket key must carry the files/ prefix: %q", got)
	}
	if got := ses.sendIn.Content.Raw; got == nil || string(got.Data) != string(rawMessage) {
		t.Fatalf("the fetched message must reach SES as Content.Raw: %+v", got)
	}
	if ses.sendIn.Content.Simple != nil {
		t.Fatalf("a raw send must not also carry Content.Simple: %+v", ses.sendIn.Content.Simple)
	}
	// Same ladder as an inlined Raw.Data from raw_size on; raw_base64 has
	// nothing to decode, so it is absent rather than reported as passing.
	names := map[string]bool{}
	for _, d := range out.ServerMetadata.Guardrails {
		if !d.Allowed {
			t.Fatalf("nothing must block: %+v", d)
		}
		names[d.Name] = true
	}
	for _, want := range []string{"raw_size", "raw_mime", "sender_allow_list"} {
		if !names[want] {
			t.Fatalf("%s missing from the ladder: %+v", want, out.ServerMetadata.Guardrails)
		}
	}
	if names["raw_base64"] {
		t.Fatalf("raw_base64 has nothing to decide for a message read from the bucket: %+v", out.ServerMetadata.Guardrails)
	}
	// The message is the caller's however it arrived, so it digests as "raw"
	// and is walked like any other caller-supplied Content.Raw.
	want := []ContentDigest{{Part: "raw", Bytes: len(rawMessage), SHA256: sha256Hex(rawMessage)}}
	if !reflect.DeepEqual(out.ServerMetadata.ContentDigests, want) {
		t.Fatalf("digests: %+v want %+v", out.ServerMetadata.ContentDigests, want)
	}
	if got := out.ServerMetadata.MimeStructure; len(got) != 1 || got[0].ContentType != "text/plain" {
		t.Fatalf("a DataKey message is walked too: %+v", got)
	}
}

func TestSendEmailRawByKeyDryRun(t *testing.T) {
	ses, files := &fakeSES{}, refFake(rawMessage)
	_, out, _ := refDeps(ses, files).sendEmail()(authedCtx("msg/email:send", "msg/read"), nil,
		rawKeyInput(true, "shared/abc/message.eml"))
	if ses.sendIn != nil {
		t.Fatal("DryRun must not call SES")
	}
	if files.getCalls != 1 {
		t.Fatalf("DryRun must still fetch the object: %d GetObject calls", files.getCalls)
	}
	if out.WouldCall == nil || string(out.WouldCall.Content.Raw.Data) != string(rawMessage) {
		t.Fatalf("would-call must carry the fetched message: %+v", out.WouldCall)
	}
}

// The read is a cost, so the free refusals happen first — exactly as they do
// for a referenced attachment. sender_allow_list is the one that cannot: the
// From header it checks is inside the object.
func TestSendEmailRawByKeyBlockedBeforeFetching(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutit func(*Deps, *schemas.SendEmailInput)
		want  string
	}{
		{"rate limit", func(d *Deps, _ *schemas.SendEmailInput) {
			d.Limiter = &guardrails.Limiter{Store: blockedStore{}, PerHour: 1, PerDay: 1}
		}, "rate_limit"},
		{"recipient", func(_ *Deps, in *schemas.SendEmailInput) {
			in.Destination.ToAddresses = []string{"stranger@example.com"}
		}, "recipient_allow_list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses, files := &fakeSES{}, refFake([]byte("never read"))
			d := refDeps(ses, files)
			in := rawKeyInput(false, "shared/abc/message.eml")
			tc.mutit(&d, &in)
			res, _, _ := d.sendEmail()(authedCtx("msg/email:send", "msg/read"), nil, in)
			if res == nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, tc.want) {
				t.Fatalf("want %s block, got %+v", tc.want, res)
			}
			if files.headCalls != 0 || files.getCalls != 0 {
				t.Fatalf("blocked send must not touch S3: heads=%d gets=%d", files.headCalls, files.getCalls)
			}
		})
	}
}

func TestSendEmailRawByKeyRefusals(t *testing.T) {
	both := rawKeyInput(false, "shared/abc/message.eml")
	both.Content.Raw.Data = base64.StdEncoding.EncodeToString(rawMessage)
	neither := rawKeyInput(false, "")
	spoofed := refFake([]byte("From: evil@x.com\r\nTo: owner@example.com\r\n\r\nb\r\n"))
	garbage := refFake([]byte("\x00\x01 no headers here"))

	for _, tc := range []struct {
		name   string
		files  *fakeFiles
		scopes []string
		in     schemas.SendEmailInput
		want   string
	}{
		{"both Data and DataKey", refFake(nil), []string{"msg/email:send", "msg/read"}, both,
			"Content.Raw must set exactly one of Data or DataKey"},
		{"neither Data nor DataKey", refFake(nil), []string{"msg/email:send", "msg/read"}, neither,
			"Content.Raw must set exactly one of Data or DataKey"},
		{"key outside shared/", refFake(nil), []string{"msg/email:send", "msg/read"},
			rawKeyInput(false, "files/shared/abc/message.eml"), "Raw.DataKey: Key must be under shared/"},
		{"no files store on this stage", nil, []string{"msg/email:send", "msg/read"},
			rawKeyInput(false, "shared/abc/message.eml"), "Raw.DataKey needs the files store"},
		{"email scope alone cannot read the bucket", refFake(rawMessage), []string{"msg/email:send"},
			rawKeyInput(false, "shared/abc/message.eml"), "msg/read"},
		// The From lives in the fetched bytes, so this one is decided after the
		// read — the documented consequence of resolving the key at all.
		{"the From in the object is not in the allow-list", spoofed, []string{"msg/email:send", "msg/read"},
			rawKeyInput(false, "shared/abc/message.eml"), "sender_allow_list"},
		{"the object is not a MIME message", garbage, []string{"msg/email:send", "msg/read"},
			rawKeyInput(false, "shared/abc/message.eml"), "raw_mime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ses := &fakeSES{}
			res, _, _ := refDeps(ses, tc.files).sendEmail()(authedCtx(tc.scopes...), nil, tc.in)
			if res == nil || !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			if text := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, tc.want) {
				t.Fatalf("error text %q lacks %q", text, tc.want)
			}
			if ses.sendIn != nil {
				t.Fatal("refused call must not reach SES")
			}
		})
	}
}

// The 40 MB ceiling is SES's, and it is checked on the assembled bytes rather
// than on the attachment budget: 10 MB of attachments assembles to ~13.7 MB, so
// reusing EmailMaxRawBytes here would refuse sends that work today. Exercised
// through the decision itself — a handler-level case would have to build a
// 40 MB message to reach it.
func TestAssembledSize(t *testing.T) {
	if d := assembledSize(sesMaxMessageBytes); !d.Allowed || d.Name != "assembled_size" {
		t.Fatalf("exactly at the ceiling must pass: %+v", d)
	}
	d := assembledSize(sesMaxMessageBytes + 1)
	if d.Allowed || !strings.Contains(d.Reason, "over SES's maximum") {
		t.Fatalf("over the ceiling must block: %+v", d)
	}
}
