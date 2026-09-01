// Package schemas defines tool input types that mirror the AWS API request
// shapes (PRD G5): property names and nesting are the PascalCase forms the
// AWS CLI's --cli-input-json accepts. Server-controlled fields (configuration
// set, identity ARNs, feedback addresses) are deliberately absent - contract
// tests assert every field here exists on the SDK input struct.
package schemas

// SendEmailInput mirrors sesv2 SendEmail (the subset callers control).
type SendEmailInput struct {
	FromEmailAddress string        `json:"FromEmailAddress" jsonschema:"Sender address; must be in the server's allow-list"`
	Destination      *Destination  `json:"Destination,omitempty" jsonschema:"Recipients of the message"`
	ReplyToAddresses []string      `json:"ReplyToAddresses,omitempty" jsonschema:"Reply-To addresses; defaults to the owner's address"`
	Content          *EmailContent `json:"Content" jsonschema:"Exactly one of Simple or Raw"`
	EmailTags        []MessageTag  `json:"EmailTags,omitempty" jsonschema:"Name/value pairs published with sending events"`
	DryRun           bool          `json:"DryRun,omitempty" jsonschema:"Validate and run guardrails, return the would-be call without sending"`
}

// Destination mirrors sesv2 Destination.
type Destination struct {
	ToAddresses  []string `json:"ToAddresses,omitempty"`
	CcAddresses  []string `json:"CcAddresses,omitempty"`
	BccAddresses []string `json:"BccAddresses,omitempty"`
}

// All returns every recipient across To, Cc, and Bcc.
func (d *Destination) All() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.ToAddresses)+len(d.CcAddresses)+len(d.BccAddresses))
	out = append(out, d.ToAddresses...)
	out = append(out, d.CcAddresses...)
	out = append(out, d.BccAddresses...)
	return out
}

// EmailContent mirrors sesv2 EmailContent (Simple or Raw; Template excluded
// per PRD non-goals).
type EmailContent struct {
	Simple *Message    `json:"Simple,omitempty" jsonschema:"Structured subject/body message"`
	Raw    *RawMessage `json:"Raw,omitempty" jsonschema:"Complete MIME message, base64-encoded inline (Data) or named by files-bucket key (DataKey)"`
}

// Message mirrors sesv2 Message.
type Message struct {
	Subject     *Content     `json:"Subject"`
	Body        *Body        `json:"Body"`
	Attachments []Attachment `json:"Attachments,omitempty"`
}

// Content mirrors sesv2 Content.
type Content struct {
	Data    string `json:"Data"`
	Charset string `json:"Charset,omitempty"`
}

// Body mirrors sesv2 Body.
type Body struct {
	Text *Content `json:"Text,omitempty"`
	HTML *Content `json:"Html,omitempty"`
}

// Attachment mirrors sesv2 Attachment (the caller-controlled subset). An
// attachment marked INLINE — or carrying a ContentId with no disposition at
// all — makes the server assemble the message itself and send it as
// Content.Raw, with the HTML part and the image as siblings inside a
// multipart/related, which is the tree a cid: reference needs to resolve. SES's
// own Simple assembly roots every attachment under a multipart/mixed, where it
// never does (docs/plans/email-inline-mime.md). RawContentKey is the one field
// with no SDK counterpart: it names an object already in the files bucket,
// which the server reads and attaches as RawContent so the bytes never travel
// through the caller's context.
type Attachment struct {
	FileName           string `json:"FileName"`
	ContentType        string `json:"ContentType,omitempty"`
	RawContent         string `json:"RawContent,omitempty" jsonschema:"Attachment bytes, base64-encoded; exactly one of RawContent or RawContentKey must be set. Decoded attachments and Raw content share the server budget (10 MB decoded by default), and SES caps the assembled message at 40 MB"`
	RawContentKey      string `json:"RawContentKey,omitempty" jsonschema:"Key of an object already in the files bucket (shared/... from files_put_object or files_list_objects) to attach instead of RawContent; exactly one of the two must be set. Requires msg/read as well as msg/email:send, and the object must still be within its expiry and inside the email budget attachments and Raw content share"`
	ContentDescription string `json:"ContentDescription,omitempty" jsonschema:"Human-readable description of the attachment"`
	ContentDisposition string `json:"ContentDisposition,omitempty" jsonschema:"Either ATTACHMENT (default) or INLINE; INLINE (like a ContentId with no disposition) makes the server assemble the message itself, placing this part in a multipart/related beside the Html body and sending it as Content.Raw, so a cid: reference renders it in the body. INLINE requires an Html body and a ContentId; a part the HTML never references still travels as an ordinary attachment. Case-sensitive."`
	// ContentId keeps the SDK's spelling (not ContentID) because the contract
	// test matches schema field names against the SDK struct by reflection.
	ContentId               string `json:"ContentId,omitempty" jsonschema:"Content-ID recorded on the attachment part, which an <img src=\"cid:<value>\"> in the Html body resolves to because the server assembles the inline parts into a multipart/related itself. Give it with or without angle brackets (chart or <chart>); the HTML always references the bare form. 1-78 characters from [A-Za-z0-9._@+-], unique across the message. A ContentId with no ContentDisposition counts as INLINE; set ATTACHMENT explicitly to carry one without embedding the part"` //nolint:revive // SDK field name, see above
	ContentTransferEncoding string `json:"ContentTransferEncoding,omitempty" jsonschema:"BASE64 (default), QUOTED_PRINTABLE, or SEVEN_BIT; case-sensitive"`
}

// RawMessage mirrors sesv2 RawMessage. DataKey is the one field with no SDK
// counterpart, and it is Attachment.RawContentKey's twin one level up: it names
// an object already in the files bucket that holds the *whole* MIME message,
// which the server reads and sends as Data. Inlining a message costs base64
// twice over — a 75 KB image is ~100 KB as a body part and ~156,000 characters
// once the message is base64-encoded again for Data — so a caller that has to
// emit that string token by token cannot reach this shape at all.
type RawMessage struct {
	Data    string `json:"Data,omitempty" jsonschema:"Complete MIME message, base64-encoded; exactly one of Data or DataKey must be set. Shares the server budget with attachments (10 MB decoded by default), and SES caps the assembled message at 40 MB"`
	DataKey string `json:"DataKey,omitempty" jsonschema:"Key of an object already in the files bucket (shared/... from files_put_object or files_list_objects) holding the complete MIME message, read by the server instead of Data; exactly one of the two must be set. Use it when the message is too large to emit as one base64 string. Requires msg/read as well as msg/email:send, and the object must still be within its expiry and inside the same email budget. The guardrail ladder is raw_size, raw_mime, sender_allow_list — raw_base64 has nothing to decide, and the From header can only be checked after the object is read"`
}

// MessageTag mirrors sesv2 MessageTag.
type MessageTag struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

// ListEmailIdentitiesInput mirrors sesv2 ListEmailIdentities.
type ListEmailIdentitiesInput struct {
	PageSize int32 `json:"PageSize,omitempty" jsonschema:"Maximum identities to return (first page only)"`
}

// GetAccountInput mirrors sesv2 GetAccount (no parameters).
type GetAccountInput struct{}
