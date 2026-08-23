// Command ops holds the owner's one-off operational actions so the repo has
// a single toolchain:
//
//	ops rotate-secret --stage dev [--origin-only|--break-glass-only]
//	ops bootstrap-user --stage dev --email you@example.com
//
// rotate-secret writes /messaging-mcp/<stage>/origin-secret (SecureString)
// and /messaging-mcp/<stage>/break-glass-sha256 (String); the break-glass
// token itself is printed once. bootstrap-user creates the single Cognito
// owner user with a temporary password (self-signup is disabled).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	cogtypes "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "rotate-secret":
		err = rotateSecret(ctx, os.Args[2:])
	case "bootstrap-user":
		err = bootstrapUser(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ops rotate-secret --stage dev|prod [--origin-only|--break-glass-only]\n       ops bootstrap-user --stage dev|prod --email addr")
	os.Exit(2)
}

func awsConfig(ctx context.Context, region string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx, config.WithRegion(region))
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func rotateSecret(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rotate-secret", flag.ExitOnError)
	stage := fs.String("stage", "", "dev or prod")
	region := fs.String("region", "us-west-2", "AWS region")
	originOnly := fs.Bool("origin-only", false, "rotate only the origin secret")
	breakGlassOnly := fs.Bool("break-glass-only", false, "rotate only the break-glass token")
	_ = fs.Parse(args)
	if *stage != "dev" && *stage != "prod" {
		return fmt.Errorf("--stage must be dev or prod")
	}
	cfg, err := awsConfig(ctx, *region)
	if err != nil {
		return err
	}
	client := ssm.NewFromConfig(cfg)
	prefix := "/messaging-mcp/" + *stage

	put := func(name, value string, kind ssmtypes.ParameterType) error {
		_, err := client.PutParameter(ctx, &ssm.PutParameterInput{
			Name: aws.String(name), Value: aws.String(value), Type: kind, Overwrite: aws.Bool(true),
		})
		return err
	}
	if !*breakGlassOnly {
		secret, err := randomToken()
		if err != nil {
			return err
		}
		if err := put(prefix+"/origin-secret", secret, ssmtypes.ParameterTypeSecureString); err != nil {
			return err
		}
		fmt.Printf("rotated %s/origin-secret (redeploy the %s stack to apply)\n", prefix, *stage)
	}
	if !*originOnly {
		token, err := randomToken()
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(token))
		if err := put(prefix+"/break-glass-sha256", hex.EncodeToString(sum[:]), ssmtypes.ParameterTypeString); err != nil {
			return err
		}
		fmt.Printf("rotated %s/break-glass-sha256\nbreak-glass token (shown once, store it now):\n  %s\n", prefix, token)
	}
	return nil
}

func bootstrapUser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bootstrap-user", flag.ExitOnError)
	stage := fs.String("stage", "", "dev or prod")
	email := fs.String("email", "", "owner email address")
	region := fs.String("region", "us-west-2", "AWS region")
	_ = fs.Parse(args)
	if (*stage != "dev" && *stage != "prod") || *email == "" {
		return fmt.Errorf("--stage dev|prod and --email are required")
	}
	cfg, err := awsConfig(ctx, *region)
	if err != nil {
		return err
	}
	outputs, err := stackOutputs(ctx, cloudformation.NewFromConfig(cfg), "aws-messaging-mcp-"+*stage)
	if err != nil {
		return err
	}
	poolID, hostedUI := outputs["UserPoolId"], outputs["HostedUiUrl"]
	if poolID == "" {
		return fmt.Errorf("stack has no UserPoolId output")
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	tempPassword := "Aa1!" + token[:16]
	_, err = cognitoidentityprovider.NewFromConfig(cfg).AdminCreateUser(ctx, &cognitoidentityprovider.AdminCreateUserInput{
		UserPoolId:        aws.String(poolID),
		Username:          email,
		TemporaryPassword: aws.String(tempPassword),
		MessageAction:     cogtypes.MessageActionTypeSuppress,
		UserAttributes: []cogtypes.AttributeType{
			{Name: aws.String("email"), Value: email},
			{Name: aws.String("email_verified"), Value: aws.String("true")},
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("Created %s in %s.\nTemporary password (change at first login): %s\n", *email, poolID, tempPassword)
	fmt.Printf("Sign in once at %s via a client OAuth flow to set a real password and enrol TOTP.\n", hostedUI)
	return nil
}

func stackOutputs(ctx context.Context, client *cloudformation.Client, name string) (map[string]string, error) {
	out, err := client.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		return nil, err
	}
	result := map[string]string{}
	for _, stack := range out.Stacks {
		for _, o := range stack.Outputs {
			result[aws.ToString(o.OutputKey)] = aws.ToString(o.OutputValue)
		}
	}
	return result, nil
}
