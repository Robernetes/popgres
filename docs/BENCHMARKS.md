# Benchmarks

These benchmarks compare popgres (a single instance and 3 instances) with PocketBase v0.40.6 on the same machine. They show the order of magnitude of the throughput and latencies; run them on your own hardware before drawing conclusions for a production setup.

Run them with:

```sh
./bench/run.sh
```

The script builds popgres from the sources and PocketBase v0.40.6 from the upstream tag, starts the environment of `bench/docker-compose.yml` and runs the load generator (`bench/main.go`), which works with any PocketBase compatible server.

## Environment

- Intel Core Ultra 9 275HX, Docker Desktop 29.7 (24 CPUs and 16 GB available to Docker), Windows 11.
- All services run in the same Docker network with CPU limits, and each target runs alone (the services of the other targets are stopped):

  | Target | Services |
  |---|---|
  | `pocketbase` | PocketBase v0.40.6 (SQLite): 2 CPUs |
  | `popgres` | 1 popgres instance (2 CPUs) + Postgres 18 (4 CPUs) |
  | `popgres-3-nodes` | 3 popgres instances (2 CPUs each) + Postgres 18 (4 CPUs), with the realtime replication source |

- The load generator runs in its own container (4 CPUs). For the 3 instances it spreads the requests over them (client side load balancing).
- Postgres 18.6 with `shared_buffers=1GB`. The binaries are built with Go 1.27.

Note that popgres uses more resources than PocketBase in these setups (the Postgres server), and that a Postgres server on another machine adds a network round trip to each query.

## Scenarios

- **Reads:** 50% views of a random record and 50% list pages (`perPage=20`, a filter on an indexed number field, sort and `skipTotal`), over 10 000 records. Public rules.
- **Writes:** concurrent record creates (5 fields). Public rules.
- **Realtime:** 500 clients subscribed to the collection, 200 records created at 20 per second; the latency is measured from the create request to the event received by each client (100 000 deliveries).

Each reads and writes measurement lasts 20 seconds and is repeated 3 times (the table shows the median run).

## Results

Measured on 2026-10-26 (`bench/results/20261026-074039.jsonl`).

### Reads

| Target | Concurrency | Requests/s | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|---:|---:|
| pocketbase | 50 | 7 324 | 3.6 | 24.2 | 41.9 |
| pocketbase | 200 | 6 192 | 17.8 | 87.5 | 360.8 |
| popgres | 50 | 8 766 | 4.4 | 14.6 | 23.5 |
| popgres | 200 | 8 166 | 16.0 | 74.8 | 126.3 |
| popgres-3-nodes | 50 | 19 657 | 2.0 | 6.7 | 10.7 |
| popgres-3-nodes | 200 | 19 728 | 7.3 | 27.7 | 42.8 |

### Writes

| Target | Concurrency | Requests/s | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|---:|---:|
| pocketbase | 50 | 3 077 | 7.8 | 61.3 | 136.5 |
| pocketbase | 200 | 3 079 | 35.9 | 198.5 | 337.8 |
| popgres | 50 | 8 161 | 4.6 | 14.1 | 26.1 |
| popgres | 200 | 5 953 | 16.5 | 72.6 | 202.0 |
| popgres-3-nodes | 50 | 9 952 | 2.7 | 16.1 | 45.4 |
| popgres-3-nodes | 200 | 13 341 | 8.5 | 47.4 | 117.3 |

### Realtime fan-out

| Target | Clients | Deliveries | Missing | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---:|---:|---:|---:|---:|---:|
| pocketbase | 500 | 100 000 | 0 | 4.2 | 9.5 | 11.5 |
| popgres | 500 | 100 000 | 0 | 5.5 | 13.8 | 33.6 |
| popgres-3-nodes | 500 | 100 000 | 0 | 11.1 | 28.0 | 32.0 |

There were no errors in any scenario.

## Observations

- **Writes** are where Postgres makes the biggest difference: SQLite has a single writer, so the concurrent creates of PocketBase queue up (p95 of 61-199 ms), while popgres keeps a p95 of 14 ms with 50 concurrent writers.
- **Reads** of a single popgres instance are slightly faster than PocketBase in this setup, and 3 instances more than double the throughput (the Postgres server, not the instances, becomes the limit).
- **Realtime:** with a single instance the events come from the app hooks, as in PocketBase. With 3 instances they come from the Postgres logical replication (decoded by the leader and published to the other instances through `LISTEN/NOTIFY`), which adds a few milliseconds. The replication source is designed for the changes made outside of the API too.
- **Replication catch up:** during the sustained writes of the 3 instances (more than 10 000 creates per second for a minute) the replication slot falls behind; it caught up 20 seconds after the writes stopped. The realtime events of a sustained write peak of that size are delivered late, not lost.

The benchmarks found and fixed three performance issues of popgres before these results: the database connections were reopened under a moderate load (the idle pool was smaller than the open one), the replication leader stored and published each transaction separately, and each instance loaded each replicated record with a separate query.

## Limitations

- A single machine with Docker Desktop: the containers share the host CPU and memory, and the network is a local bridge.
- The load generator balances the requests over the 3 instances itself; a load balancer in front of them (eg. nginx) adds its own overhead, which is not measured here.
- The scenarios use public rules; the API rules add their filters to the queries (and the realtime checks) in both PocketBase and popgres.
- Supabase is not included.
