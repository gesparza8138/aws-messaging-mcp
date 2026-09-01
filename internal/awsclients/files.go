package awsclients

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// FilesStore is the s3 subset the files tools call on the files bucket.
type FilesStore interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	// GetObject reads a shared object back; ses_send_email uses it to attach
	// an object by key instead of inline base64 (PRD 5.1 rule 7).
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	CopyObject(ctx context.Context, in *s3.CopyObjectInput, opts ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// FilesPresigner mints the presigned PUT for files_create_upload_url.
type FilesPresigner interface {
	PresignPutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error)
}

// MetricReader reads CloudWatch metrics (the bucket-size quota backstop).
type MetricReader interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
}

// BucketSizeBytes returns the latest BucketSizeBytes datapoint for bucket.
// The metric is daily and lags up to ~24 h (M4b-3); zero with no error means
// no datapoint yet (empty or brand-new bucket).
func BucketSizeBytes(ctx context.Context, cw MetricReader, bucket string) (int64, error) {
	if cw == nil {
		return 0, errors.New("metric reader not configured")
	}
	now := time.Now()
	out, err := cw.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(now.Add(-48 * time.Hour)),
		EndTime:   aws.Time(now),
		MetricDataQueries: []cwtypes.MetricDataQuery{{
			Id: aws.String("size"),
			MetricStat: &cwtypes.MetricStat{
				Period: aws.Int32(86400),
				Stat:   aws.String("Maximum"),
				Metric: &cwtypes.Metric{
					Namespace:  aws.String("AWS/S3"),
					MetricName: aws.String("BucketSizeBytes"),
					Dimensions: []cwtypes.Dimension{
						{Name: aws.String("BucketName"), Value: aws.String(bucket)},
						{Name: aws.String("StorageType"), Value: aws.String("StandardStorage")},
					},
				},
			},
		}},
	})
	if err != nil {
		return 0, err
	}
	for _, r := range out.MetricDataResults {
		if len(r.Values) > 0 {
			return int64(r.Values[len(r.Values)-1]), nil
		}
	}
	return 0, nil
}
