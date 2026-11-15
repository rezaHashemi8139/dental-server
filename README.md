# Dental server

Accounts, installations and local-database keys for the dental desktop app.

- **Go** 1.27
- **PostgreSQL** 18
- **ConnectRPC** (connect-go): RPCs defined in Protobuf, served over plain `net/http`.
  Speaks gRPC (used by the desktop app), gRPC-Web and Connect (JSON, handy for `curl`).
- **pgx** for PostgreSQL, **goose** for migrations, **buf** for code generation.

## Run

First time only (never overwrite an existing `.env`: its keys can't be recovered):

```powershell
.\scripts\dev.ps1   # creates .env with fresh random keys; then set DATABASE_URL and run it again
```

Or by hand: copy `.env.example` to `.env`, generate `KEY_ENCRYPTION_KEY` and `SIGNING_KEY`
with the command noted there, set `DATABASE_URL`, then `go run .`.

Migrations in `internal/database/migrations/` are applied automatically on startup.

Or use the scripts (PowerShell):

```powershell
.\scripts\dev.ps1                 # development: go run with .env (add -Generate to regenerate gen/ first)
.\scripts\build.ps1               # production build into bin/ (vet + test first)
.\scripts\build.ps1 -OS linux     # cross-compile for a Linux server
```

If Windows blocks the scripts, run once: `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`.

Quick check with curl (Connect protocol, JSON):

```sh
curl -X POST -H "Content-Type: application/json" -d '{}' localhost:8080/dental.v1.HealthService/Check
```

## Changing the API

1. Edit or add `.proto` files in `proto/dental/v1/`.
2. Run `go generate ./...` (lints the protos, regenerates `gen/`).
3. Implement the new methods in `internal/service/` and register the handler in `main.go`.

buf and the protoc plugins are pinned in `go.mod` as tools, so nothing needs to be installed globally.

## Layout

```
proto/dental/v1/        API definitions (shared with the desktop app)
gen/                    generated code — do not edit
internal/config/        settings from environment / .env
internal/database/      PostgreSQL connection + migrations
internal/service/       RPC implementations
main.go                 server setup, interceptors, graceful shutdown
```

## Production

Set `TLS_CERT_FILE` and `TLS_KEY_FILE`: the desktop app only talks to the server over TLS.
Without them the server runs plain HTTP/2 (h2c), which is for local development only.
