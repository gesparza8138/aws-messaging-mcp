// Command server is the MCP server. On Lambda (AWS_LAMBDA_RUNTIME_API set)
// it serves Function URL events; otherwise it listens on --listen for local
// development (`make dev`).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"

	"github.com/gesparza8138/aws-messaging-mcp/internal/awsclients"
	"github.com/gesparza8138/aws-messaging-mcp/internal/guardrails"

	"github.com/gesparza8138/aws-messaging-mcp/internal/auth"
	"github.com/gesparza8138/aws-messaging-mcp/internal/httpapi"
	"github.com/gesparza8138/aws-messaging-mcp/internal/lambdaadapter"
	"github.com/gesparza8138/aws-messaging-mcp/internal/mcpserver"
	"github.com/gesparza8138/aws-messaging-mcp/internal/settings"
)

func main() {
	listen := flag.String("listen", ":8000", "local listen address (ignored on Lambda)")
	flag.Parse()

	ctx := context.Background()
	s, err := settings.ResolveOriginSecret(ctx, settings.FromEnv(os.LookupEnv), os.LookupEnv, fetchParameter)
	if err != nil {
		log.Fatalf("settings: %v", err)
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.AWSRegion))
	if err != nil {
		log.Fatalf("aws config: %v", err)
	}
	deps := mcpserver.Deps{
		Settings: s,
		SES:      sesv2.NewFromConfig(awsCfg),
	}
	if s.RateLimitTable != "" {
		deps.Limiter = &guardrails.Limiter{
			Store:   &awsclients.DynamoCounters{Client: dynamodb.NewFromConfig(awsCfg), Table: s.RateLimitTable},
			PerHour: s.RateLimitPerHour,
			PerDay:  s.RateLimitPerDay,
		}
	}
	handler := httpapi.NewHandler(httpapi.Config{
		Settings: s,
		Verifier: auth.NewVerifier(s.CognitoIssuer, s.AllowedClientIDs, auth.NewJWKSProvider(s.JWKSURL())),
		MCP:      mcpserver.NewHandler(deps),
	})

	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(lambdaadapter.New(handler).Invoke)
		return
	}
	log.Printf("listening on %s (stage %s)", *listen, s.Stage)
	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// fetchParameter reads a decrypted SSM parameter (cold start only).
func fetchParameter(ctx context.Context, name string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", err
	}
	out, err := ssm.NewFromConfig(cfg).GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(name),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Parameter.Value), nil
}
