# Claude Code Gateway (ccgw)

A self-hosted gateway that routes [Claude Code](https://claude.ai/code) traffic through Google Vertex AI for cost attribution, usage analytics, and centralised governance. Phase 1 is a single Go binary that proxies requests, injects an ADC bearer token, and writes one JSON usage row per request to stdout. SSE streaming is preserved end-to-end (`FlushInterval: -1`) so Claude Code feels native.

## Requirements

- Go 1.23+
- A Google Cloud project with Vertex AI enabled, the Anthropic Claude models opted in via Model Garden, and billing attached.
- The [Google Cloud SDK](https://cloud.google.com/sdk/docs/install) (`gcloud`) installed locally.
- Application Default Credentials on the machine running the gateway.

## First-time GCP setup

Skip this section if you already have a project with Vertex AI + Anthropic models enabled.

### 1. Create the project

```bash
gcloud projects create <PROJECT_ID> --name="Claude Code Gateway"
gcloud config set project <PROJECT_ID>
```

`<PROJECT_ID>` must be globally unique across all of GCP, 6–30 chars, lowercase letters/digits/hyphens.

### 2. Attach a billing account

Vertex AI calls cost money, so the project needs billing. List the accounts you can use, then link one:

```bash
gcloud billing accounts list
gcloud billing projects link <PROJECT_ID> --billing-account=<BILLING_ACCOUNT_ID>
```

### 3. Enable the Vertex AI API

```bash
gcloud services enable aiplatform.googleapis.com --project=<PROJECT_ID>
```

### 4. Opt in to Anthropic models in Model Garden

The Claude models are opt-in **per project** and the opt-in must be done through the web console — there is no `gcloud` equivalent.

1. Open <https://console.cloud.google.com/vertex-ai/model-garden?project=PROJECT_ID> (substitute your project).
2. Filter by **Anthropic** or search for **Claude**.
3. Click each model you intend to use (e.g. *Claude Opus 4.7*, *Claude Sonnet 4.6*, *Claude Haiku 4.5*).
4. On each model card click **Enable**, then accept Anthropic's terms (only required the first time per project).

### 5. Grant yourself the Vertex AI user role

```bash
gcloud projects add-iam-policy-binding <PROJECT_ID> \
  --member="user:<YOUR_EMAIL>" \
  --role="roles/aiplatform.user"
```

`roles/aiplatform.user` is the minimum needed for inference. Use a service account binding here instead of `user:` if the gateway will eventually run under a service account.

## Authentication

The gateway uses Application Default Credentials for upstream calls. Set it up once:

```bash
gcloud auth application-default login
```

This opens a browser. The credentials are written to `~/.config/gcloud/application_default_credentials.json` and auto-refreshed by the gateway for the process lifetime.

## Configuration

All configuration is via environment variables.

| Variable                 | Default   | Description                                                                    |
| ------------------------ | --------- | ------------------------------------------------------------------------------ |
| `CCGW_VERTEX_PROJECT_ID` | *(required)* | GCP project that hosts the Vertex AI quota.                                  |
| `CCGW_REGION`            | `global`  | Vertex AI region. `global` maps to `aiplatform.googleapis.com`; anything else maps to `<region>-aiplatform.googleapis.com`. |
| `CCGW_LISTEN_ADDR`       | `:8080`   | TCP address to listen on.                                                      |
| `CCGW_WRITE_TIMEOUT`     | `30m`     | HTTP server write timeout. Claude Code sessions stream for a long time, so this is generous. |
| `CCGW_LOG_USAGE_STDOUT`  | `true`    | Reserved for Phase 2; currently the stdout sink is always on.                  |

## Running the gateway

```bash
export CCGW_VERTEX_PROJECT_ID=<PROJECT_ID>
export CCGW_REGION=global
go run ./cmd/ccgateway
```

Expect:

```
ccgw listening on :8080, upstream=aiplatform.googleapis.com region=global
```

Leave this terminal running — every usage row will be printed here as Claude Code makes requests.

## Pointing Claude Code at the gateway

In a **second** terminal:

```bash
export CLAUDE_CODE_USE_VERTEX=1
export CLOUD_ML_REGION=global
export ANTHROPIC_VERTEX_PROJECT_ID=<PROJECT_ID>
export VERTEX_BASE_URL=http://localhost:8080
claude
```

Claude Code v2.1.121 and newer also honour `ANTHROPIC_VERTEX_BASE_URL`.

## Smoke test

Before running real traffic, sanity-check the upstream:

```bash
# ADC is alive
gcloud auth application-default print-access-token >/dev/null && echo "ADC OK"

# Vertex AI API is enabled in the target project
gcloud services list --enabled --project=<PROJECT_ID> \
  --filter="config.name:aiplatform.googleapis.com"

# Principal can list Anthropic models in the project (confirms IAM + Model Garden opt-in)
curl -sS \
  -H "Authorization: Bearer $(gcloud auth application-default print-access-token)" \
  -H "x-goog-user-project: <PROJECT_ID>" \
  "https://aiplatform.googleapis.com/v1/projects/<PROJECT_ID>/locations/us-east5/publishers/anthropic/models" \
  | head -40
```

Then start the gateway (above), point a second Claude Code at it (above), and ask it something trivial like *"what is 2+2"*. You should see:

- The response stream feels native — no multi-second pauses while the client appears to hang. (If it pauses in 1–5s chunks, `FlushInterval` isn't taking effect.)
- The gateway terminal prints one JSON usage row when the stream completes, with non-zero `input_tokens` / `output_tokens` and `"stream_complete": true`.

## Troubleshooting

| Symptom | Cause | Fix |
| ------- | ----- | --- |
| `config: CCGW_VERTEX_PROJECT_ID is required` at startup | Env var not exported in the gateway's shell | `export CCGW_VERTEX_PROJECT_ID=<PROJECT_ID>` before `go run` |
| `401 Unauthorized` from upstream | ADC token missing or expired | `gcloud auth application-default login` |
| `403 Permission denied` from upstream | Principal lacks `roles/aiplatform.user`, or Anthropic models not yet enabled in Model Garden for this project | Re-run the IAM binding in Step 5; re-check Model Garden opt-in for the specific model |
| `404 Publisher model not found` | The model isn't offered in the chosen region | Try `CCGW_REGION=us-east5` and re-export `CLOUD_ML_REGION=us-east5` for Claude Code |
| Response trickles in 1–5s chunks | Something is buffering the SSE stream | Confirm the gateway is on `FlushInterval: -1` (it is, by default); check there's no reverse-proxy in front of it |
| Usage row prints with zero tokens | Stream parse silently failed, or the response wasn't `text/event-stream` | Inspect `status_code` and `stream_complete` in the row; non-SSE responses are logged with token counts at zero by design |

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

## Bazel

Both `go build` and Bazel work. The `BUILD.bazel` files are generated by [gazelle](https://github.com/bazelbuild/bazel-gazelle) from `go.mod`; do not edit them by hand.

```bash
bazel build //...
bazel test //...

# After adding a new Go file or changing imports:
bazel run //:gazelle

# After adding a new module to go.mod:
bazel mod tidy
```

## Roadmap

- **Phase 1 (here):** local single-binary gateway, stdout usage rows.
- **Phase 2:** BigQuery sink, Cloud Run deployment config, accurate cache-TTL attribution.
- **Phase 3:** caller-side OAuth bearer validation; populate `email` per request.
- **Phase 4:** stop-loss job, per-user budgets, dashboards.

## License

Apache 2.0. See [LICENSE](LICENSE).
