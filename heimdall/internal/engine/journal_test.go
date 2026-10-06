package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/heimdall-dev/heimdall/internal/render"
)

type journalStub struct {
	Cluster
	updates atomic.Int32
	update  func(context.Context, *unstructured.Unstructured) (*unstructured.Unstructured, error)
}

func (s *journalStub) Update(ctx context.Context, _ Resource, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.updates.Add(1)
	if s.update != nil {
		return s.update(ctx, o)
	}
	return o.DeepCopy(), nil
}

func journalSession(t *testing.T, stub *journalStub) (context.Context, *session) {
	t.Helper()
	work, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return work, &session{e: New(stub, time.Minute, nil), work: work, cancel: cancel, holder: "holder",
		record: record{Operation: "apply", Holder: "holder", LeaseUntil: time.Now().Add(leaseDuration)},
		object: &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": journalName, "namespace": "preview"}}}}
}

func TestStepPreservesLostLeaseCause(t *testing.T) {
	stub := &journalStub{}
	work, s := journalSession(t, stub)
	stub.update = func(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		if stub.updates.Load() == 2 {
			return nil, context.DeadlineExceeded // the renewal request timed out
		}
		return o.DeepCopy(), nil
	}
	var observed []Event
	s.e.observe = func(ev Event) { observed = append(observed, ev) }
	result := &Result{Generation: 3}
	err := s.e.step(work, s, result, "dependencies/start", func(ctx context.Context) error {
		if err := s.save(context.Background()); errorCode(err) != "engine.lock_lost" {
			t.Fatalf("renewal: %v", err)
		}
		<-ctx.Done()
		return ctx.Err() // Kubernetes' wait commonly returns only cancellation
	})
	if errorCode(err) != "engine.lock_lost" || errorCode(context.Cause(work)) != "engine.lock_lost" {
		t.Fatalf("ownership cause lost: err %v cause %v", err, context.Cause(work))
	}
	if len(result.Events) != 1 || result.Events[0].State != "failed" || result.Events[0].Code != "engine.lock_lost" {
		t.Fatalf("terminal result event: %+v", result.Events)
	}
	if len(observed) != 2 || observed[1].State != "failed" || observed[1].Code != "engine.lock_lost" {
		t.Fatalf("terminal observer event: %+v", observed)
	}
	if stub.updates.Load() != 2 {
		t.Fatalf("lost lease was written again: %d updates", stub.updates.Load())
	}
}

func TestExpiredLeaseCannotBeRenewedOrWritten(t *testing.T) {
	stub := &journalStub{}
	work, s := journalSession(t, stub)
	expired := time.Now().Add(-time.Second)
	s.record.LeaseUntil = expired
	for range 2 {
		if err := s.save(context.Background()); errorCode(err) != "engine.lock_lost" {
			t.Fatalf("expired lease: %v", err)
		}
	}
	if work.Err() == nil || stub.updates.Load() != 0 || !s.record.LeaseUntil.Equal(expired) {
		t.Fatalf("expired holder mutated journal: updates %d lease %s cause %v", stub.updates.Load(), s.record.LeaseUntil, context.Cause(work))
	}
}

func TestCompletionJournalFailureIsATerminalStepEvent(t *testing.T) {
	stub := &journalStub{}
	work, s := journalSession(t, stub)
	stub.update = func(_ context.Context, o *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		if stub.updates.Load() == 2 {
			return nil, errors.New("journal write failed")
		}
		return o.DeepCopy(), nil
	}
	var observed []Event
	s.e.observe = func(ev Event) { observed = append(observed, ev) }
	result := &Result{Generation: 3}
	err := s.e.step(work, s, result, "dependencies/start", func(context.Context) error { return nil })
	if errorCode(err) != "engine.lock_lost" || result.Phase != "failed" || len(result.Events) != 1 || result.Events[0].Code != "engine.lock_lost" || result.Events[0].State != "failed" {
		t.Fatalf("completion failure reported as success: err %v result %+v", err, result)
	}
	if len(observed) != 2 || observed[1].State != "failed" || observed[1].Code != "engine.lock_lost" {
		t.Fatalf("terminal observer event: %+v", observed)
	}
}

func TestJournalWriteIsBoundedByExistingLease(t *testing.T) {
	stub := &journalStub{}
	work, s := journalSession(t, stub)
	lease := time.Now().Add(100 * time.Millisecond)
	s.record.LeaseUntil = lease
	stub.update = func(ctx context.Context, _ *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(lease) {
			t.Errorf("journal deadline %s, want existing lease %s", deadline, lease)
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := s.save(context.Background()); errorCode(err) != "engine.lock_lost" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired write: %v", err)
	}
	if work.Err() == nil || !s.record.LeaseUntil.Equal(lease) {
		t.Fatal("failed write extended the local lease or left work active")
	}
}

type closeLeaseStub struct {
	*journalStub
	current *unstructured.Unstructured
}

func (s *closeLeaseStub) Get(context.Context, Resource, string, string) (*unstructured.Unstructured, error) {
	return s.current.DeepCopy(), nil
}

func TestClosingJournalCannotReleaseBeyondItsLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holder     string
		expired    bool
		wantWrites int32
	}{
		{"active holder is bounded by its lease", "holder", false, 1},
		{"expired holder cannot release", "holder", true, 0},
		{"another holder cannot be released", "another-holder", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &journalStub{}
			_, s := journalSession(t, journal)
			s.stopped = make(chan struct{})
			close(s.stopped)
			leaseUntil := time.Now().UTC().Add(100 * time.Millisecond)
			if tc.expired {
				leaseUntil = leaseUntil.Add(-time.Second)
			}
			current := s.object.DeepCopy()
			b, err := json.Marshal(record{Holder: tc.holder, LeaseUntil: leaseUntil})
			if err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedField(current.Object, string(b), "data", "record"); err != nil {
				t.Fatal(err)
			}
			s.e.cluster = &closeLeaseStub{journalStub: journal, current: current}
			journal.update = func(ctx context.Context, _ *unstructured.Unstructured) (*unstructured.Unstructured, error) {
				if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(leaseUntil) {
					t.Errorf("release deadline %s, want existing lease %s", deadline, leaseUntil)
				}
				<-ctx.Done()
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Errorf("release did not expire at the ownership deadline: %v", ctx.Err())
				}
				return nil, ctx.Err()
			}
			s.close()
			if journal.updates.Load() != tc.wantWrites {
				t.Fatalf("release writes %d, want %d", journal.updates.Load(), tc.wantWrites)
			}
		})
	}
}

func TestStoppingRenewalKeepsDestroyContext(t *testing.T) {
	stub := &journalStub{}
	work, s := journalSession(t, stub)
	started := make(chan struct{})
	stub.update = func(ctx context.Context, _ *unstructured.Unstructured) (*unstructured.Unstructured, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	renew, cancel := context.WithCancel(work)
	done := make(chan error, 1)
	go func() { done <- s.save(renew) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || work.Err() != nil {
		t.Fatalf("stopping renewal cancelled destroy: err %v cause %v", err, context.Cause(work))
	}
}

type destroyLeaseStub struct {
	*journalStub
	blockGet, blockDelete bool
	deletes               int
	leaseUntil            time.Time
	t                     *testing.T
}

func (s *destroyLeaseStub) List(context.Context, Resource, string, string) ([]unstructured.Unstructured, error) {
	return nil, nil
}

func (s *destroyLeaseStub) Get(ctx context.Context, _ Resource, _, name string) (*unstructured.Unstructured, error) {
	if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(s.leaseUntil) {
		s.t.Errorf("namespace Get is not bounded by lease: %s", deadline)
	}
	if s.blockGet {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	o := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Namespace"}}
	o.SetName(name)
	o.SetLabels(map[string]string{render.LabelEnv: "preview"})
	return o, nil
}

func (s *destroyLeaseStub) Delete(ctx context.Context, _ Resource, _ *unstructured.Unstructured) error {
	s.deletes++
	if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(s.leaseUntil) {
		s.t.Errorf("namespace Delete is not bounded by lease: %s", deadline)
	}
	if s.blockDelete {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (s *destroyLeaseStub) Wait(ctx context.Context, _ Resource, _ *unstructured.Unstructured, _ bool) error {
	if deadline, ok := ctx.Deadline(); !ok || !deadline.After(s.leaseUntil) {
		s.t.Errorf("deletion watch cannot outlive lease: %s", deadline)
	}
	return nil
}

func TestDestroyNamespaceMutationIsBoundedByLease(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		blockGet, blockDelete bool
		wantDeletes           int
	}{
		{"Get times out before Delete", true, false, 0},
		{"Delete deadline is ownership deadline", false, true, 1},
		{"read-only deletion watch can run longer", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &journalStub{}
			work, s := journalSession(t, journal)
			stub := &destroyLeaseStub{journalStub: journal, blockGet: tc.blockGet, blockDelete: tc.blockDelete, t: t}
			s.e.cluster = stub
			s.renewCancel = func() {}
			s.stopped = make(chan struct{})
			close(s.stopped)
			s.e.observe = func(ev Event) {
				if ev.Stage == "namespace" {
					// Simulate very little ownership time remaining when renewal stops.
					stub.leaseUntil = time.Now().Add(100 * time.Millisecond)
					s.record.LeaseUntil = stub.leaseUntil
				}
			}
			_, err := s.e.destroyContents(work, s, &Result{Generation: 3}, "preview", map[string]string{render.LabelEnv: "preview"})
			if tc.blockGet || tc.blockDelete {
				if errorCode(err) != "engine.lock_lost" {
					t.Fatalf("expired namespace mutation: %v", err)
				}
			} else if err != nil {
				t.Fatalf("deletion watch: %v", err)
			}
			if stub.deletes != tc.wantDeletes {
				t.Fatalf("namespace Delete calls %d, want %d", stub.deletes, tc.wantDeletes)
			}
		})
	}
}
