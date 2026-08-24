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
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
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
	case "rotate-signing-key":
		err = rotateSigningKey(ctx, os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ops rotate-secret --stage dev|prod [--origin-only|--break-glass-only]\n       ops bootstrap-user --stage dev|prod --email addr\n       ops rotate-signing-key --stage dev|prod")
	os.Exit(2)
}

// rotateSigningKey generates the CloudFront signing key pair for the files
// bucket (M4b-2): private key to SSM SecureString, public PEM to SSM for the
// deploy workflows to feed the SigningPublicKey resource. Re-running rotates
// the pair; the next deploy swaps the key group and every old link dies
// (the R9 emergency lever).
func rotateSigningKey(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rotate-signing-key", flag.ExitOnError)
	stage := fs.String("stage", "", "dev or prod")
	region := fs.String("region", "us-west-2", "AWS region")
	_ = fs.Parse(args)
	if *stage != "dev" && *stage != "prod" {
		return fmt.Errorf("--stage must be dev or prod")
	}
	cfg, err := awsConfig(ctx, *region)
	if err != nil {
		return err
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	client := ssm.NewFromConfig(cfg)
	for name, put := range map[string]struct {
		value string
		kind  ssmtypes.ParameterType
	}{
		"/messaging-mcp/" + *stage + "/files-signing-key":    {string(privPEM), ssmtypes.ParameterTypeSecureString},
		"/messaging-mcp/" + *stage + "/files-public-key-pem": {string(pubPEM), ssmtypes.ParameterTypeString},
	} {
		if _, err := client.PutParameter(ctx, &ssm.PutParameterInput{
			Name:      aws.String(name),
			Value:     aws.String(put.value),
			Type:      put.kind,
			Overwrite: aws.Bool(true),
		}); err != nil {
			return fmt.Errorf("put %s: %w", name, err)
		}
		fmt.Println("wrote", name)
	}
	fmt.Printf("signing key rotated for %s; redeploy the stage to register the new public key (old links die on the key-group swap)\n", *stage)
	return nil
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
