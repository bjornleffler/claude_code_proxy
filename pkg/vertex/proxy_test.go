// Copyright 2026 Bjorn Leffler
// SPDX-License-Identifier: Apache-2.0

package vertex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bjornleffler/claude_code_proxy/pkg/config"
	"github.com/bjornleffler/claude_code_proxy/pkg/usage"
)

// stubTokenSource is a TokenSource that returns a fixed token. The proxy
// test captures the token from the upstream's Authorization header to
// confirm the director injected it.
type stubTokenSource struct{ tok string }

// Token returns the canned token.
func (s stubTokenSource) Token() (string, error) { return s.tok, nil }

// capturingSink records every row written to it and signals on a channel
// the first time it receives a row. Tests poll the channel to know when
// the streaming parse goroutine has finished.
type capturingSink struct {
	mu   sync.Mutex
	rows []usage.Row
	rcvd chan usage.Row
}

// newCapturingSink constructs an empty capturingSink ready for use.
func newCapturingSink() *capturingSink {
	return &capturingSink{rcvd: make(chan usage.Row, 4)}
}

// Write records r and non-blockingly publishes it on the signal channel.
func (c *capturingSink) Write(_ context.Context, r usage.Row) { //nolint:unused
	c.mu.Lock()
	c.rows = append(c.rows, r)
	c.mu.Unlock()
	select {
	case c.rcvd <- r:
	default:
	}
}

// canned is the SSE stream the fake upstream serves in the happy-path test.
// Keeping it as a single const makes byte-for-byte comparison trivial.
const canned = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":42,"cache_read_input_tokens":7}}}` + "\n" +
	"\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","usage":{"output_tokens":25}}` + "\n" +
	"\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n" +
	"\n"

// TestProxyStreamsSSE verifies the core proxy contract: bytes flow through
// unchanged, the bearer token is injected, and a usage row appears with
// the parsed token counts after the body is read.
func TestProxyStreamsSSE(t *testing.T) {
	const wantToken = "Bearer test-token-xyz"

	gotAuth := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case gotAuth <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, canned)
	}))
	t.Cleanup(upstream.Close)

	upURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	cfg := &config.Config{Region: "global", VertexProjectID: "test-project"}
	sink := newCapturingSink()
	proxy := newWithSource(cfg, sink, stubTokenSource{tok: "test-token-xyz"}, upURL.Scheme, upURL.Host)

	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)

	// Use a model-bearing path so ModelIDPinned extraction is exercised too.
	reqURL := proxySrv.URL + "/v1/projects/p/locations/global/publishers/anthropic/models/claude-opus-4-7@20251022:streamRawPredict"
	resp, err := http.Post(reqURL, "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := string(body); got != canned {
		t.Errorf("body bytes differ: got %d bytes, want %d", len(got), len(canned))
	}

	if got := <-gotAuth; got != wantToken {
		t.Errorf("upstream Authorization = %q, want %q", got, wantToken)
	}

	select {
	case row := <-sink.rcvd:
		if row.Model != "claude-opus-4-7" {
			t.Errorf("row.Model = %q", row.Model)
		}
		if row.ModelIDPinned != "claude-opus-4-7@20251022" {
			t.Errorf("row.ModelIDPinned = %q", row.ModelIDPinned)
		}
		if row.InputTokens != 42 {
			t.Errorf("row.InputTokens = %d", row.InputTokens)
		}
		if row.OutputTokens != 25 {
			t.Errorf("row.OutputTokens = %d", row.OutputTokens)
		}
		if row.CacheRead != 7 {
			t.Errorf("row.CacheRead = %d", row.CacheRead)
		}
		if !row.StreamComplete {
			t.Error("row.StreamComplete = false")
		}
		if row.StatusCode != http.StatusOK {
			t.Errorf("row.StatusCode = %d", row.StatusCode)
		}
		if row.Region != "global" {
			t.Errorf("row.Region = %q", row.Region)
		}
		if row.GatewayVersion != gatewayVersion {
			t.Errorf("row.GatewayVersion = %q", row.GatewayVersion)
		}
		if len(row.RequestID) != 32 {
			t.Errorf("row.RequestID = %q (want 32-char hex)", row.RequestID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sink never received a row")
	}
}

// TestProxyNonStreamingLogsRow verifies that a non-SSE response (e.g. an
// error body) still produces a row, with status code populated and token
// counts zero.
func TestProxyNonStreamingLogsRow(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	t.Cleanup(upstream.Close)

	upURL, _ := url.Parse(upstream.URL)
	cfg := &config.Config{Region: "us-east5", VertexProjectID: "p"}
	sink := newCapturingSink()
	proxy := newWithSource(cfg, sink, stubTokenSource{tok: "t"}, upURL.Scheme, upURL.Host)

	proxySrv := httptest.NewServer(proxy)
	t.Cleanup(proxySrv.Close)

	resp, err := http.Get(proxySrv.URL + "/anything")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case row := <-sink.rcvd:
		if row.StatusCode != http.StatusBadRequest {
			t.Errorf("row.StatusCode = %d", row.StatusCode)
		}
		if row.Region != "us-east5" {
			t.Errorf("row.Region = %q", row.Region)
		}
		if row.OutputTokens != 0 {
			t.Errorf("row.OutputTokens = %d (want 0 for non-SSE)", row.OutputTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sink never received a row")
	}
}

// TestExtractPinnedModelID covers the URL-path parser for the pinned model.
func TestExtractPinnedModelID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/projects/p/locations/l/publishers/anthropic/models/claude-opus-4-7@20251022:streamRawPredict", "claude-opus-4-7@20251022"},
		{"/v1/.../models/claude-haiku-4-5:rawPredict", "claude-haiku-4-5"},
		{"/v1/.../models/claude-opus-4-7", "claude-opus-4-7"},
		{"/no-model-here", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := extractPinnedModelID(tc.path); got != tc.want {
			t.Errorf("extractPinnedModelID(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}
