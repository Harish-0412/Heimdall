package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/heimdall-dev/heimdall/internal/controlapi"
	"github.com/heimdall-dev/heimdall/internal/controlruntime"
	"github.com/heimdall-dev/heimdall/internal/githubapp"
	"github.com/heimdall-dev/heimdall/internal/tracecontext"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		slog.Error("API stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	stopTrace := tracecontext.Initialize()
	defer func() { _ = stopTrace(context.Background()) }()
	listen := flag.String("listen", ":8080", "internal HTTP address behind the HTTPS ingress")
	local := flag.Bool("local", false, "allow bounded in-memory logs for local development")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s, err := controlruntime.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close()
	opts := controlapi.Options{}
	if address := os.Getenv("HEIMDALL_REDIS_URL"); address != "" {
		o, err := redis.ParseURL(address)
		if err != nil {
			return errors.New("invalid ephemeral Redis address")
		}
		r := redis.NewClient(o)
		defer r.Close()
		if r.Ping(ctx).Err() != nil {
			return errors.New("ephemeral Redis unavailable")
		}
		broker := controlapi.NewRedisLogBroker(r)
		if err := broker.Ready(ctx); err != nil {
			return errors.New("ephemeral Redis persistence must be disabled and verifiable")
		}
		opts.LogBroker = broker
	} else if !*local {
		return errors.New("HEIMDALL_REDIS_URL is required outside local mode")
	}
	if os.Getenv("HEIMDALL_GITHUB_APP_ID") != "" {
		service, err := controlruntime.GitHub(s)
		if err != nil {
			return err
		}
		opts.BuildCallback = service.BuildHandler()
		if os.Getenv("HEIMDALL_SQS_URL") != "" {
			q, err := controlruntime.Queue(ctx)
			if err != nil {
				return err
			}
			secret, err := controlruntime.Secret("HEIMDALL_GITHUB_WEBHOOK_SECRET_FILE", 32)
			if err != nil {
				return err
			}
			opts.GitHubWebhook = &githubapp.Webhook{Secret: secret, Queue: q}
		}
	}
	server := &http.Server{Addr: *listen, Handler: controlapi.New(s, opts).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	slog.Info("control API listening", "address", *listen)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("HTTP listener failed")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}
