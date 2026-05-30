# claude-bot

Single-user SimpleX <-> Claude Code bridge. Architecture lives in `DESIGN.md`;
the trust model and deployment posture live in `SECURITY.md` — **read that
before running the bot on any host you share with other users**.

## Build

```sh
go mod tidy
go build ./...
go test ./...
```

## Run

```sh
./claude-bot -config /path/to/config.toml
```

A config example is at `internal/config/testdata/example.toml`.

## Nix

```sh
nix build .#default
```

Dependencies are vendored in-tree (`vendor/`), so the build is reproducible
with no manual hash step. After changing `go.mod`/`go.sum`, regenerate the
vendor directory with `go mod vendor` and commit it.

## Status

Skeleton only. WS dial and `claude` exec are stubbed; orchestration,
live-message FSM, markdown translator, slash commands, and stream-json
parser arrive in milestone 2.
