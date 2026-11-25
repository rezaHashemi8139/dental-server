# Dental server

Accounts, installations, local-database keys, synced patient records (never financial
data) and app updates for the dental desktop app.

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

Or by hand: copy `.env.example` to `.env`, generate `KEY_ENCRYPTION_KEY`, `SIGNING_KEY` and
`ADMIN_TOKEN_KEY` with the command noted there, set `DATABASE_URL`, then `go run .`.

Migrations in `internal/database/migrations/` are applied automatically on startup.
If PostgreSQL is unreachable the server still starts, retries in the background and
answers RPCs with `Unavailable` until the database is ready. Open http://localhost:8080/
for a status page that shows what is wrong.

Or use the scripts (PowerShell):

```powershell
.\scripts\dev.ps1                 # development: rebuilds and restarts on every change (.go/.sql/.html/.proto/.env)
.\scripts\dev.ps1 -NoWatch        # development, run once without restarts
.\scripts\build.ps1               # production build into bin/ (vet + test first)
.\scripts\build.ps1 -OS linux     # cross-compile for a Linux server
```

If Windows blocks the scripts, run once: `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned`.

The admin panel (`AdminService`: plans and selling subscriptions) needs an administrator.
Create the first one on the server; it reads the password from standard input:

```powershell
go run . create-admin -username admin -full-name "Site Admin"
```

Further administrators can then be created from the panel (`AdminService.CreateAdmin`).
Admin sessions are a JWT in an HttpOnly cookie plus a CSRF token sent in the `X-CSRF-Token` header
(see `AdminService` in `proto/dental/v1/admin.proto`). The session behind the JWT is checked on
every call, so `AdminService.Logout` (or disabling the administrator) ends it at once.

In VS Code the same commands are tasks (`.vscode/tasks.json`): `Ctrl+Shift+P` → **Tasks: Run Task**
(`dev`, `generate`, `build`, `build: linux`, `test`, `vet`, `tidy`), or `Ctrl+Shift+B` for `build`.
With the recommended **Task Runner** extension they are also listed in the Explorer sidebar; click one to run it.

Quick check with curl (Connect protocol, JSON):

```sh
curl -X POST -H "Content-Type: application/json" -d '{}' localhost:8080/dental.v1.HealthService/Check
```

## Changing the API

1. Edit or add `.proto` files in `proto/dental/v1/`. Put input rules (lengths, formats...)
   on the fields with [protovalidate](https://protovalidate.com) options (`buf.validate.field`):
   every request is checked against them by `service.ValidateInterceptor` before it reaches a handler.
2. Run `go generate ./...` (lints the protos, regenerates `gen/`).
3. Implement the new methods in `internal/service/` and register the handler in `main.go`.

buf and the protoc plugins are pinned in `go.mod` as tools, so nothing needs to be installed globally.

## Layout

```
proto/dental/v1/        API definitions (shared with the desktop app)
gen/                    generated code — do not edit
internal/config/        settings from environment / .env
internal/database/      PostgreSQL connection + migrations
internal/languages/     downloadable interface languages for the app (see its README)
internal/releases/      desktop app updates: release.json + installer from UPDATES_DIR, forced updates
internal/service/       RPC implementations
internal/statuspage/    HTML status page at / (server + database health)
main.go                 server setup, interceptors, graceful shutdown
createadmin.go          `create-admin` command: the first administrator of the admin panel
```

## Production

Set `TLS_CERT_FILE` and `TLS_KEY_FILE`: the desktop app only talks to the server over TLS.
Without them the server runs plain HTTP/2 (h2c), which is for local development only.

## Publishing a desktop app update

1. In the desktop repository run `pnpm build`. It asks for the version and whether the
   update is forced, and writes the signed release to its `build/update/` folder.
2. Copy the files of that folder into `UPDATES_DIR` (default `updates/` next to the binary),
   replacing the previous release. It is picked up without a restart.
3. Installed apps find it within the hour (or at start) and offer it. If it is forced,
   older apps stop until they install it. To force it later, raise `minimumVersion` in
   `release.json` by hand.
