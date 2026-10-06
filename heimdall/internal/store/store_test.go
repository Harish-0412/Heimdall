//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heimdall-dev/heimdall/internal/domain"
	"github.com/heimdall-dev/heimdall/internal/store"
	"github.com/heimdall-dev/heimdall/internal/store/storetest"
	"github.com/jackc/pgx/v5"
)

func TestPostgresControlPlane(t *testing.T) {
	f := storetest.New(t)
	ctx := context.Background()
	s := f.Store
	p := f.Principal
	create := func(number int64) domain.Environment {
		t.Helper()
		e, err := s.CreateEnvironment(ctx, p, domain.CreateEnvironment{RepositoryID: f.Repository.ID, ClusterID: f.Cluster.ID, PullRequest: number, Owner: "maintainer", Commit: strings.Repeat("a", 40), Spec: json.RawMessage(`{"tenant":"alpha","repo":"app","pullRequest":1,"config":{"inline":"metadata only"}}`)}, "")
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	t.Run("role and RLS fail closed", func(t *testing.T) {
		if unsafe, err := store.New(ctx, f.AdminDSN); err == nil {
			unsafe.Close()
			t.Fatal("privileged role accepted")
		}
		e := create(1)
		if _, err := s.GetEnvironment(ctx, f.Other, e.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross tenant read: %v", err)
		}
		if _, err := s.ActEnvironment(ctx, f.Other, e.ID, e.Version, domain.Action{Kind: "delete"}, ""); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cross tenant mutation: %v", err)
		}
		if _, err := s.CreateEnvironment(ctx, p, domain.CreateEnvironment{RepositoryID: f.Repository.ID, ClusterID: f.OtherCluster.ID, PullRequest: 88, Owner: "test", Commit: "sha", Spec: json.RawMessage(`{}`)}, ""); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("cross tenant FK allowed: %v", err)
		}
		conn, err := pgx.Connect(ctx, f.AppDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var count int
		if err = conn.QueryRow(ctx, `SELECT count(*) FROM heimdall.environments`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("missing tenant context exposes rows %d: %v", count, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id',$1,true)`, p.TenantID); err != nil {
			t.Fatal(err)
		}
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM heimdall.environments`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("valid tenant rows %d: %v", count, err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = conn.QueryRow(ctx, `SELECT count(*) FROM heimdall.environments`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("context leaked after commit %d: %v", count, err)
		}
		if _, err = conn.Exec(ctx, `SET ROLE postgres`); err == nil {
			t.Fatal("application role inherited owner")
		}
		admin, err := pgx.Connect(ctx, f.AdminDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close(ctx)
		if err = admin.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='heimdall' AND c.relkind='r' AND c.relname NOT IN ('tenant_routes','installation_routes') AND NOT(c.relrowsecurity AND c.relforcerowsecurity)`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("tables missing FORCE RLS: %d %v", count, err)
		}
		if _, err = admin.Exec(ctx, `UPDATE heimdall.deployments SET commit_sha='tampered'`); err == nil {
			t.Fatal("immutable deployment mutable even by owner")
		}
		if _, err = admin.Exec(ctx, `UPDATE heimdall.audit_log SET action='tampered'`); err == nil {
			t.Fatal("immutable audit mutable even by owner")
		}
	})
	t.Run("idempotency transaction and CAS concurrency", func(t *testing.T) {
		in := domain.CreateEnvironment{RepositoryID: f.Repository.ID, ClusterID: f.Cluster.ID, PullRequest: 2, Owner: "owner", Commit: "commit", Spec: json.RawMessage(`{}`)}
		e, err := s.CreateEnvironment(ctx, p, in, "create-2")
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.CreateEnvironment(ctx, p, in, "create-2")
		if err != nil || again.ID != e.ID {
			t.Fatalf("replay %v", err)
		}
		in.Commit = "different"
		if _, err = s.CreateEnvironment(ctx, p, in, "create-2"); !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("payload reuse %v", err)
		}
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.ActEnvironment(ctx, p, e.ID, e.Version, domain.Action{Kind: "retry"}, "")
				results <- err
			}()
		}
		wg.Wait()
		close(results)
		ok, conflict := 0, 0
		for err := range results {
			if err == nil {
				ok++
			} else if errors.Is(err, domain.ErrConflict) {
				conflict++
			} else {
				t.Fatal(err)
			}
		}
		if ok != 1 || conflict != 1 {
			t.Fatalf("CAS results %d/%d", ok, conflict)
		}
		current, err := s.GetEnvironment(ctx, p, e.ID)
		if err != nil || current.Generation != 2 || current.Version != 2 {
			t.Fatalf("generation/version %#v %v", current, err)
		}
	})
	t.Run("agent isolation stale reports and convergent snapshots", func(t *testing.T) {
		e := create(3)
		agent := domain.Principal{TenantID: p.TenantID, ActorID: "agent", Role: "agent", ClusterID: f.Cluster.ID}
		wrong := agent
		wrong.ClusterID = f.OtherCluster.ID
		u := domain.StatusUpdate{Generation: e.Generation, Version: e.Version, Phase: "Provisioning", Status: json.RawMessage(`{"phase":"Provisioning"}`), EventID: "provisioning-3"}
		if _, err := s.ReportStatus(ctx, wrong, e.ID, u); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cluster isolation %v", err)
		}
		current, err := s.ReportStatus(ctx, agent, e.ID, u)
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.ReportStatus(ctx, agent, e.ID, u)
		if err != nil || again.Version != current.Version {
			t.Fatalf("status duplicate %v", err)
		}
		u.Status = json.RawMessage(`{"phase":"Provisioning","namespace":"different"}`)
		if _, err = s.ReportStatus(ctx, agent, e.ID, u); !errors.Is(err, domain.ErrIdempotencyConflict) {
			t.Fatalf("status id payload conflict %v", err)
		}
		current, err = s.ActEnvironment(ctx, p, e.ID, current.Version, domain.Action{Kind: "retry"}, "")
		if err != nil {
			t.Fatal(err)
		}
		u.EventID = "stale-3"
		if _, err = s.ReportStatus(ctx, agent, e.ID, u); !errors.Is(err, domain.ErrStaleGeneration) {
			t.Fatalf("stale %v", err)
		}
		rows, err := s.Timeline(ctx, p, e.ID, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, event := range rows {
			if event.Kind == "stale" {
				found = true
			}
		}
		if !found {
			t.Fatal("stale report not retained")
		}
		ready := domain.StatusUpdate{Generation: current.Generation, Version: current.Version, Phase: "Ready", EventID: "ready-3", Status: json.RawMessage(fmt.Sprintf(`{"phase":"Ready","deployedGeneration":%d,"operation":{"generation":%d,"result":"Succeeded"}}`, current.Generation, current.Generation))}
		ready.Status = json.RawMessage(fmt.Sprintf(`{"phase":"Ready","deployedGeneration":%d,"operation":{"generation":%d,"result":"Succeeded","startedAt":"2026-10-05T00:00:00Z"},"steps":[{"name":"guardrails","state":"succeeded","durationSeconds":1},{"name":"dependencies","state":"succeeded","durationSeconds":2},{"name":"application","state":"succeeded","durationSeconds":3},{"name":"smoke","state":"succeeded","durationSeconds":1}]}`, current.Generation, current.Generation))
		current, err = s.ReportStatus(ctx, agent, e.ID, ready)
		if err != nil {
			t.Fatalf("reconnect convergence %v", err)
		}
		stageTimeline, err := s.Timeline(ctx, p, e.ID, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		stages := 0
		for _, event := range stageTimeline {
			if event.Kind == "stage" {
				stages++
			}
		}
		if stages != 4 {
			t.Fatalf("completed step history count %d", stages)
		}
		ready.EventID = "ready-3-observation"
		ready.Version = current.Version
		current, err = s.ReportStatus(ctx, agent, e.ID, ready)
		if err != nil {
			t.Fatal(err)
		}
		stageTimeline, err = s.Timeline(ctx, p, e.ID, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		stages = 0
		for _, event := range stageTimeline {
			if event.Kind == "stage" {
				stages++
			}
		}
		if stages != 4 {
			t.Fatalf("step history duplicated %d", stages)
		}
		snap, err := s.Snapshot(ctx, agent)
		if err != nil || len(snap.Environments) < 1 || snap.Revision < 1 {
			t.Fatalf("consistent snapshot %+v %v", snap, err)
		}
		var smoke domain.Environment
		smoke = current
		smoke, _ = s.ActEnvironment(ctx, p, e.ID, current.Version, domain.Action{Kind: "reset", Reason: "user reset"}, "")
		if smoke.Generation != current.Generation+1 || smoke.ResetNonce != 1 {
			t.Fatal("reset nonce not advanced")
		}
	})
	t.Run("credential rotation lost response and revocation", func(t *testing.T) {
		enroll, err := s.IssueCredential(ctx, p, domain.CredentialInput{ActorID: "cluster-agent", ClusterID: f.Cluster.ID, Role: "agent", Kind: "enrollment", TTL: 5 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ExchangeCredentialWithNonce(ctx, enroll.Token, "enrollment", f.OtherCluster.ID, "nonce-12345678"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("wrong scope %v", err)
		}
		pair, err := s.ExchangeCredentialWithNonce(ctx, enroll.Token, "enrollment", f.Cluster.ID, "nonce-12345678")
		if err != nil {
			t.Fatal(err)
		}
		again, err := s.ExchangeCredentialWithNonce(ctx, enroll.Token, "enrollment", f.Cluster.ID, "nonce-12345678")
		if err != nil || again.Access.Token != pair.Access.Token || again.Refresh.Token != pair.Refresh.Token {
			t.Fatalf("lost response replay %v", err)
		}
		if _, err = s.ExchangeCredentialWithNonce(ctx, enroll.Token, "enrollment", f.Cluster.ID, "different-nonce"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("replay nonce changed %v", err)
		}
		principal, err := s.Authenticate(ctx, pair.Access.Token)
		if err != nil || principal.ClusterID != f.Cluster.ID || principal.Role != "agent" {
			t.Fatalf("access principal %+v %v", principal, err)
		}
		if _, err = s.Authenticate(ctx, pair.Refresh.Token); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("refresh as access %v", err)
		}
		rotated, err := s.ExchangeCredentialWithNonce(ctx, pair.Refresh.Token, "refresh", f.Cluster.ID, "refresh-nonce-123")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RevokeClusterCredentials(ctx, p, f.Cluster.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Authenticate(ctx, rotated.Access.Token); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("revoke access %v", err)
		}
		if _, err = s.ExchangeCredentialWithNonce(ctx, pair.Refresh.Token, "refresh", f.Cluster.ID, "refresh-nonce-123"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("revoke replay %v", err)
		}
		fake := strings.Replace(pair.Access.Token, p.TenantID, f.Other.TenantID, 1)
		if _, err = s.Authenticate(ctx, fake); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("tenant hint trusted %v", err)
		}
	})
	t.Run("durable inbox leases and replay", func(t *testing.T) {
		for _, id := range []string{"delivery-a", "delivery-b"} {
			if inserted, err := s.IngestWebhook(ctx, p, domain.WebhookDelivery{DeliveryID: id, Event: "pull_request", RepositoryID: f.Repository.ID, PullRequest: 41, Payload: json.RawMessage(`{"action":"opened"}`)}); err != nil || !inserted {
				t.Fatalf("ingest %v", err)
			}
		}
		if inserted, err := s.IngestWebhook(ctx, p, domain.WebhookDelivery{DeliveryID: "delivery-a", Event: "pull_request", RepositoryID: f.Repository.ID, PullRequest: 41, Payload: json.RawMessage(`{"action":"opened"}`)}); err != nil || inserted {
			t.Fatalf("durable duplicate %v", err)
		}
		d, err := s.ClaimWebhookWithLease(ctx, p, "delivery-a", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.CompleteWebhook(ctx, p, d.DeliveryID, "wrong-worker"); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("worker lease %v", err)
		}
		time.Sleep(1100 * time.Millisecond)
		reclaimed, err := s.ClaimWebhook(ctx, p, d.DeliveryID)
		if err != nil || reclaimed.LeaseToken == d.LeaseToken || reclaimed.Attempts != 2 {
			t.Fatalf("crash reclaim %+v %v", reclaimed, err)
		}
		if err = s.CompleteWebhook(ctx, p, d.DeliveryID, d.LeaseToken); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("old lease %v", err)
		}
		if err = s.CompleteWebhook(ctx, p, d.DeliveryID, reclaimed.LeaseToken); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimWebhook(ctx, p, "delivery-a"); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("completed replay %v", err)
		}
		b, err := s.ClaimWebhook(ctx, p, "delivery-b")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RetryWebhook(ctx, p, b.DeliveryID, b.LeaseToken, "temporary upstream failure"); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimWebhook(ctx, p, b.DeliveryID); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("retry availability %v", err)
		}
	})
	t.Run("canonical PR head fencing pending runtime and build replay", func(t *testing.T) {
		now := time.Now().UTC()
		obs := domain.PullRequestObservation{RepositoryID: f.Repository.ID, Number: 4, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), State: "open", Owner: "owner", ConfigDigest: strings.Repeat("c", 64), BaselineDigest: strings.Repeat("d", 64), UpdatedAt: now}
		e, err := s.ObservePullRequest(ctx, p, obs)
		if err != nil {
			t.Fatal(err)
		}
		agent := domain.Principal{TenantID: p.TenantID, ActorID: "agent", Role: "agent", ClusterID: f.Cluster.ID}
		snap, err := s.Snapshot(ctx, agent)
		if err != nil {
			t.Fatal(err)
		}
		for _, desired := range snap.Environments {
			if desired.ID == e.ID {
				t.Fatal("unbuilt new environment projected")
			}
		}
		b := domain.BuildInput{RepositoryID: f.Repository.ID, Number: 4, HeadSHA: obs.HeadSHA, Bundle: "ghcr.io/alpha/bundle@sha256:" + strings.Repeat("e", 64), ConfigDigest: obs.ConfigDigest, BaselineDigest: obs.BaselineDigest, RunID: "100", RunAttempt: 1, WorkflowRef: f.Repository.TrustedWorkflowRef, Spec: json.RawMessage(`{"tenant":"alpha","config":{"inline":"metadata only"}}`), ExpiresAt: time.Now().Add(time.Hour)}
		e, err = s.CommitBuild(ctx, p, b)
		if err != nil {
			t.Fatal(err)
		}
		b.ExpiresAt = time.Now().Add(time.Hour)
		again, err := s.CommitBuild(ctx, p, b)
		if err != nil || again.Generation != e.Generation {
			t.Fatalf("build replay stable fingerprint %v", err)
		}
		obs.HeadSHA = strings.Repeat("f", 40)
		obs.ExpectedVersion = 1
		obs.UpdatedAt = now.Add(time.Second)
		pending, err := s.ObservePullRequest(ctx, p, obs)
		if err != nil {
			t.Fatal(err)
		}
		if pending.Generation != e.Generation+1 {
			t.Fatal("new push not fenced")
		}
		competing := obs
		competing.HeadSHA = "same-second-new-head"
		if _, err = s.ObservePullRequest(ctx, p, competing); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("concurrent live read CAS %v", err)
		}
		var spec struct {
			Generation int64 `json:"generation"`
		}
		json.Unmarshal(pending.Spec, &spec)
		if spec.Generation != e.Generation {
			t.Fatal("pending push advanced last approved runtime")
		}
		if _, err = s.CommitBuild(ctx, p, b); !errors.Is(err, domain.ErrStaleGeneration) {
			t.Fatalf("old CI callback %v", err)
		}
		stale := obs
		stale.ExpectedVersion = 2
		stale.HeadSHA = b.HeadSHA
		stale.UpdatedAt = now
		latest, err := s.ObservePullRequest(ctx, p, stale)
		if err != nil || latest.Commit != obs.HeadSHA {
			t.Fatalf("older live observation reverted head %v", err)
		}
		obs.State = "closed"
		obs.ExpectedVersion = 2
		obs.UpdatedAt = now.Add(2 * time.Second)
		closed, err := s.ObservePullRequest(ctx, p, obs)
		if err != nil || closed.DesiredState != "Destroyed" {
			t.Fatalf("PR close %+v %v", closed, err)
		}
		if _, err = s.CommitBuild(ctx, p, b); !errors.Is(err, domain.ErrStaleGeneration) {
			t.Fatalf("closed PR build %v", err)
		}
	})
	t.Run("explicit no baseline approval and fixture hash binding", func(t *testing.T) {
		obs := domain.PullRequestObservation{RepositoryID: f.Repository.ID, Number: 5, HeadSHA: "head-5", State: "open", Owner: "owner", NeedsApproval: true, ConfigDigest: strings.Repeat("1", 64), Message: "An authorized maintainer must approve this configuration."}
		e, err := s.ObservePullRequest(ctx, p, obs)
		if err != nil {
			t.Fatal(err)
		}
		b := domain.BuildInput{RepositoryID: f.Repository.ID, Number: 5, HeadSHA: obs.HeadSHA, Bundle: "bundle@sha256:" + strings.Repeat("2", 64), ConfigDigest: obs.ConfigDigest, RunID: "101", RunAttempt: 1, WorkflowRef: f.Repository.TrustedWorkflowRef, Spec: json.RawMessage(`{}`)}
		if _, err = s.CommitBuild(ctx, p, b); !errors.Is(err, domain.ErrForbidden) {
			t.Fatalf("approval bypass %v", err)
		}
		if err = s.ApprovePullRequest(ctx, p, f.Repository.ID, 5, "old-head", "", ""); !errors.Is(err, domain.ErrStaleGeneration) {
			t.Fatalf("approval wrong head %v", err)
		}
		if err = s.ApprovePullRequest(ctx, p, f.Repository.ID, 5, obs.HeadSHA, "", ""); err != nil {
			t.Fatal(err)
		}
		e, err = s.CommitBuild(ctx, p, b)
		if err != nil || e.BuildState != "ready" {
			t.Fatalf("approved deploy %v", err)
		}
		a := domain.DataAttestation{RepositoryID: f.Repository.ID, PullRequest: 5, ConfigSHA256: obs.ConfigDigest, SeedSHA256: strings.Repeat("3", 64), Reason: "sanitized test fixture", Sanitised: true, ExpiresAt: time.Now().Add(time.Hour)}
		if err = s.PutDataAttestation(ctx, p, a); err != nil {
			t.Fatal(err)
		}
		saved, err := s.GetDataAttestationForConfig(ctx, p, a.RepositoryID, 5, a.ConfigSHA256)
		if err != nil || saved.ApprovedBy != p.ActorID {
			t.Fatalf("attestation actor %v", err)
		}
		if _, err = s.GetDataAttestation(ctx, p, a.RepositoryID, 5, a.ConfigSHA256, strings.Repeat("4", 64)); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("seed content binding %v", err)
		}
		if _, err = s.GetDataAttestationForConfig(ctx, f.Other, a.RepositoryID, 5, a.ConfigSHA256); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("fixture tenant isolation %v", err)
		}
	})
	t.Run("atomic intent inbox completion and serialized GitHub writes", func(t *testing.T) {
		e := create(6)
		_, err := s.IngestWebhook(ctx, p, domain.WebhookDelivery{DeliveryID: "action-atomic", Event: "issue_comment", RepositoryID: f.Repository.ID, PullRequest: 6, Payload: json.RawMessage(`{"command":"retry"}`)})
		if err != nil {
			t.Fatal(err)
		}
		delivery, err := s.ClaimWebhook(ctx, p, "action-atomic")
		if err != nil {
			t.Fatal(err)
		}
		before, err := s.Timeline(ctx, p, e.ID, 0, 200)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ActEnvironment(ctx, p, e.ID, e.Version, domain.Action{Kind: "retry", DeliveryID: delivery.DeliveryID, LeaseToken: "wrong"}, ""); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("intent accepted with lost inbox lease %v", err)
		}
		after, err := s.GetEnvironment(ctx, p, e.ID)
		if err != nil || after.Generation != e.Generation || after.Version != e.Version {
			t.Fatalf("intent did not roll back %+v %v", after, err)
		}
		events, err := s.Timeline(ctx, p, e.ID, 0, 200)
		if err != nil || len(events) != len(before) {
			t.Fatalf("events did not roll back %v", err)
		}
		e, err = s.ActEnvironment(ctx, p, e.ID, e.Version, domain.Action{Kind: "retry", DeliveryID: delivery.DeliveryID, LeaseToken: delivery.LeaseToken}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ClaimWebhook(ctx, p, delivery.DeliveryID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("intent did not consume inbox %v", err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		delivered := make(chan error, 1)
		go func() {
			delivered <- s.WithGitHubDelivery(ctx, p, e.ID, func(current domain.Environment, prior domain.GitHubDelivery) (domain.GitHubDelivery, error) {
				close(entered)
				<-release
				return domain.GitHubDelivery{EnvironmentID: e.ID, Generation: current.Generation, CommentID: 44, CheckID: 45}, nil
			})
		}()
		<-entered
		changed := make(chan error, 1)
		go func() {
			_, err := s.ActEnvironment(ctx, p, e.ID, e.Version, domain.Action{Kind: "retry"}, "")
			changed <- err
		}()
		select {
		case err := <-changed:
			t.Fatalf("intent raced an external write: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-delivered; err != nil {
			t.Fatal(err)
		}
		if err := <-changed; err != nil {
			t.Fatal(err)
		}
		state, err := s.GetGitHubDelivery(ctx, p, e.ID)
		if err != nil || state.CommentID != 44 || state.Generation != e.Generation {
			t.Fatalf("delivery state %+v %v", state, err)
		}
	})
	t.Run("tenant quota and outbox commit lease", func(t *testing.T) {
		if err := s.SetQuota(ctx, p, domain.Quota{MaxEnvironments: 1, MaxCPUMilli: 1000, MaxMemoryMi: 1024, MaxStorageMi: 1024}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateEnvironment(ctx, p, domain.CreateEnvironment{RepositoryID: f.Repository.ID, ClusterID: f.Cluster.ID, PullRequest: 99, Owner: "owner", Commit: "commit", Spec: json.RawMessage(`{}`)}, ""); !errors.Is(err, domain.ErrQuotaExceeded) {
			t.Fatalf("quota bypass %v", err)
		}
		outbox, err := s.ClaimOutbox(ctx, p, 200)
		if err != nil || len(outbox) == 0 {
			t.Fatalf("transactional outbox absent %v", err)
		}
		item := outbox[0]
		if err = s.FinishOutbox(ctx, p, item.ID, "wrong", item.Generation, domain.GitHubDelivery{}); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatalf("outbox lease %v", err)
		}
		if err = s.FinishOutbox(ctx, p, item.ID, item.LeaseToken, item.Generation, domain.GitHubDelivery{}); err != nil {
			t.Fatal(err)
		}
	})
}
