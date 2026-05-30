# claude-bot — design

Single-user SimpleX bot. Frontend to Claude Code via `claude -p`. Runs as a dedicated systemd service on a sprites.dev (NixOS) machine. Whitelist of one. Compiled from source on the host.

## Components

```
                ┌──────────────────────┐
                │ user (SimpleX phone) │
                └──────────┬───────────┘
                           │  SimpleX network
                ┌──────────▼───────────┐
                │ simplex-chat (sidecar│   systemd unit
                │ Haskell bin, WS:5225)│   (independent restart)
                └──────────┬───────────┘
                           │  ws://127.0.0.1:5225 (JSON)
                ┌──────────▼───────────┐
                │ claude-bot (Go)      │   systemd unit, user=claude-bot
                │  ┌─simplex client    │     hardening: ProtectSystem=strict,
                │  ├─bot orchestrator  │     ProtectHome=tmpfs,
                │  ├─claude runner ────┼──┐  ReadWritePaths=/var/lib/claude-bot,
                │  └─sqlite store     │  │  NoNewPrivileges=true, PrivateTmp=true
                └──────────────────────┘  │
                                          ▼
                              exec claude -p --resume <id>
                                     --model <m>
                                     --permission-mode bypassPermissions
                                     --allowedTools …
                                     --output-format stream-json --verbose
                              cwd=/var/lib/claude-bot/workspace
```

## Module layout

Module path: `claude-bot` (local, not published).

```
cmd/claude-bot/main.go             # wire-up, signal handling
internal/config/                   # TOML schema + Load
internal/simplex/                  # WS client, events, reconnect, command wrappers
internal/claude/                   # subprocess runner, stream-json parser, error classes
internal/store/                    # sqlite (state, live_messages, turns)
internal/bot/                      # orchestrator, handler, commands, live-turn FSM,
                                   # chunker, markdown translator
internal/log/                      # slog setup
nix/{package,module}.nix
nix/tests/{e2e.nix, fake-simplex/, fake-claude.sh}
```

Package boundaries:
- `simplex` knows nothing about claude; speaks WS JSON, exposes events.
- `claude` knows nothing about simplex; runs subprocess, exposes events.
- `bot` is the only package that depends on both. Glue.
- `store`, `config`, `log` know nothing about either.

Interfaces (the seams that matter for testing) live in each adapter package; `bot` depends on those.

## WS protocol shape

Verified against `simplex-chat v6.4.10.0` started with `simplex-chat -p 5225`.

**Envelope.** Every command and response uses a JSON envelope on text frames. Bare-text commands (no envelope) return `chatCmdError "invalid request"`.

```
TX (command):       {"corrId":"<id>", "cmd":"<cli-command-string>"}
RX (response):      {"corrId":"<id>", "resp":{"type":"<type>", ...}}      // corrId echoed
RX (push event):    {"resp":{"type":"<type>", ...}}                       // no corrId
```

`corrId` presence is the discriminator: with-corrId is the response to a request you sent, without is a spontaneous push event from the server.

**Commands we use.** The `cmd` value is the same string the simplex-chat TUI accepts.

| Command | Response `type` | Notes |
|---|---|---|
| `/contacts` | `contactsList` | `contacts[].contactId`, `contacts[].localDisplayName`. Use this on bootstrap to discover the admin contactId. |
| `/_send @<cid> text <body>` | `newChatItems` | Plain (non-live) send. Returns one item in `chatItems[0].chatItem.meta.itemId`. |
| `/_send @<cid> live=on text <body>` | `newChatItems` | Live message — `meta.itemLive: true`. |
| `/_send @<cid> live=on json [{"msgContent":{"type":"text","text":"…"}, "quotedItemId":N}]` | `newChatItems` | JSON form; needed when quoting. `quotedItem.itemId` echoes back. |
| `/_update item @<cid> <itemId> live=on text <body>` | `chatItemUpdated` | Mid-stream live update. Single item in `chatItem` (NOT `chatItems[]`). |
| `/_update item @<cid> <itemId> text <body>` | `chatItemUpdated` | Finalise — drops `live=on`, sets `meta.itemLive: false`. |
| `/_get chat @<cid> count=N` | `apiChat` | Per-contact history. Items at `chat.chatItems[]`. Used for orphan cleanup. |
| any malformed | `chatCmdError` | `chatError.errorType.message` describes. |

**Push events.** Inbound messages and out-of-band updates arrive without `corrId`:

| `type` | Meaning | Action |
|---|---|---|
| `newChatItems` | Items added to a chat. Includes both peer-sent (`directRcv`) and our own sends (`directSnd`). | Filter on `chatItem.chatDir.type == "directRcv"` and `chatInfo.contact.contactId == allowed_contact_id`. |
| `chatItemsStatusesUpdated` | Delivery status changes (sent → rcvd, etc.) for our outbound items. | Ignore. |
| `chatItemUpdated` | Item edited (incl. our own live updates echoing back). | Ignore unless implementing read-receipt UI. |
| `contactSubSummary`, `userContactSubSummary`, `terminalEvent` | Subscription state on connect. | Log at debug, ignore. |

**Item shape (the part we extract).** For both `newChatItems[*]` and `chatItemUpdated`'s single `chatItem`:

```jsonc
{
  "chatInfo":  { "contact": { "contactId": 4, "localDisplayName": "..." } },
  "chatItem":  {
    "chatDir":     { "type": "directRcv" | "directSnd" },
    "content":     { "msgContent": { "type": "text", "text": "..." } },
    "meta":        { "itemId": 14, "itemLive": false, "itemEdited": false },
    "quotedItem":  { "itemId": 12, ... }    // present only when quoting
  }
}
```

`content.msgContent.type` may also be `image`, `file`, `voice`, etc. — branch on it; the bot currently treats only `text` as the prompt body and looks at sibling fields for attachments.

**Identity gotcha.** `contactId == 1` typically belongs to the bot's own user (visible as `userContactId`/`userContactSubSummary`). The admin's contactId is whatever appears under `contactSubSummary.contactSubscriptions[].contact.contactId` (or the same field under `contactsList.contacts[]`). The example config's `allowed_contact_id = 1` is a placeholder, not a default — read the real value off `/contacts` during bootstrap step 5.

## Config (`/etc/claude-bot/config.toml`)

```toml
[simplex]
ws_url = "ws://127.0.0.1:5225"
allowed_contact_id = 1

[claude]
binary = "/usr/local/bin/claude"
workspace = "/var/lib/claude-bot/workspace"
model = "claude-opus-4-7"
allowed_tools = ["Read","Grep","Glob","Edit","Write","Bash","WebFetch"]
disallowed_tools = []
show_tool_use = false
show_cost_footer = true
turn_timeout = "30m"

[storage]
db_path = "/var/lib/claude-bot/state.db"
inbox_dir = "/var/lib/claude-bot/workspace/inbox"
inbox_retention = "30d"
max_attachment_size = "100MiB"   # 0 = unlimited; SI (MB=10^6) + IEC (MiB=2^20) suffixes

[live_message]
update_interval = "3s"
chunk_threshold = 4096

[log]
level = "info"
format = "json"
log_full_messages = false
```

Credentials: bot inherits whatever `claude` finds. Default path is subscription via `claude /login` once as `claude-bot` user. Alternative: `ANTHROPIC_API_KEY` via `EnvironmentFile=/etc/claude-bot/secrets.env` (mode 0600).

## DB schema

```sql
CREATE TABLE bot_state    (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE live_messages(item_id INTEGER PRIMARY KEY, contact_id INTEGER NOT NULL,
                           finalised INTEGER NOT NULL DEFAULT 0, started_at TEXT NOT NULL);
CREATE TABLE turns        (id INTEGER PRIMARY KEY AUTOINCREMENT,
                           session_id TEXT, started_at TEXT NOT NULL, ended_at TEXT,
                           cost_usd REAL, status TEXT, error TEXT);
```

Claude's own JSONL transcripts (in `~/.claude/projects/`) are the source of truth for conversation content; we don't dupe.

## Session model

Plan B: resumed session per user. First message captures `session_id` from the `init` event in the stream-json output. Subsequent messages pass `--resume <id>`. `/new` command clears the stored session. Single user, single active session. Persisted in `bot_state.current_session_id`.

## Turn lifecycle

1. WS event `newChatItems` → bot accepts only if `contactId == allowed_contact_id`; else log+drop.
2. Slash command? `/new`/`/help`/`/status`/`/cost` queue FIFO; `/stop` is synchronous (cancels current turn).
3. Otherwise enqueue. Single worker goroutine runs one turn at a time.
4. Worker:
   - Ingest any attachments → `<workspace>/inbox/<ts>_<name>`, append `[attached: ./inbox/…]` to prompt.
   - `/_send @<cid> live=on json [{msgContent:..., quotedItemId:<promptItemId>}]` → save returned itemId.
   - Spawn `claude` with `--resume <stored session_id>` (or fresh if none), `--model`, `--permission-mode bypassPermissions`, `--allowedTools`, `--output-format stream-json --verbose`. `cmd.Dir = workspace`.
   - Parse stream-json. On `init`: persist `session_id`. On `assistant` text: append to buffer (after markdown translation), flush every 3s via `/_update item @<cid> <itemId> live=on text <body>`. On `tool_use`: ignored (config). On size threshold: finalise current live message (non-live update), open a new live message (no quote), continue.
   - On `result`: finalise with full buffer + cost footer; insert `turns` row.
5. Errors funnel into typed cases (`auth`, `rate_limit`, `timeout`, `crash`); each finalises the live message with a tagged suffix.

## Live-message FSM

SimpleX live messages: start with `APISendMessages live=on`, update every ≥3s with `APIUpdateChatItem live=on`, finalise with `APIUpdateChatItem` (no `live=on`). Each update sends full cumulative text, not a delta. Recipient client renders incrementally.

```go
type LiveTurn struct {
    contactID    int64
    promptItemID int64   // user's message — quoted on first send only
    itemID       int64   // current live message
    buffer       strings.Builder
    flushed      string  // last text actually sent (skip no-op flushes)
    lastFlush    time.Time
}
// flush  → APIUpdateChatItem(... live=on)         every ≥3s if buffer != flushed
// rotate → finalise current itemID, send new live (no quote), reset buffer
// finalise → APIUpdateChatItem(... live=off); mark live_messages.finalised
```

Cadence: 3s direct (matches terminal client convention; not server-enforced but treat as safe).
Rotation: when cumulative buffer exceeds `chunk_threshold` (4KB), finalise + open new live message.

## Slash commands

`/new` reset session, `/stop` cancel current turn, `/help` list commands, `/status` session id + last turn + binary version, `/cost` total cost from `turns` table. `/stop` synchronous, others FIFO. `/new` mid-turn requires `/stop` first.

Tiny dispatcher:
```go
type Cmd struct { Name, Args string }
func parseCommand(text string) (Cmd, bool) {
    if !strings.HasPrefix(text, "/") { return Cmd{}, false }
    rest := strings.TrimPrefix(text, "/")
    name, args, _ := strings.Cut(rest, " ")
    return Cmd{Name: name, Args: strings.TrimSpace(args)}, true
}
```

## Resilience

- WS reconnect with capped backoff (1s→30s); buffered events redelivered by simplex-chat.
- SIGTERM: cancel current turn ctx → claude SIGTERM → finalise current live message with `⚠️ interrupted` → exit (≤1s grace).
- Startup: read the local `live_messages` sqlite mirror for rows with `finalised=0`, finalise each on the wire with `⚠️ bot restarted`. The mirror is the source of truth for items we authored — no wire-side `/_get chat` probe.
- Turn timeout (30m): SIGTERM claude, finalise with `⏱️ timeout`, record `status='timeout'`. Session ID survives — next turn resumes.
- Cancelled turns recorded in `turns` with whatever cost was reported up to cancellation.

## Markdown translation

SimpleX dialect collides with CommonMark (`**bold**` vs `*bold*`, `*italic*` vs `_italic_`). Translator at `internal/bot/markdown.go`, applied to text as it enters the live-message buffer:

```
**x**  → *x*           (bold; do this first)
*x*    → _x_           (italic)
~~x~~  → ~x~           (strike)
# h    → *h*           (heading → bold)
```lang\nbody\n```  → body (drop fences; SimpleX has no code-block rendering)
[text](url)  → text (url)   (autolink picks up the URL)
```

Tables, nested lists, footnotes, complex code blocks — best-effort, may not survive cleanly. Document in README.

## Threading

Bot's first live message of each turn quotes the user's prompt via `quotedItemId`. Subsequent live messages within the same turn (rotated for size) don't quote. Visual: each turn anchored to its prompt.

## Media

- **Inbound:** a received chat item carries attachment metadata in a sibling `file` block (`CIFile`: `fileId`/`fileName`/`fileSize`/`fileStatus`). For each offered file (status `rcvInvitation`) the bot issues `/freceive <fileId> <workspace>/inbox/<ts>_<safe-name>`, blocks until the matching `rcvFileComplete` push confirms the bytes are on disk, then suffixes the prompt with `[attached: ./inbox/<ts>_<name>]` (one line per file). Filenames are sanitised to a single component — no separators, parent refs, leading dots, or whitespace (the `/freceive` grammar is space-delimited). A file with no caption is still a valid prompt. Claude reads the file via its own tool. Background goroutine deletes inbox files older than `inbox_retention`.
  - **Size cap (issue #33):** before issuing `/freceive`, the bot compares the sender-advertised `fileSize` against `storage.max_attachment_size` (default `100MiB`; `0` = unlimited). Oversized files are skipped before any bytes are downloaded and the user is notified (`⚠️ attachment … is too large`), so a single huge — or a flood of large — attachment can't exhaust the inbox disk between sweeps. The size is parsed from a human-readable `ByteSize` (SI `MB`=10^6 / IEC `MiB`=2^20, parallel to the `Duration` type). The check trusts the wire-reported size: the bot serves a single whitelisted contact, so this is reliability hardening rather than a defence against a malicious peer lying about size.
  - Receiving runs on the WS event-loop goroutine and blocks (per-file timeout) until the transfer completes; acceptable for a single-user loopback bot. Wire shapes follow the simplex-chat TypeScript client types but are **unverified against a live instance** — in particular whether `/freceive` honours an absolute destination path (vs. a path relative to simplex-chat's files folder). Smoke-test before relying on it.
- **Outbound:** text-only. Claude writes files in workspace; user reads via subsequent prompt.

## Whitelisting

By `contactId` (numeric local DB id, stable post-handshake). Config holds `allowed_contact_id`. Any other contactId: silently drop, log at warn level. Incoming contact requests: silently reject — never auto-accept.

## Logging & observability

`slog` JSON to stderr (via `journalctl`).
- Info: turn start/end, ws connect/disconnect, accept/reject, session id (first 8).
- Warn: reconnects, orphan cleanup, rate limits.
- Error: claude crashes, DB errors, panics.
- Prompt previews truncated to 80 chars unless `log_full_messages=true`. Never log full body in prod. Never log API keys, displayName.

No HTTP, no Prometheus, no `/health`. `journalctl -u claude-bot` is the dashboard.

## Testing

- **Unit:** stream-json parser, paragraph chunker, command dispatcher, live-message FSM, markdown translator. All pure-logic leaves.
- **Integration:** fake WS server (`gorilla/websocket` over `httptest`) for the simplex client. Exercises the JSON dialect.
- **E2E:** NixOS test (`nix flake check`) — fake `simplex-chat` and fake `claude`, real bot binary, real systemd unit. Validates packaging, hardening, whitelist enforcement, end-to-end wiring.

Coverage target: parser + chunker + dispatcher + FSM + translator. Skip exhaustive testing of glue code.

## Bootstrap (one-time)

1. Build & install binary. Install systemd unit (or import nix module locally).
2. Create `claude-bot` user; provision `/var/lib/claude-bot`.
3. As `claude-bot`: `claude /login` (or drop `ANTHROPIC_API_KEY` in EnvironmentFile).
4. As `claude-bot`: `simplex-chat` interactive — set name, `/ad` for address, share link to phone, accept request. Then `/contacts` and copy the admin's `contactId` (NOT the `userContactId` shown for the bot itself; see "Identity gotcha" in WS protocol shape). Quit.
5. Write `config.toml` with that contactId.
6. `systemctl enable --now simplex-chat claude-bot`.

Backups: `state.db` + `~/.local/share/simplex/`.

## Decision log (locked)

| # | Decision |
|---|----------|
| 1 | Single user, plan B (resumed session), `/new` resets, sqlite stores session ID + turns. |
| 2 | systemd-managed sidecar `simplex-chat`, bot connects via WS. |
| 3 | TOML config. Identity by `contactId`. |
| 4 | Single fixed cwd. `bypassPermissions` + allowlist. Dedicated unix user + systemd hardening. Network allowed. |
| 5 | Live-message streaming (D), 3s cadence, `live=on` flag, chunked into multiple live messages on size threshold, hide tools, FIFO queue. |
| 6 | Pass `--model` from config. |
| 7 | A.2 reconnect / B.2 graceful finalise / C.1 hard kill on timeout / D.1 FIFO redelivery. `/stop` and orphan cleanup on startup. |
| 8 | Slash commands: `/new`, `/stop`, `/help`, `/status`, `/cost`. Keep `turns` table. |
| 9 | Layout B (community standard). Module `claude-bot` (local). |
| 10 | slog JSON, no metrics. Unit tests on five pure leaves + one e2e with fakes. |
| 11 | NixOS test scope B (fake everything). Flake exposes `packages`, `nixosModules.default`, `checks.e2e`. Production = source copy + local module import on sprites.dev. |
| 12 | Inbound: copy to inbox + path in prompt. Outbound: text-only. 30d inbox retention. |
| 13 | Credentials agnostic (subscription default). Bootstrap manual. Edge cases G.1–G.5 handled. |
| 14 | Markdown translator (CommonMark → SimpleX dialect). Quote-reply on first live message of each turn. |
