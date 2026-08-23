package schemas

// Contract tests (PRD §11.4): every property a tool exposes must exist on the
// AWS SDK's input struct with a compatible shape, so the schemas track the
// real API (G5). Server-injected fields are asserted to exist as well.

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// serverInjected are SendEmail members the server owns (PRD 5.1 rule 2);
// they must exist on the SDK struct even though callers cannot set them.
var serverInjected = []string{"ConfigurationSetName"}

// toolOnly are schema fields with no SDK counterpart by design.
var toolOnly = map[string]bool{"DryRun": true}

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
// conventions differ from ours.
func sdkName(name string) string {
	if name == "HTML" {
		return "Html"
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
