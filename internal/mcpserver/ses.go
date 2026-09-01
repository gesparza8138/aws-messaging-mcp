package mcpserver

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
)

// ServerMetadata accompanies every send-tool result (PRD 5.1 rule 4).
type ServerMetadata struct {
	Guardrails []guardrails.Decision `json:"guardrails"`
	DryRun     bool                  `json:"dry_run"`
}

// SendEmailOutput is the ses_send_email result.
type SendEmailOutput struct {
	MessageID      string                `json:"MessageId,omitempty"`
	WouldCall      *sesv2.SendEmailInput `json:"WouldCall,omitempty"`
	ServerMetadata ServerMetadata        `json:"ServerMetadata"`
}

func (d Deps) sendEmail() mcp.ToolHandlerFor[schemas.SendEmailInput, SendEmailOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.SendEmailInput) (*mcp.CallToolResult, SendEmailOutput, error) {
		if res := requireScope(ctx, "msg/email:send"); res != nil {
			return res, SendEmailOutput{}, nil
		}
		s := d.Settings
		var result guardrails.Result

		// Shape rules first (PRD 5.3): exactly one of Simple / Raw.
		if in.Content == nil || (in.Content.Simple == nil) == (in.Content.Raw == nil) {
			return toolError("Content must contain exactly one of Simple or Raw"), SendEmailOutput{}, nil
		}
		recipients := in.Destination.All()
		result.Add(guardrails.MaxRecipients(len(recipients), s.MaxRecipients))
		result.Add(guardrails.RecipientsAllowed(recipients, s.RecipientAllowList))
		// The guardrails decode the base64 payloads; the builder reuses the
		// bytes instead of decoding them a second time.
		var rawDecoded []byte
		var attDecoded [][]byte
		if in.Content.Raw != nil {
			var decisions []guardrails.Decision
			rawDecoded, decisions = guardrails.RawEmail(in.Content.Raw.Data, s.EmailMaxRawBytes, s.SESSenderAddresses)
			for _, dec := range decisions {
				result.Add(dec)
			}
		} else {
			result.Add(guardrails.SenderAllowed(in.FromEmailAddress, s.SESSenderAddresses))
			if len(in.Content.Simple.Attachments) > 0 {
				atts := make([]guardrails.AttachmentInput, len(in.Content.Simple.Attachments))
				for i, a := range in.Content.Simple.Attachments {
					atts[i] = guardrails.AttachmentInput{FileName: a.FileName, RawContent: a.RawContent}
				}
				var decisions []guardrails.Decision
				attDecoded, decisions = guardrails.EmailAttachments(atts, s.EmailMaxRawBytes)
				for _, dec := range decisions {
					result.Add(dec)
				}
			}
		}
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "ses_send_email"))
		}
		meta := ServerMetadata{Guardrails: result.Decisions, DryRun: in.DryRun}
		out := SendEmailOutput{ServerMetadata: meta}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, SendEmailOutput{}, nil
		}

		call := buildSendEmail(in, s.SESConfigurationSet, s.SESReplyTo, rawDecoded, attDecoded)
		if in.DryRun {
			out.WouldCall = call
			return nil, out, nil
		}
		resp, err := d.SES.SendEmail(ctx, call)
		if err != nil {
			res := toolError(awsclients.ErrorText(err))
			res.StructuredContent = out
			return res, SendEmailOutput{}, nil
		}
		out.MessageID = aws.ToString(resp.MessageId)
		return nil, out, nil
	}
}

// buildSendEmail maps the tool input onto the SDK input, injecting the
// server-owned fields (PRD 5.1 rule 2). rawDecoded and attDecoded are the
// bytes the guardrails already decoded; attDecoded is index-aligned with the
// Simple attachments.
func buildSendEmail(in schemas.SendEmailInput, configurationSet, defaultReplyTo string, rawDecoded []byte, attDecoded [][]byte) *sesv2.SendEmailInput {
	call := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(in.FromEmailAddress),
		Content:          &sestypes.EmailContent{},
	}
	if configurationSet != "" {
		call.ConfigurationSetName = aws.String(configurationSet)
	}
	if in.Destination != nil {
		call.Destination = &sestypes.Destination{
			ToAddresses:  in.Destination.ToAddresses,
			CcAddresses:  in.Destination.CcAddresses,
			BccAddresses: in.Destination.BccAddresses,
		}
	}
	switch {
	case len(in.ReplyToAddresses) > 0:
		call.ReplyToAddresses = in.ReplyToAddresses
	case defaultReplyTo != "":
		call.ReplyToAddresses = []string{defaultReplyTo}
	}
	for _, tag := range in.EmailTags {
		call.EmailTags = append(call.EmailTags, sestypes.MessageTag{
			Name: aws.String(tag.Name), Value: aws.String(tag.Value),
		})
	}
	if in.Content.Raw != nil {
		call.Content.Raw = &sestypes.RawMessage{Data: rawDecoded}
		return call
	}
	simple := in.Content.Simple
	msg := &sestypes.Message{}
	if simple.Subject != nil {
		msg.Subject = content(simple.Subject)
	}
	if simple.Body != nil {
		msg.Body = &sestypes.Body{}
		if simple.Body.Text != nil {
			msg.Body.Text = content(simple.Body.Text)
		}
		if simple.Body.HTML != nil {
			msg.Body.Html = content(simple.Body.HTML)
		}
	}
	for i, a := range simple.Attachments {
		att := sestypes.Attachment{FileName: aws.String(a.FileName)}
		if i < len(attDecoded) {
			att.RawContent = attDecoded[i]
		}
		if a.ContentType != "" {
			att.ContentType = aws.String(a.ContentType)
		}
		if a.ContentDescription != "" {
			att.ContentDescription = aws.String(a.ContentDescription)
		}
		if a.ContentDisposition != "" {
			att.ContentDisposition = sestypes.AttachmentContentDisposition(a.ContentDisposition)
		}
		if a.ContentId != "" {
			att.ContentId = aws.String(a.ContentId)
		}
		if a.ContentTransferEncoding != "" {
			att.ContentTransferEncoding = sestypes.AttachmentContentTransferEncoding(a.ContentTransferEncoding)
		}
		msg.Attachments = append(msg.Attachments, att)
	}
	call.Content.Simple = msg
	return call
}

func content(c *schemas.Content) *sestypes.Content {
	out := &sestypes.Content{Data: aws.String(c.Data)}
	if c.Charset != "" {
		out.Charset = aws.String(c.Charset)
	}
	return out
}

// Identity is one entry in the ses_list_email_identities result.
type Identity struct {
	IdentityName   string `json:"IdentityName"`
	IdentityType   string `json:"IdentityType"`
	SendingEnabled bool   `json:"SendingEnabled"`
}

// ListIdentitiesOutput is the ses_list_email_identities result.
type ListIdentitiesOutput struct {
	EmailIdentities []Identity `json:"EmailIdentities"`
}

func (d Deps) listIdentities() mcp.ToolHandlerFor[schemas.ListEmailIdentitiesInput, ListIdentitiesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.ListEmailIdentitiesInput) (*mcp.CallToolResult, ListIdentitiesOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, ListIdentitiesOutput{}, nil
		}
		size := in.PageSize
		if size <= 0 || size > 100 {
			size = 25
		}
		resp, err := d.SES.ListEmailIdentities(ctx, &sesv2.ListEmailIdentitiesInput{PageSize: aws.Int32(size)})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), ListIdentitiesOutput{}, nil
		}
		out := ListIdentitiesOutput{EmailIdentities: []Identity{}}
		for _, id := range resp.EmailIdentities {
			out.EmailIdentities = append(out.EmailIdentities, Identity{
				IdentityName:   aws.ToString(id.IdentityName),
				IdentityType:   string(id.IdentityType),
				SendingEnabled: id.SendingEnabled,
			})
		}
		return nil, out, nil
	}
}

// AccountOutput is the ses_get_account result.
type AccountOutput struct {
	ProductionAccessEnabled bool    `json:"ProductionAccessEnabled"`
	SendingEnabled          bool    `json:"SendingEnabled"`
	Max24HourSend           float64 `json:"Max24HourSend"`
	MaxSendRate             float64 `json:"MaxSendRate"`
	SentLast24Hours         float64 `json:"SentLast24Hours"`
}

func (d Deps) getAccount() mcp.ToolHandlerFor[schemas.GetAccountInput, AccountOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ schemas.GetAccountInput) (*mcp.CallToolResult, AccountOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, AccountOutput{}, nil
		}
		resp, err := d.SES.GetAccount(ctx, &sesv2.GetAccountInput{})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), AccountOutput{}, nil
		}
		out := AccountOutput{
			ProductionAccessEnabled: resp.ProductionAccessEnabled,
			SendingEnabled:          resp.SendingEnabled,
		}
		if resp.SendQuota != nil {
			out.Max24HourSend = resp.SendQuota.Max24HourSend
			out.MaxSendRate = resp.SendQuota.MaxSendRate
			out.SentLast24Hours = resp.SendQuota.SentLast24Hours
		}
		return nil, out, nil
	}
}

// requireScope returns an isError result when the caller lacks scope.
func requireScope(ctx context.Context, scope string) *mcp.CallToolResult {
	principal, ok := httpapi.PrincipalFrom(ctx)
	if !ok {
		return toolError("no authenticated principal in context")
	}
	if err := auth.RequireScope(principal, scope); err != nil {
		return toolError(err.Error())
	}
	return nil
}
