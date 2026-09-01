// Package mcpserver builds the MCP server and its tools on the official Go
// SDK, served over stateless Streamable HTTP with JSON responses (PRD R3).
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
	"github.com/gesparza8138/aws-messaging-mcp/internal/signing"
)

// Version is stamped into the MCP server implementation info.
const Version = "1.1.0"

// Deps wires the tools to their backends. A nil SES (or EUM) leaves that
// tool family unregistered (tests that only exercise the auth chain use this).
type Deps struct {
	Settings settings.Settings
	SES      awsclients.SES
	EUM      awsclients.EUM
	Media    awsclients.MediaStore
	EventLog awsclients.EventLog
	Files    awsclients.FilesStore
	Presign  awsclients.FilesPresigner
	Metrics  awsclients.MetricReader
	Signer   *signing.Signer
	Limiter  *guardrails.Limiter
}

// HelloInput is the hello tool's input.
type HelloInput struct {
	Name string `json:"name,omitempty" jsonschema:"Whom to greet"`
}

// HelloOutput echoes stage and caller identity so the auth chain is verifiable.
type HelloOutput struct {
	Message    string `json:"message"`
	Stage      string `json:"stage"`
	Caller     string `json:"caller"`
	AuthMethod string `json:"auth_method"`
}

// NewHandler returns the Streamable HTTP handler for the tool set. Host-header
// (DNS-rebinding) protection is disabled because CloudFront terminates the
// public hostname; the origin secret and bearer auth are the real gate.
func NewHandler(d Deps) http.Handler {
	server := NewServer(d)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true,
	})
}

// NewServer builds the MCP server with every tool registered; cmd/gendocs
// connects to it in-memory to render the tool reference pages.
func NewServer(d Deps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "aws-messaging-mcp", Version: Version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "hello",
		Description: "Verify the full auth chain end to end; requires msg/read.",
	}, d.hello())
	if d.SES != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:         "ses_send_email",
			OutputSchema: sendEmailOutputSchema(),
			Description:  "Send an email via Amazon SES (sesv2 SendEmail shape); requires msg/email:send. Supports DryRun. Embed images with Simple attachments (ContentDisposition INLINE plus a ContentId the HTML cites as cid:) rather than hand-building Raw MIME; SES assembles the message itself. An attachment already in the files bucket is attached by key (RawContentKey from files_put_object or files_list_objects) instead of inline bytes; that path also requires msg/read. Raw content and decoded attachments share a 10 MB budget by default, and SES caps the assembled message at 40 MB. Verify payload integrity with ServerMetadata.content_digests (SHA-256 and byte count per attachment, or for the raw message) rather than re-reading the echoed bytes.",
		}, d.sendEmail())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "ses_list_email_identities",
			Description: "List verified SES sender identities; requires msg/read.",
		}, d.listIdentities())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "ses_get_account",
			Description: "SES account status: sandbox/production, quotas; requires msg/read.",
		}, d.getAccount())
	}
	if d.EUM != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "sms_send_text_message",
			Description: "Send an SMS via AWS End User Messaging (SendTextMessage shape); requires msg/sms:send. Supports DryRun.",
		}, d.sendTextMessage())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "sms_send_media_message",
			Description: "Send an MMS with images via AWS End User Messaging (SendMediaMessage shape); requires msg/sms:send. Supports DryRun.",
		}, d.sendMediaMessage())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "sms_describe_phone_numbers",
			Description: "List the origination phone numbers; requires msg/read.",
		}, d.describePhoneNumbers())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "sms_get_message_status",
			Description: "Delivery status for a MessageId from the event trail; requires msg/read.",
		}, d.getMessageStatus())
	}
	if d.Files != nil {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "files_put_object",
			Description: "Upload an inline file (≤4 MB) and get back a CloudFront-signed download link; requires msg/files:write. Supports DryRun.",
		}, d.filesPutObject())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "files_create_upload_url",
			Description: "Presigned PUT URL for large files (≤500 MB, 15-minute validity); sign afterwards with files_create_signed_url; requires msg/files:write.",
		}, d.filesCreateUploadURL())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "files_create_signed_url",
			Description: "Sign (or re-sign) a shared object into a CloudFront download link, optionally IP-restricted; requires msg/files:write.",
		}, d.filesCreateSignedURL())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "files_list_objects",
			Description: "List shared objects with sizes and link expiries; requires msg/read.",
		}, d.filesListObjects())
		mcp.AddTool(server, &mcp.Tool{
			Name:        "files_delete_object",
			Description: "Delete a shared object so its links immediately 403; requires msg/files:write.",
		}, d.filesDeleteObject())
	}
	return server
}

func (d Deps) hello() mcp.ToolHandlerFor[HelloInput, HelloOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in HelloInput) (*mcp.CallToolResult, HelloOutput, error) {
		if res := requireScope(ctx, "msg/read"); res != nil {
			return res, HelloOutput{}, nil
		}
		principal, _ := httpapi.PrincipalFrom(ctx)
		name := in.Name
		if name == "" {
			name = "world"
		}
		return nil, HelloOutput{
			Message:    "Hello, " + name + "!",
			Stage:      d.Settings.Stage,
			Caller:     principal.Subject,
			AuthMethod: principal.Method,
		}, nil
	}
}

// toolError builds an isError result so the model can read the refusal
// instead of the transport failing (PRD 5.1 rule 5).
func toolError(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}

var _ = auth.RequireScope // referenced via requireScope in ses.go
