package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/gesparza8138/aws-messaging-mcp/internal/schemas"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
	"github.com/gesparza8138/aws-messaging-mcp/internal/signing"
)

type fakeFiles struct {
	putIn    *s3.PutObjectInput
	putErr   error
	headMeta map[string]string
	headErr  error
	copyIn   *s3.CopyObjectInput
	copyErr  error
	delIn    *s3.DeleteObjectInput
	delErr   error
	listKeys []string
	listErr  error
}

func (f *fakeFiles) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putIn = in
	return &s3.PutObjectOutput{ETag: aws.String("etag")}, f.putErr
}

func (f *fakeFiles) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if f.headErr != nil {
		return nil, f.headErr
	}
	return &s3.HeadObjectOutput{Metadata: f.headMeta, ContentType: aws.String("application/pdf")}, nil
}

func (f *fakeFiles) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	f.copyIn = in
	return &s3.CopyObjectOutput{}, f.copyErr
}

func (f *fakeFiles) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.delIn = in
	return &s3.DeleteObjectOutput{}, f.delErr
}

func (f *fakeFiles) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := &s3.ListObjectsV2Output{}
	now := time.Now()
	for _, k := range f.listKeys {
		out.Contents = append(out.Contents, s3object(k, now))
	}
	return out, nil
}

func s3object(key string, modified time.Time) s3types.Object {
	return s3types.Object{Key: aws.String(key), Size: aws.Int64(11), LastModified: aws.Time(modified)}
}

type fakePresign struct{ err error }

func (f *fakePresign) PresignPutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.PresignOptions)) (*v4.PresignedHTTPRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &v4.PresignedHTTPRequest{URL: "https://bucket.s3.amazonaws.com/" + aws.ToString(in.Key) + "?sig=x"}, nil
}

type fakeMetrics struct {
	bytes float64
	err   error
}

func (f *fakeMetrics) GetMetricData(context.Context, *cloudwatch.GetMetricDataInput, ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []cwtypes.MetricDataResult{{Values: []float64{f.bytes}}}}, nil
}

func filesDeps(t *testing.T, files *fakeFiles, presign *fakePresign, metrics *fakeMetrics) Deps {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{
		Settings: settings.Settings{
			Stage:               "test",
			PublicBaseURL:       "https://mcp.example.com",
			FilesBucket:         "files-bucket",
			FilesKeyPairID:      "KTEST",
			FilesMaxExpiryDays:  365,
			FilesMaxBodyBytes:   1024,
			FilesMaxUploadBytes: 4096,
			FilesQuotaBytes:     1 << 20,
		},
		Files:   files,
		Presign: presign,
		Metrics: metrics,
		Signer:  &signing.Signer{KeyPairID: "KTEST", PrivateKey: key},
	}
}

func putInput(dryRun bool) schemas.FilesPutObjectInput {
	return schemas.FilesPutObjectInput{
		FileName:    "report.pdf",
		ContentType: "application/pdf",
		Body:        "hello world",
		ExpiresIn:   "P3D",
		DryRun:      dryRun,
	}
}

func TestFilesPutObjectDryRun(t *testing.T) {
	files := &fakeFiles{}
	d := filesDeps(t, files, nil, &fakeMetrics{bytes: 100})
	res, out, err := d.filesPutObject()(authedCtx("msg/files:write"), nil, putInput(true))
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if files.putIn != nil {
		t.Fatal("DryRun must not upload")
	}
	if out.WouldCall == nil || !strings.HasPrefix(out.Key, "shared/") || !strings.HasSuffix(out.Key, "/report.pdf") {
		t.Fatalf("output: %+v", out)
	}
	if out.WouldCall.Metadata["expires-at"] == "" || out.SignedURL != "" {
		t.Fatalf("dry run shape: %+v", out.WouldCall)
	}
}

func TestFilesPutObjectRealSigns(t *testing.T) {
	files := &fakeFiles{}
	d := filesDeps(t, files, nil, &fakeMetrics{bytes: 100})
	res, out, err := d.filesPutObject()(authedCtx("msg/files:write"), nil, putInput(false))
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if files.putIn == nil || aws.ToString(files.putIn.ContentType) != "application/pdf" {
		t.Fatalf("upload input: %+v", files.putIn)
	}
	if !strings.Contains(out.SignedURL, "https://mcp.example.com/files/shared/") ||
		!strings.Contains(out.SignedURL, "Key-Pair-Id=KTEST") || !strings.Contains(out.SignedURL, "Expires=") {
		t.Fatalf("signed url: %s", out.SignedURL)
	}
	if !strings.Contains(aws.ToString(files.putIn.ContentDisposition), "attachment") {
		t.Fatalf("disposition: %v", files.putIn.ContentDisposition)
	}
}

func TestFilesPutObjectGuardrails(t *testing.T) {
	d := filesDeps(t, &fakeFiles{}, nil, &fakeMetrics{bytes: 100})
	cases := map[string]schemas.FilesPutObjectInput{
		"risky-type": {FileName: "x.html", ContentType: "text/html", Body: "x", ExpiresIn: "P3D"},
		"both-expiry": {FileName: "a", ContentType: "text/plain", Body: "x",
			ExpiresIn: "P3D", DateLessThan: "2030-01-01T00:00:00Z"},
		"too-long": {FileName: "a", ContentType: "text/plain", Body: "x", ExpiresIn: "P400D"},
		"too-big":  {FileName: "a", ContentType: "text/plain", Body: strings.Repeat("x", 2048), ExpiresIn: "P1D"},
	}
	for name, in := range cases {
		res, _, _ := d.filesPutObject()(authedCtx("msg/files:write"), nil, in)
		if res == nil || !res.IsError || !strings.Contains(text(t, res), "blocked by guardrail") {
			t.Fatalf("%s: expected block", name)
		}
	}
	bad := putInput(false)
	bad.ContentEncoding = "base64"
	bad.Body = "!!!"
	if res, _, _ := d.filesPutObject()(authedCtx("msg/files:write"), nil, bad); res == nil || !strings.Contains(text(t, res), "base64") {
		t.Fatal("bad base64 accepted")
	}
	over := filesDeps(t, &fakeFiles{}, nil, &fakeMetrics{bytes: float64(1 << 21)})
	if res, _, _ := over.filesPutObject()(authedCtx("msg/files:write"), nil, putInput(false)); res == nil || !strings.Contains(text(t, res), "bucket_quota") {
		t.Fatal("quota not enforced")
	}
	// metric outage fails open
	open := filesDeps(t, &fakeFiles{}, nil, &fakeMetrics{err: errors.New("cw down")})
	if res, _, _ := open.filesPutObject()(authedCtx("msg/files:write"), nil, putInput(true)); res != nil {
		t.Fatalf("metric outage must fail open: %s", text(t, res))
	}
	if res, _, _ := d.filesPutObject()(authedCtx("msg/read"), nil, putInput(false)); res == nil || !res.IsError {
		t.Fatal("missing scope accepted")
	}
	failing := filesDeps(t, &fakeFiles{putErr: errors.New("s3 down")}, nil, &fakeMetrics{bytes: 1})
	if res, _, _ := failing.filesPutObject()(authedCtx("msg/files:write"), nil, putInput(false)); res == nil || !res.IsError {
		t.Fatal("put failure not surfaced")
	}
}

func TestFilesCreateUploadURL(t *testing.T) {
	d := filesDeps(t, &fakeFiles{}, &fakePresign{}, &fakeMetrics{bytes: 1})
	in := schemas.FilesCreateUploadURLInput{FileName: "video.mp4", ContentType: "video/mp4", ContentLength: 2048}
	res, out, err := d.filesCreateUploadURL()(authedCtx("msg/files:write"), nil, in)
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if !strings.Contains(out.UploadURL, out.Key) || out.RequiredHeaders["Content-Length"] != "2048" ||
		out.NextStep != "files_create_signed_url" {
		t.Fatalf("output: %+v", out)
	}
	in.ContentLength = 1 << 20
	if res, _, _ := d.filesCreateUploadURL()(authedCtx("msg/files:write"), nil, in); res == nil || !res.IsError {
		t.Fatal("oversize upload accepted")
	}
	failing := filesDeps(t, &fakeFiles{}, &fakePresign{err: errors.New("no")}, &fakeMetrics{bytes: 1})
	in.ContentLength = 10
	if res, _, _ := failing.filesCreateUploadURL()(authedCtx("msg/files:write"), nil, in); res == nil || !res.IsError {
		t.Fatal("presign failure not surfaced")
	}
}

func TestFilesCreateSignedURL(t *testing.T) {
	old := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	files := &fakeFiles{headMeta: map[string]string{"expires-at": old}}
	d := filesDeps(t, files, nil, nil)
	in := schemas.FilesCreateSignedURLInput{Key: "shared/ab/report.pdf", ExpiresIn: "P30D"}
	res, out, err := d.filesCreateSignedURL()(authedCtx("msg/files:write"), nil, in)
	if err != nil || res != nil {
		t.Fatalf("unexpected: res=%v err=%v", res, err)
	}
	if out.PolicyType != "canned" || files.copyIn == nil ||
		files.copyIn.Metadata["expires-at"] == old {
		t.Fatalf("expiry not extended: %+v", files.copyIn)
	}
	// shorter re-sign must NOT shrink the stored expiry
	files.copyIn = nil
	far := time.Now().Add(300 * 24 * time.Hour).UTC().Format(time.RFC3339)
	files.headMeta = map[string]string{"expires-at": far}
	if _, _, err := d.filesCreateSignedURL()(authedCtx("msg/files:write"), nil, in); err != nil {
		t.Fatal(err)
	}
	if files.copyIn != nil {
		t.Fatal("stored expiry must not shrink")
	}
	ip := in
	ip.IPAddress = "203.0.113.0/24"
	_, out, _ = d.filesCreateSignedURL()(authedCtx("msg/files:write"), nil, ip)
	if out.PolicyType != "custom" || !strings.Contains(out.SignedURL, "Policy=") {
		t.Fatalf("custom policy: %+v", out)
	}
	if res, _, _ := d.filesCreateSignedURL()(authedCtx("msg/files:write"), nil, schemas.FilesCreateSignedURLInput{Key: "outside/x", ExpiresIn: "P1D"}); res == nil || !res.IsError {
		t.Fatal("non-shared key accepted")
	}
	missing := filesDeps(t, &fakeFiles{headErr: errors.New("404")}, nil, nil)
	if res, _, _ := missing.filesCreateSignedURL()(authedCtx("msg/files:write"), nil, in); res == nil || !strings.Contains(text(t, res), "not found") {
		t.Fatal("missing object accepted")
	}
}

func TestFilesListAndDelete(t *testing.T) {
	files := &fakeFiles{listKeys: []string{"files/shared/a/x.pdf"}, headMeta: map[string]string{"expires-at": "2027-01-01T00:00:00Z"}}
	d := filesDeps(t, files, nil, nil)
	res, out, err := d.filesListObjects()(authedCtx("msg/read"), nil, schemas.FilesListObjectsInput{})
	if err != nil || res != nil || len(out.Objects) != 1 || out.Objects[0].ExpiresAt == "" {
		t.Fatalf("list: res=%v err=%v out=%+v", res, err, out)
	}
	dres, dout, err := d.filesDeleteObject()(authedCtx("msg/files:write"), nil, schemas.FilesDeleteObjectInput{Key: "shared/a/x.pdf"})
	if err != nil || dres != nil || !dout.Deleted || aws.ToString(files.delIn.Key) != "files/shared/a/x.pdf" {
		t.Fatalf("delete: %+v %+v", dres, dout)
	}
	if res, _, _ := d.filesDeleteObject()(authedCtx("msg/files:write"), nil, schemas.FilesDeleteObjectInput{Key: "secret/x"}); res == nil || !res.IsError {
		t.Fatal("non-shared delete accepted")
	}
	failing := filesDeps(t, &fakeFiles{listErr: errors.New("x")}, nil, nil)
	if res, _, _ := failing.filesListObjects()(authedCtx("msg/read"), nil, schemas.FilesListObjectsInput{}); res == nil || !res.IsError {
		t.Fatal("list failure not surfaced")
	}
}

func TestCleanupFiles(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	files := &fakeFiles{listKeys: []string{"files/shared/a/old.pdf"}, headMeta: map[string]string{"expires-at": past}}
	d := filesDeps(t, files, nil, nil)
	if err := d.CleanupFiles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if files.delIn == nil || aws.ToString(files.delIn.Key) != "files/shared/a/old.pdf" {
		t.Fatalf("expired object not deleted: %+v", files.delIn)
	}
	files.delIn = nil
	files.headMeta = map[string]string{"expires-at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	if err := d.CleanupFiles(context.Background()); err != nil || files.delIn != nil {
		t.Fatalf("live object deleted: %v %+v", err, files.delIn)
	}
	if err := filesDeps(t, &fakeFiles{listErr: errors.New("x")}, nil, nil).CleanupFiles(context.Background()); err == nil {
		t.Fatal("list error not propagated")
	}
}
