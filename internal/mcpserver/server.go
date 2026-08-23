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
)

// Version is stamped into the MCP server implementation info.
const Version = "0.2.0"

// Deps wires the tools to their backends. A nil SES leaves the email tools
// unregistered (tests that only exercise the auth chain use this).
type Deps struct {
	Settings settings.Settings
	SES      awsclients.SES
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
			Name:        "ses_send_email",
			Description: "Send an email via Amazon SES (sesv2 SendEmail shape); requires msg/email:send. Supports DryRun.",
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
