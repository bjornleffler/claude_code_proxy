// Copyright 2026 Bjorn Leffler
// SPDX-License-Identifier: Apache-2.0

// Package vertex implements the reverse proxy that forwards Claude Code
// requests to Vertex AI. The proxy injects an Application Default Credentials
// bearer token on each request and tees the response body through an SSE
// parser to emit a usage row per request without buffering the stream.
package vertex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/bjornleffler/claude_code_proxy/pkg/config"
	"github.com/bjornleffler/claude_code_proxy/pkg/sseparse"
	"github.com/bjornleffler/claude_code_proxy/pkg/usage"
)

// gatewayVersion is recorded on every usage row to make later log analysis
// easier when the gateway's behaviour changes.
const gatewayVersion = "phase1-dev"

// cloudPlatformScope is the broad OAuth2 scope Vertex AI accepts.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// TokenSource produces an OAuth2 bearer token to attach to upstream requests.
// It exists so tests can substitute a stub instead of faking ADC.
type TokenSource interface {
	// Token returns a fresh (or cached, auto-refreshed) access token.
	Token() (string, error)
}

// adcTokenSource adapts an oauth2.TokenSource to the TokenSource interface.
// The wrapped source caches and auto-refreshes for the process lifetime.
type adcTokenSource struct {
	ts oauth2.TokenSource
}

// Token returns the current ADC access token.
func (a *adcTokenSource) Token() (string, error) {
	t, err := a.ts.Token()
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

// ctxKey is the unexported type used for per-request values carried through
// the reverse proxy from ServeHTTP into ModifyResponse.
type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyStart
)

// Proxy is an httputil.ReverseProxy specialised for Vertex AI: it injects
// an ADC bearer token, sets the upstream host correctly so TLS SNI works,
// and parses usage out of SSE responses without buffering them.
type Proxy struct {
	rp             *httputil.ReverseProxy
	sink           usage.Sink
	cfg            *config.Config
	ts             TokenSource
	upstreamScheme string
	upstreamHost   string
}

// New constructs a production Proxy that targets the Vertex AI host derived
// from cfg and authenticates using Application Default Credentials.
func New(cfg *config.Config, sink usage.Sink) (*Proxy, error) {
	ctx := context.Background()
	ts, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("default token source: %w", err)
	}
	return newWithSource(cfg, sink, &adcTokenSource{ts: ts}, "https", cfg.UpstreamHost()), nil
}

// newWithSource is the test-friendly constructor: scheme and host are
// supplied directly so a httptest.Server can stand in for Vertex AI and
// the TokenSource can be a stub.
func newWithSource(cfg *config.Config, sink usage.Sink, ts TokenSource, scheme, host string) *Proxy {
	p := &Proxy{
		sink:           sink,
		cfg:            cfg,
		ts:             ts,
		upstreamScheme: scheme,
		upstreamHost:   host,
	}
	p.rp = &httputil.ReverseProxy{
		Director: p.director,
		// FlushInterval -1 forces an immediate flush after every Write, which
		// is essential for native-feeling SSE. Any positive interval would
		// silently buffer the stream in 1+s chunks.
		FlushInterval:  -1,
		ModifyResponse: p.modifyResponse,
	}
	return p
}

// ServeHTTP attaches a request ID and start timestamp to the request context
// and hands it to the wrapped ReverseProxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	ctx := context.WithValue(r.Context(), ctxKeyRequestID, requestID)
	ctx = context.WithValue(ctx, ctxKeyStart, time.Now())
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// director rewrites the outbound request to target the Vertex AI host and
// attaches a fresh ADC bearer token. Any caller-supplied Authorization is
// dropped so a misconfigured client cannot accidentally forward credentials
// to Vertex.
func (p *Proxy) director(r *http.Request) {
	r.URL.Scheme = p.upstreamScheme
	r.URL.Host = p.upstreamHost
	// Setting r.Host here, not just the URL host, ensures the outbound
	// Host header (and therefore TLS SNI) matches the upstream certificate.
	r.Host = p.upstreamHost
	r.Header.Del("Authorization")
	if tok, err := p.ts.Token(); err == nil && tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
}

// modifyResponse runs after headers come back from Vertex but before any
// body bytes flow to the client. For SSE responses it wraps the body so a
// background goroutine can parse usage; for non-SSE responses it logs a
// minimal row immediately.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	ctx := resp.Request.Context()
	requestID, _ := ctx.Value(ctxKeyRequestID).(string)
	start, _ := ctx.Value(ctxKeyStart).(time.Time)

	row := usage.Row{
		RequestID:      requestID,
		Timestamp:      time.Now().UTC(),
		StatusCode:     resp.StatusCode,
		// LatencyMS captures time-to-first-byte, which is what matters for
		// the interactive feel of Claude Code — not total stream duration.
		LatencyMS:      time.Since(start).Milliseconds(),
		GatewayVersion: gatewayVersion,
		Region:         p.cfg.Region,
		ModelIDPinned:  extractPinnedModelID(resp.Request.URL.Path),
	}

	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Non-streaming (e.g. error JSON): nothing to parse, log now.
		p.sink.Write(context.Background(), row)
		return nil
	}

	parser := sseparse.NewParser()
	resp.Body = parser.Wrap(resp.Body)
	go func() {
		u := parser.Result()
		row.Model = u.Model
		row.InputTokens = u.InputTokens
		row.OutputTokens = u.OutputTokens
		row.CacheCreate5m = u.CacheCreate5m
		row.CacheCreate1h = u.CacheCreate1h
		row.CacheRead = u.CacheRead
		row.StreamComplete = u.StreamComplete
		// Use a fresh background context: the request's context is cancelled
		// the moment the client closes the response body.
		p.sink.Write(context.Background(), row)
	}()
	return nil
}

// newRequestID returns a 32-character hex request identifier seeded from
// crypto/rand. Errors are ignored because crypto/rand only fails on
// platforms where the OS RNG is unavailable.
func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// extractPinnedModelID pulls the pinned model ID from a Vertex AI prediction
// URL. The Vertex path is structured as
//
//	/v1/projects/{p}/locations/{l}/publishers/anthropic/models/{model}:{action}
//
// so the model ID lives between "/models/" and the next ':' or '/'. Returns
// the empty string when the path doesn't match.
func extractPinnedModelID(path string) string {
	_, rest, ok := strings.Cut(path, "/models/")
	if !ok {
		return ""
	}
	if j := strings.IndexAny(rest, ":/"); j >= 0 {
		return rest[:j]
	}
	return rest
}
