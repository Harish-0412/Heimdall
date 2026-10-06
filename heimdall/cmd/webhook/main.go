package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/heimdall-dev/heimdall/internal/controlruntime"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
)

func main() {
	if err := run(); err != nil {
		slog.Error("webhook startup failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	stopTrace := tracecontext.Initialize()
	defer func() { _ = stopTrace(context.Background()) }()
	ctx := context.Background()
	q, err := controlruntime.Queue(ctx)
	if err != nil {
		return err
	}
	var secret []byte
	if arn := os.Getenv("HEIMDALL_GITHUB_WEBHOOK_SECRET_ARN"); arn != "" {
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return errors.New("webhook identity unavailable")
		}
		value, err := secretsmanager.NewFromConfig(cfg).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
		if err != nil {
			return errors.New("webhook secret unavailable")
		}
		if value.SecretString != nil {
			secret = []byte(*value.SecretString)
		} else {
			secret = value.SecretBinary
		}
	} else {
		secret, err = controlruntime.Secret("HEIMDALL_GITHUB_WEBHOOK_SECRET_FILE", 32)
		if err != nil {
			return err
		}
	}
	if len(secret) < 32 {
		return errors.New("webhook secret must contain at least 32 bytes")
	}
	h := &githubapp.Webhook{Secret: secret, Queue: q}
	lambda.Start(h.HandleLambda)
	return nil
}
