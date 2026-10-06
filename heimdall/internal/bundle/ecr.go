package bundle

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"oras.land/oras-go/v2/registry/remote/auth"
)

var ecrHost = regexp.MustCompile(`^([0-9]{12})\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)

// ECRCredential uses the AWS credential chain, including the cluster agent's
// IRSA role. No AWS credentials are copied into any preview workload.
func ECRCredential(ctx context.Context, host string) (auth.Credential, error) {
	m := ecrHost.FindStringSubmatch(host)
	if m == nil {
		return auth.EmptyCredential, errors.New("registry is not a supported private ECR endpoint")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(m[2]))
	if err != nil {
		return auth.EmptyCredential, errors.New("agent registry identity unavailable")
	}
	r, err := ecr.NewFromConfig(cfg).GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return auth.EmptyCredential, errors.New("agent registry authorization failed")
	}
	for _, a := range r.AuthorizationData {
		if a.ProxyEndpoint == nil || strings.TrimPrefix(*a.ProxyEndpoint, "https://") != host || a.AuthorizationToken == nil {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(*a.AuthorizationToken)
		if err != nil {
			break
		}
		user, password, ok := strings.Cut(string(b), ":")
		if ok && user == "AWS" && password != "" {
			return auth.Credential{Username: user, Password: password}, nil
		}
	}
	return auth.EmptyCredential, errors.New("invalid registry authorization response")
}

func IsECR(host string) bool { return ecrHost.MatchString(host) }
