package schemas

// FilesPutObjectInput mirrors the caller-controlled subset of s3 PutObject
// for the upload half of files_put_object (PRD §5.3); the signing half's
// fields (ExpiresIn/DateLessThan) and the conveniences (FileName, DryRun,
// ContentEncoding as a base64 switch) are tool-only. Bucket is never a
// parameter: the server owns the files bucket.
type FilesPutObjectInput struct {
	Key                string            `json:"Key,omitempty" jsonschema:"Optional object name; the server always prefixes shared/<uuid>/ and defaults to FileName"`
	FileName           string            `json:"FileName" jsonschema:"Name the download should carry, e.g. report.pdf"`
	ContentType        string            `json:"ContentType" jsonschema:"MIME type; text/html and executables are refused"`
	Body               string            `json:"Body" jsonschema:"UTF-8 text, or base64 when ContentEncoding is base64; at most 4 MB decoded"`
	ContentEncoding    string            `json:"ContentEncoding,omitempty" jsonschema:"base64 or identity (default identity)"`
	ContentDisposition string            `json:"ContentDisposition,omitempty" jsonschema:"attachment (default) or inline"`
	Metadata           map[string]string `json:"Metadata,omitempty" jsonschema:"Extra object metadata"`
	ExpiresIn          string            `json:"ExpiresIn,omitempty" jsonschema:"ISO-8601 duration for the link, e.g. P3D; exactly one of ExpiresIn/DateLessThan"`
	DateLessThan       string            `json:"DateLessThan,omitempty" jsonschema:"Absolute RFC 3339 link expiry"`
	DryRun             bool              `json:"DryRun,omitempty" jsonschema:"Validate and run guardrails, return the would-be call without uploading"`
}

// FilesCreateUploadURLInput is the presigned-PUT request (tool shape; the
// presigner has no public input struct to mirror).
type FilesCreateUploadURLInput struct {
	FileName      string `json:"FileName" jsonschema:"Object name, e.g. video.mp4"`
	ContentType   string `json:"ContentType" jsonschema:"MIME type the PUT must use"`
	ContentLength int64  `json:"ContentLength" jsonschema:"Exact byte size the PUT must send"`
}

// FilesCreateSignedURLInput signs (or re-signs) an existing object.
type FilesCreateSignedURLInput struct {
	Key          string `json:"Key" jsonschema:"Object key, e.g. shared/<uuid>/video.mp4"`
	ExpiresIn    string `json:"ExpiresIn,omitempty" jsonschema:"ISO-8601 duration; exactly one of ExpiresIn/DateLessThan"`
	DateLessThan string `json:"DateLessThan,omitempty" jsonschema:"Absolute RFC 3339 expiry"`
	IPAddress    string `json:"IpAddress,omitempty" jsonschema:"Optional CIDR; produces a custom policy restricted to it"`
}

// FilesListObjectsInput lists the shared objects.
type FilesListObjectsInput struct {
	MaxKeys int32 `json:"MaxKeys,omitempty" jsonschema:"Page size, 1-1000 (default 100)"`
}

// FilesDeleteObjectInput revokes an object (its signed URLs then 403).
type FilesDeleteObjectInput struct {
	Key string `json:"Key" jsonschema:"Object key to delete"`
}
