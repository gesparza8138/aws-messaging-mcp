// Package awsclients narrows the AWS SDK clients to the operations the tools
// use, behind interfaces so tests substitute fakes, and maps API errors to
// tool-readable text (PRD 5.1 rule 5).
package awsclients

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/smithy-go"
)

// SES is the subset of sesv2 the email tools call.
type SES interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
	ListEmailIdentities(ctx context.Context, in *sesv2.ListEmailIdentitiesInput, opts ...func(*sesv2.Options)) (*sesv2.ListEmailIdentitiesOutput, error)
	GetAccount(ctx context.Context, in *sesv2.GetAccountInput, opts ...func(*sesv2.Options)) (*sesv2.GetAccountOutput, error)
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
