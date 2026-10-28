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

FROM golang:1.27-trixie@sha256:7b072c590a326b9d2a43b165ae247f6e8eafff32e1ad154ab97139e89f053be9 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/Robernetes/popgres.Version=${VERSION} -X github.com/Robernetes/popgres/apis.MCPVersion=${VERSION}" \
      -o /out/popgres ./examples/base \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/popgres-import ./cmd/popgres-import

FROM debian:trixie-slim@sha256:89c1ea91e897f0fe0a3731595330bc45b8bdef4047464a07116f891a94054b31

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
