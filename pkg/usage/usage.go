// Package usage defines the canonical per-request usage row and the Sink
// abstraction that delivers rows to a downstream destination (stdout in
// Phase 1, BigQuery in Phase 2).
package usage

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Row is the canonical schema for a single gateway request. JSON tag names
// are snake_case so the same struct can be encoded directly into BigQuery
// rows in Phase 2 without a separate mapping layer.
type Row struct {
	RequestID      string    `json:"request_id"`
	Timestamp      time.Time `json:"timestamp"`
	Email          string    `json:"email"`
	Model          string    `json:"model"`
	ModelIDPinned  string    `json:"model_id_pinned"`
	InputTokens    int64     `json:"input_tokens"`
	OutputTokens   int64     `json:"output_tokens"`
	CacheCreate5m  int64     `json:"cache_create_5m"`
	CacheCreate1h  int64     `json:"cache_create_1h"`
	CacheRead      int64     `json:"cache_read"`
	LatencyMS      int64     `json:"latency_ms"`
	StatusCode     int       `json:"status_code"`
	GatewayVersion string    `json:"gateway_version"`
	Region         string    `json:"region"`
	StreamComplete bool      `json:"stream_complete"`
}

// Sink delivers usage rows to a downstream destination. Implementations must
// be safe for concurrent use and must not block the request hot path on
// remote I/O.
type Sink interface {
	// Write records a single usage row. Implementations should treat the
	// call as fire-and-forget; errors must not propagate back to the caller.
	Write(ctx context.Context, r Row)
}

// StdoutSink writes one JSON-encoded Row per line to an io.Writer (typically
// os.Stdout). Writes are serialized with a mutex so concurrent callers cannot
// interleave bytes into a single line.
type StdoutSink struct {
	W  io.Writer
	mu sync.Mutex
}

// Write JSON-encodes r and emits it as a single line. Encoding or write
// errors are intentionally swallowed: usage logging must never affect the
// caller's request path.
func (s *StdoutSink) Write(_ context.Context, r Row) {
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.W.Write(b)
	_, _ = s.W.Write([]byte("\n"))
}
