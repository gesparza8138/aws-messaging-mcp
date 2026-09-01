package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"
	eumtypes "github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
)

// SendTextOutput is the sms_send_text_message result.
type SendTextOutput struct {
	MessageID      string                                   `json:"MessageId,omitempty"`
	WouldCall      *pinpointsmsvoicev2.SendTextMessageInput `json:"WouldCall,omitempty"`
	ServerMetadata ServerMetadata                           `json:"ServerMetadata"`
}

func (d Deps) sendTextMessage() mcp.ToolHandlerFor[schemas.SendTextMessageInput, SendTextOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.SendTextMessageInput) (*mcp.CallToolResult, SendTextOutput, error) {
		if res := requireScope(ctx, "msg/sms:send"); res != nil {
			return res, SendTextOutput{}, nil
		}
		s := d.Settings
		var result guardrails.Result
		result.Add(guardrails.DestinationCountryUS(in.DestinationPhoneNumber))
		result.Add(guardrails.RecipientsAllowed([]string{in.DestinationPhoneNumber}, s.RecipientAllowList))
		result.Add(guardrails.OriginationAllowed(in.OriginationIdentity, s.OriginationIdentity))
		price, priceDecision := guardrails.MaxPriceCapped(in.MaxPrice, s.SMSMaxPrice)
		result.Add(priceDecision)
		if strings.TrimSpace(in.MessageBody) == "" {
			return toolError("MessageBody must not be empty"), SendTextOutput{}, nil
		}
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "sms_send_text_message"))
		}
		out := SendTextOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions, DryRun: in.DryRun}}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, SendTextOutput{}, nil
		}
		call := buildSendText(in, s, price)
		if in.DryRun {
			out.WouldCall = call
			return nil, out, nil
		}
		resp, err := d.EUM.SendTextMessage(ctx, call)
		if err != nil {
			res := toolError(awsclients.ErrorText(err))
			res.StructuredContent = out
			return res, SendTextOutput{}, nil
		}
		out.MessageID = aws.ToString(resp.MessageId)
		return nil, out, nil
	}
}

func buildSendText(in schemas.SendTextMessageInput, s settings.Settings, price string) *pinpointsmsvoicev2.SendTextMessageInput {
	call := &pinpointsmsvoicev2.SendTextMessageInput{
		DestinationPhoneNumber: aws.String(in.DestinationPhoneNumber),
		OriginationIdentity:    aws.String(s.SendingIdentity()),
		MessageBody:            aws.String(in.MessageBody),
		MaxPrice:               aws.String(price),
		MessageType:            eumtypes.MessageTypeTransactional,
	}
	if strings.EqualFold(in.MessageType, string(eumtypes.MessageTypePromotional)) {
		call.MessageType = eumtypes.MessageTypePromotional
	}
	if s.EUMConfigurationSet != "" {
		call.ConfigurationSetName = aws.String(s.EUMConfigurationSet)
	}
	if s.ProtectConfigurationID != "" {
		call.ProtectConfigurationId = aws.String(s.ProtectConfigurationID)
	}
	if in.TimeToLive > 0 {
		call.TimeToLive = aws.Int32(in.TimeToLive)
	}
	if len(in.Context) > 0 {
		call.Context = in.Context
	}
	return call
}

// SendMediaOutput is the sms_send_media_message result.
type SendMediaOutput struct {
	MessageID      string                                    `json:"MessageId,omitempty"`
	WouldCall      *pinpointsmsvoicev2.SendMediaMessageInput `json:"WouldCall,omitempty"`
	MediaURL       string                                    `json:"MediaUrl,omitempty"`
	ServerMetadata ServerMetadata                            `json:"ServerMetadata"`
}

func (d Deps) sendMediaMessage() mcp.ToolHandlerFor[schemas.SendMediaMessageInput, SendMediaOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.SendMediaMessageInput) (*mcp.CallToolResult, SendMediaOutput, error) {
		if res := requireScope(ctx, "msg/sms:send"); res != nil {
			return res, SendMediaOutput{}, nil
		}
		s := d.Settings
		var result guardrails.Result
		result.Add(guardrails.DestinationCountryUS(in.DestinationPhoneNumber))
		result.Add(guardrails.RecipientsAllowed([]string{in.DestinationPhoneNumber}, s.RecipientAllowList))
		result.Add(guardrails.OriginationAllowed(in.OriginationIdentity, s.OriginationIdentity))
		price, priceDecision := guardrails.MaxPriceCapped(in.MaxPrice, s.SMSMaxPrice)
		result.Add(priceDecision)
		result.Add(guardrails.MediaURLsInBucket(in.MediaUrls, s.MediaBucket))

		var uploadBytes []byte
		if in.MediaUpload != nil {
			raw, err := base64.StdEncoding.DecodeString(in.MediaUpload.Base64Content)
			if err != nil {
				return toolError("MediaUpload.Base64Content is not valid base64"), SendMediaOutput{}, nil
			}
			uploadBytes = raw
			result.Add(guardrails.MediaUploadAllowed(in.MediaUpload.ContentType, len(raw), s.MediaMaxBytes))
		}
		if len(in.MediaUrls) == 0 && in.MediaUpload == nil {
			return toolError("provide MediaUrls or MediaUpload (or use sms_send_text_message for text-only)"), SendMediaOutput{}, nil
		}
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "sms_send_media_message"))
		}
		out := SendMediaOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions, DryRun: in.DryRun}}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, SendMediaOutput{}, nil
		}

		mediaURLs := in.MediaUrls
		if in.MediaUpload != nil {
			key := mediaKey(in.MediaUpload.FileName)
			if !in.DryRun {
				if _, err := d.Media.PutObject(ctx, &s3.PutObjectInput{
					Bucket:      aws.String(s.MediaBucket),
					Key:         aws.String(key),
					Body:        strings.NewReader(string(uploadBytes)),
					ContentType: aws.String(in.MediaUpload.ContentType),
				}); err != nil {
					res := toolError(awsclients.ErrorText(err))
					res.StructuredContent = out
					return res, SendMediaOutput{}, nil
				}
			}
			out.MediaURL = "s3://" + s.MediaBucket + "/" + key
			mediaURLs = append(mediaURLs, out.MediaURL)
		}

		call := buildSendMedia(in, s, price, mediaURLs)
		if in.DryRun {
			out.WouldCall = call
			return nil, out, nil
		}
		resp, err := d.EUM.SendMediaMessage(ctx, call)
		if err != nil {
			res := toolError(awsclients.ErrorText(err))
			res.StructuredContent = out
			return res, SendMediaOutput{}, nil
		}
		out.MessageID = aws.ToString(resp.MessageId)
		return nil, out, nil
	}
}

// mediaKey namespaces uploads and keeps names collision-free without
// depending on wall-clock uniqueness alone.
func mediaKey(fileName string) string {
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	name := path.Base(strings.TrimSpace(fileName))
	if name == "" || name == "." || name == "/" {
		name = "media"
	}
	return fmt.Sprintf("mms/%d-%s-%s", time.Now().UnixMilli(), hex.EncodeToString(suffix[:]), name)
}

func buildSendMedia(in schemas.SendMediaMessageInput, s settings.Settings, price string, mediaURLs []string) *pinpointsmsvoicev2.SendMediaMessageInput {
	call := &pinpointsmsvoicev2.SendMediaMessageInput{
		DestinationPhoneNumber: aws.String(in.DestinationPhoneNumber),
		OriginationIdentity:    aws.String(s.SendingIdentity()),
		MediaUrls:              mediaURLs,
		MaxPrice:               aws.String(price),
	}
	if in.MessageBody != "" {
		call.MessageBody = aws.String(in.MessageBody)
	}
	if s.EUMConfigurationSet != "" {
		call.ConfigurationSetName = aws.String(s.EUMConfigurationSet)
	}
	if s.ProtectConfigurationID != "" {
		call.ProtectConfigurationId = aws.String(s.ProtectConfigurationID)
	}
	if in.TimeToLive > 0 {
		call.TimeToLive = aws.Int32(in.TimeToLive)
	}
	if len(in.Context) > 0 {
		call.Context = in.Context
	}
	return call
}

// PhoneNumber is one entry in the sms_describe_phone_numbers result.
type PhoneNumber struct {
	PhoneNumber        string   `json:"PhoneNumber"`
	PhoneNumberID      string   `json:"PhoneNumberId"`
	Status             string   `json:"Status"`
	NumberType         string   `json:"NumberType"`
	NumberCapabilities []string `json:"NumberCapabilities"`
}

// DescribePhoneNumbersOutput is the sms_describe_phone_numbers result.
type DescribePhoneNumbersOutput struct {
	PhoneNumbers []PhoneNumber `json:"PhoneNumbers"`
}

func (d Deps) describePhoneNumbers() mcp.ToolHandlerFor[schemas.DescribePhoneNumbersInput, DescribePhoneNumbersOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.DescribePhoneNumbersInput) (*mcp.CallToolResult, DescribePhoneNumbersOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, DescribePhoneNumbersOutput{}, nil
		}
		call := &pinpointsmsvoicev2.DescribePhoneNumbersInput{}
		if len(in.PhoneNumberIDs) > 0 {
			call.PhoneNumberIds = in.PhoneNumberIDs
		}
		if in.MaxResults > 0 && in.MaxResults <= 100 {
			call.MaxResults = aws.Int32(in.MaxResults)
		}
		resp, err := d.EUM.DescribePhoneNumbers(ctx, call)
		if err != nil {
			return toolError(awsclients.ErrorText(err)), DescribePhoneNumbersOutput{}, nil
		}
		out := DescribePhoneNumbersOutput{PhoneNumbers: []PhoneNumber{}}
		for _, p := range resp.PhoneNumbers {
			capabilities := make([]string, 0, len(p.NumberCapabilities))
			for _, c := range p.NumberCapabilities {
				capabilities = append(capabilities, string(c))
			}
			out.PhoneNumbers = append(out.PhoneNumbers, PhoneNumber{
				PhoneNumber:        aws.ToString(p.PhoneNumber),
				PhoneNumberID:      aws.ToString(p.PhoneNumberId),
				Status:             string(p.Status),
				NumberType:         string(p.NumberType),
				NumberCapabilities: capabilities,
			})
		}
		return nil, out, nil
	}
}

// MessageEvent is one trail entry for a message.
type MessageEvent struct {
	EventType string `json:"EventType"`
	Timestamp string `json:"Timestamp"`
}

// MessageStatusOutput is the sms_get_message_status result.
type MessageStatusOutput struct {
	MessageID string         `json:"MessageId"`
	Status    string         `json:"Status"`
	Events    []MessageEvent `json:"Events"`
}

// getMessageStatus looks the MessageId up in the stage's EUM event trail
// (PRD M3-3: the API has no per-message read).
func (d Deps) getMessageStatus() mcp.ToolHandlerFor[schemas.GetMessageStatusInput, MessageStatusOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.GetMessageStatusInput) (*mcp.CallToolResult, MessageStatusOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, MessageStatusOutput{}, nil
		}
		id := strings.TrimSpace(in.MessageID)
		if id == "" {
			return toolError("MessageId is required"), MessageStatusOutput{}, nil
		}
		resp, err := d.EventLog.FilterLogEvents(ctx, &cloudwatchlogs.FilterLogEventsInput{
			LogGroupName:  aws.String(d.Settings.EUMEventsLogGroup),
			StartTime:     aws.Int64(time.Now().Add(-72 * time.Hour).UnixMilli()),
			FilterPattern: aws.String(fmt.Sprintf("%q", id)),
		})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), MessageStatusOutput{}, nil
		}
		out := MessageStatusOutput{MessageID: id, Status: "UNKNOWN", Events: []MessageEvent{}}
		for _, event := range resp.Events {
			var entry struct {
				DetailType string `json:"detail-type"`
				Time       string `json:"time"`
				Detail     struct {
					EventType string `json:"eventType"`
				} `json:"detail"`
			}
			if json.Unmarshal([]byte(aws.ToString(event.Message)), &entry) != nil {
				continue
			}
			kind := entry.Detail.EventType
			if kind == "" {
				kind = entry.DetailType
			}
			out.Events = append(out.Events, MessageEvent{EventType: kind, Timestamp: entry.Time})
			out.Status = kind
		}
		if len(out.Events) == 0 {
			out.Status = "NO_EVENTS_YET"
		}
		return nil, out, nil
	}
}
