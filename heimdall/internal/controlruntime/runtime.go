// Package controlruntime contains process configuration shared by API/worker.
// Operator secrets are read at startup, never logged or accepted over HTTP.
package controlruntime

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/heimdall-dev/heimdall/internal/api/v1alpha1"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
	"github.com/heimdall-dev/heimdall/internal/orchestrator"
	"github.com/heimdall-dev/heimdall/internal/queue"
	"github.com/heimdall-dev/heimdall/internal/store"
)

func Secret(name string, min int) ([]byte, error) {
	path := os.Getenv(name)
	if path == "" {
		return nil, errors.New(name + " must name a local secret file")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < min || len(b) > 64<<10 {
		return nil, errors.New(name + " is unreadable or invalid")
	}
	return b, nil
}
func OpenStore(ctx context.Context) (*store.Store, error) {
	dsn := os.Getenv("HEIMDALL_DATABASE_URL")
	if dsn == "" {
		return nil, errors.New("HEIMDALL_DATABASE_URL is required")
	}
	key, err := Secret("HEIMDALL_CREDENTIAL_KEY_FILE", 32)
	if err != nil {
		return nil, err
	}
	s, err := store.NewWithOptions(ctx, dsn, store.Options{CredentialDerivationKey: key})
	if err != nil {
		return nil, errors.New("control database unavailable or application role is unsafe")
	}
	return s, nil
}
func Queue(ctx context.Context) (*queue.SQS, error) {
	url := os.Getenv("HEIMDALL_SQS_URL")
	if url == "" || !strings.HasSuffix(url, ".fifo") {
		return nil, errors.New("HEIMDALL_SQS_URL must be a FIFO queue URL")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("queue identity unavailable")
	}
	endpoint := os.Getenv("HEIMDALL_SQS_ENDPOINT")
	q := sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
	})
	return &queue.SQS{Client: q, URL: url}, nil
}
func GitHub(s *store.Store) (*orchestrator.Service, error) {
	id, err := strconv.ParseInt(os.Getenv("HEIMDALL_GITHUB_APP_ID"), 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("HEIMDALL_GITHUB_APP_ID is required")
	}
	key, err := Secret("HEIMDALL_GITHUB_PRIVATE_KEY_FILE", 64)
	if err != nil {
		return nil, err
	}
	gh, err := githubapp.NewClient(githubapp.ClientOptions{AppID: id, PrivateKey: key})
	if err != nil {
		return nil, err
	}
	v, err := githubapp.NewOIDCVerifier(githubapp.OIDCOptions{Audience: os.Getenv("HEIMDALL_OIDC_AUDIENCE")})
	if err != nil {
		return nil, err
	}
	return orchestrator.New(orchestrator.Options{Store: s, GitHub: gh, OIDC: v, SeedApproval: func(ctx context.Context, p domain.Principal, repo string, pr int64, sha string) (v1alpha1.DataApproval, error) {
		a, err := s.GetDataAttestationForConfig(ctx, p, repo, pr, sha)
		return v1alpha1.DataApproval{SHA256: a.SeedSHA256, ApprovedBy: a.ApprovedBy, Reason: a.Reason, Sanitised: a.Sanitised}, err
	}})
}
