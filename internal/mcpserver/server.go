// Package mcpserver builds the MCP server and its tools on the official Go
// SDK, served over stateless Streamable HTTP with JSON responses (PRD R3).
package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
)

// Version is stamped into the MCP server implementation info.
const Version = "0.2.0"

// HelloInput is the hello tool's input (mirrors the M1 Python tool).
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

// NewHandler returns the Streamable HTTP handler for a server exposing the
// M1 tool set. Host-header (DNS-rebinding) protection is disabled because
// CloudFront terminates the public hostname; the origin secret and bearer
// auth are the real gate.
func NewHandler(stage string) http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "aws-messaging-mcp", Version: Version}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "hello",
		Description: "Verify the full auth chain end to end; requires msg/read.",
	}, helloHandler(stage))
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true,
	})
}

func helloHandler(stage string) mcp.ToolHandlerFor[HelloInput, HelloOutput] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in HelloInput) (*mcp.CallToolResult, HelloOutput, error) {
		principal, ok := httpapi.PrincipalFrom(ctx)
		if !ok {
			return toolError("no authenticated principal in context"), HelloOutput{}, nil
		}
		if err := auth.RequireScope(principal, "msg/read"); err != nil {
			return toolError(err.Error()), HelloOutput{}, nil
		}
		name := in.Name
		if name == "" {
			name = "world"
		}
		return nil, HelloOutput{
			Message:    "Hello, " + name + "!",
			Stage:      stage,
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
