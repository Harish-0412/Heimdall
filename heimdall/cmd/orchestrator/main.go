package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlruntime"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		slog.Error("orchestrator stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	stopTrace := tracecontext.Initialize()
	defer func() { _ = stopTrace(context.Background()) }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s, err := controlruntime.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	service, err := controlruntime.GitHub(s)
	if err != nil {
		return err
	}
	q, err := controlruntime.Queue(ctx)
	if err != nil {
		return err
	}
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return service.RunQueue(ctx, q) })
	g.Go(func() error {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			if err := service.DispatchOutbox(ctx); err != nil && ctx.Err() == nil {
				slog.Error("outbox retry pending", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
			}
		}
	})
	return g.Wait()
}
