# Migrating from PocketBase

`popgres-import` copies a PocketBase v0.23+ `pb_data` directory into a popgres (Postgres) database: the collections, the records, the settings, the request logs and the local files.

It is a separate binary because it is the only part of popgres that reads SQLite.

## Steps

1. Stop the PocketBase app (or make a copy of `pb_data` from a backup) so that the SQLite files are consistent.
2. Prepare an empty Postgres 17+ database for popgres (with the `pgcrypto` extension, and `vector` if you plan to use vector fields).
3. Run the import:

   ```sh
   DATABASE_URL="postgres://user:pass@host:5432/mydb" \
     popgres-import -dir ./popgres_data ./pb_data
   ```

   | Flag | Description |
   |---|---|
   | `-db` | The target database (default `DATABASE_URL`). |
   | `-dir` | The popgres data directory where the local `storage` files are copied (not copied if empty). |
   | `-encryptionEnv` | The env variable with the encryption key of the PocketBase settings, if they were encrypted (`--encryptionEnv`). |
   | `-skipLogs` | Skip the request logs (`auxiliary.db`). |
   | `-force` | Import into a database that already has collections: the imported collections and their records are replaced, so the import can be repeated. |
   | `-debug` | Print the executed SQL. |

4. Read the report and fix what it lists (see below). The command exits with code 3 when something needs manual changes.
5. Start popgres with the same `DATABASE_URL` and data directory. If the PocketBase settings were encrypted (`--encryptionEnv`), start popgres with the same encryption key env variable.
6. Your PocketBase clients (JS/Dart SDKs) work without changes; the auth tokens issued by PocketBase stay valid because the records token keys and the collection secrets are imported.

## What is imported

- The collections (with their ids, fields, rules, indexes and options) and the view collections. The view queries are converted to Postgres identifier quotes.
- The records of all collections, with the values converted to the Postgres types (dates to `timestamptz`, `0`/`1` to booleans, JSON text to `jsonb`, single and multiple values to the field format).
- The settings (`_params`) and the request logs (`auxiliary.db`).
- The files: the local `storage` directory is copied with `-dir`. If PocketBase used S3, the imported settings point to the same bucket, so there is nothing to copy.

The JS hooks and migrations (`pb_hooks`, `pb_migrations`) are not part of the database: copy them next to the popgres binary. Check them for SQLite specific SQL (see [COMPATIBILITY.md](COMPATIBILITY.md)).

## The report

```
collections: 12
  posts                    1250 records
  ...
files: local storage, copied to ./popgres_data
views that need to be adapted manually: 1
  stats: ...
indexes that could not be created (removed, recreate them manually): 1
  posts: CREATE INDEX idx_x ON posts (json_extract(data, '$.a')) (...)
API rules that fail in Postgres (fix them manually): 1
  posts: listRule: ERROR: operator does not exist: text > timestamp with time zone ...
API rules enforced only by the API (not translatable to RLS policies): 2
  posts: createRule: @request.body.status = 'draft'
```

- **Views:** the view queries are SQL; the ones with SQLite only syntax or functions are not created and need to be rewritten in Postgres SQL (eg. in the dashboard).
- **Indexes:** the indexes with SQLite only expressions are removed from their collection; recreate them with Postgres syntax.
- **API rules that fail:** each rule is executed in Postgres (as a guest and as a record of each auth collection). SQLite compares values of any type, Postgres doesn't (eg. a text field with a date field); fix the rule, otherwise the API requests that use it fail.
- **API rules enforced only by the API:** the rules that use request data (`@request.body`, `@request.query`, `@request.headers`, ...) work the same in the API, but they can't be applied by the Postgres RLS policies for the direct SQL clients (see [RLS.md](RLS.md)). No change is needed.

## Repeating the import

The import into an empty database is deterministic. To repeat it (eg. after fixing the source), run it again with `-force`: the collections and their records are replaced and the logs already imported are skipped.
