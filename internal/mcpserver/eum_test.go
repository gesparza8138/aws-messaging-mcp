package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"
	eumtypes "github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
)

type fakeEUM struct {
	textIn  *pinpointsmsvoicev2.SendTextMessageInput
	mediaIn *pinpointsmsvoicev2.SendMediaMessageInput
	sendErr error
	descErr error
}

func (f *fakeEUM) SendTextMessage(_ context.Context, in *pinpointsmsvoicev2.SendTextMessageInput, _ ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.SendTextMessageOutput, error) {
	f.textIn = in
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &pinpointsmsvoicev2.SendTextMessageOutput{MessageId: aws.String("sms-123")}, nil
}

func (f *fakeEUM) SendMediaMessage(_ context.Context, in *pinpointsmsvoicev2.SendMediaMessageInput, _ ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.SendMediaMessageOutput, error) {
	f.mediaIn = in
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return &pinpointsmsvoicev2.SendMediaMessageOutput{MessageId: aws.String("mms-123")}, nil
}

func (f *fakeEUM) DescribePhoneNumbers(_ context.Context, _ *pinpointsmsvoicev2.DescribePhoneNumbersInput, _ ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.DescribePhoneNumbersOutput, error) {
	if f.descErr != nil {
		return nil, f.descErr
	}
	return &pinpointsmsvoicev2.DescribePhoneNumbersOutput{PhoneNumbers: []eumtypes.PhoneNumberInformation{{
		PhoneNumber:        aws.String("+18885550100"),
		PhoneNumberId:      aws.String("pn-1"),
		Status:             eumtypes.NumberStatusActive,
		NumberType:         eumtypes.NumberTypeTollFree,
		NumberCapabilities: []eumtypes.NumberCapability{eumtypes.NumberCapabilitySms, eumtypes.NumberCapabilityMms},
	}}}, nil
}

type fakeMedia struct {
	putIn  *s3.PutObjectInput
	putErr error
}

func (f *fakeMedia) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putIn = in
	return &s3.PutObjectOutput{}, f.putErr
}

type fakeLogs struct {
	messages []string
	err      error
}

func (f *fakeLogs) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	// Mimic the service: a quoted-term filter pattern matches messages
	// containing the term.
	term := strings.Trim(aws.ToString(in.FilterPattern), `"`)
	out := &cloudwatchlogs.FilterLogEventsOutput{}
	for _, m := range f.messages {
		if term == "" || strings.Contains(m, term) {
			out.Events = append(out.Events, cwltypes.FilteredLogEvent{Message: aws.String(m)})
		}
	}
	return out, nil
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

func eumDeps(eum *fakeEUM, media *fakeMedia, logs *fakeLogs) Deps {
	return Deps{
		Settings: settings.Settings{
			Stage:                  "test",
			EUMConfigurationSet:    "sms-cfgset",
			OriginationIdentity:    "+18885550100",
			ProtectConfigurationID: "protect-1",
			SMSMaxPrice:            "0.05",
			MediaBucket:            "media-bucket",
			MediaMaxBytes:          64,
			EUMEventsLogGroup:      "/test/eum-events",
			RecipientAllowList:     []string{"+12065550100"},
		},
		EUM:      eum,
		Media:    media,
		EventLog: logs,
	}
}

func textInput(dryRun bool) schemas.SendTextMessageInput {
	return schemas.SendTextMessageInput{
		DestinationPhoneNumber: "+12065550100",
		MessageBody:            "hello",
		DryRun:                 dryRun,
	}
}

func TestSendTextDryRunInjectsServerFields(t *testing.T) {
	eum := &fakeEUM{}
	res, out, err := eumDeps(eum, nil, nil).sendTextMessage()(authedCtx("msg/sms:send"), nil, textInput(true))
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if eum.textIn != nil {
		t.Fatal("DryRun must not call EUM")
	}
	call := out.WouldCall
	if call == nil || aws.ToString(call.ConfigurationSetName) != "sms-cfgset" ||
		aws.ToString(call.ProtectConfigurationId) != "protect-1" ||
		aws.ToString(call.OriginationIdentity) != "+18885550100" ||
		aws.ToString(call.MaxPrice) != "0.05" || call.MessageType != eumtypes.MessageTypeTransactional {
		t.Fatalf("server fields not injected: %+v", call)
	}
	if !out.ServerMetadata.DryRun || len(out.ServerMetadata.Guardrails) == 0 {
		t.Fatalf("metadata: %+v", out.ServerMetadata)
	}
}

func TestSendTextRealSendAndOptions(t *testing.T) {
	eum := &fakeEUM{}
	in := textInput(false)
	in.MessageType = "promotional"
	in.TimeToLive = 60
	in.Context = map[string]string{"k": "v"}
	in.MaxPrice = "0.02"
	res, out, err := eumDeps(eum, nil, nil).sendTextMessage()(authedCtx("msg/sms:send"), nil, in)
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if out.MessageID != "sms-123" || out.WouldCall != nil {
		t.Fatalf("output: %+v", out)
	}
	if eum.textIn.MessageType != eumtypes.MessageTypePromotional ||
		aws.ToInt32(eum.textIn.TimeToLive) != 60 || eum.textIn.Context["k"] != "v" ||
		aws.ToString(eum.textIn.MaxPrice) != "0.02" {
		t.Fatalf("options not mapped: %+v", eum.textIn)
	}
}

func TestSendTextGuardrailsAndShape(t *testing.T) {
	d := eumDeps(&fakeEUM{}, nil, nil)
	cases := map[string]schemas.SendTextMessageInput{
		"non-us":            {DestinationPhoneNumber: "+442071234567", MessageBody: "x"},
		"not-in-allow-list": {DestinationPhoneNumber: "+19995550100", MessageBody: "x"},
		"foreign-origin":    {DestinationPhoneNumber: "+12065550100", MessageBody: "x", OriginationIdentity: "+15550000000"},
		"bad-price":         {DestinationPhoneNumber: "+12065550100", MessageBody: "x", MaxPrice: "lots"},
	}
	for name, in := range cases {
		res, _, err := d.sendTextMessage()(authedCtx("msg/sms:send"), nil, in)
		if err != nil || res == nil || !res.IsError || !strings.Contains(text(t, res), "blocked by guardrail") {
			t.Fatalf("%s: expected guardrail block, got %v", name, res)
		}
	}
	if res, _, _ := d.sendTextMessage()(authedCtx("msg/sms:send"), nil, schemas.SendTextMessageInput{DestinationPhoneNumber: "+12065550100"}); res == nil || !strings.Contains(text(t, res), "MessageBody") {
		t.Fatal("empty body accepted")
	}
	if res, _, _ := d.sendTextMessage()(authedCtx("msg/read"), nil, textInput(false)); res == nil || !res.IsError {
		t.Fatal("missing scope accepted")
	}
	eum := &fakeEUM{sendErr: errors.New("boom")}
	if res, _, _ := eumDeps(eum, nil, nil).sendTextMessage()(authedCtx("msg/sms:send"), nil, textInput(false)); res == nil || !res.IsError {
		t.Fatal("API error not surfaced")
	}
}

func TestSendMediaUploadPath(t *testing.T) {
	eum := &fakeEUM{}
	media := &fakeMedia{}
	in := schemas.SendMediaMessageInput{
		DestinationPhoneNumber: "+12065550100",
		MessageBody:            "pic",
		MediaUpload: &schemas.MediaUpload{
			FileName:      "photo.jpg",
			ContentType:   "image/jpeg",
			Base64Content: base64.StdEncoding.EncodeToString([]byte("tiny-image")),
		},
	}
	res, out, err := eumDeps(eum, media, nil).sendMediaMessage()(authedCtx("msg/sms:send"), nil, in)
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if out.MessageID != "mms-123" || !strings.HasPrefix(out.MediaURL, "s3://media-bucket/mms/") {
		t.Fatalf("output: %+v", out)
	}
	if media.putIn == nil || aws.ToString(media.putIn.ContentType) != "image/jpeg" {
		t.Fatalf("upload input: %+v", media.putIn)
	}
	if len(eum.mediaIn.MediaUrls) != 1 || eum.mediaIn.MediaUrls[0] != out.MediaURL {
		t.Fatalf("media urls: %v", eum.mediaIn.MediaUrls)
	}

	// DryRun: no upload, no send, but the would-be call carries the URL.
	in.DryRun = true
	media.putIn = nil
	eum.mediaIn = nil
	_, out, _ = eumDeps(eum, media, nil).sendMediaMessage()(authedCtx("msg/sms:send"), nil, in)
	if media.putIn != nil || eum.mediaIn != nil || out.WouldCall == nil || len(out.WouldCall.MediaUrls) != 1 {
		t.Fatalf("dry run behaviour: put=%v send=%v would=%+v", media.putIn, eum.mediaIn, out.WouldCall)
	}
}

func TestSendMediaValidation(t *testing.T) {
	d := eumDeps(&fakeEUM{}, &fakeMedia{}, nil)
	base := schemas.SendMediaMessageInput{DestinationPhoneNumber: "+12065550100"}
	if res, _, _ := d.sendMediaMessage()(authedCtx("msg/sms:send"), nil, base); res == nil || !strings.Contains(text(t, res), "MediaUrls or MediaUpload") {
		t.Fatal("empty media accepted")
	}
	bad := base
	bad.MediaUpload = &schemas.MediaUpload{FileName: "x.jpg", ContentType: "image/jpeg", Base64Content: "!!!"}
	if res, _, _ := d.sendMediaMessage()(authedCtx("msg/sms:send"), nil, bad); res == nil || !strings.Contains(text(t, res), "base64") {
		t.Fatal("bad base64 accepted")
	}
	huge := base
	huge.MediaUpload = &schemas.MediaUpload{FileName: "x.jpg", ContentType: "image/jpeg",
		Base64Content: base64.StdEncoding.EncodeToString(make([]byte, 100))}
	if res, _, _ := d.sendMediaMessage()(authedCtx("msg/sms:send"), nil, huge); res == nil || !res.IsError {
		t.Fatal("oversize upload accepted")
	}
	foreign := base
	foreign.MediaUrls = []string{"s3://other/x.jpg"}
	if res, _, _ := d.sendMediaMessage()(authedCtx("msg/sms:send"), nil, foreign); res == nil || !res.IsError {
		t.Fatal("foreign bucket accepted")
	}
	if res, _, _ := d.sendMediaMessage()(authedCtx("msg/read"), nil, base); res == nil || !res.IsError {
		t.Fatal("missing scope accepted")
	}
	failing := eumDeps(&fakeEUM{}, &fakeMedia{putErr: errors.New("s3 down")}, nil)
	ok := base
	ok.MediaUpload = &schemas.MediaUpload{FileName: "x.jpg", ContentType: "image/jpeg",
		Base64Content: base64.StdEncoding.EncodeToString([]byte("y"))}
	if res, _, _ := failing.sendMediaMessage()(authedCtx("msg/sms:send"), nil, ok); res == nil || !res.IsError {
		t.Fatal("upload failure not surfaced")
	}
	failingSend := eumDeps(&fakeEUM{sendErr: errors.New("eum down")}, &fakeMedia{}, nil)
	if res, _, _ := failingSend.sendMediaMessage()(authedCtx("msg/sms:send"), nil, ok); res == nil || !res.IsError {
		t.Fatal("send failure not surfaced")
	}
}

func TestDescribePhoneNumbers(t *testing.T) {
	d := eumDeps(&fakeEUM{}, nil, nil)
	res, out, err := d.describePhoneNumbers()(authedCtx("msg/read"), nil, schemas.DescribePhoneNumbersInput{MaxResults: 5, PhoneNumberIDs: []string{"pn-1"}})
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if len(out.PhoneNumbers) != 1 || out.PhoneNumbers[0].PhoneNumber != "+18885550100" ||
		out.PhoneNumbers[0].Status != "ACTIVE" || len(out.PhoneNumbers[0].NumberCapabilities) != 2 {
		t.Fatalf("output: %+v", out)
	}
	if res, _, _ := d.describePhoneNumbers()(authedCtx(), nil, schemas.DescribePhoneNumbersInput{}); res == nil || !res.IsError {
		t.Fatal("missing scope accepted")
	}
	if res, _, _ := eumDeps(&fakeEUM{descErr: errors.New("x")}, nil, nil).describePhoneNumbers()(authedCtx("msg/read"), nil, schemas.DescribePhoneNumbersInput{}); res == nil || !res.IsError {
		t.Fatal("API error not surfaced")
	}
}

func TestGetMessageStatus(t *testing.T) {
	logs := &fakeLogs{messages: []string{
		`{"detail-type":"Text Message Delivered","time":"2026-08-24T00:00:00Z","detail":{"eventType":"TEXT_DELIVERED","messageId":"sms-123"}}`,
		`not-json sms-123`,
	}}
	d := eumDeps(&fakeEUM{}, nil, logs)
	res, out, err := d.getMessageStatus()(authedCtx("msg/read"), nil, schemas.GetMessageStatusInput{MessageID: "sms-123"})
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if out.Status != "TEXT_DELIVERED" || len(out.Events) != 1 || out.Events[0].Timestamp == "" {
		t.Fatalf("output: %+v", out)
	}
	if _, out, _ := d.getMessageStatus()(authedCtx("msg/read"), nil, schemas.GetMessageStatusInput{MessageID: "unseen"}); out.Status != "NO_EVENTS_YET" {
		t.Fatalf("no-events status: %+v", out)
	}
	if res, _, _ := d.getMessageStatus()(authedCtx("msg/read"), nil, schemas.GetMessageStatusInput{}); res == nil || !strings.Contains(text(t, res), "MessageId") {
		t.Fatal("empty id accepted")
	}
	if res, _, _ := d.getMessageStatus()(authedCtx(), nil, schemas.GetMessageStatusInput{MessageID: "x"}); res == nil || !res.IsError {
		t.Fatal("missing scope accepted")
	}
	if res, _, _ := eumDeps(&fakeEUM{}, nil, &fakeLogs{err: errors.New("denied")}).getMessageStatus()(authedCtx("msg/read"), nil, schemas.GetMessageStatusInput{MessageID: "x"}); res == nil || !res.IsError {
		t.Fatal("log error not surfaced")
	}
}

func TestMediaKeyShape(t *testing.T) {
	k1, k2 := mediaKey("a.jpg"), mediaKey("a.jpg")
	if k1 == k2 || !strings.HasPrefix(k1, "mms/") || !strings.HasSuffix(k1, "-a.jpg") {
		t.Fatalf("keys: %s %s", k1, k2)
	}
	if k := mediaKey("../../evil.jpg"); strings.Contains(k, "..") {
		t.Fatalf("path traversal survives: %s", k)
	}
	if k := mediaKey(""); !strings.HasSuffix(k, "-media") {
		t.Fatalf("empty name fallback: %s", k)
	}
}
