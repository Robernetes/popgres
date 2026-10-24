-- Prepares a fresh Postgres server for the popgres test suite (ADR-0013).
--
-- Run it as the container superuser. It is idempotent:
--
--   psql -v ON_ERROR_STOP=1 -v app_password="$APP_PASSWORD" -f scripts/ci-db-setup.sql
--
-- It creates the non-superuser role popgres_app, the databases popgres and
-- popgres_test owned by it, and the vector and pgcrypto extensions in both
-- (vector is not a trusted extension, so a superuser must create it).

\set ON_ERROR_STOP on

SELECT format(
    'CREATE ROLE popgres_app LOGIN CREATEDB CREATEROLE REPLICATION NOSUPERUSER NOBYPASSRLS PASSWORD %L',
    :'app_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'popgres_app')
\gexec

-- keep the attributes and the password in sync when the role already exists
SELECT format(
    'ALTER ROLE popgres_app LOGIN CREATEDB CREATEROLE REPLICATION NOSUPERUSER NOBYPASSRLS PASSWORD %L',
    :'app_password'
)
\gexec

SELECT 'CREATE DATABASE popgres OWNER popgres_app'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'popgres')
\gexec

SELECT 'CREATE DATABASE popgres_test OWNER popgres_app'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'popgres_test')
\gexec

\connect popgres
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;

\connect popgres_test
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
