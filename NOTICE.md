# NOTICE

**popgres** is a fork of [PocketBase](https://github.com/pocketbase/pocketbase) v0.40.6 (commit `9d281cc`), created and maintained by Gani Georgiev and its contributors under the MIT License. The original copyright and license are kept unchanged in [`LICENSE.md`](LICENSE.md).

popgres is an independent project. It is not affiliated with, endorsed by, or supported by PocketBase or its author. Please do not report popgres issues to the PocketBase project.

## What changes in this fork

- PostgreSQL 17+ is the only supported database. SQLite is removed from the main binary; it is used only by the separate `popgres-import` tool to read existing PocketBase data.
- Horizontal scaling (multiple instances sharing one database), API rules compiled to Postgres row level security, realtime through logical replication, an embedded Postgres for development, a vector field type (pgvector) and a built-in MCP server.
- The binary is named `popgres`, the Go module is `github.com/Robernetes/popgres` and the default data directory is `popgres_data`.

The REST API and the official PocketBase SDKs are kept compatible.

The upstream git history up to `v0.40.6` is preserved in this repository to keep authorship and to allow porting upstream fixes.
