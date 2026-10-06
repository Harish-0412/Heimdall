package engine

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type record struct {
	Generation    int64     `json:"generation"`
	Digest        string    `json:"digest"`
	Phase         string    `json:"phase"`
	Operation     string    `json:"operation"`
	ResetNonce    int64     `json:"resetNonce,omitempty"`
	ResetComplete bool      `json:"resetComplete,omitempty"`
	Holder        string    `json:"holder,omitempty"`
	LeaseUntil    time.Time `json:"leaseUntil"`
	Events        []Event   `json:"events,omitempty"`
}
type session struct {
	mu          sync.Mutex
	e           *Engine
	object      *unstructured.Unstructured
	record      record
	work        context.Context
	cancel      context.CancelCauseFunc
	renewCancel context.CancelFunc
	stopped     chan struct{}
	holder      string
}

const leaseDuration = 60 * time.Second

func decodeRecord(o *unstructured.Unstructured) (record, error) {
	var r record
	err := json.Unmarshal([]byte(str(o, "data", "record")), &r)
	return r, err
}
func (s *session) save(ctx context.Context) error {
	if cause := context.Cause(s.work); cause != nil {
		return cause
	}
	now := time.Now().UTC()
	if !now.Before(s.record.LeaseUntil) {
		return s.lose(nil)
	}
	// Never renew or mutate after the lease could have been claimed by
	// another process. A slow API request is bounded by the existing lease.
	write, cancel := context.WithDeadline(ctx, s.record.LeaseUntil)
	defer cancel()
	rec := s.record
	rec.Holder = s.holder
	rec.LeaseUntil = now.Add(leaseDuration)
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	o := s.object.DeepCopy()
	if err = unstructured.SetNestedField(o.Object, string(b), "data", "record"); err != nil {
		return err
	}
	updated, err := s.e.cluster.Update(write, configmaps, o)
	if err != nil {
		// Destroy intentionally stops its renewal child before deleting the
		// journal. Cancelling that child must not cancel the deletion watch.
		if errors.Is(err, context.Canceled) && ctx.Err() == context.Canceled && s.work.Err() == nil {
			return err
		}
		return s.lose(err)
	}
	if !time.Now().Before(s.record.LeaseUntil) {
		return s.lose(nil)
	}
	s.record.Holder, s.record.LeaseUntil = rec.Holder, rec.LeaseUntil
	s.object = updated
	return nil
}

func (s *session) lose(cause error) error {
	err := failure("engine.lock_lost", "operation journal ownership was lost; retry after the active operation finishes", cause)
	s.cancel(err)
	return err
}
func (e *Engine) acquire(ctx context.Context, ns string, labels map[string]string) (context.Context, *session, error) {
	holder := rand.Text()
	for {
		o, err := e.cluster.Get(ctx, configmaps, ns, journalName)
		var r record
		if apierrors.IsNotFound(err) {
			r = record{Holder: holder, LeaseUntil: time.Now().UTC().Add(leaseDuration), Phase: "pending"}
			b, _ := json.Marshal(r)
			o = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": journalName, "namespace": ns}, "data": map[string]any{"record": string(b)}}}
			o.SetLabels(labels)
			o, err = e.cluster.Create(ctx, configmaps, o)
			if apierrors.IsAlreadyExists(err) {
				continue
			}
		} else if err == nil {
			if !owned(o, labels) {
				return nil, nil, failure("engine.ownership", "operation journal belongs to another environment", nil)
			}
			r, err = decodeRecord(o)
			if err != nil {
				return nil, nil, failure("engine.journal_invalid", "operation journal is invalid", err)
			}
			if r.Holder != "" && time.Now().Before(r.LeaseUntil) {
				return nil, nil, failure("engine.busy", "another operation is active for this environment", nil)
			}
			r.Holder = holder
			r.LeaseUntil = time.Now().UTC().Add(leaseDuration)
			b, _ := json.Marshal(r)
			_ = unstructured.SetNestedField(o.Object, string(b), "data", "record")
			o, err = e.cluster.Update(ctx, configmaps, o)
			if apierrors.IsConflict(err) {
				continue
			}
		}
		if err != nil {
			return nil, nil, failure("engine.journal", "cannot acquire operation journal", err)
		}
		work, cancel := context.WithCancelCause(ctx)
		s := &session{e: e, object: o, record: r, work: work, cancel: cancel, stopped: make(chan struct{}), holder: holder}
		renewCtx, renewCancel := context.WithCancel(work)
		s.renewCancel = renewCancel
		go s.renew(renewCtx)
		return work, s, nil
	}
}
func (s *session) renew(ctx context.Context) {
	defer close(s.stopped)
	ticker := time.NewTicker(leaseDuration / 4)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			renew, cancel := context.WithTimeout(ctx, leaseDuration/4)
			s.mu.Lock()
			err := s.save(renew)
			s.mu.Unlock()
			cancel()
			if err != nil {
				return
			}
		}
	}
}
func (s *session) close() {
	s.cancel(nil)
	<-s.stopped
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.e.cluster.Get(ctx, configmaps, s.object.GetNamespace(), journalName)
	if err != nil {
		return
	}
	r, err := decodeRecord(current)
	if err != nil || r.Holder != s.holder || !time.Now().Before(r.LeaseUntil) {
		return
	}
	leaseUntil := r.LeaseUntil
	r.Holder = ""
	r.LeaseUntil = time.Time{}
	b, _ := json.Marshal(r)
	_ = unstructured.SetNestedField(current.Object, string(b), "data", "record")
	// Releasing ownership is also a journal mutation. It must complete
	// within the lease we just verified, even when close's timeout is longer.
	release, releaseCancel := context.WithDeadline(ctx, leaseUntil)
	defer releaseCancel()
	_, _ = s.e.cluster.Update(release, configmaps, current)
}
func (s *session) begin(ctx context.Context, generation int64, digest, operation string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation < s.record.Generation {
		return failure("engine.stale_generation", "generation is older than the accepted specification", nil)
	}
	// Destroy is authorized by immutable namespace ownership, and must not
	// depend on still having deployment config/data. Apply/reset still require
	// the exact deployment digest when reusing a generation.
	if operation != "destroy" && generation == s.record.Generation && s.record.Digest != "" && digest != s.record.Digest {
		return failure("engine.generation_conflict", "changed specification requires a new generation", nil)
	}
	if operation == "reset" && (generation != s.record.Generation || s.record.Phase != "ready" && s.record.Operation != "reset") {
		return failure("engine.not_ready", "reset requires a successfully applied current generation", nil)
	}
	if operation == "apply" && generation == s.record.Generation && s.record.Operation == "reset" && s.record.ResetNonce > 0 && !s.record.ResetComplete {
		return failure("engine.reset_incomplete", "retry the incomplete reset nonce before reapplying this generation", nil)
	}
	if generation > s.record.Generation {
		s.record.ResetNonce = 0
		s.record.ResetComplete = false
	}
	s.record.Generation = generation
	s.record.Digest = digest
	s.record.Operation = operation
	s.record.Phase = map[string]string{"apply": "applying", "reset": "resetting", "destroy": "destroying"}[operation]
	return s.save(ctx)
}
func (s *session) event(ctx context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record.Events = append(s.record.Events, event)
	// A bounded recent journal. The caller's observer can export the full stream.
	if len(s.record.Events) > 128 {
		s.record.Events = s.record.Events[len(s.record.Events)-128:]
	}
	s.record.Phase = event.State
	if err := s.save(ctx); err != nil {
		return err
	}
	if s.e.observe != nil {
		s.e.observe(event)
	}
	return nil
}
