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

The cluster tables are `UNLOGGED`: their data is temporary and it is lost if the Postgres server crashes (at most a cron job could run twice in the same slot and the rate limits are reset).

Without `POPGRES_CLUSTER` the nodes still synchronize the collections and settings cache, run the migrations and the cron jobs once and share the auth timeouts, but the realtime events are not sent to the other nodes and the rate limits are per node.

## Limitations

- The realtime API rule of a deleted record is checked on the other nodes against its snapshot. A rule that relies on a parent record deleted in the same transaction (eg. a cascade delete) doesn't match on the other nodes.
- The caches are eventually consistent: a request received by another node a few milliseconds after a collection or settings change could still use the previous state.
- The realtime events that a node receives while its database connection is down are lost (as with a PocketBase client that loses its connection).
- Each node keeps one extra database connection for LISTEN.
