//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

func TestPostgresStatusDiagnosisEvidence(t *testing.T) {
	f := storetest.New(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, f.AdminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	agent := domain.Principal{TenantID: f.Principal.TenantID, ActorID: "agent", Role: "agent", ClusterID: f.Cluster.ID}
	for index, tc := range []struct {
		name     string
		evidence []string
		want     []string
	}{
		{"omitted before workload exists", nil, []string{}},
		{"explicit empty", []string{}, []string{}},
		{"workload evidence", []string{"image pull denied"}, []string{"image pull denied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := f.Store.CreateEnvironment(ctx, f.Principal, domain.CreateEnvironment{RepositoryID: f.Repository.ID, ClusterID: f.Cluster.ID, PullRequest: int64(700 + index), Owner: "maintainer", Commit: "commit", Spec: json.RawMessage(`{}`)}, "")
			if err != nil {
				t.Fatal(err)
			}
			u := domain.StatusUpdate{Generation: e.Generation, Version: e.Version, Phase: "Failed", EventID: "preparation-failed", Status: json.RawMessage(`{"phase":"Failed"}`), Diagnoses: []domain.Diagnosis{{Code: "source.bundle_unavailable", Summary: "Configuration artifact unavailable", Evidence: tc.evidence}}}
			failed, err := f.Store.ReportStatus(ctx, agent, e.ID, u)
			if err != nil || failed.Phase != "Failed" || failed.Version != e.Version+1 {
				t.Fatalf("failure must commit with its diagnosis: %+v %v", failed, err)
			}
			replay, err := f.Store.ReportStatus(ctx, agent, e.ID, u)
			if err != nil || replay.Version != failed.Version {
				t.Fatalf("retry must not duplicate the report: %+v %v", replay, err)
			}
			var evidence []byte
			var count int
			if err = admin.QueryRow(ctx, `SELECT evidence FROM heimdall.diagnoses WHERE tenant_id=$1 AND environment_id=$2`, f.Principal.TenantID, e.ID).Scan(&evidence); err != nil {
				t.Fatal(err)
			}
			var got []string
			if err = json.Unmarshal(evidence, &got); err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("persisted evidence = %s, want %v: %v", evidence, tc.want, err)
			}
			if err = admin.QueryRow(ctx, `SELECT count(*) FROM heimdall.diagnoses WHERE tenant_id=$1 AND environment_id=$2`, f.Principal.TenantID, e.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("diagnosis count after replay = %d: %v", count, err)
			}
			if _, err = f.Store.ActEnvironment(ctx, f.Principal, e.ID, failed.Version, domain.Action{Kind: "delete"}, ""); err != nil {
				t.Fatalf("failed preview must remain deletable: %v", err)
			}
		})
	}
}
