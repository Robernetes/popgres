# Operations

## Health checks

| Request | Use | Response |
|---|---|---|
| `GET /api/health` | liveness (the process serves requests) | always `200`, the same as PocketBase |
| `GET /api/health?ready` | readiness (eg. the load balancer or a Kubernetes readiness probe) | `200`, or `503` if the database doesn't answer in 3 seconds or the cluster LISTEN connection of the instance is down |

```json
{"message":"API is not ready.","code":503,"data":{"ready":{"database":true,"cluster":false}}}
```

An instance without its LISTEN connection doesn't receive the collections and settings changes of the other instances (it reloads them when it reconnects), so it is better to stop sending it traffic.

For the superusers the health data also has:

- `realtime`: the realtime source and the replication slot metrics (see [REALTIME.md](REALTIME.md#monitoring));
- `cluster`: the instance id, whether the LISTEN connection is connected, whether the instance is the replication leader and the counters of the dropped cluster messages (a dispatch queue was full), of the invalid ones (wrong signature) and of the realtime events that couldn't be broadcasted;
- `dbPool`: the main pool connections (max, open, in use, idle) and how many times and how long (milliseconds) the requests waited for a free connection.

The counters are per instance and they are reset on restart.

## Database connections

Each instance opens:

| Connections | Default | Setting |
|---|---|---|
| Main pool | up to 20 | `POPGRES_DB_MAX_CONNS` (or `popgres.Config.DataMaxOpenConns`) |
| Logs pool | up to 5 | `core.BaseAppConfig.AuxMaxOpenConns` |
| Cluster LISTEN | 1 | |
| Realtime replication (only the leader with `POPGRES_REALTIME_SOURCE=replication`) | 2 | |

The total for all instances must fit in the server `max_connections` (100 by default, shared with the other clients and minus the reserved ones). For example 3 instances use up to 3 × (20 + 5 + 1) + 2 = 80 connections.

The idle connections are closed after 3 minutes and each connection is closed after 30 minutes (`POPGRES_DB_CONN_MAX_LIFETIME`, a Go duration like `1h`, `0` disables it), so that the connections are rebalanced after a failover and the server processes don't grow forever.

A larger pool doesn't make the queries faster once the server CPUs are busy. If `dbPool.waitCount` keeps growing and the server has spare CPU, increase `POPGRES_DB_MAX_CONNS`.

## Connection poolers

Connect the app instances **directly** to Postgres or through a pooler in **session mode** (eg. PgBouncer `pool_mode = session`). The transaction mode is not supported for the app connections because they use session features:

- the cluster `LISTEN` connection;
- the session advisory locks (eg. the migrations, the backups and the replication leader);
- the `search_path` startup parameter of the logs pool;
- the prepared statements of the driver.

The realtime replication connection uses the replication protocol, which the poolers don't support: with `POPGRES_REALTIME_SOURCE=replication` the `DATABASE_URL` must point to the Postgres server (otherwise the leader can't start the replication: it logs the error and retries, and the realtime events are not sent).

The pools of popgres are small, so a pooler in front of the app is usually not needed. The [direct SQL clients](RLS.md) can use a pooler in transaction mode if they set the RLS token for each transaction (`popgres_rls.set_claims($1, true)`).

## Collection changes and table locks

Creating, updating or deleting a collection changes its table and the RLS policies. These statements need an exclusive lock of the table, and while they wait for it all other queries of the table wait too. So they wait at most 5 seconds (unless the app role has its own `lock_timeout`) and the collection change fails with the error "the collection table is locked by another transaction" (eg. a long running direct SQL query or an idle open transaction). Retry it after the other transaction ends.

Limit the transactions of the direct SQL clients (see [Untrusted database roles](CLUSTER.md#untrusted-database-roles)).

## Multiple popgres databases on one server

Some objects of Postgres are server-wide, so the databases of the same server are not fully isolated:

- **The RLS roles** (`popgres_anon`, `popgres_authenticated` and `popgres_api`) are shared by all popgres databases. A login role granted `popgres_authenticated` for one database can connect to the other databases where it has the `CONNECT` privilege and read their public (`""` rule) records. New databases grant `CONNECT` to `PUBLIC` by default, so revoke it and grant it only to the roles of each database:

  ```sql
  REVOKE CONNECT ON DATABASE app1 FROM PUBLIC;
  GRANT CONNECT ON DATABASE app1 TO app1_owner, app1_reporting;
  ```

- **The `REPLICATION` attribute** (required by the `replication` realtime source) allows the role to stream the changes of any database and to take a physical copy of the whole server (eg. with `pg_basebackup`). Use a separate app role per database and restrict its replication connections in `pg_hba.conf`, or don't give it the attribute (the realtime events come from the hooks).
- **The replication slots** are per server. Each database has its own slot (`popgres_rt_<database>`, with a hash suffix if the database name has characters other than lowercase letters, digits and `_`), so `max_replication_slots` and `max_wal_senders` must allow one per database that uses the `replication` source.
- The advisory locks and the cluster channels are per database, so the instances of different databases don't interfere.

If the apps of the databases don't trust each other, use separate Postgres servers.
