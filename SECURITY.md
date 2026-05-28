# Security model

claude-bot is a single-user, single-host bridge. It exposes no public surface
of its own — the threat model is local to the machine running both
`simplex-chat` and `claude-bot`.

## Trust boundary

The bot trusts everything on `ws://127.0.0.1:5225` because `simplex-chat`
listens there with no authentication. That is standard for the upstream
binary, and we don't try to fix it inside the bot — any process running on
the host with `connect(127.0.0.1:5225)` reachable can do everything the bot
can do.

What this means in practice:

- A second local user, or any process running as a different user that can
  reach the loopback port, can:
  - Impersonate the admin contact and drive Claude.
  - Read every inbound and outbound message in plaintext as it flows past
    over the WS push channel.
  - Send arbitrary `/_send` commands as the bot's SimpleX user.
- Whitelisting by `allowed_contact_id` is **not** a security boundary
  against a local adversary. It's there to ignore messages from other
  contacts the bot's account might accumulate (e.g. from accidental address
  shares), not to authenticate the WS peer.

## Required deployment posture

The deployment expects the host to enforce the boundary at the OS level:

1. **Dedicated user.** Run `claude-bot` as a system user that does nothing
   else. The shipped NixOS module sets this up (`User=claude-bot`).
2. **systemd hardening.** `ProtectHome=tmpfs`, `ProtectSystem=strict`,
   `NoNewPrivileges=true`, `PrivateTmp=true`,
   `ReadWritePaths=/var/lib/claude-bot` — these block the bot's process
   from poking around the rest of the system, and (by symmetry) limit
   what a compromised Claude tool-use can do via the bot's user.
3. **No other untrusted local users.** The simplex-chat WS port is open
   to any local user. If you can't guarantee single-tenant operation, do
   not run this bot.

## Defence-in-depth options (not enforced by the bot)

If multi-tenant operation is unavoidable, the operator can:

- Bind simplex-chat's WS endpoint to a unix socket inside the bot's
  network namespace and run the bot with `PrivateNetwork=true`. That gives
  WS access while denying anyone else on the host.
- If TCP must stay, use `nftables` to filter the loopback port by uid so
  only the `claude-bot` user can connect.
- Run the whole stack inside a container or VM with its own network
  namespace.

None of these are configured by the bot itself — they belong to the unit
file and host-level firewall.

## Data at rest

The bot creates `cfg.Claude.Workspace` and `cfg.Storage.InboxDir` with
mode `0o700`, and `cfg.Storage.DBPath` (plus its `-wal`/`-shm` companions)
with mode `0o600`. That keeps the contents out of reach of other local
users even when systemd hardening isn't in effect (e.g. during local
development), but is meaningful only insofar as the trust boundary above
holds.

Claude's own conversation transcripts live in `~/.claude/projects/` for
the user the bot runs as. Treat backups of `state.db` and that directory
as sensitive.

## Reporting

Open an issue with the `security` label. There is no separate disclosure
channel — this is a single-user bot, not a deployed service.
