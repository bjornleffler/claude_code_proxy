// Copyright 2026 Bjorn Leffler
// SPDX-License-Identifier: Apache-2.0

// Command ccgateway runs the Claude Code Gateway: a reverse proxy that
// forwards Claude Code traffic through Google Vertex AI while logging usage
// rows to stdout.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bjornleffler/claude_code_proxy/pkg/config"
	"github.com/bjornleffler/claude_code_proxy/pkg/usage"
	"github.com/bjornleffler/claude_code_proxy/pkg/vertex"
)

// shutdownTimeout is the grace period for in-flight requests when SIGINT
// or SIGTERM arrives. Long enough that an interactive Claude Code turn can
// finish naturally; short enough that container orchestrators don't escalate
// to SIGKILL.
const shutdownTimeout = 30 * time.Second

// main wires together config, sink, proxy, and HTTP server, then blocks on
// ListenAndServe until a signal triggers graceful shutdown.
func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var sink usage.Sink = &usage.StdoutSink{W: os.Stdout}
	proxy, err := vertex.New(cfg, sink)
	if err != nil {
		log.Fatalf("proxy: %v", err)
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: proxy,
		// WriteTimeout is generous because Claude Code sessions stream for
		// a long time; ReadTimeout is unset for the same reason.
		WriteTimeout: cfg.WriteTimeout,
		ReadTimeout:  0,
	}
	log.Printf("ccgw listening on %s, upstream=%s region=%s",
		cfg.ListenAddr, cfg.UpstreamHost(), cfg.Region)

	// Graceful shutdown on SIGINT/SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
