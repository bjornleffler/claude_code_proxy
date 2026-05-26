// Copyright 2026 Bjorn Leffler
// SPDX-License-Identifier: Apache-2.0

// Package sseparse parses an Anthropic-flavoured Server-Sent Events stream
// for usage accounting without buffering or blocking the client byte path.
//
// The Parser.Wrap method returns an io.ReadCloser that the HTTP client reads
// from directly; every byte read from the upstream response is also tee'd
// into a background goroutine that parses message_start, message_delta, and
// message_stop events. Parsing failures are silent: a request that fails to
// parse still produces a Row, just with zero token counts.
package sseparse

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// scannerMaxLineBytes caps a single SSE data: line at 4 MiB. Anthropic
// payloads (especially content_block_delta carrying tool input) can be
// large, but they fit comfortably under this ceiling.
const scannerMaxLineBytes = 4 * 1024 * 1024

// Usage holds the accounting fields extracted from a single Anthropic
// message stream. Zero values are valid (and expected when parsing fails).
type Usage struct {
	Model          string
	InputTokens    int64
	OutputTokens   int64
	CacheCreate5m  int64
	CacheCreate1h  int64
	CacheRead      int64
	StreamComplete bool
}

// Parser parses one Anthropic SSE message stream. A Parser is single-use:
// Wrap must be called at most once per Parser, and Result blocks until the
// parsing goroutine finishes (signalled by EOF or error on the wrapped body).
type Parser struct {
	mu       sync.Mutex
	usage    Usage
	done     chan struct{}
	doneOnce sync.Once
}

// NewParser returns a Parser ready to wrap a response body.
func NewParser() *Parser {
	return &Parser{done: make(chan struct{})}
}

// Wrap returns an io.ReadCloser that the caller reads in place of body.
// Every byte read flows through to the caller unchanged; a duplicate copy
// is fed into a background goroutine that parses usage events. Closing the
// returned reader closes body and unblocks the parser.
func (p *Parser) Wrap(body io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	tee := io.TeeReader(body, pw)

	go p.parse(pr)

	return &teeBody{
		src:     body,
		tee:     tee,
		pw:      pw,
		closing: &sync.Once{},
	}
}

// Result blocks until parsing completes and returns the accumulated Usage.
// It is safe to call from any goroutine; concurrent callers will all see
// the same final value.
func (p *Parser) Result() Usage {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usage
}

// signalDone closes the done channel exactly once. Multiple call sites
// (parser goroutine exit, explicit close before EOF) make idempotency
// important.
func (p *Parser) signalDone() {
	p.doneOnce.Do(func() { close(p.done) })
}

// parse reads the SSE stream from r and dispatches recognised events.
// It always signals done before returning, even on scanner error.
func (p *Parser) parse(r io.Reader) {
	defer p.signalDone()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), scannerMaxLineBytes)

	var (
		eventType string
		dataLines []string
	)

	flush := func() {
		if len(dataLines) > 0 {
			p.dispatch(eventType, strings.Join(dataLines, "\n"))
		}
		eventType = ""
		dataLines = nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		// Blank line terminates the current event.
		if line == "" {
			flush()
			continue
		}
		// Lines starting with ':' are SSE comments / heartbeats.
		if strings.HasPrefix(line, ":") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(rest)
			continue
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			// Per SSE spec, strip a single leading space if present.
			rest = strings.TrimPrefix(rest, " ")
			dataLines = append(dataLines, rest)
			continue
		}
	}
	// Flush any trailing event that wasn't followed by a blank line.
	flush()
}

// dispatch decodes the JSON payload of a single SSE event and updates the
// accumulated Usage. JSON errors are intentionally swallowed.
func (p *Parser) dispatch(event, data string) {
	switch event {
	case "message_start":
		p.handleMessageStart(data)
	case "message_delta":
		p.handleMessageDelta(data)
	case "message_stop":
		p.mu.Lock()
		p.usage.StreamComplete = true
		p.mu.Unlock()
	}
}

// messageStartPayload mirrors the subset of Anthropic's message_start event
// that carries usage accounting. The split cache_creation field is preferred;
// the older flat cache_creation_input_tokens is honoured as a fallback.
type messageStartPayload struct {
	Message struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            *struct {
				Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// handleMessageStart applies a message_start payload to the parser's Usage.
func (p *Parser) handleMessageStart(data string) {
	var ms messageStartPayload
	if err := json.Unmarshal([]byte(data), &ms); err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ms.Message.Model != "" {
		p.usage.Model = ms.Message.Model
	}
	if ms.Message.Usage.InputTokens > 0 {
		p.usage.InputTokens = ms.Message.Usage.InputTokens
	}
	if ms.Message.Usage.CacheReadInputTokens > 0 {
		p.usage.CacheRead = ms.Message.Usage.CacheReadInputTokens
	}
	if ms.Message.Usage.CacheCreation != nil {
		// New split format: explicit 5m and 1h ephemeral buckets.
		p.usage.CacheCreate5m = ms.Message.Usage.CacheCreation.Ephemeral5m
		p.usage.CacheCreate1h = ms.Message.Usage.CacheCreation.Ephemeral1h
	} else if ms.Message.Usage.CacheCreationInputTokens > 0 {
		// Old aggregate format: attribute all writes to the 5m bucket.
		// Phase 2 will inspect the request body's cache_control TTL for
		// accurate attribution.
		p.usage.CacheCreate5m = ms.Message.Usage.CacheCreationInputTokens
	}
}

// messageDeltaPayload mirrors the subset of message_delta carrying usage.
type messageDeltaPayload struct {
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// handleMessageDelta applies a message_delta to the Usage. Both token
// counters are cumulative-to-date, so we keep the running maximum rather
// than summing.
func (p *Parser) handleMessageDelta(data string) {
	var md messageDeltaPayload
	if err := json.Unmarshal([]byte(data), &md); err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if md.Usage.OutputTokens > p.usage.OutputTokens {
		p.usage.OutputTokens = md.Usage.OutputTokens
	}
	if md.Usage.InputTokens > p.usage.InputTokens {
		p.usage.InputTokens = md.Usage.InputTokens
	}
}

// teeBody is the io.ReadCloser returned by Parser.Wrap. It delegates Read
// to a TeeReader so that every byte flowing to the HTTP client is also
// copied to the parser pipe. Close (and any terminal Read error) closes the
// pipe writer so the parser goroutine unblocks and ultimately signals done.
type teeBody struct {
	src     io.ReadCloser
	tee     io.Reader
	pw      *io.PipeWriter
	closing *sync.Once
}

// Read forwards bytes to the caller and tees a copy to the parser. Any
// terminal condition (EOF or error) closes the pipe so the parser exits.
func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.tee.Read(p)
	if err != nil {
		t.closePipe()
	}
	return n, err
}

// Close closes the upstream body and the parser pipe. It is safe to call
// multiple times; only the first call has effect.
func (t *teeBody) Close() error {
	t.closePipe()
	return t.src.Close()
}

// closePipe closes the pipe writer exactly once. The parser sees EOF on
// the read side, finishes any partially-buffered event, and then signals
// done from its own defer. Closing here must not signal done directly,
// because the parser may still be mid-dispatch on the last event.
func (t *teeBody) closePipe() {
	t.closing.Do(func() {
		_ = t.pw.Close()
	})
}
