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

// Attachment mirrors sesv2 Attachment (the caller-controlled subset).
type Attachment struct {
	FileName    string `json:"FileName"`
	ContentType string `json:"ContentType,omitempty"`
	RawContent  string `json:"RawContent" jsonschema:"Attachment bytes, base64-encoded"`
}

// RawMessage mirrors sesv2 RawMessage.
type RawMessage struct {
	Data string `json:"Data" jsonschema:"Complete MIME message, base64-encoded"`
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
