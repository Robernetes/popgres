# popgres and PocketBase (upstream)

popgres is a fork of [PocketBase](https://github.com/pocketbase/pocketbase) **v0.40.6** (commit `9d281cc`, MIT, see [NOTICE.md](NOTICE.md)). This guide lists what popgres changes in the PocketBase code and how to bring the changes of a new PocketBase version.

The `main` branch keeps the PocketBase history: it starts at the PocketBase `v0.40.6` tag and the popgres commits are on top of it, so a new PocketBase version can be merged with `git merge`.

```bash
git remote add upstream https://github.com/pocketbase/pocketbase.git
git remote set-url --push upstream no_push   # read only
git config remote.upstream.tagOpt --no-tags  # the PocketBase tags could clash with the popgres ones (eg. v1.0.0)
git fetch upstream refs/tags/v0.40.6:refs/tags/upstream/v0.40.6
```

The PocketBase tags are fetched one by one with an `upstream/` prefix (see below).

## What popgres changes

`git diff upstream/v0.40.6 main` shows five kinds of changes.

### 1. The module path

`github.com/pocketbase/pocketbase` is `github.com/Robernetes/popgres`. About 230 inherited files change only their imports. The other `github.com/pocketbase/*` modules (`dbx`, `ozzo-validation`, `tygoja`) are still used as they are.

### 2. New files

The popgres features are in their own files and packages, so they don't conflict with the PocketBase changes:

| Area | Files |
|---|---|
| Postgres connection and helpers | `core/db_dsn.go`, `core/collection_ddl.go`, `core/db_view_deps.go`, `tools/dbutils/pg_functions.go`, `tools/search/filter_pg.go`, `migrations/1640987999_popgres_functions.go` |
| Cluster | `core/cluster/` (LISTEN/NOTIFY, key-value store, locks, rate limits, replication), `core/cluster_hooks.go`, `core/cluster_secret.go`, `apis/cluster_forward.go`, `apis/serve_cluster.go`, `apis/realtime_cluster.go`, `apis/realtime_access_memo.go`, `migrations/1761400000_popgres_cluster.go` |
| Realtime from the logical replication | `core/realtime_replication.go`, `core/cluster/replication.go` |
| Row level security | `core/rls.go`, `core/rls_strict.go`, `core/rls_sync.go`, `apis/collection_rls.go`, `apis/record_crud_rls.go`, `apis/rls_token.go`, `migrations/1761500000_popgres_rls.go`, `migrations/1761600000_popgres_rls_tokens.go` |
| Backups with pg_dump | `core/backup_pg.go`, `cmd/restore.go` |
| pgvector | `core/field_vector.go`, `tools/types/vector.go`, `apis/record_vector_search.go` |
| MCP server | `apis/mcp/`, `apis/mcp_api.go` |
| Embedded Postgres (dev mode) | `popgres_dev.go`, `popgres_dev_binaries.go` |
| PocketBase importer (the only SQLite code) | `cmd/popgres-import/` |
| Tests | `tests/db.go`, `tests/fixtures.go`, `tests/rls.go`, `tests/golden.go`, `tests/golden/`, `tests/e2e/`, `tests/sdk/js/`, `tests/cmd/`, `tests/data/data.pg.sql` |
| Admin dashboard | `ui/src/collections/collectionRLSTab.js`, `ui/src/fields/vector/`, `ui/src/settings/application/mcpAccordion.js`, `ui/src/settings/application/realtimeAccordion.js` |
| Build and docs | `Dockerfile`, `docker-compose.*.yml`, `scripts/`, `bench/`, `docs/` |

### 3. Inherited files changed by popgres

Every change in an inherited Go file is marked with a `// popgres:` comment that explains it, so the changes can be found with:

```bash
git grep -n "popgres:" -- '*.go'
```

The main changes by area:

| Area | Files | What changes |
|---|---|---|
| App and connection | `core/app.go`, `core/base.go`, `core/db_connect.go`, `core/db_connect_nodefaultdriver.go`, `core/db_retry.go` | The pgx driver and a connection string instead of the SQLite files; one pool for the data and one for the logs schema (`_aux`); the config of the database URLs, the cluster, the realtime source and the RLS strict mode; the cluster start and stop; the retries by SQLSTATE (serialization, deadlock, lock timeout); no SQLite `PRAGMA`/`VACUUM` cron. The App interface adds `Cluster()` and `IsRLSStrict()`. |
| Schema and SQL | `core/db_table.go`, `core/view.go`, `core/collection_validate.go`, `core/collection_model.go`, `core/collection_record_table_sync.go`, `core/log_query.go`, `core/validators/db.go`, `migrations/1640988000_init.go`, `migrations/1640988000_aux_init.go` | The Postgres catalog instead of `PRAGMA`/`sqlite_master`; the dependent views are dropped and recreated (Postgres doesn't keep broken views); Postgres DDL (`TIMESTAMPTZ`, `JSONB`, `popgres_id()`); the unique violation errors from the pgconn error detail; the collection changes wait at most `CollectionDDLLockTimeout` and sync the RLS policies. |
| Field types | `core/field.go`, `core/field_*.go` | The Postgres column types: `TIMESTAMPTZ` dates (an empty date is `NULL`), `JSONB` multiple values and JSON, `DOUBLE PRECISION` numbers, `popgres_id()` default ids and `lower()` instead of `COLLATE NOCASE`. |
| Filters and search | `tools/search/filter.go`, `tools/search/provider.go`, `tools/search/simple_field_resolver.go`, `tools/search/token_functions.go`, `tools/search/multi_match_subquery.go`, `tools/dbutils/json.go`, `tools/dbutils/index.go`, `tools/types/datetime.go` | Typed comparisons (`ILIKE`, `IS DISTINCT FROM`, `TRUE`/`FALSE`, JSON and type hints), `LEFT JOIN ... ON TRUE`, the jsonb functions and the `popgres_*` SQL functions instead of `json_each`/`json_extract`/`strftime`, the Postgres index builder, no `_rowid_` sort. |
| Cluster | `core/notify_watcher.go`, `core/migrations_runner.go`, `tools/cron/cron.go`, `apis/installer.go`, `apis/backup.go`, `apis/backup_create.go`, `apis/record_auth_password_reset_confirm.go`, `apis/record_auth_verification_confirm.go`, `apis/serve.go`, `tools/subscriptions/client.go` | LISTEN/NOTIFY instead of the notify files watcher; cluster locks for the migrations and the installer superuser; a cron job runs once per cluster; the active backup and the resend timeouts are shared by the nodes; the realtime client ids start with their node id. |
| Records API | `apis/record_crud.go`, `apis/record_auth_with_password.go`, `apis/realtime.go`, `apis/record_auth_with_oauth2*.go`, `apis/health.go`, `apis/collection.go`, `apis/logs.go`, `apis/sql.go` | The RLS strict mode queries; the realtime events of the other nodes and the forwarding of the client requests; the cluster and pool health data; the collection RLS endpoint; the SQL console with Postgres statements. |
| Backups | `core/backup.go`, `core/backup_restore.go` | `pg_dump` backups (`core/backup_pg.go`); the in-place restore is replaced by the `popgres restore` command. |
| Tests helpers | `tests/app.go`, `tests/api.go`, `tests/dynamic_stubs.go` | A temporary Postgres database per test app; the comparison of the API responses with the PocketBase golden files. |
| Binary | `popgres.go`, `examples/base/main.go`, `plugins/jsvm/internal/types/types.go` | The `popgres` names, the `--dev` mode and the `restore` command; no `update` command (it downloads PocketBase releases). |

### 4. Removed and renamed files

- `pocketbase.go` and `pocketbase_test.go` are `popgres.go` and `popgres_test.go`.
- The SQLite only upgrade migrations from older PocketBase versions (`1717233556_v0.23_migrate*.go`, `1763020353_update_default_auth_alert_templates.go`, `1778828400_normalize_indexes.go`) are removed: a popgres database starts with the v0.40.6 schema (`1640988000_init.go`). PocketBase data is imported with `popgres-import` (see [docs/MIGRATING.md](docs/MIGRATING.md)).
- `modernc_versions_check.go` is removed (the main binary doesn't use SQLite).

### 5. Tests

- 49 inherited test files are adapted to Postgres: the expected SQL, the column types and defaults reported by Postgres, the error messages, and the order of the results where Postgres doesn't guarantee the SQLite order. The tests changes are not marked: compare them with `git diff upstream/v0.40.6 main -- <file>`.
- The fixtures are `tests/data/data.pg.sql`, generated from the PocketBase test data (`tests/data/data.db`) by `scripts/gen-fixtures.sh`.
- `tests/golden/pocketbase-v0.40.6-tests.txt` lists the tests that pass in PocketBase v0.40.6. `go run ./tests/cmd/parity` checks that each of them passes in popgres, or is skipped with a `popgres: <reason>` message, or is listed with its reason in `tests/golden/parity-renamed.txt`.
- `tests/golden/api/` has the responses of the PocketBase v0.40.6 API scenarios. Every `ApiScenario` response is compared with them (`tests/golden.go`); the accepted differences are in `tests/golden/accepted.txt` and [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## Merging a new PocketBase version

1. **Read the PocketBase changelog** of the new versions. Look for new SQL, new migrations, new field types, new filter functions and new in-memory state (caches, `app.Store()`, timers).

2. **Merge the tag** in a branch:

   ```bash
   git fetch upstream refs/tags/vX.Y.Z:refs/tags/upstream/vX.Y.Z
   git switch -c upstream-vX.Y.Z
   git merge upstream/vX.Y.Z
   ```

3. **Resolve the conflicts.** Keep the intent of each `// popgres:` change: Postgres SQL instead of SQLite SQL, the cluster state instead of the process memory, the RLS sync after the collection changes. Most conflicts are in `core/base.go`, `core/db_table.go`, `core/notify_watcher.go`, `tools/search/` and `tools/dbutils/`.

4. **Rename the module path** in the new and changed PocketBase files (only `pocketbase/pocketbase`, not the other `github.com/pocketbase/*` modules):

   ```bash
   git grep -l "github.com/pocketbase/pocketbase" -- '*.go' ':!cmd/popgres-import/**' \
     | xargs sed -i 's#github.com/pocketbase/pocketbase#github.com/Robernetes/popgres#g'
   go mod tidy
   ```

   Then check that `go.mod` doesn't require a SQLite driver for the main binary (only `cmd/popgres-import` can link `modernc.org/sqlite`, the CI checks it).

5. **Port the new SQL to Postgres.** `./scripts/check-sql-patterns.sh` finds the SQLite only SQL (`json_each`, `PRAGMA`, `sqlite_master`, `_rowid_`, `strftime`). Use the dbx placeholders (`{{table}}`, `[[column]]`, `{:param}`) and the `popgres_*` SQL functions of `tools/dbutils/pg_functions.go`. A new field type needs a Postgres column type, a new filter function needs its Postgres version in `tools/search/`.

6. **Port the new in-memory state to the cluster.** A state that must be the same on every node (eg. a cache invalidated by a change) needs a cluster message after the commit (`core/cluster_hooks.go`), and a short-lived value shared by the nodes goes to the cluster key-value store (`app.Cluster().KV()`).

7. **Check the RLS policies** if the change touches the API rules, the record field resolver or the auth collections (`core/rls.go`, `tests/rls.go`).

8. **Rebuild the admin dashboard** after merging `ui/src` (`npm --prefix ui ci && npm --prefix ui run build`) and commit `ui/dist`.

9. **Update the test references** with the new PocketBase version:
   - run the PocketBase tests of the new tag on SQLite in a Linux container (`go test -json -count=1 ./...`) and generate the new list of passing tests, with the same format and normalizations as `tests/golden/pocketbase-v0.40.6-tests.txt` (see `tests/cmd/parity`);
   - capture the API responses of the new tag: copy `tests/golden.go` and the `captureGolden` call of `tests/api.go` to a checkout of the tag, run its tests with `POPGRES_GOLDEN=write` and then `POPGRES_GOLDEN=merge` (to mask the values that change between runs), and copy `tests/golden/api/`;
   - update the `-ref` default of `tests/cmd/parity`, `scripts/ci-test.sh` and the PocketBase version in the docs.

10. **Run the checks** as the CI: the whole suite against Postgres 17 and 18, the parity, the RLS strict mode, `tests/e2e`, the JS SDK tests and the lint (see [CONTRIBUTING.md](CONTRIBUTING.md)). Document every new difference in [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) and add the merge to the [CHANGELOG](CHANGELOG.md).
