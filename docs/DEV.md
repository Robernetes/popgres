# Development mode

`popgres serve --dev` (or `go run . serve`, which enables the dev mode by default) without a `DATABASE_URL` starts an embedded Postgres server, so there is nothing to install to try popgres:

```bash
popgres serve --dev
```

- The server is the official Postgres 18 build of [zonky](https://github.com/zonkyio/embedded-postgres-binaries), used through [embedded-postgres](https://github.com/fergusstrange/embedded-postgres). The first start downloads it (15 to 35 MB) from Maven Central, verifies it with the SHA-256 checksums built into popgres and caches it in the user cache directory (eg. `~/.cache/popgres/embedded-postgres` on Linux). The platforms without a popgres checksum (eg. Alpine Linux or 32-bit ARM) are downloaded by the library and verified only with the checksum published next to the binaries.
- The data is stored in `popgres_data/.pg` and the server listens only on `localhost`, on a free port of each start.
- It has `wal_level=logical`, so the replication realtime source works too.
- It is configured for development (`fsync=off`): don't use it for production data. With a `DATABASE_URL` nothing embedded is started.
- The server is stopped when popgres stops. If popgres was killed, the next start stops the previous server first.

`popgres dev reset` deletes the embedded Postgres data (`popgres_data/.pg`). popgres must be stopped.

## Limitations

- The embedded binaries don't include `pg_dump`, so the backups are not available unless the Postgres client tools are installed (or `POPGRES_PG_DUMP` points to them).
- The embedded binaries don't include pgvector. Use the Docker image of `docker-compose.dev.yml` (or any Postgres with pgvector) for the vector features.
