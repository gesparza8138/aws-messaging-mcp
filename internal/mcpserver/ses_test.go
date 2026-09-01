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
	if got := contentDigests(nil, atts, [][]byte{nil, []byte("ok")}); !reflect.DeepEqual(got, want) {
		t.Fatalf("digests: %+v want %+v", got, want)
	}
	if got := contentDigests(nil, atts, nil); got != nil {
		t.Fatalf("no decoded bytes must digest nothing: %+v", got)
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

func TestBuildSendEmailCharsetsAndAttachments(t *testing.T) {
	in := simpleInput(false)
	in.Content.Simple.Subject.Charset = "UTF-8"
	in.Content.Simple.Body.HTML = &schemas.Content{Data: `<img src="cid:logo">`, Charset: "UTF-8"}
	in.Content.Simple.Attachments = []schemas.Attachment{{
		FileName:                "logo.png",
		ContentType:             "image/png",
		RawContent:              base64.StdEncoding.EncodeToString([]byte("png")),
		ContentDescription:      "the logo",
		ContentDisposition:      "INLINE",
		ContentId:               "logo",
		ContentTransferEncoding: "BASE64",
	}}
	call := buildSendEmail(in, "", "", nil, [][]byte{[]byte("png")})
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
	if att.ContentDisposition != sestypes.AttachmentContentDispositionInline || aws.ToString(att.ContentId) != "logo" {
		t.Fatalf("inline disposition/cid: %+v", att)
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
			reference(false, "shared/abc/a.bin"), "2048 bytes, over the 1024-byte attachment budget", 0},
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

func TestSendEmailInlineAttachmentThroughGuardrails(t *testing.T) {
	ses := &fakeSES{}
	in := simpleInput(true)
	in.Content.Simple.Attachments = []schemas.Attachment{{
		FileName:           "logo.png",
		ContentType:        "image/png",
		RawContent:         base64.StdEncoding.EncodeToString([]byte("png")),
		ContentDisposition: "INLINE",
		ContentId:          "logo",
	}}
	_, out, _ := testDeps(ses).sendEmail()(authedCtx("msg/email:send"), nil, in)
	if out.WouldCall == nil || len(out.WouldCall.Content.Simple.Attachments) != 1 {
		t.Fatalf("would-call attachments: %+v", out.WouldCall)
	}
	if string(out.WouldCall.Content.Simple.Attachments[0].RawContent) != "png" {
		t.Fatalf("guardrail-decoded bytes must reach the call: %+v", out.WouldCall.Content.Simple.Attachments[0])
	}
	var sized, warned bool
	for _, d := range out.ServerMetadata.Guardrails {
		sized = sized || (d.Name == "attachment_size" && d.Allowed)
		// Allowed, but reported: SES will not render this inline.
		warned = warned || (d.Name == "inline_not_rendered" && d.Allowed)
	}
	if !sized {
		t.Fatalf("attachment_size decision missing: %+v", out.ServerMetadata.Guardrails)
	}
	if !warned {
		t.Fatalf("inline_not_rendered decision missing: %+v", out.ServerMetadata.Guardrails)
	}
}
