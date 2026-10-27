<p align="center">
    <img src="assets/brand/popgres_logo.png" alt="popgres logo" width="160" />
    <br />
    <img src="assets/brand/popgres_name.png" alt="popgres" width="320" />
</p>

<p align="center">
    The PocketBase experience on PostgreSQL.
</p>

popgres is a fork of [PocketBase](https://pocketbase.io) v0.40.6 that runs on **PostgreSQL 17+** instead of SQLite.

It keeps the PocketBase experience (a single binary with the REST API, realtime, auth, files and the dashboard UI) and the compatibility with the official [JS](https://github.com/pocketbase/js-sdk) and [Dart](https://github.com/pocketbase/dart-sdk) SDKs, and adds what Postgres makes possible:

- **Multiple instances** behind a load balancer, coordinated only through Postgres ([CLUSTER.md](docs/CLUSTER.md)).
- **Row level security**: the collection API rules are compiled to Postgres RLS policies, so they also apply to direct SQL clients ([RLS.md](docs/RLS.md)).
- **Realtime from the database**: optionally, the realtime events come from the Postgres logical replication, including the changes made outside of the API ([REALTIME.md](docs/REALTIME.md)).
- **Vector search** with a pgvector `vector` field and a similarity search endpoint ([VECTOR.md](docs/VECTOR.md)).
- **MCP server** for AI agents, with tools generated from the collections and the same API rules ([MCP.md](docs/MCP.md)).
- **Embedded Postgres for development**: `popgres serve --dev` without a database starts a local Postgres ([DEV.md](docs/DEV.md)).
- **Operations**: readiness checks, connection pools, poolers and multiple databases on one server ([OPERATIONS.md](docs/OPERATIONS.md)).

The differences with PocketBase are listed in [COMPATIBILITY.md](docs/COMPATIBILITY.md).

## Quick start

### Docker

```sh
docker build -t popgres .

docker run -d -p 8090:8090 \
  -e DATABASE_URL="postgres://user:pass@db-host:5432/popgres" \
  -v popgres_data:/app/popgres_data \
  popgres
```

Open `http://localhost:8090/_/` to create the first superuser. The image includes the Postgres client tools for the backups and the `popgres-import` command.

### Binary

Download the `popgres` archive for your platform from the releases page and run:

```sh
DATABASE_URL="postgres://user:pass@localhost:5432/popgres" ./popgres serve
```

The database needs the `pgcrypto` extension (and `vector` for the vector fields). The app role needs the `CREATEROLE` attribute for the RLS roles (see [RLS.md](docs/RLS.md)).

### Development

```sh
./popgres serve --dev
```

Without a `DATABASE_URL`, the dev mode downloads and starts an embedded Postgres in `popgres_data/.pg`.

## Migrating from PocketBase

```sh
DATABASE_URL="postgres://..." popgres-import -dir ./popgres_data ./pb_data
```

The collections, records, settings, logs and files are imported, and the report lists what needs manual changes (eg. SQLite specific view queries). Your SDK clients and their auth tokens keep working. See [MIGRATING.md](docs/MIGRATING.md).

## Benchmarks

On the same machine, a single popgres instance handles about 2 to 2.6 times the concurrent writes of PocketBase (SQLite has a single writer), and 3 instances more than double the reads of a single one. See [BENCHMARKS.md](docs/BENCHMARKS.md) for the setup, the results and their limitations.

## Use as a Go framework

popgres is a Go library with the same API as PocketBase (hooks, routes, migrations, the JS VM plugin):

```go
package main

import (
    "log"

    "github.com/Robernetes/popgres"
    "github.com/Robernetes/popgres/core"
)

func main() {
    app := popgres.New()

    app.OnServe().BindFunc(func(se *core.ServeEvent) error {
        se.Router.GET("/hello", func(re *core.RequestEvent) error {
            return re.String(200, "Hello world!")
        })

        return se.Next()
    })

    if err := app.Start(); err != nil {
        log.Fatal(err)
    }
}
```

The raw SQL of the hooks and migrations must be Postgres SQL (see [COMPATIBILITY.md](docs/COMPATIBILITY.md)).

## Development of popgres

```sh
# Postgres 18 + pgvector for development (credentials in .env)
docker compose -f docker-compose.dev.yml up -d

# the test suite (POPGRES_TEST_URL is the template database of the tests)
POPGRES_TEST_URL="postgres://user:pass@localhost:5433/popgres_test" go test ./...

# the JS SDK compatibility tests against a server built from the sources
DATABASE_URL="postgres://..." ./scripts/sdk-test.sh

# the dashboard UI (Node 25+)
npm --prefix ui ci && npm --prefix ui run build
```

## License

popgres is distributed under the MIT license, the same as PocketBase. PocketBase is created and maintained by Gani Georgiev; popgres is not affiliated with the PocketBase project.
