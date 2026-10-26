# Contributing to popgres

Thanks for your interest in popgres! Bug reports, fixes, tests and documentation improvements are welcome.

- [Before you start](#before-you-start)
- [Prerequisites](#prerequisites)
- [Development database](#development-database)
- [Running popgres](#running-popgres)
- [Tests](#tests)
- [Lint and checks](#lint-and-checks)
- [Dashboard UI](#dashboard-ui)
- [Project rules](#project-rules)
- [Pull requests](#pull-requests)

## Before you start

- For a bug, open an [issue](https://github.com/Robernetes/popgres/issues/new/choose) with the steps to reproduce it.
- For a new feature or a bigger change, open an issue first to discuss it, so that your work fits the project before you spend time on it.
- Security problems are reported privately, see [SECURITY.md](.github/SECURITY.md).
- popgres is a fork of PocketBase v0.40.6. Don't report the popgres problems to the PocketBase project.

## Prerequisites

- Go 1.27+
- Docker (for the development Postgres)
- Node 25+ (only for the dashboard UI and the JS SDK tests)
- bash and `jq` for the scripts in `scripts/` (on Windows, Git Bash or WSL)

Fork the repository and clone your fork:

```sh
git clone https://github.com/<your-username>/popgres.git
cd popgres
```

Create a branch from `main` for each change.

## Development database

`docker-compose.dev.yml` starts PostgreSQL 18 with pgvector and the logical replication enabled, on `localhost:5433`.

1. Create a `.env` file from the example and set your own passwords (`.env` is not committed):

   ```sh
   cp .env.example .env
   ```

2. Start the database:

   ```sh
   docker compose -f docker-compose.dev.yml up -d
   ```

3. Create the non-superuser role `popgres_app` and the test template database `popgres_test`, the same as the CI (replace `<app-password>` with the `APP_PASSWORD` of your `.env`):

   ```sh
   docker exec -i popgres_db psql -U admin -d postgres -v ON_ERROR_STOP=1 \
     -v app_password='<app-password>' < scripts/ci-db-setup.sql
   ```

   The script is idempotent. `admin` is the `POSTGRES_USER` of your `.env`.

Use the `popgres_app` role for the tests: a superuser skips the row level security policies, so some tests can't check them.

## Running popgres

```sh
# against the popgres database of the development container
DATABASE_URL="postgres://admin:<postgres-password>@localhost:5433/popgres?sslmode=disable" \
  go run ./examples/base serve

# or with an embedded Postgres, without Docker
go run ./examples/base serve --dev
```

Then open `http://localhost:8090/_/`.

## Tests

The tests need `POPGRES_TEST_URL`, the template database of the tests. Each test package clones it into temporary `popgres_t_*` and `popgres_fx_*` databases.

```sh
export POPGRES_TEST_URL="postgres://popgres_app:<app-password>@localhost:5433/popgres_test?sslmode=disable"

# a single package or test
go test ./tools/search/... -run TestFilter

# the whole suite (the test databases share the Postgres connections, so -p 4)
make test

# the whole suite and the parity check with the PocketBase v0.40.6 tests, as the CI
make test-accept

# the end to end tests (startup, cluster, replication, RLS, MCP)
go test -count=1 ./tests/e2e/...

# the official JS SDK tests against a server built from the sources
DATABASE_URL="postgres://..." ./scripts/sdk-test.sh

# drop the leftover test databases of interrupted runs
make test-clean
```

On Windows and macOS, `./scripts/test-linux.sh` runs `go test` in a Linux container like the CI.

The API responses are compared with the PocketBase v0.40.6 responses in `tests/golden/`. A change that alters a response must keep it identical to PocketBase, or be documented in [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) and `tests/golden/accepted.txt` with the reason.

## Lint and checks

```sh
make lint                          # golangci-lint, same config as the CI
./scripts/check-sql-patterns.sh    # no SQLite only SQL outside the importer
```

## Dashboard UI

The dashboard is a Svelte + Vite app in `ui/`, embedded in the binary from `ui/dist`:

```sh
npm --prefix ui ci
npm --prefix ui run dev     # http://localhost:5173, expects popgres on :8090
npm --prefix ui run build   # rebuild ui/dist before committing UI changes
```

## Project rules

- **Compatibility:** the REST API, the JSON responses and the official PocketBase SDKs must keep working. Don't change a response shape without a strong reason documented in `docs/COMPATIBILITY.md`.
- **Postgres only:** popgres supports PostgreSQL 17 and 18. Don't use PostgreSQL 18 only features (eg. `uuidv7()`, `RETURNING OLD/NEW`).
- **SQL:**
  - use the dbx placeholders: `{{table}}`, `[[column]]` and `{:param}`;
  - never concatenate user values into SQL, always use parameters;
  - no SQLite syntax (backticks, double quoted strings, `json_each`, `strftime`, `PRAGMA`, `sqlite_master`, `_rowid_`).
- **No SQLite in the main binary:** only `cmd/popgres-import` may import a SQLite driver.
- **Multiple instances:** popgres can run as several instances sharing one database. Shared state goes to Postgres, not to `app.Store()` or the local disk. Events between instances are published after the commit (`OnModelAfter*Success`), and the `NOTIFY` payloads carry only ids (8 KB limit).
- **Background goroutines** never call `os.Exit` or `panic`: log the error and retry.
- **Style:** follow the PocketBase code style (naming, structure, English comments). Add tests for the changes.

## Pull requests

- Keep each PR focused on one change, and update the documentation in `docs/` when the behavior changes.
- The CI must be green: lint, build, UI build, the test suite on PostgreSQL 17 and 18, the embedded Postgres and the JS SDK tests. The workflows of the first time contributors run after a maintainer approves them.
- Commit messages use a short prefix: `feat:`, `fix:`, `perf:`, `docs:`, `test:`, `build:`, `ci:` (eg. `fix: keep the realtime subscriptions after a reconnect`).
- You are responsible for the code you submit, whatever tools you used to write it: make sure you understand it and that it's tested.

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE.md) of the project.
