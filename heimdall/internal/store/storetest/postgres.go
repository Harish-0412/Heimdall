// Package storetest provisions real PostgreSQL using the production migrations
// and the restricted application role. It is shared by control-plane tests.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/heimdall-dev/heimdall/internal/config"
	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

type Fixture struct {
	Store                       *store.Store
	AdminDSN, AppDSN            string
	Principal, Other            domain.Principal
	Cluster, OtherCluster       domain.Cluster
	Repository, OtherRepository domain.Repository
}

func New(t testing.TB) *Fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	password := uuid.NewString()
	c, err := postgres.Run(ctx, "public.ecr.aws/docker/library/postgres:17-alpine", postgres.WithDatabase("heimdall"), postgres.WithUsername("postgres"), postgres.WithPassword(password), postgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start required real PostgreSQL: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Terminate(cleanupCtx); err != nil {
			t.Errorf("terminate PostgreSQL: %v", err)
		}
	})
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("production migration: %v", err)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "ALTER ROLE heimdall_app PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	admin.Close(ctx)
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword("heimdall_app", password)
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		t.Fatal(err)
	}
	s, err := store.NewWithOptions(ctx, parsed.String(), store.Options{CredentialDerivationKey: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	f := &Fixture{Store: s, AdminDSN: dsn, AppDSN: parsed.String()}
	policy := config.DefaultPolicy()
	policy.AllowedSecrets = []string{}
	policy.AllowedRegistries = []string{"ghcr.io/"}
	policyData, _ := json.Marshal(policy)
	makeTenant := func(slug string, installation int64) (domain.Principal, domain.Cluster, domain.Repository) {
		p := domain.Principal{TenantID: uuid.NewString(), TenantSlug: slug, ActorID: "test-admin", Role: "admin", Kind: "user"}
		if err := store.ProvisionTenant(ctx, dsn, domain.Tenant{ID: p.TenantID, Slug: slug, Name: slug}, policyData, domain.Quota{MaxEnvironments: 8, MaxCPUMilli: 8000, MaxMemoryMi: 16384, MaxStorageMi: 32768}); err != nil {
			t.Fatal(err)
		}
		cluster, err := s.CreateCluster(ctx, p, domain.Cluster{Name: slug + "-cluster", Tier: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.PutInstallation(ctx, p, domain.Installation{ID: installation, Account: slug}); err != nil {
			t.Fatal(err)
		}
		repo, err := s.PutRepository(ctx, p, domain.Repository{GitHubID: installation * 10, InstallationID: installation, ClusterID: cluster.ID, FullName: slug + "/app", DefaultBranch: "main", Enabled: true, TrustedWorkflowRef: "platform/heimdall/.github/workflows/build.yaml@" + hex.EncodeToString(make([]byte, 20)), TrustedWorkflowSHA: hex.EncodeToString(make([]byte, 20))})
		if err != nil {
			t.Fatal(err)
		}
		return p, cluster, repo
	}
	f.Principal, f.Cluster, f.Repository = makeTenant("alpha", 101)
	f.Other, f.OtherCluster, f.OtherRepository = makeTenant("beta", 102)
	return f
}
