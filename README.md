# Claude Code Gateway (ccgw)

A self-hosted gateway that routes [Claude Code](https://claude.ai/code) traffic through Google Vertex AI for cost attribution, usage analytics, and centralised governance. Phase 1 is a single Go binary that proxies requests, injects an ADC bearer token, and writes one JSON usage row per request to stdout. SSE streaming is preserved end-to-end (`FlushInterval: -1`) so Claude Code feels native.

## Requirements

- Go 1.23+
- A Google Cloud project with Vertex AI enabled and the Claude models you intend to call available in your chosen region.
- Application Default Credentials on the machine running the gateway.

## Authentication

The gateway uses ADC for upstream calls. Set it up once:

```bash
gcloud auth application-default login
```

The credentials must have permission to call Vertex AI in your project (`roles/aiplatform.user` is sufficient).

## Configuration

All configuration is via environment variables.

| Variable                 | Default   | Description                                                                    |
| ------------------------ | --------- | ------------------------------------------------------------------------------ |
| `CCGW_VERTEX_PROJECT_ID` | *(required)* | GCP project that hosts the Vertex AI quota.                                  |
| `CCGW_REGION`            | `global`  | Vertex AI region. `global` maps to `aiplatform.googleapis.com`; anything else maps to `<region>-aiplatform.googleapis.com`. |
| `CCGW_LISTEN_ADDR`       | `:8080`   | TCP address to listen on.                                                      |
| `CCGW_WRITE_TIMEOUT`     | `30m`     | HTTP server write timeout. Claude Code sessions stream for a long time, so this is generous. |
| `CCGW_LOG_USAGE_STDOUT`  | `true`    | Reserved for Phase 2; currently the stdout sink is always on.                  |

## Running

```bash
export CCGW_VERTEX_PROJECT_ID=my-gcp-project
go run ./cmd/ccgateway
```

You should see:

```
ccgw listening on :8080, upstream=aiplatform.googleapis.com region=global
```

## Pointing Claude Code at the gateway

```bash
export CLAUDE_CODE_USE_VERTEX=1
export CLOUD_ML_REGION=global
export ANTHROPIC_VERTEX_PROJECT_ID=my-gcp-project
export VERTEX_BASE_URL=http://localhost:8080
claude
```

Claude Code v2.1.121 and newer also honour `ANTHROPIC_VERTEX_BASE_URL`.

## What's logged

One JSON object per request, written to stdout when the response stream completes (or immediately, for non-SSE responses). Field names are snake_case so the same schema can flow into BigQuery in Phase 2 without remapping.

```json
{
  "request_id": "0a3f8c8e4b6a4d6f9d2c5b8e1f3a4c7d",
  "timestamp": "2026-05-26T12:34:56.789Z",
  "email": "",
  "model": "claude-opus-4-7",
  "model_id_pinned": "claude-opus-4-7@20251022",
  "input_tokens": 1234,
  "output_tokens": 567,
  "cache_create_5m": 0,
  "cache_create_1h": 0,
  "cache_read": 4096,
  "latency_ms": 312,
  "status_code": 200,
  "gateway_version": "phase1-dev",
  "region": "global",
  "stream_complete": true
}
```

`email` will stay empty until Phase 3 adds caller-side OAuth validation. `latency_ms` is time-to-first-byte (when response headers arrive), not total stream duration — that's the latency the developer actually feels.

## Tests

```bash
go test ./... -race
go vet ./...
```

## Roadmap

- **Phase 1 (here):** local single-binary gateway, stdout usage rows.
- **Phase 2:** BigQuery sink, Cloud Run deployment config, accurate cache-TTL attribution.
- **Phase 3:** caller-side OAuth bearer validation; populate `email` per request.
- **Phase 4:** stop-loss job, per-user budgets, dashboards.

## License

Apache 2.0. See [LICENSE](LICENSE).
