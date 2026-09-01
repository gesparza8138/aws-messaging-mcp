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
	Raw    *RawMessage `json:"Raw,omitempty" jsonschema:"Complete MIME message, base64-encoded"`
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

// Attachment mirrors sesv2 Attachment (the caller-controlled subset). SES
// assembles Simple attachments under a multipart/mixed root, so INLINE +
// ContentId does not embed an image: a cid: reference only resolves when the
// HTML part and the image part are siblings inside a multipart/related, and
// the attachment is delivered as an ordinary one instead. Content.Raw with a
// hand-built multipart/related is the path that works today; server-side
// assembly is planned (docs/plans/email-inline-mime.md). RawContentKey is the
// one field with no SDK counterpart: it names an object already in the files
// bucket, which the server reads and attaches as RawContent so the bytes
// never travel through the caller's context.
type Attachment struct {
	FileName           string `json:"FileName"`
	ContentType        string `json:"ContentType,omitempty"`
	RawContent         string `json:"RawContent,omitempty" jsonschema:"Attachment bytes, base64-encoded; exactly one of RawContent or RawContentKey must be set. Decoded attachments and Raw content share the server budget (10 MB decoded by default), and SES caps the assembled message at 40 MB"`
	RawContentKey      string `json:"RawContentKey,omitempty" jsonschema:"Key of an object already in the files bucket (shared/... from files_put_object or files_list_objects) to attach instead of RawContent; exactly one of the two must be set. Requires msg/read as well as msg/email:send, and the object must still be within its expiry and inside the attachment budget"`
	ContentDescription string `json:"ContentDescription,omitempty" jsonschema:"Human-readable description of the attachment"`
	ContentDisposition string `json:"ContentDisposition,omitempty" jsonschema:"Either ATTACHMENT (default) or INLINE; INLINE does NOT render the image in the body today, because SES assembles Simple attachments under multipart/mixed rather than the multipart/related a cid: reference needs - send Content.Raw with a hand-built multipart/related message for that. Case-sensitive."`
	// ContentId keeps the SDK's spelling (not ContentID) because the contract
	// test matches schema field names against the SDK struct by reflection.
	ContentId               string `json:"ContentId,omitempty" jsonschema:"Content-ID recorded on the attachment part; a cid:<value> reference in the HTML body does not resolve to it, because SES assembles Simple attachments under multipart/mixed - use Content.Raw with a multipart/related message for a rendered inline image"` //nolint:revive // SDK field name, see above
	ContentTransferEncoding string `json:"ContentTransferEncoding,omitempty" jsonschema:"BASE64 (default), QUOTED_PRINTABLE, or SEVEN_BIT; case-sensitive"`
}

// RawMessage mirrors sesv2 RawMessage.
type RawMessage struct {
	Data string `json:"Data" jsonschema:"Complete MIME message, base64-encoded; shares the server budget with attachments (10 MB decoded by default), and SES caps the assembled message at 40 MB"`
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
