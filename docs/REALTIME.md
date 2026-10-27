# Realtime events

popgres has two sources for the realtime record events (`POPGRES_REALTIME_SOURCE`):

| Source | Events | Requirements |
|---|---|---|
| `hooks` (default) | The changes made through the app (REST API, hooks, migrations), the same as PocketBase. | None |
| `replication` | All committed changes of the collection tables, including the ones made outside of the app (direct SQL, other services, ETL). | `wal_level=logical` and a database role with the `REPLICATION` attribute |

With `replication` the source falls back to `hooks` (with a warning in the logs) if the server `wal_level` is not `logical` or the role doesn't have the `REPLICATION` attribute.

## How the replication source works

- One app instance (the leader, elected with a Postgres advisory lock) consumes the logical replication slot `popgres_rt_<database>` with the `pgoutput` plugin and the publication `popgres_rt_pub`. If the leader stops, another instance takes over and continues from the same slot. If the leader host dies without closing its connections, the server releases the lock after about a minute (the lock connection has an `idle_session_timeout`).
- The publication contains all collection tables (it is kept in sync when collections are created). The collection tables use `REPLICA IDENTITY FULL` so that the deleted records events have all fields.
- For each committed change the leader stores an event in the `_realtime_events` table and notifies all instances, which broadcast it to their own clients with the same API rules checks. The deleted records are checked against their last state (see the limitations below).
- The delivery is "at least once" internally and the instances discard the duplicated events, so a client receives each change once.

The events of the changes made through the API also come from the replication, so they are sent after the transaction commit is decoded (usually a few milliseconds later instead of right after the request).

## Monitoring

A replication slot retains the WAL until it is consumed. The superusers health data (`GET /api/health`) and the dashboard (Settings > Application > Realtime) show the slot status, the lag and the retained WAL size, and a warning is logged if the retained WAL exceeds 1 GB.

It is recommended to set `max_slot_wal_keep_size` on the server (eg. `1GB`) so that an unconsumed slot can't fill the disk. If the slot is invalidated because of this limit, the leader recreates it with a warning and the events of the changes in between are lost (PocketBase doesn't resend the missed events either).

The slot is a server-wide object. If the realtime replication is no longer used, drop it:

```sql
SELECT pg_drop_replication_slot('popgres_rt_<database>');
```

## Limitations

- The API rule of a deleted record is checked against its last state. A rule that relies on a parent record deleted in the same transaction (eg. a cascade delete) doesn't match.
- The logical decoding starts only after a consistent point of all the server transactions. After a start or a leader change the first events could be delayed by up to about 15 seconds if there were long running transactions (they are not lost).
- `TRUNCATE` doesn't send events.
