package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/heimdall-dev/heimdall/internal/engine"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err       error
		code      string
		retryable bool
	}{
		{&engine.Error{Code: "engine.job_failed"}, "engine.job_failed", false},
		{&engine.Error{Code: "engine.busy"}, "engine.busy", true},
		{&engine.Error{Code: "engine.lock_lost", Cause: context.Canceled}, "engine.lock_lost", true},
		{context.Canceled, CodeInterrupted, true},
		{context.DeadlineExceeded, "engine.timeout", true},
		{errors.New("connection refused"), CodeCluster, true},
		{failf(CodeConfigInvalid, false, "x"), CodeConfigInvalid, false},
	}
	for _, c := range cases {
		f := classify(c.err)
		if f.Code != c.code || f.Retryable != c.retryable {
			t.Errorf("classify(%v) = %s/%v, want %s/%v", c.err, f.Code, f.Retryable, c.code, c.retryable)
		}
	}
	if f := classify(errors.New("secret token=abc in an API error")); f.Message == "" || f.Message == "secret token=abc in an API error" {
		t.Error("raw errors must not reach status")
	}
}
