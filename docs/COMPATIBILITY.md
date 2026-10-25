# Compatibility with PocketBase

popgres is a fork of PocketBase v0.40.6 that runs only on PostgreSQL 17+.

The REST API, the JSON responses, the filter syntax and the official JS and Dart SDKs are compatible with PocketBase. This document lists the known differences.

The API test scenarios are compared with the responses of PocketBase v0.40.6 (`tests/golden/api`). The accepted differences are listed in `tests/golden/accepted.txt` and each of them must be explained here.

## Database

- The data is stored in a Postgres database (`DATABASE_URL`). The logs are stored in the `_aux` schema of the same database or in a separate database (`POPGRES_LOGS_URL`).
- There are no `data.db` and `auxiliary.db` files in `pb_data`. The directory keeps only the local files (storage, backups, etc.).
- To migrate an existing `pb_data` use the `popgres-import` command.

### Column types

| Field | SQLite (PocketBase) | Postgres (popgres) |
|---|---|---|
| text, email, url, editor, password, single select/file/relation | `TEXT` | `TEXT` |
| number | `NUMERIC` | `DOUBLE PRECISION` |
| bool | `BOOLEAN` (0/1) | `BOOLEAN` |
| date, autodate | `TEXT` | `TIMESTAMPTZ` (an empty date is `NULL`) |
| json, geoPoint, multiple select/file/relation | `JSON` (text) | `JSONB` |

The API still returns an empty date as `""`.

`TableInfo()` (and `PRAGMA table_info` based code) reports the Postgres column types and default expressions, for example `'[]'::jsonb`.

### JSON values

The json field values are stored as `jsonb`. Postgres doesn't keep the original formatting of the value:

- the object keys are returned sorted by their length and then by their bytes (`{"b":1,"aa":2,"a":3}` is returned as `{"a":3,"b":1,"aa":2}`);
- duplicated object keys are removed (the last one is kept);
- the whitespace between the tokens is not preserved.

### Database errors

The error messages that include a raw database error (eg. an invalid view query, an invalid index or a failed SQL console query) contain the Postgres error instead of the SQLite one, for example `ERROR: syntax error at or near "CREATE" (SQLSTATE 42601)` instead of `SQL logic error: near "CREATE": syntax error (1)`.

## Filters and sorting

The filter syntax is the same. The differences come from the stricter Postgres type system:

- `~` and `!~` use `ILIKE`, which is case-insensitive also for the non-ASCII characters (the SQLite `LIKE` is case-insensitive only for ASCII).
- `true` and `false` compared with a non-bool field are converted to `1` and `0` as in SQLite (eg. `title = true` matches the title `"1"`).
- A value that is not valid for the type of the other operand (eg. `number = "abc"` or `created > "abc"`) is compared as text instead of failing.
- `strftime()` is emulated with a Postgres function. It supports the same format specifiers and modifiers as the SQLite function for the values used by PocketBase (dates, unix timestamps and julian days).
- Sorting by `@rowid` is not supported and returns a `400` error. Use for example `sort=-created,id`.
- A list request without `sort` returns the records in the Postgres physical order. It is the insertion order for new tables but it changes after updates (SQLite returns the rowid order). Use `sort` when the order matters.

## View collections

The view queries are executed by Postgres and must be written in Postgres SQL:

- identifiers are quoted with double quotes (`"demo1"."number"`), not backticks or brackets. Mixed case column names must be quoted (`a."emailVisibility"`);
- every `JOIN` needs an `ON` clause (`LEFT JOIN demo2 b ON true`);
- the `GROUP BY` rules are stricter: every selected column must be grouped or aggregated (grouping by the primary key is enough for the other columns of the same table);
- there is no `total()` function and the numeric aggregations of text values need an explicit cast;
- an unnamed expression is named by Postgres (`count(*)` is named `count`, `1 + 1` is named `?column?`), so always use an alias.

The `id` column of a view is wrapped with `CAST(id as TEXT)` when it is not a text column (for example a number, bool, date or json value).

The field types are detected from the referenced collection fields and from the Postgres column types of the view.

## Indexes

The collection indexes are stored and returned in the PocketBase format (``CREATE INDEX `idx` ON `table` (`column`)``). When they are created in the database:

- the identifiers are quoted with double quotes;
- `COLLATE NOCASE` is replaced with a `lower()` expression index.

The custom index expressions must be valid Postgres SQL.

## SQL in hooks and migrations

`$app.db()`, `app.DB()` and the raw SQL of the JS and Go migrations run on Postgres. The dbx placeholders are the same (`{{table}}`, `[[column]]` and `{:param}`) but the SQL must be Postgres SQL (no `json_each`, `strftime`, `PRAGMA`, `sqlite_master`, etc.).

`app.ConcurrentDB()` and `app.NonconcurrentDB()` return the same Postgres connection pool, and the same applies to the auxiliary (logs) builders.

## SQL console

The SQL console (`POST /api/sql`) runs Postgres SQL. The write statements (`INSERT`, `UPDATE`, `CREATE`, `TRUNCATE`, etc.) run in a transaction. When a read query has several statements separated by `;`, they are executed in a transaction and the rows of the last one are returned.

The result columns are described by Postgres:

- an unnamed expression is named by Postgres (`SELECT 1` returns the column `?column?`, SQLite returns `1`);
- the column `type` is the Postgres type name (eg. `INT4`, `TEXT`), SQLite returns the declared column type or an empty string for the expressions;
- the column `nullable` is `false` when Postgres doesn't report it (the expressions), SQLite always reports `true`.

## Backups

- A backup contains a `pg_dump` of the database (so the backup sizes differ from the PocketBase ones) (`data.dump`, plus `auxiliary.dump` when the logs are in a separate database) and the local files of `pb_data`.
- The backups require `pg_dump` (and `pg_restore` to restore) in the `PATH`, with the same or a newer major version than the server. The paths can be changed with `POPGRES_PG_DUMP` and `POPGRES_PG_RESTORE`.
- A backup can be restored only from the command line with `popgres restore <backup> --yes` (stop all the app instances first). The restore API (`POST /api/backups/{key}/restore`) and the dashboard button return an error.

## Multiple instances

popgres can run multiple instances with the same database (see [CLUSTER.md](CLUSTER.md)). This adds a few differences even for a single instance:

- `GET /api/crons` lists the `__popgresClusterCleanup__` system job, which deletes the expired rows of the cluster tables every 5 minutes.
- The database has the `UNLOGGED` tables `_kv_ttl`, `_cron_runs`, `_rate_limits` and `_realtime_events`.
- The resend timeouts of the password reset and verification emails and the Apple OAuth2 user name are stored in the `_kv_ttl` table instead of the app memory store (`$app.store()`).

In cluster mode (`POPGRES_CLUSTER=true`) the realtime client ids start with the id of their node (`<node>_<random>`, with the same length of 40 characters).

## Removed features

- The `__pbDBOptimize__` cron job (`PRAGMA optimize`, `VACUUM`). Postgres autovacuum is used instead, so the job is not listed by `GET /api/crons`.
- The logs `VACUUM` after their deletion.
