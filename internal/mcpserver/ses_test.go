package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	var sized bool
	for _, d := range out.ServerMetadata.Guardrails {
		sized = sized || (d.Name == "attachment_size" && d.Allowed)
	}
	if !sized {
		t.Fatalf("attachment_size decision missing: %+v", out.ServerMetadata.Guardrails)
	}
}
