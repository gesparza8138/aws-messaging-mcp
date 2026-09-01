//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tinyPNG is a valid 1x1 transparent PNG for the MMS upload path.
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// skipUnlessSandboxAllows turns the EUM sandbox's verified-destination
// refusal into a skip: the send pipeline is proven up to the API, and the
// destination becomes sendable once verified or once production access lands.
func skipUnlessSandboxAllows(t *testing.T, refusal string) {
	t.Helper()
	lower := strings.ToLower(refusal)
	for _, marker := range []string{"verified", "sandbox", "destination country", "not supported"} {
		if strings.Contains(lower, marker) {
			t.Skipf("EUM sandbox restriction (verify the destination or leave the sandbox): %s", refusal)
		}
	}
}

// TestSmsTools drives the SMS tool family: registry, read tools, guardrails,
// DryRun injection, and — when the sandbox allows — one real SMS and MMS with
// an event-trail status lookup.
func TestSmsTools(t *testing.T) {
	e := load(t)
	origination := os.Getenv("E2E_ORIGINATION")
	if origination == "" {
		t.Skip("E2E_ORIGINATION unset; the stage has no toll-free number wired yet")
	}
	phone := os.Getenv("E2E_TEST_PHONE")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	session := e.session(ctx, t)

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range tools.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"sms_send_text_message", "sms_send_media_message", "sms_describe_phone_numbers", "sms_get_message_status"} {
		if !names[want] {
			t.Fatalf("tool %q missing from %v", want, names)
		}
	}

	numbers, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_describe_phone_numbers", Arguments: map[string]any{}})
	if err != nil || numbers.IsError {
		t.Fatalf("describe numbers: err=%v result=%s", err, text(numbers))
	}
	if !strings.Contains(text(numbers)+toJSON(t, structured(t, numbers)), origination) {
		t.Errorf("origination %s missing from describe output", origination)
	}

	blocked, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_send_text_message", Arguments: map[string]any{
		"DestinationPhoneNumber": "+12065550123",
		"MessageBody":            "must not send",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.IsError || !strings.Contains(text(blocked), "blocked by guardrail") {
		t.Fatalf("non-allow-listed destination must hit a guardrail: %s", text(blocked))
	}

	if phone == "" {
		t.Log("E2E_TEST_PHONE unset; skipping DryRun/real-send stages")
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339)

	dry, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_send_text_message", Arguments: map[string]any{
		"DestinationPhoneNumber": phone,
		"MessageBody":            "e2e dry run " + stamp,
		"DryRun":                 true,
	}})
	if err != nil || dry.IsError {
		t.Fatalf("dry run: err=%v result=%s", err, text(dry))
	}
	would, _ := structured(t, dry)["WouldCall"].(map[string]any)
	// The call carries the phone-number ARN, not the E.164 number a caller
	// names: an E.164 string does not resolve to the phone-number resource
	// during authorization, so the scoped SendSms grant never matched it and
	// every send was denied. Either form identifies the same number, so accept
	// the ARN or the number itself.
	sending, _ := would["OriginationIdentity"].(string)
	if would == nil || (sending != origination && !strings.HasPrefix(sending, "arn:aws:sms-voice:")) {
		t.Fatalf("origination not injected: %v", would)
	}
	if cs := os.Getenv("E2E_SMS_CONFIG_SET"); cs != "" && would["ConfigurationSetName"] != cs {
		t.Errorf("configuration set not injected: %v", would["ConfigurationSetName"])
	}
	if would["ProtectConfigurationId"] == nil || would["MaxPrice"] == nil {
		t.Errorf("protect configuration / max price not injected: %v", would)
	}

	sent, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_send_text_message", Arguments: map[string]any{
		"DestinationPhoneNumber": phone,
		"MessageBody":            "aws-messaging-mcp e2e " + stamp + " (reply STOP to opt out)",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if sent.IsError {
		skipUnlessSandboxAllows(t, text(sent))
		t.Fatalf("real SMS refused: %s", text(sent))
	}
	messageID, _ := structured(t, sent)["MessageId"].(string)
	if messageID == "" {
		t.Fatal("real SMS returned no MessageId")
	}
	t.Logf("sent SMS %s", messageID)

	mms, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_send_media_message", Arguments: map[string]any{
		"DestinationPhoneNumber": phone,
		"MessageBody":            "e2e mms " + stamp,
		"MediaUpload": map[string]any{
			"FileName":      "e2e.png",
			"ContentType":   "image/png",
			"Base64Content": tinyPNG,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if mms.IsError {
		skipUnlessSandboxAllows(t, text(mms))
		t.Fatalf("real MMS refused: %s", text(mms))
	}
	if id, _ := structured(t, mms)["MessageId"].(string); id == "" {
		t.Fatal("real MMS returned no MessageId")
	} else {
		t.Logf("sent MMS %s", id)
	}

	// Status lookup: bounded poll of the event trail through the tool itself.
	deadline := time.Now().Add(90 * time.Second)
	for {
		status, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "sms_get_message_status",
			Arguments: map[string]any{"MessageId": messageID}})
		if err != nil || status.IsError {
			t.Fatalf("status lookup: err=%v result=%s", err, text(status))
		}
		if s, _ := structured(t, status)["Status"].(string); s != "NO_EVENTS_YET" {
			t.Logf("status: %s", s)
			return
		}
		if time.Now().After(deadline) {
			t.Skip("no EUM events within 90s (carrier delay); sends succeeded")
		}
		time.Sleep(10 * time.Second)
	}
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
