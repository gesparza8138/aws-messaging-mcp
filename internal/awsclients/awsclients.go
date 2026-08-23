// Package awsclients narrows the AWS SDK clients to the operations the tools
// use, behind interfaces so tests substitute fakes, and maps API errors to
// tool-readable text (PRD 5.1 rule 5).
package awsclients

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/smithy-go"
)

// SES is the subset of sesv2 the email tools call.
type SES interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
	ListEmailIdentities(ctx context.Context, in *sesv2.ListEmailIdentitiesInput, opts ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error)
	GetAccount(ctx context.Context, in *sesv2.GetAccountInput, opts ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
}

// EUM is the subset of pinpoint-sms-voice-v2 the SMS/MMS tools call.
type EUM interface {
	SendTextMessage(ctx context.Context, in *pinpointsmsvoicev2.SendTextMessageInput, opts ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.SendTextMessageOutput, error)
	SendMediaMessage(ctx context.Context, in *pinpointsmsvoicev2.SendMediaMessageInput, opts ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.SendMediaMessageOutput, error)
	DescribePhoneNumbers(ctx context.Context, in *pinpointsmsvoicev2.DescribePhoneNumbersInput, opts ...func(*pinpointsmsvoicev2.Options)) (*pinpointsmsvoicev2.DescribePhoneNumbersOutput, error)
}

// MediaStore uploads inline MMS attachments to the media bucket.
type MediaStore interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// EventLog reads the stage's EUM event trail (sms_get_message_status).
type EventLog interface {
	FilterLogEvents(ctx context.Context, in *cloudwatchlogs.FilterLogEventsInput, opts ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error)
}

// ErrorText renders an AWS API error as "Code: Message" for tool errors,
// preserving both fields per PRD 5.1; other errors render as-is.
func ErrorText(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() + ": " + apiErr.ErrorMessage()
	}
	return err.Error()
}

// DynamoCounters implements guardrails.CounterStore on a DynamoDB table with
// partition key "pk" (S) and TTL attribute "expires_at".
type DynamoCounters struct {
	Client interface {
		UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	}
	Table string
}

// IncrementWindow atomically increments the window counter and returns it.
func (d *DynamoCounters) IncrementWindow(ctx context.Context, key string, _ time.Time, ttl time.Duration) (int, error) {
	expires := time.Now().Add(ttl).Unix()
	out, err := d.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(d.Table),
		Key: map[string]ddbtypes.AttributeValue{
			"pk": &ddbtypes.AttributeValueMemberS{Value: key},
		},
		UpdateExpression: aws.String("ADD #c :one SET expires_at = if_not_exists(expires_at, :exp)"),
		ExpressionAttributeNames: map[string]string{
			"#c": "count",
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one": &ddbtypes.AttributeValueMemberN{Value: "1"},
			":exp": &ddbtypes.AttributeValueMemberN{Value: aws.ToString(aws.String(itoa(expires)))},
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, err
	}
	if n, ok := out.Attributes["count"].(*ddbtypes.AttributeValueMemberN); ok {
		var count int
		for _, c := range n.Value {
			count = count*10 + int(c-'0')
		}
		return count, nil
	}
	return 0, errors.New("counter attribute missing from UpdateItem response")
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
