package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"log"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
)

const expiresAtMetaKey = "expires-at"

// bucketPrefix aligns S3 keys with the public URI: CloudFront forwards the
// full /files/shared/... path to the origin, so objects live under
// files/shared/... while tool-facing keys stay shared/... .
const bucketPrefix = "files/"

// sharedKey builds shared/<random>/<name>; the random segment keeps keys
// unguessable and collision-free (PRD §5.2).
func sharedKey(fileName string) string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	name := path.Base(strings.TrimSpace(fileName))
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	return "shared/" + hex.EncodeToString(buf[:]) + "/" + name
}

func (d Deps) fileURL(key string) string {
	return d.Settings.PublicBaseURL + "/files/" + key
}

// quota reads the last-known bucket size; a metric error fails open with a
// warning decision rather than blocking uploads on a telemetry outage
// (the quota is a lagging backstop either way, M4b-3).
func (d Deps) quota(ctx context.Context, incoming int64) guardrails.Decision {
	size, err := awsclients.BucketSizeBytes(ctx, d.Metrics, d.Settings.FilesBucket)
	if err != nil {
		return guardrails.Decision{Name: "bucket_quota", Allowed: true,
			Reason: "size metric unavailable; quota not evaluated"}
	}
	return guardrails.BucketQuota(size, incoming, d.Settings.FilesQuotaBytes)
}

// PutObjectOutput is the files_put_object result.
type PutObjectOutput struct {
	Key            string             `json:"Key,omitempty"`
	Bucket         string             `json:"Bucket,omitempty"`
	SizeBytes      int64              `json:"SizeBytes,omitempty"`
	SignedURL      string             `json:"SignedUrl,omitempty"`
	ExpiresAt      string             `json:"ExpiresAt,omitempty"`
	WouldCall      *s3.PutObjectInput `json:"WouldCall,omitempty"`
	ServerMetadata ServerMetadata     `json:"ServerMetadata"`
}

func (d Deps) filesPutObject() mcp.ToolHandlerFor[schemas.FilesPutObjectInput, PutObjectOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.FilesPutObjectInput) (*mcp.CallToolResult, PutObjectOutput, error) {
		if res := requireScope(ctx, "msg/files:write"); res != nil {
			return res, PutObjectOutput{}, nil
		}
		s := d.Settings
		body := []byte(in.Body)
		if strings.EqualFold(in.ContentEncoding, "base64") {
			decoded, err := base64.StdEncoding.DecodeString(in.Body)
			if err != nil {
				return toolError("Body is not valid base64"), PutObjectOutput{}, nil
			}
			body = decoded
		}
		var result guardrails.Result
		result.Add(guardrails.ContentTypeAllowed(in.ContentType, s.AllowRiskyContentTypes))
		result.Add(guardrails.SizeWithin("body", int64(len(body)), s.FilesMaxBodyBytes))
		expiry, expiryDecision := guardrails.LinkExpiry(in.ExpiresIn, in.DateLessThan, s.FilesMaxExpiryDays, time.Now())
		result.Add(expiryDecision)
		result.Add(d.quota(ctx, int64(len(body))))
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "files_put_object"))
		}
		out := PutObjectOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions, DryRun: in.DryRun}}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, PutObjectOutput{}, nil
		}

		fileName := in.FileName
		if in.Key != "" {
			fileName = in.Key
		}
		key := sharedKey(fileName)
		disposition := "attachment"
		if strings.EqualFold(in.ContentDisposition, "inline") {
			disposition = "inline"
		}
		metadata := map[string]string{expiresAtMetaKey: expiry.UTC().Format(time.RFC3339)}
		for k, v := range in.Metadata {
			metadata[strings.ToLower(k)] = v
		}
		call := &s3.PutObjectInput{
			Bucket:             aws.String(s.FilesBucket),
			Key:                aws.String(bucketPrefix + key),
			ContentType:        aws.String(in.ContentType),
			ContentDisposition: aws.String(disposition + `; filename="` + path.Base(key) + `"`),
			Metadata:           metadata,
		}
		out.Key, out.Bucket = key, s.FilesBucket
		out.SizeBytes = int64(len(body))
		out.ExpiresAt = metadata[expiresAtMetaKey]
		if in.DryRun {
			out.WouldCall = call
			return nil, out, nil
		}
		call.Body = strings.NewReader(string(body))
		if _, err := d.Files.PutObject(ctx, call); err != nil {
			res := toolError(awsclients.ErrorText(err))
			res.StructuredContent = out
			return res, PutObjectOutput{}, nil
		}
		signed, err := d.Signer.SignedURL(d.fileURL(key), expiry, "")
		if err != nil {
			res := toolError("uploaded, but signing failed: " + err.Error())
			res.StructuredContent = out
			return res, PutObjectOutput{}, nil
		}
		out.SignedURL = signed
		return nil, out, nil
	}
}

// UploadURLOutput is the files_create_upload_url result.
type UploadURLOutput struct {
	Key             string            `json:"Key"`
	UploadURL       string            `json:"UploadUrl"`
	UploadExpiresAt string            `json:"UploadExpiresAt"`
	RequiredHeaders map[string]string `json:"RequiredHeaders"`
	NextStep        string            `json:"NextStep"`
	ServerMetadata  ServerMetadata    `json:"ServerMetadata"`
}

func (d Deps) filesCreateUploadURL() mcp.ToolHandlerFor[schemas.FilesCreateUploadURLInput, UploadURLOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.FilesCreateUploadURLInput) (*mcp.CallToolResult, UploadURLOutput, error) {
		if res := requireScope(ctx, "msg/files:write"); res != nil {
			return res, UploadURLOutput{}, nil
		}
		s := d.Settings
		var result guardrails.Result
		result.Add(guardrails.ContentTypeAllowed(in.ContentType, s.AllowRiskyContentTypes))
		result.Add(guardrails.SizeWithin("upload", in.ContentLength, s.FilesMaxUploadBytes))
		result.Add(d.quota(ctx, in.ContentLength))
		if d.Limiter != nil {
			result.Add(d.Limiter.Check(ctx, "files_create_upload_url"))
		}
		out := UploadURLOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions}}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, UploadURLOutput{}, nil
		}
		key := sharedKey(in.FileName)
		expires := 15 * time.Minute
		presigned, err := d.Presign.PresignPutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.FilesBucket),
			Key:           aws.String(bucketPrefix + key),
			ContentType:   aws.String(in.ContentType),
			ContentLength: aws.Int64(in.ContentLength),
		}, func(o *s3.PresignOptions) { o.Expires = expires })
		if err != nil {
			return toolError(awsclients.ErrorText(err)), UploadURLOutput{}, nil
		}
		out.Key = key
		out.UploadURL = presigned.URL
		out.UploadExpiresAt = time.Now().Add(expires).UTC().Format(time.RFC3339)
		out.RequiredHeaders = map[string]string{
			"Content-Type":   in.ContentType,
			"Content-Length": aws.ToString(aws.String(itoa64(in.ContentLength))),
		}
		out.NextStep = "files_create_signed_url"
		return nil, out, nil
	}
}

func itoa64(v int64) string {
	if v <= 0 {
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

// SignedURLOutput is the files_create_signed_url result.
type SignedURLOutput struct {
	SignedURL      string         `json:"SignedUrl,omitempty"`
	ExpiresAt      string         `json:"ExpiresAt,omitempty"`
	PolicyType     string         `json:"PolicyType,omitempty"`
	ServerMetadata ServerMetadata `json:"ServerMetadata"`
}

func (d Deps) filesCreateSignedURL() mcp.ToolHandlerFor[schemas.FilesCreateSignedURLInput, SignedURLOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.FilesCreateSignedURLInput) (*mcp.CallToolResult, SignedURLOutput, error) {
		if res := requireScope(ctx, "msg/files:write"); res != nil {
			return res, SignedURLOutput{}, nil
		}
		s := d.Settings
		if !strings.HasPrefix(in.Key, "shared/") {
			return toolError("Key must be under shared/"), SignedURLOutput{}, nil
		}
		var result guardrails.Result
		expiry, expiryDecision := guardrails.LinkExpiry(in.ExpiresIn, in.DateLessThan, s.FilesMaxExpiryDays, time.Now())
		result.Add(expiryDecision)
		out := SignedURLOutput{ServerMetadata: ServerMetadata{Guardrails: result.Decisions}}
		if blocked, isBlocked := result.Blocked(); isBlocked {
			res := toolError("blocked by guardrail " + blocked.Name + ": " + blocked.Reason)
			res.StructuredContent = out
			return res, SignedURLOutput{}, nil
		}
		head, err := d.Files.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(s.FilesBucket), Key: aws.String(bucketPrefix + in.Key),
		})
		if err != nil {
			return toolError("object not found: " + awsclients.ErrorText(err)), SignedURLOutput{}, nil
		}
		// Re-signing keeps the cleanup job honest: bump expires-at to the
		// later of the stored and requested expiry (PRD §5.3).
		if stored, parseErr := time.Parse(time.RFC3339, head.Metadata[expiresAtMetaKey]); parseErr != nil || expiry.After(stored) {
			metadata := map[string]string{}
			for k, v := range head.Metadata {
				metadata[k] = v
			}
			metadata[expiresAtMetaKey] = expiry.UTC().Format(time.RFC3339)
			if _, err := d.Files.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:             aws.String(s.FilesBucket),
				Key:                aws.String(bucketPrefix + in.Key),
				CopySource:         aws.String(s.FilesBucket + "/" + bucketPrefix + in.Key),
				MetadataDirective:  "REPLACE",
				Metadata:           metadata,
				ContentType:        head.ContentType,
				ContentDisposition: head.ContentDisposition,
			}); err != nil {
				return toolError("could not extend the object expiry: " + awsclients.ErrorText(err)), SignedURLOutput{}, nil
			}
		}
		signed, err := d.Signer.SignedURL(d.fileURL(in.Key), expiry, in.IPAddress)
		if err != nil {
			return toolError(err.Error()), SignedURLOutput{}, nil
		}
		out.SignedURL = signed
		out.ExpiresAt = expiry.UTC().Format(time.RFC3339)
		out.PolicyType = "canned"
		if in.IPAddress != "" {
			out.PolicyType = "custom"
		}
		return nil, out, nil
	}
}

// FileEntry is one row of files_list_objects.
type FileEntry struct {
	Key          string `json:"Key"`
	SizeBytes    int64  `json:"SizeBytes"`
	LastModified string `json:"LastModified"`
	ExpiresAt    string `json:"ExpiresAt,omitempty"`
}

// ListFilesOutput is the files_list_objects result.
type ListFilesOutput struct {
	Objects []FileEntry `json:"Objects"`
}

func (d Deps) filesListObjects() mcp.ToolHandlerFor[schemas.FilesListObjectsInput, ListFilesOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.FilesListObjectsInput) (*mcp.CallToolResult, ListFilesOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, ListFilesOutput{}, nil
		}
		maxKeys := in.MaxKeys
		if maxKeys <= 0 || maxKeys > 1000 {
			maxKeys = 100
		}
		resp, err := d.Files.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:  aws.String(d.Settings.FilesBucket),
			Prefix:  aws.String(bucketPrefix + "shared/"),
			MaxKeys: aws.Int32(maxKeys),
		})
		if err != nil {
			return toolError(awsclients.ErrorText(err)), ListFilesOutput{}, nil
		}
		out := ListFilesOutput{Objects: []FileEntry{}}
		for _, obj := range resp.Contents {
			entry := FileEntry{
				Key:          strings.TrimPrefix(aws.ToString(obj.Key), bucketPrefix),
				SizeBytes:    aws.ToInt64(obj.Size),
				LastModified: aws.ToTime(obj.LastModified).UTC().Format(time.RFC3339),
			}
			if head, err := d.Files.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(d.Settings.FilesBucket), Key: obj.Key,
			}); err == nil {
				entry.ExpiresAt = head.Metadata[expiresAtMetaKey]
			}
			out.Objects = append(out.Objects, entry)
		}
		return nil, out, nil
	}
}

// DeleteFileOutput is the files_delete_object result.
type DeleteFileOutput struct {
	Key     string `json:"Key"`
	Deleted bool   `json:"Deleted"`
}

func (d Deps) filesDeleteObject() mcp.ToolHandlerFor[schemas.FilesDeleteObjectInput, DeleteFileOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in schemas.FilesDeleteObjectInput) (*mcp.CallToolResult, DeleteFileOutput, error) {
		if res := requireScope(ctx, "msg/files:write"); res != nil {
			return res, DeleteFileOutput{}, nil
		}
		if !strings.HasPrefix(in.Key, "shared/") {
			return toolError("Key must be under shared/"), DeleteFileOutput{}, nil
		}
		if _, err := d.Files.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(d.Settings.FilesBucket), Key: aws.String(bucketPrefix + in.Key),
		}); err != nil {
			return toolError(awsclients.ErrorText(err)), DeleteFileOutput{}, nil
		}
		return nil, DeleteFileOutput{Key: in.Key, Deleted: true}, nil
	}
}

// CleanupFiles deletes shared objects whose expires-at has passed; the daily
// scheduler invokes it via the task mux (M4b-1).
func (d Deps) CleanupFiles(ctx context.Context) error {
	var token *string
	deleted := 0
	for {
		page, err := d.Files.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(d.Settings.FilesBucket),
			Prefix:            aws.String(bucketPrefix + "shared/"),
			ContinuationToken: token,
		})
		if err != nil {
			return err
		}
		for _, obj := range page.Contents {
			head, err := d.Files.HeadObject(ctx, &s3.HeadObjectInput{
				Bucket: aws.String(d.Settings.FilesBucket), Key: obj.Key,
			})
			if err != nil {
				continue
			}
			expires, err := time.Parse(time.RFC3339, head.Metadata[expiresAtMetaKey])
			if err != nil || expires.After(time.Now()) {
				continue
			}
			if _, err := d.Files.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(d.Settings.FilesBucket), Key: obj.Key,
			}); err == nil {
				deleted++
			}
		}
		if page.NextContinuationToken == nil {
			break
		}
		token = page.NextContinuationToken
	}
	log.Printf("files-cleanup: deleted %d expired object(s)", deleted)
	return nil
}
