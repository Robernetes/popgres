# Linux image to run the popgres tests locally the same way as the CI
# (Go and the Postgres 18 client tools used by the backups).
#
# Built and used by scripts/test-linux.sh.
FROM golang:1.27

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates postgresql-common \
    && /usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y \
    && apt-get install -y --no-install-recommends postgresql-client-18 \
    && rm -rf /var/lib/apt/lists/*
