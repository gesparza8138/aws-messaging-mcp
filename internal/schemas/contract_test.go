package schemas

// Contract tests (PRD §11.4): every property a tool exposes must exist on the
// AWS SDK's input struct with a compatible shape, so the schemas track the
// real API (G5). Server-injected fields are asserted to exist as well.

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// serverInjected are send-tool members the server owns (PRD 5.1 rule 2);
// they must exist on the SDK structs even though callers cannot set them.
var serverInjected = []string{"ConfigurationSetName"}

// eumServerInjected is the EUM equivalent (PRD 5.3).
var eumServerInjected = []string{"ConfigurationSetName", "ProtectConfigurationId", "DryRun"}

// toolOnly are schema fields with no SDK counterpart by design. DryRun keeps
// the M2 server-side semantics on every send tool (the EUM APIs have their
// own DryRun field, but it is server-controlled — plan M3-2); MediaUpload is
// the inline-attachment convenience; RawContentKey is its email counterpart —
// a files-bucket key the server reads and resolves into the SDK's RawContent
// before the call is built, so nothing new reaches SES.
var toolOnly = map[string]bool{"DryRun": true, "MediaUpload": true, "RawContentKey": true}

func assertFieldsExist(t *testing.T, schema, sdk reflect.Type) {
	t.Helper()
	for i := 0; i < schema.NumField(); i++ {
		field := schema.Field(i)
		if toolOnly[field.Name] {
			continue
		}
		sdkField, ok := sdk.FieldByName(sdkName(field.Name))
		if !ok {
			t.Errorf("%s.%s has no counterpart on %s", schema.Name(), field.Name, sdk.Name())
			continue
		}
		if base(field.Type).Kind() == reflect.Struct && base(sdkField.Type).Kind() == reflect.Struct &&
			base(field.Type).PkgPath() == schema.PkgPath() {
			assertFieldsExist(t, base(field.Type), base(sdkField.Type))
		}
		if field.Type.Kind() == reflect.Slice && sdkField.Type.Kind() != reflect.Slice {
			t.Errorf("%s.%s is a list but SDK %s is %s", schema.Name(), field.Name, sdkField.Name, sdkField.Type.Kind())
		}
	}
}

// sdkName maps schema field names to SDK field names where Go's initialism
// conventions differ from the SDK's (the JSON tags carry the wire names).
func sdkName(name string) string {
	switch name {
	case "HTML":
		return "Html"
	case "PhoneNumberIDs":
		return "PhoneNumberIds"
	}
	return name
}

func base(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	return t
}

func TestSendEmailSchemaMatchesSDK(t *testing.T) {
	assertFieldsExist(t, reflect.TypeOf(SendEmailInput{}), reflect.TypeOf(sesv2.SendEmailInput{}))
	// Nested types the walker crosses package boundaries on:
	assertFieldsExist(t, reflect.TypeOf(Destination{}), reflect.TypeOf(sestypes.Destination{}))
	assertFieldsExist(t, reflect.TypeOf(EmailContent{}), reflect.TypeOf(sestypes.EmailContent{}))
	assertFieldsExist(t, reflect.TypeOf(Message{}), reflect.TypeOf(sestypes.Message{}))
	assertFieldsExist(t, reflect.TypeOf(Body{}), reflect.TypeOf(sestypes.Body{}))
	assertFieldsExist(t, reflect.TypeOf(Content{}), reflect.TypeOf(sestypes.Content{}))
	assertFieldsExist(t, reflect.TypeOf(Attachment{}), reflect.TypeOf(sestypes.Attachment{}))
	assertFieldsExist(t, reflect.TypeOf(RawMessage{}), reflect.TypeOf(sestypes.RawMessage{}))
	assertFieldsExist(t, reflect.TypeOf(MessageTag{}), reflect.TypeOf(sestypes.MessageTag{}))

	sdk := reflect.TypeOf(sesv2.SendEmailInput{})
	for _, name := range serverInjected {
		if _, ok := sdk.FieldByName(name); !ok {
			t.Errorf("server-injected field %s no longer exists on the SDK input", name)
		}
	}
}

func TestListIdentitiesSchemaMatchesSDK(t *testing.T) {
	assertFieldsExist(t, reflect.TypeOf(ListEmailIdentitiesInput{}), reflect.TypeOf(sesv2.ListEmailIdentitiesInput{}))
}

func TestSendTextMessageSchemaMatchesSDK(t *testing.T) {
	sdk := reflect.TypeOf(pinpointsmsvoicev2.SendTextMessageInput{})
	assertFieldsExist(t, reflect.TypeOf(SendTextMessageInput{}), sdk)
	for _, name := range eumServerInjected {
		if _, ok := sdk.FieldByName(name); !ok {
			t.Errorf("server-controlled field %s no longer exists on the SDK input", name)
		}
	}
}

func TestSendMediaMessageSchemaMatchesSDK(t *testing.T) {
	sdk := reflect.TypeOf(pinpointsmsvoicev2.SendMediaMessageInput{})
	assertFieldsExist(t, reflect.TypeOf(SendMediaMessageInput{}), sdk)
	for _, name := range eumServerInjected {
		if _, ok := sdk.FieldByName(name); !ok {
			t.Errorf("server-controlled field %s no longer exists on the SDK input", name)
		}
	}
}

func TestDescribePhoneNumbersSchemaMatchesSDK(t *testing.T) {
	assertFieldsExist(t, reflect.TypeOf(DescribePhoneNumbersInput{}), reflect.TypeOf(pinpointsmsvoicev2.DescribePhoneNumbersInput{}))
}

func TestDestinationAll(t *testing.T) {
	var d *Destination
	if d.All() != nil {
		t.Fatal("nil destination has no recipients")
	}
	d = &Destination{ToAddresses: []string{"a"}, CcAddresses: []string{"b"}, BccAddresses: []string{"c"}}
	if got := d.All(); len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("all: %v", got)
	}
}
