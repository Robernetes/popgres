# popgres server image.
#
#   docker build -t popgres:local .
#   docker run --rm -p 8090:8090 -e DATABASE_URL=postgres://... popgres:local
#
# The image includes the Postgres 18 client tools (pg_dump/pg_restore) used by the backups
# and the PocketBase importer (popgres-import, see docs/MIGRATING.md).
#
# The server runs as the non-root user "popgres" (uid/gid 10001), so a bind mounted
# data directory must be writable by it (eg. chown -R 10001:10001 ./popgres_data).
#
# The base images are pinned by digest (updated by Dependabot).

FROM golang:1.27-trixie@sha256:c65ecfe613b539ac74c62a4c3b2c27e8f8a6076384c8f27e8a3c0f3be17cc385 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/Robernetes/popgres.Version=${VERSION} -X github.com/Robernetes/popgres/apis.MCPVersion=${VERSION}" \
      -o /out/popgres ./examples/base \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/popgres-import ./cmd/popgres-import

FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates postgresql-common \
    && /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y \
    && apt-get install -y --no-install-recommends postgresql-client-18 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 popgres \
    && useradd --system --uid 10001 --gid popgres --home-dir /app --shell /usr/sbin/nologin popgres \
    && mkdir -p /app/popgres_data \
    && chown -R popgres:popgres /app

ENV POPGRES_PG_DUMP=/usr/lib/postgresql/18/bin/pg_dump \
    POPGRES_PG_RESTORE=/usr/lib/postgresql/18/bin/pg_restore

COPY --from=build /out/popgres /out/popgres-import /usr/local/bin/

WORKDIR /app

USER 10001:10001

EXPOSE 8090

ENTRYPOINT ["popgres"]
CMD ["serve", "--http=0.0.0.0:8090", "--dir=/app/popgres_data"]
