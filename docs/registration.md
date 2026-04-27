# Bulk Registration

[← Back to README](../README.md)

notion-manager can provision Notion accounts in bulk through a Provider-pluggable pipeline. Microsoft SSO (consumer MSA) is the first provider and is wired up by default; new OAuth integrations (Google, GitHub, …) plug in by implementing `internal/regjob/providers.Provider`.

This document covers:

- The credential format and how peer mailboxes are paired
- The dashboard "Register" drawer
- The `/admin/register/*` HTTP API
- Job persistence, retry, and SSE event protocol
- The standalone `notion-manager-register` CLI

## Credential format (Microsoft provider)

One account per line, four fields separated by `----`:

```text
<email>----<password>----<client_id>----<refresh_token>
```

- `email` — primary mailbox / Microsoft sign-in
- `password` — MSA password
- `client_id` — Microsoft app registration client id used to drive the OAuth refresh
- `refresh_token` — long-lived MSA refresh token tied to that `client_id`

Empty lines and `#` comment lines are ignored.

### Why pairs matter

Notion's onboarding flow requires email verification on the new tenant. The runner uses **the next row's mailbox** as the verification target for the current row (so row 0 verifies into row 1's inbox, row 1 verifies into row 2's, …). The last row wraps back to the first.

Always paste credentials in **even** counts so every row has a partner. Odd counts technically work — the last/first wrap is just a fallback — but they double the chance of mailbox contention.

## Dashboard drawer

`/dashboard/` → top-right `+ Register` button.

- **Provider** — defaults to Microsoft. Other registered providers show up as tabs once added.
- **Concurrency** — how many login flows run in parallel. Defaults to the provider's `RecommendedConcurrency()` (Microsoft = 1). MSA has aggressive anti-abuse heuristics past ~5 concurrent flows per IP, so keep this low.
- **Proxy (per job)** — optional `http`/`https`/`socks5`/`socks5h` upstream applied to every dial of this job. Empty falls back to the global `proxy.notion_proxy`. Validated up-front; bad schemes are rejected with a 400.
- **Credentials** — paste the bulk text directly.

After pressing **Start**, the drawer streams a live progress list driven by SSE. Failed rows render a red badge with the truncated provider error message; success rows show the new `space_id` / `user_id` and the file path that was written into `accounts/`.

## HTTP API

All endpoints under `/admin/register*` and `/admin/accounts/*` require a valid dashboard session (cookie set by `/dashboard/auth/login`). They return JSON.

### List providers

```http
GET /admin/register/providers
```

```json
{
  "providers": [
    {
      "id": "microsoft",
      "display": "Microsoft",
      "format_hint": "每行一个账号：email----password----client_id----refresh_token",
      "recommended_concurrency": 1,
      "enabled": true
    }
  ]
}
```

### Start a job

```http
POST /admin/register/start
Content-Type: application/json

{
  "provider": "microsoft",
  "input": "<bulk credentials>",
  "concurrency": 1,
  "proxy": "socks5h://127.0.0.1:1080"
}
```

`provider` and `proxy` are optional. `provider` defaults to the first registered provider (currently `microsoft`). `proxy` defaults to the global `proxy.notion_proxy`.

Response (returns immediately; the job runs in the background):

```json
{
  "job_id": "f3b2…",
  "provider": "microsoft",
  "total": 8,
  "concurrency": 1,
  "proxy": "socks5h://127.0.0.1:1080"
}
```

### Inspect jobs

```http
GET /admin/register/jobs?limit=50            # recent jobs (newest first; cap 100)
GET /admin/register/jobs/{id}                 # single-job snapshot
GET /admin/register/jobs/{id}/events          # SSE stream
DELETE /admin/register/jobs/{id}              # drop job + sidecar
```

Snapshot shape (`Job`):

```json
{
  "id": "f3b2…",
  "created_at": 1735000000000,
  "ended_at":   1735000080000,
  "provider":   "microsoft",
  "proxy":      "socks5h://127.0.0.1:1080",
  "concurrency": 1,
  "total": 8, "ok": 7, "fail": 1, "done": 8,
  "state": "done",
  "steps": [
    {
      "email": "alice@outlook.com",
      "status": "ok",
      "space_id": "9fa8…",
      "user_id":  "0c11…",
      "file":     "alice_outlook_com.json",
      "started_at": 1735000010000,
      "ended_at":   1735000020000
    },
    {
      "email": "bob@hotmail.com",
      "status": "fail",
      "message": "checkpassword.srf returned 'PWE_ANS_INVALID_CRED'"
    }
  ]
}
```

### SSE events

`GET /admin/register/jobs/{id}/events` streams `text/event-stream`. The first frame is always `snapshot` carrying the full job (so a freshly attached client can render without one extra round trip). Then `step` frames arrive as steps complete. The connection ends with a `done` frame when the job finishes.

```text
event: snapshot
data: { ...full Job... }

event: step
data: { "step_idx": 3, "step": { "email": "...", "status": "ok", ... } }

event: done
data: { ...full Job (terminal)... }
```

### Retry failed rows

```http
POST /admin/register/jobs/{id}/retry
```

Re-runs only the rows that finished with `status: "fail"`. The original credentials are recovered from the server-side **sidecar** that `start` persisted at job-creation time, so the dashboard never re-asks for the secret. The endpoint creates a *new* job (history of the original job is preserved) and responds with the new id:

```json
{
  "job_id": "9c48…",
  "provider": "microsoft",
  "total": 1,
  "concurrency": 1,
  "proxy": "socks5h://127.0.0.1:1080",
  "retry_of": "f3b2…"
}
```

The sidecar lives under `accounts/.register_inputs/<job_id>.json`. If a job is too old and the sidecar has been pruned, the endpoint returns `410 Gone` and the UI suggests re-uploading.

### Delete an account

```http
DELETE /admin/accounts/{email}
```

Removes the matching JSON file from `accounts/` and drops the live `AccountPool` entry. `email` matches `user_email` inside the JSON file (case-insensitive).

## Persistence

| File | Owner | Purpose |
| --- | --- | --- |
| `accounts/<email>.json` | regjob runner / Chrome extension | Account record loaded by the pool |
| `accounts/.register_history.json` | regjob.Store | Recent jobs (default cap 100) — survives restarts |
| `accounts/.register_inputs/<job_id>.json` | regjob.Store | Sidecar with original credentials, used by retry |
| `accounts/.token_stats.json` | UsageStats | Lifetime + 30-day Token usage |

All four are excluded from git via `.gitignore`. The history caps and paths are tunable through the `register:` block in `config.yaml`:

```yaml
register:
  history_file: "accounts/.register_history.json"
  history_memory_cap: 100
  default_concurrency: 1
```

## Standalone CLI

`cmd/register/` ships an offline-friendly CLI that drives the same `internal/msalogin` flow without going through the HTTP server. Useful for headless boxes or when you don't want to expose the dashboard.

```bash
go build -o notion-manager-register ./cmd/register
notion-manager-register \
  -accounts ./accounts \
  -input creds.txt \
  -timeout 5m \
  -proxy socks5h://127.0.0.1:1080
```

Notable flags:

- `-accounts` — output directory for the generated JSON files (default `accounts`)
- `-input` — credentials file; omitted = read from stdin (heredoc / pipe)
- `-timeout` — total budget per account (default 5 minutes; the per-HTTP-call cap is `timeout / 10`)
- `-proxy` — same scheme rules as the HTTP API
- `-dump-cookies` — stop after `token_v2` and write notion.so cookies to disk (debug)
- `-dry-run` — parse + log the credentials without performing any network call

Exit code is non-zero if any row failed.

## Troubleshooting

- **`PWE_ANS_INVALID_CRED`** — wrong password or the account is rate-limited. Wait, then retry.
- **`incomplete session: missing space_id/user_id/token_v2`** — onboarding failed before Notion bound a workspace. Common after MSA flagged the IP; retry behind a different proxy.
- **`unsupported proxy scheme`** — only `http`, `https`, `socks5`, `socks5h` are accepted.
- **Stuck on "running" forever** — the per-HTTP-call timeout is `loginTimeout` (30 s); the whole job can still be slow if MSA returns a series of 429s. The runner cancels the run if the underlying transport hangs past `timeout`.
- **`410 Gone` on retry** — the sidecar was deleted (job too old or `register.history_memory_cap` exceeded). Paste the credentials again into a new job.
