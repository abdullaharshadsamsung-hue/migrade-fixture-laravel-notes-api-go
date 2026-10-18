# Notes API (Go)

A Notes HTTP API migrated from PHP/Laravel to Go using the standard library. The migration covered 3 of 3 modules. Notes are persisted through a file-backed store (`internal/note/filestore.go`). This replaces the original Laravel `NoteRepository`, which used the `Storage` facade.

> **Migration status**
>
> | Check | Result |
> |---|---|
> | Build | ✅ Passed |
> | Target unit tests (`go test ./...`) | ✅ Passed |
> | Behavior compared with the original Laravel app | ✅ Passed |
>
> The migration tool also gave itself a confidence score of 96%. That score is a self-assessment, not a measured result. Before deploying, complete the items in [Manual review required](#manual-review-required).

---

## Tech stack

| Concern | Technology |
|---|---|
| Language | Go (version pinned in `go.mod`) |
| HTTP | Go standard library |
| Dependency management | Go modules (`go.mod`, `go.sum`) |
| Persistence | File-based note store (`internal/note/filestore.go`) |
| Containerization | `Dockerfile`, `docker-compose.yml` |

---

## Prerequisites

- **Go toolchain.** Use the version declared in `go.mod`.
- **A POSIX shell.** The install command uses `if [ ... ]` syntax and `mkdir -p`.
- **Write access to `/app/bin`.** The install step builds the binary to the absolute path `/app/bin/server`. This path matches the container layout. If you run the step on a host machine, make sure `/app/bin` can be created and written by your user.

---

## Getting started

Run these steps in this order, starting from a fresh clone.

### 1. Install dependencies and build

```sh
go mod download && if [ -d ./cmd/server ]; then mkdir -p /app/bin && go build -o /app/bin/server ./cmd/server; else echo 'No ./cmd/server package present; skipping server build'; fi
```

This command does two things:

1. It downloads module dependencies.
2. It compiles `./cmd/server` to `/app/bin/server`.

### 2. Configure environment variables

Export the variables listed in [Environment variables](#environment-variables):

```sh
export PORT=...
export JWT_SECRET=...
export DATABASE_URL=...
```

### 3. Database setup

No database setup command was detected for this project, so there is nothing to run here.

The application stores notes through a file store rather than a database migration or seed. See [Known limitations](#known-limitations) for how the notes file location is resolved.

### 4. Additional setup commands

None were needed. No extra dependency-resolution or fix commands were required during migration.

### 5. Run the server

```sh
/app/bin/server
```

---

## Running tests

```sh
go test ./...
```

This command passed during migration.

---

## Environment variables

These variables were detected as required. Their exact parsing and defaults live in `internal/config/config.go`, so confirm the details there.

| Variable | Required | Purpose |
|---|---|---|
| `PORT` | Yes | Port the HTTP server listens on. |
| `JWT_SECRET` | Yes | Secret used for JWT signing and verification. |
| `DATABASE_URL` | Yes (detected) | Database connection string carried over from the original configuration. Notes are stored through a file store, so check `internal/config/config.go` to see whether and how this value is still used. |

---

## Architecture overview

```
cmd/server/main.go           # Entry point: loads config, wires dependencies, starts the HTTP server
cmd/server/router.go         # Router code in the main package
internal/config/config.go    # Reads environment variables into application config
internal/httpapi/handlers.go # HTTP handlers for the Notes API endpoints
internal/httpapi/router.go   # Route registration for the HTTP API
internal/note/filestore.go   # File-backed note persistence (replaces Laravel Storage facade usage)
Dockerfile                   # Container image definition
docker-compose.yml           # Compose service definition
go.mod / go.sum              # Module definition and dependency checksums
```

The request flow is:

1. `cmd/server` starts the server.
2. The router dispatches each request to a handler in `internal/httpapi`.
3. The handler reads or writes notes through `internal/note/filestore.go`.
4. Configuration is supplied by `internal/config`.

The package boundaries keep concerns separate:

- `internal/httpapi` handles HTTP.
- `internal/note` handles storage.
- `internal/config` handles environment and config.

---

## Migration notes

The following table maps the original Laravel components to their Go replacements.

| Laravel (original) | Go (migrated) |
|---|---|
| Laravel API route definitions | `internal/httpapi/router.go` (and `cmd/server/router.go`, see review notes) |
| Controller actions | `internal/httpapi/handlers.go` |
| `NoteRepository` model using the `Storage` facade | `internal/note/filestore.go` |
| `.env` / `config/*.php` resolved through the IoC container | `internal/config/config.go`, read from environment variables |
| Framework bootstrap (`artisan serve`, service providers) | `cmd/server/main.go` |

The migration also changed several broader aspects of the app:

- **No IoC container or facades.** Dependencies are constructed explicitly and passed in. The framework no longer resolves them at runtime.
- **Configuration.** Configuration comes from environment variables only. Laravel config files such as the filesystems config no longer exist in this codebase.
- **Deployment.** The app is a single compiled binary (`/app/bin/server`) instead of a PHP runtime plus framework.

---

## Known limitations

### `Storage` facade in `NoteRepository::load` / `NoteRepository::save`

The migration tool could not translate this component automatically.

**Why it could not be migrated:** Laravel facades resolve the filesystem manager and default disk from the IoC container and the Laravel filesystems config at runtime. Go has no direct equivalent. The effective path of the notes file was defined by Laravel config, not by code.

**Recommended approach:**

1. Inject a file path, or a small storage interface, through the constructor.
2. Use native file I/O:
   - Read with `os.ReadFile`.
   - Write to a temp file, then rename it into place for atomic replacement.
   - Guard reads and writes with a mutex.
3. Resolve the disk root from the original PHP config so the Go service reads and writes the same notes file as the Laravel app.

`internal/note/filestore.go` is the migrated storage layer. Verify that it follows this approach and that its configured path matches the original disk root (see below).

No commands failed during migration.

---

## Manual review required

- [ ] **Original Laravel routes → `internal/httpapi/router.go`** (low confidence)
  - Compare every route against the original: HTTP method, path, path parameters, middleware such as auth/JWT, and the handler it maps to.
  - Pay particular attention to Laravel conventions that do not carry over implicitly, such as the `/api` prefix, route model binding, and middleware groups.
- [ ] **`cmd/server/router.go` vs. `internal/httpapi/router.go`**
  - Both files contain router code.
  - Confirm which one `cmd/server/main.go` actually uses.
  - Make sure routes are not defined twice or out of sync. Remove the redundant file if appropriate.
- [ ] **`internal/note/filestore.go`** (replacement for the unmigratable `Storage` facade usage)
  - The file path must resolve to the same notes file location as the original Laravel disk configuration.
  - Writes must be atomic (temp file plus rename).
  - Concurrent access must be serialized with a mutex.
  - Missing-file and empty-file behavior must match `NoteRepository::load`.
- [ ] **`internal/config/config.go`**
  - Confirm how `PORT`, `JWT_SECRET`, and `DATABASE_URL` are read and what defaults apply, if any.
  - Confirm whether `DATABASE_URL` is still needed.
  - Confirm how the notes file path is configured.
- [ ] **`Dockerfile` / `docker-compose.yml`**
  - Confirm the container provides the environment variables above.
  - Confirm the notes file location is persisted, for example via a volume, so data survives container restarts.