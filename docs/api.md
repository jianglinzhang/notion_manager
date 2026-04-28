# API Usage

[← Back to README](../README.md)

`/v1/messages` accepts Anthropic Messages API payloads and is the only endpoint
intended for external clients. All admin / dashboard surfaces live under
`/admin/*` and `/dashboard/` and use a separate session-cookie auth model
(see [Dashboard](dashboard.md)).

## Authentication

Send the API key the same way you would talk to Anthropic:

```bash
-H "Authorization: Bearer <api_key>"
# or
-H "x-api-key: <api_key>"
```

The accepted key is `proxy.api_key` in `config.yaml` (env override
`PROXY_API_KEY`).

## Standard request

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opus-4.6",
    "max_tokens": 1024,
    "messages": [
      { "role": "user", "content": "Describe the main components of this project." }
    ]
  }'
```

If `model` is omitted, the service falls back to `proxy.default_model`.

## Request headers

| Header | Effect |
| --- | --- |
| `Authorization: Bearer <key>` / `x-api-key: <key>` | API key auth (one is required) |
| `Content-Type: application/json` | Required |
| `Accept: text/event-stream` | Optional; combined with `"stream": true` you get an SSE stream that mirrors Anthropic's event protocol |
| `X-Web-Search: true|false` | Per-request override for web search (default from `proxy.enable_web_search`) |
| `X-Workspace-Search: true|false` | Per-request override for workspace search (default from `proxy.enable_workspace_search`) |

Unknown headers are ignored.

## ASK mode (read-only Notion replies)

Notion's frontend has an **Ask** toggle that disables page edits and tool calls,
producing pure read-only answers. notion-manager exposes the same flag in two
ways:

1. **Global default** – `proxy.ask_mode_default: true` in `config.yaml`, or
   toggled live from the dashboard `Settings` panel.
2. **Per-request suffix** – append `-ask` to the model name. The suffix is
   stripped server-side before model resolution, so any model alias works:

```bash
# Force ASK mode for this single request even if the global default is off
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4.6-ask",
    "messages": [
      { "role": "user", "content": "Summarise this workspace without editing it." }
    ]
  }'
```

The matrix is `OR`-style: ASK mode is on whenever **either** the suffix or the
global default is set. Per-request `-ask` cannot disable the global default; if
you need a hard escape you must clear `ask_mode_default`.

## Search overrides

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -H "X-Web-Search: true" \
  -H "X-Workspace-Search: false" \
  -d '{
    "model": "sonnet-4.6",
    "messages": [
      { "role": "user", "content": "Search for recent information about Go 1.25." }
    ]
  }'
```

## File uploads

Supported media types:

- `image/png`
- `image/jpeg`
- `image/gif`
- `image/webp`
- `application/pdf`
- `text/csv`

```bash
curl http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "sonnet-4.6",
    "max_tokens": 600,
    "messages": [{
      "role": "user",
      "content": [
        {
          "type": "document",
          "source": {
            "type": "base64",
            "media_type": "application/pdf",
            "data": "<base64>"
          }
        },
        {
          "type": "text",
          "text": "Summarize this PDF."
        }
      ]
    }]
  }'
```

The proxy uploads, polls processing, then injects the transcription / image
into the conversation transparently.

## Research mode

Research mode is triggered by model name:

- `researcher`
- `fast-researcher`

```bash
curl -N http://localhost:3000/v1/messages \
  -H "x-api-key: <api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "researcher",
    "stream": true,
    "max_tokens": 16000,
    "thinking": { "type": "enabled", "budget_tokens": 50000 },
    "messages": [
      { "role": "user", "content": "Map common architectural patterns used by Notion AI proxy tools." }
    ]
  }'
```

Research mode is single-turn, ignores file uploads, ignores `tools`, and uses
the longer `proxy.research_timeout` (default `360s`) timeout path.

## Streaming

Set `"stream": true` on the body. The proxy emits the standard Anthropic SSE
sequence (`message_start`, `content_block_*`, `message_delta`, `message_stop`)
plus a `usage_stats` event in the final delta containing the input / output
tokens that were recorded against the serving account.

## Errors

Errors follow Anthropic's shape:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "message": "messages is required"
  }
}
```

Common cases:

- `401 unauthorized` – missing / wrong API key
- `400 invalid_request_error` – malformed JSON, empty `messages`, or unsupported attachment type
- `429 rate_limit_error` – every account in the pool is currently exhausted; the response message describes which quota tripped
- `502 bad_gateway` – upstream Notion error after retries
