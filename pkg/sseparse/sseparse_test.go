// Copyright 2026 Bjorn Leffler
// SPDX-License-Identifier: Apache-2.0

package sseparse

import (
	"io"
	"strings"
	"testing"
	"time"
)

// readAll drains the wrapped body and returns its bytes. The tests use
// this to mirror what a real HTTP client would do.
func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return string(b)
}

// resultWithTimeout returns parser.Result but fails the test if Result
// blocks for more than the supplied duration. This catches goroutine leaks
// and missing pipe-close paths.
func resultWithTimeout(t *testing.T, p *Parser, d time.Duration) Usage {
	t.Helper()
	resultCh := make(chan Usage, 1)
	go func() { resultCh <- p.Result() }()
	select {
	case u := <-resultCh:
		return u
	case <-time.After(d):
		t.Fatal("Result blocked past timeout — parser likely deadlocked")
		return Usage{}
	}
}

// TestHappyPath exercises the most common stream shape: message_start,
// one or more message_delta updates, then message_stop.
func TestHappyPath(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":42,"cache_read_input_tokens":7}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":10}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":25}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := NewParser()
	body := p.Wrap(io.NopCloser(strings.NewReader(stream)))
	got := readAll(t, body)
	if got != stream {
		t.Errorf("forwarded bytes differ from input (got %d bytes, want %d)", len(got), len(stream))
	}

	u := resultWithTimeout(t, p, 2*time.Second)
	if u.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q", u.Model)
	}
	if u.InputTokens != 42 {
		t.Errorf("InputTokens = %d", u.InputTokens)
	}
	if u.CacheRead != 7 {
		t.Errorf("CacheRead = %d", u.CacheRead)
	}
	if u.OutputTokens != 25 {
		t.Errorf("OutputTokens = %d (want 25 via max-not-sum)", u.OutputTokens)
	}
	if !u.StreamComplete {
		t.Error("StreamComplete = false, want true")
	}
}

// TestCacheCreationSplit covers the newer cache_creation object with
// explicit 5m and 1h ephemeral buckets.
func TestCacheCreationSplit(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":100,"cache_creation":{"ephemeral_5m_input_tokens":1500,"ephemeral_1h_input_tokens":2500}}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := NewParser()
	body := p.Wrap(io.NopCloser(strings.NewReader(stream)))
	_ = readAll(t, body)

	u := resultWithTimeout(t, p, 2*time.Second)
	if u.CacheCreate5m != 1500 {
		t.Errorf("CacheCreate5m = %d, want 1500", u.CacheCreate5m)
	}
	if u.CacheCreate1h != 2500 {
		t.Errorf("CacheCreate1h = %d, want 2500", u.CacheCreate1h)
	}
}

// TestCacheCreationAggregate covers the older flat cache_creation_input_tokens
// field. All writes must land in the 5m bucket; Phase 2 will refine this by
// inspecting the request body's cache_control TTL.
func TestCacheCreationAggregate(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":100,"cache_creation_input_tokens":4096}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	p := NewParser()
	body := p.Wrap(io.NopCloser(strings.NewReader(stream)))
	_ = readAll(t, body)

	u := resultWithTimeout(t, p, 2*time.Second)
	if u.CacheCreate5m != 4096 {
		t.Errorf("CacheCreate5m = %d, want 4096 (aggregate → 5m bucket)", u.CacheCreate5m)
	}
	if u.CacheCreate1h != 0 {
		t.Errorf("CacheCreate1h = %d, want 0", u.CacheCreate1h)
	}
}

// TestTruncatedStream verifies that a stream cut off mid-event neither
// deadlocks Result() nor corrupts the bytes already forwarded.
func TestTruncatedStream(t *testing.T) {
	// Note: no trailing blank line, no message_stop.
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":11}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":3}}`,
	}, "\n")

	p := NewParser()
	body := p.Wrap(io.NopCloser(strings.NewReader(stream)))
	got := readAll(t, body)
	if got != stream {
		t.Errorf("forwarded bytes differ from truncated input")
	}

	u := resultWithTimeout(t, p, 2*time.Second)
	// message_start dispatches on the blank line that follows it.
	if u.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q (message_start should have dispatched)", u.Model)
	}
	if u.InputTokens != 11 {
		t.Errorf("InputTokens = %d", u.InputTokens)
	}
	// The trailing message_delta has no blank-line terminator; the parser
	// flushes on EOF, so it should still be picked up.
	if u.OutputTokens != 3 {
		t.Errorf("OutputTokens = %d, want 3 (EOF flush of trailing event)", u.OutputTokens)
	}
	if u.StreamComplete {
		t.Error("StreamComplete = true, but stream never sent message_stop")
	}
}

// TestEarlyClose verifies that closing the wrapped body before EOF still
// unblocks Result(). This mirrors a client cancellation mid-stream.
func TestEarlyClose(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"m1","model":"claude-opus-4-7","usage":{"input_tokens":11}}}`,
		``,
		// More data the client never reads.
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":99}}`,
		``,
	}, "\n")

	p := NewParser()
	body := p.Wrap(io.NopCloser(strings.NewReader(stream)))
	// Read a small prefix, then close — simulating client disconnect.
	buf := make([]byte, 32)
	if _, err := body.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Result must still return rather than hang.
	_ = resultWithTimeout(t, p, 2*time.Second)
}
