package schemas

// SendTextMessageInput mirrors pinpoint-sms-voice-v2 SendTextMessage (the
// subset callers control). ConfigurationSetName and ProtectConfigurationId
// are server-injected; the API's own DryRun field is server-controlled and
// the tool-level DryRun below keeps the M2 semantics (guardrails + the
// would-be call, no AWS request).
type SendTextMessageInput struct {
	DestinationPhoneNumber string            `json:"DestinationPhoneNumber" jsonschema:"Recipient in E.164, e.g. +12065550100; must satisfy the recipient allow-list where configured"`
	MessageBody            string            `json:"MessageBody" jsonschema:"The text to send"`
	OriginationIdentity    string            `json:"OriginationIdentity,omitempty" jsonschema:"Origination number; defaults to the server's toll-free number and must match it"`
	MessageType            string            `json:"MessageType,omitempty" jsonschema:"TRANSACTIONAL (default) or PROMOTIONAL"`
	MaxPrice               string            `json:"MaxPrice,omitempty" jsonschema:"Per-message USD ceiling; the server caps it at its configured maximum"`
	TimeToLive             int32             `json:"TimeToLive,omitempty" jsonschema:"Seconds the message may spend queued, 5-259200"`
	Context                map[string]string `json:"Context,omitempty" jsonschema:"Key/value pairs logged to the event destination"`
	DryRun                 bool              `json:"DryRun,omitempty" jsonschema:"Validate and run guardrails, return the would-be call without sending"`
}

// SendMediaMessageInput mirrors pinpoint-sms-voice-v2 SendMediaMessage.
// MediaUrls must point into the server's media bucket; MediaUpload is a
// tool-side convenience that uploads inline content there first.
type SendMediaMessageInput struct {
	DestinationPhoneNumber string            `json:"DestinationPhoneNumber" jsonschema:"Recipient in E.164"`
	MessageBody            string            `json:"MessageBody,omitempty" jsonschema:"Optional text accompanying the media"`
	MediaUrls              []string          `json:"MediaUrls,omitempty" jsonschema:"s3:// URLs inside the server's media bucket"`
	MediaUpload            *MediaUpload      `json:"MediaUpload,omitempty" jsonschema:"Inline media; the server uploads it to the media bucket and substitutes the s3:// URL"`
	OriginationIdentity    string            `json:"OriginationIdentity,omitempty" jsonschema:"Origination number; defaults to the server's toll-free number and must match it"`
	MaxPrice               string            `json:"MaxPrice,omitempty" jsonschema:"Per-message USD ceiling; the server caps it at its configured maximum"`
	TimeToLive             int32             `json:"TimeToLive,omitempty" jsonschema:"Seconds the message may spend queued, 5-259200"`
	Context                map[string]string `json:"Context,omitempty" jsonschema:"Key/value pairs logged to the event destination"`
	DryRun                 bool              `json:"DryRun,omitempty" jsonschema:"Validate and run guardrails, return the would-be call without sending"`
}

// MediaUpload is the inline-attachment convenience (no SDK counterpart).
type MediaUpload struct {
	FileName      string `json:"FileName" jsonschema:"Object name, e.g. photo.jpg"`
	ContentType   string `json:"ContentType" jsonschema:"image/jpeg, image/png, or image/gif"`
	Base64Content string `json:"Base64Content" jsonschema:"Base64-encoded file content, at most 5 MB decoded"`
}

// DescribePhoneNumbersInput mirrors pinpoint-sms-voice-v2
// DescribePhoneNumbers (the subset callers control).
type DescribePhoneNumbersInput struct {
	PhoneNumberIDs []string `json:"PhoneNumberIds,omitempty" jsonschema:"Specific phone-number ids; empty lists every number"`
	MaxResults     int32    `json:"MaxResults,omitempty" jsonschema:"Page size, 1-100"`
}

// GetMessageStatusInput drives the event-trail lookup (PRD M3-3); there is
// no per-message API in pinpoint-sms-voice-v2, so this has no SDK mirror.
type GetMessageStatusInput struct {
	MessageID string `json:"MessageId" jsonschema:"MessageId returned by a send tool"`
}
