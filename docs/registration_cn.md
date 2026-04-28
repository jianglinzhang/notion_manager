# 批量注册

[← 返回 README](../README_CN.md)

notion-manager 通过可插拔的 Provider 流水线批量开账号。第一个内置 Provider 是 Microsoft 消费者账号（MSA），开箱即用；后续 Google / GitHub 等 OAuth 注册都按 `internal/regjob/providers.Provider` 接口接入。

本文档涵盖：

- 凭据格式与“配对邮箱”机制
- Dashboard 注册抽屉
- `/admin/register/*` HTTP API
- Job 持久化、重试、SSE 事件协议
- 独立 CLI `notion-manager-register`

## 凭据格式（Microsoft Provider）

每行一个账号，四个字段使用 `----` 分隔：

```text
<email>----<password>----<client_id>----<refresh_token>
```

- `email` — 主邮箱 / Microsoft 登录账号
- `password` — MSA 密码
- `client_id` — 用于 OAuth refresh 的 Microsoft 应用 client_id
- `refresh_token` — 与该 `client_id` 绑定的长期 refresh token

空行和以 `#` 开头的注释行会被忽略。

### 为什么必须配对

Notion 完成 onboarding 时需要一次邮箱二次验证。注册器会**用下一行的邮箱**作为当前行的验证目标（第 0 行验证写到第 1 行的收件箱，第 1 行验证写到第 2 行……，最后一行回卷到第一行）。

请尽量保持**偶数行**输入。奇数行可以跑（最后/第一行会环绕回卷），但被同一个邮箱争用的概率会翻倍。

## Dashboard 注册抽屉

`/dashboard/` 右上角 `+ Register` 按钮。

- **Provider** — 默认 Microsoft，新增 Provider 后会自动出现新 Tab
- **Concurrency** — 并发数；默认值取自 Provider 自身的 `RecommendedConcurrency()`（Microsoft = 1）。MSA 在每 IP > 5 并发后反作弊会非常激进，建议保持小值
- **Proxy（单批）** — 可选 `http`/`https`/`socks5`/`socks5h` 上游代理，对当前 Job 全部 dial 生效。空值回退到全局 `proxy.notion_proxy`。会在提交时校验 scheme，错误的写法直接 400
- **Credentials** — 直接粘贴上面的批量文本

点击 **Start** 后抽屉以 SSE 实时刷新进度列表。失败行会在右侧显示截断后的错误信息；成功行展示新建的 `space_id` / `user_id` 以及落盘到 `accounts/` 的文件名。

## HTTP API

`/admin/register*` 与 `/admin/accounts/*` 都需要有效的 Dashboard 会话（`/dashboard/auth/login` 设置的 cookie），返回 JSON。

### 列出 Provider

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

### 启动 Job

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

`provider` 与 `proxy` 都可省略。`provider` 默认取已注册列表的第一个（当前是 `microsoft`）；`proxy` 默认取全局 `proxy.notion_proxy`。

响应（立刻返回，Job 在后台跑）：

```json
{
  "job_id": "f3b2…",
  "provider": "microsoft",
  "total": 8,
  "concurrency": 1,
  "proxy": "socks5h://127.0.0.1:1080"
}
```

### 查询 Job

```http
GET /admin/register/jobs?limit=50            # 最近 Job（最新在前；上限 100）
GET /admin/register/jobs/{id}                 # 单个 Job 快照
GET /admin/register/jobs/{id}/events          # SSE 事件流
DELETE /admin/register/jobs/{id}              # 删除 Job 及其 sidecar
```

Job 快照结构：

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

### SSE 事件

`GET /admin/register/jobs/{id}/events` 是标准 `text/event-stream`。第一帧一定是 `snapshot`，承载完整 Job 状态（前端连进来时无需再多打一次接口）。之后每个 Step 完成发一次 `step`。Job 终态时发一次 `done`。

```text
event: snapshot
data: { ...full Job... }

event: step
data: { "step_idx": 3, "step": { "email": "...", "status": "ok", ... } }

event: done
data: { ...full Job (terminal)... }
```

### 失败行重试

```http
POST /admin/register/jobs/{id}/retry
```

只重新跑原 Job 中 `status: "fail"` 的行。原始凭据由服务端 sidecar 还原（`start` 时已落盘），Dashboard 不需要再次输入密钥。**会创建一个新 Job**（保留原 Job 历史），响应中的 `retry_of` 指向原 Job：

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

Sidecar 落盘位置 `accounts/.register_inputs/<job_id>.json`。如果 Job 太旧、sidecar 已被回收，重试会返回 `410 Gone`，前端提示重新粘贴。

### 删除账号

```http
DELETE /admin/accounts/{email}
```

按 JSON 文件中的 `user_email`（大小写不敏感）匹配并删除：磁盘文件 + 内存池中的 Account 都会被移除。

## 持久化

| 文件 | 写入方 | 用途 |
| --- | --- | --- |
| `accounts/<email>.json` | regjob runner / Chrome 扩展 | 账号池加载的账号记录 |
| `accounts/.register_history.json` | regjob.Store | 最近 Job 列表（默认上限 100），重启不丢 |
| `accounts/.register_inputs/<job_id>.json` | regjob.Store | sidecar，保存原始凭据用于重试 |
| `accounts/.token_stats.json` | UsageStats | 累计 + 30 天 Token 用量 |

四个文件都已在 `.gitignore` 中。容量 / 路径在 `config.yaml` 里调整：

```yaml
register:
  history_file: "accounts/.register_history.json"
  history_memory_cap: 100
  default_concurrency: 1
```

## 独立 CLI

`cmd/register/` 是不依赖 HTTP server 的离线版本，直接驱动 `internal/msalogin`。无头机器或不想暴露 Dashboard 时使用。

```bash
go build -o notion-manager-register ./cmd/register
notion-manager-register \
  -accounts ./accounts \
  -input creds.txt \
  -timeout 5m \
  -proxy socks5h://127.0.0.1:1080
```

常用参数：

- `-accounts` — 生成的 JSON 文件输出目录（默认 `accounts`）
- `-input` — 凭据文件；省略则从 stdin 读（heredoc / pipe）
- `-timeout` — 单账号总预算（默认 5 分钟；单次 HTTP 调用的上限是 `timeout / 10`）
- `-proxy` — 协议规则与 HTTP API 一致
- `-dump-cookies` — 拿到 `token_v2` 后停止，把 notion.so cookie 写到磁盘（调试用）
- `-dry-run` — 解析并打印凭据，不发起任何网络请求

任意一行失败时退出码非 0。

## 排障

- **`PWE_ANS_INVALID_CRED`** — 密码错或账号被风控。等一会再试
- **`incomplete session: missing space_id/user_id/token_v2`** — onboarding 没把 workspace 绑上来。多半是 MSA 把 IP 风控掉了；换个代理重试
- **`unsupported proxy scheme`** — 只接受 `http`、`https`、`socks5`、`socks5h`
- **一直停在 running** — 单次 HTTP 调用上限是 30 秒（`loginTimeout`）；如果 MSA 持续 429，整个 Job 仍然会很慢。底层 transport 卡死超过 `timeout` 后 runner 会兜底取消
- **重试 `410 Gone`** — sidecar 已被回收（Job 太旧或超过 `register.history_memory_cap`）。新建 Job 重新粘贴凭据
