## What and why

<!-- What does this change do, and why? Link the related issue (eg. "Fixes #12"). -->

## How it was tested

<!-- The tests added or changed, and how you ran them (eg. "go test ./apis/... with PostgreSQL 18"). -->

## Checklist

- [ ] Tests added or updated for the change.
- [ ] `make lint` and `./scripts/check-sql-patterns.sh` pass.
- [ ] The PocketBase API responses and the SDK compatibility are unchanged, or the difference is documented in `docs/COMPATIBILITY.md`.
- [ ] The new SQL is Postgres SQL with parameters (no concatenated user values) and works on PostgreSQL 17.
- [ ] The change works with several instances sharing the database (no shared state in memory or on the local disk).
- [ ] The documentation in `docs/` is updated, if the behavior changes.
- [ ] `ui/dist` is rebuilt, if the dashboard UI changed.
