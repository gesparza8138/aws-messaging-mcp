package awsclients

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

type apiError struct{}

func (*apiError) Error() string                 { return "boom" }
func (*apiError) ErrorCode() string             { return "Throttling" }
func (*apiError) ErrorMessage() string          { return "rate exceeded" }
func (*apiError) ErrorFault() smithy.ErrorFault { return smithy.FaultClient }

func TestErrorText(t *testing.T) {
	if got := ErrorText(&apiError{}); got != "Throttling: rate exceeded" {
		t.Fatalf("api error: %q", got)
	}
	if got := ErrorText(errors.New("plain")); got != "plain" {
		t.Fatalf("plain error: %q", got)
	}
}

type fakeDDB struct {
	in  *dynamodb.UpdateItemInput
	out *dynamodb.UpdateItemOutput
	err error
}

func (f *fakeDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	f.in = in
	return f.out, f.err
}

func TestIncrementWindow(t *testing.T) {
	ddb := &fakeDDB{out: &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{
		"count": &ddbtypes.AttributeValueMemberN{Value: "42"},
	}}}
	store := &DynamoCounters{Client: ddb, Table: "counters"}
	n, err := store.IncrementWindow(context.Background(), "tool#hour#x", time.Now(), time.Hour)
	if err != nil || n != 42 {
		t.Fatalf("count: %d err: %v", n, err)
	}
	if key := ddb.in.Key["pk"].(*ddbtypes.AttributeValueMemberS).Value; key != "tool#hour#x" {
		t.Fatalf("key: %q", key)
	}

	ddb.err = errors.New("ddb down")
	if _, err := store.IncrementWindow(context.Background(), "k", time.Now(), time.Hour); err == nil {
		t.Fatal("client error must propagate")
	}
	ddb.err = nil
	ddb.out = &dynamodb.UpdateItemOutput{Attributes: map[string]ddbtypes.AttributeValue{}}
	if _, err := store.IncrementWindow(context.Background(), "k", time.Now(), time.Hour); err == nil {
		t.Fatal("missing counter attribute must error")
	}
}

func TestItoa(t *testing.T) {
	for v, want := range map[int64]string{0: "0", 7: "7", 1787450000: "1787450000"} {
		if got := itoa(v); got != want {
			t.Fatalf("itoa(%d) = %q", v, got)
		}
	}
}
