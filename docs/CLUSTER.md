# Running multiple instances

popgres can run multiple app instances (nodes) with the same Postgres database behind a load balancer, without sticky sessions. The nodes coordinate only through Postgres: there is no Redis or other service to operate.

## Quick start

Start every node with the same database and `POPGRES_CLUSTER=true`:

```bash
DATABASE_URL=postgres://user:pass@db:5432/app POPGRES_CLUSTER=true popgres serve --http=0.0.0.0:8090
```

`docker-compose.cluster.yml` is a demo with 3 nodes behind an nginx round-robin load balancer (`scripts/cluster/nginx.conf`).

### Requirements

- **S3 file storage.** The nodes can't share local files, so the server doesn't start in cluster mode if the S3 storage is not enabled, and it can't be disabled from the settings. Enable it first with a single instance (without `POPGRES_CLUSTER`).
- **TLS at the load balancer.** The HTTPS server with automatic certificates (`--https`) is not supported in cluster mode.
- **Trusted proxy.** Set the client IP header of the load balancer in Settings > Application > Trusted proxy (eg. `X-Forwarded-For`), so that the rate limits and the realtime client checks use the real client IP.
- **SSE through the load balancer.** Disable the response buffering and use a long read timeout for `/api/realtime` (see the nginx config).
- **Readiness check.** Use `GET /api/health?ready` as the load balancer health check, so that an instance without its database or cluster connection doesn't receive traffic (see [OPERATIONS.md](OPERATIONS.md)).
- **Connections.** All instances share the server `max_connections` (see [Database connections](OPERATIONS.md#database-connections)).
- The backups are created on the local disk of the node unless the backups S3 storage is enabled.

The `automigrate` (`migratecmd`) and `hooksWatch` (`jsvm`) plugin options are disabled in cluster mode because they write or watch local files of a single node.

## What is shared between the nodes

| State | How |
|---|---|
| Collections and settings cache | The node that saves them notifies the others (LISTEN/NOTIFY) and they reload their cache. In cluster mode a collection missing from the cache is also looked up in the database (eg. a collection created a few milliseconds ago by another node). |
| Realtime events | The node that changes a record notifies the others with the record ids and each node broadcasts the event to its own clients. The deleted records are sent as a snapshot (the `_realtime_events` table) and the API rules are checked against it. |
| Realtime subscriptions | The client id starts with the id of its node. A subscriptions request or an OAuth2 redirect received by another node is forwarded to the client node. |
| Cron jobs | Each scheduled job runs only once per schedule slot across all nodes (the `_cron_runs` table). A manual run from the dashboard runs on the node that receives the request. |
| Rate limits | The counters are stored in the `_rate_limits` table. |
| Auth emails resend timeouts, Apple OAuth2 name | The `_kv_ttl` table. |
| Migrations, installer superuser, active backup | Postgres advisory locks. |

The cluster tables are `UNLOGGED`: their data is temporary and it is lost if the Postgres server crashes (at most a cron job could run twice in the same slot and the rate limits are reset). Their data is not included in the backups.

Without `POPGRES_CLUSTER` the nodes still synchronize the collections and settings cache, run the migrations and the cron jobs once and share the auth timeouts, but the realtime events are not sent to the other nodes and the rate limits are per node.

## Cluster messages

- Postgres doesn't restrict LISTEN and NOTIFY with privileges: any role that can connect to the database can use them (eg. the logins of the [direct SQL clients](RLS.md)). So the nodes use a channel whose name is derived from a random secret, and each message is signed with it (HMAC-SHA256). The secret is created on the first start and stored in the `_params` table (`popgresCluster`), which is not accessible by the RLS roles. The messages with an invalid signature are ignored and counted.
- The messages could be lost (eg. while the LISTEN connection reconnects or if a node can't handle them fast enough), so each node also compares its cached collections and settings with the database after each (re)connection and every minute, and reloads them if they changed. A rule change is applied by all nodes within a minute even if its message is lost.
- The realtime events are handled separately from the collections and settings changes, so a burst of realtime events doesn't delay them.

## Upgrades

The nodes of different popgres versions could be unable to exchange messages (eg. v1.0 nodes don't sign them), so stop all nodes before starting the new version. The collections, settings and RLS state are reloaded on start.

## Untrusted database roles

The roles used by the direct SQL clients can't read or write the app tables beyond their RLS policies, but they can still hold [advisory locks](https://www.postgresql.org/docs/current/explicit-locking.html#ADVISORY-LOCKS) and open connections. Limit them on the server:

```sql
ALTER ROLE reporting CONNECTION LIMIT 5;
ALTER ROLE reporting SET statement_timeout = '30s';
ALTER ROLE reporting SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE reporting SET idle_session_timeout = '15min';
```

## Limitations

- The realtime API rule of a deleted record is checked on the other nodes against its snapshot. A rule that relies on a parent record deleted in the same transaction (eg. a cascade delete) doesn't match on the other nodes.
- The caches are eventually consistent: a request received by another node a few milliseconds after a collection or settings change could still use the previous state (up to a minute if the change message is lost).
- The realtime events that a node receives while its database connection is down are lost (as with a PocketBase client that loses its connection).
- Each node keeps one extra database connection for LISTEN.
