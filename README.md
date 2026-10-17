# aredn-map-backend

Scans an AREDN mesh and serves what it learned. Starting from one or more seeds it reads
each node's own pages, records the radio links and neighbour tables those pages publish,
probes reachability with ICMP, and keeps the result in memory.

There is no database. The store lives in the process, so a restart starts from the seeds
again. A sweep re-reads everything it knows, so a node that disappeared stops being reported.

## Deployment

This service answers on its own origin only: it knows nothing about where the frontend
lives. Host the frontend, built from `aredn-map-frontend`, as a static site on its own
domain, point it here at build time with `NUXT_PUBLIC_API_BASE`, and list
that frontend origin under `server.allow_origins`. Leaving `allow_origins` empty answers
`Access-Control-Allow-Origin: *`, which is enough for a browser to read this API from
anywhere: the requests carry no cookies or session.

`server.static_dir` is an opt-in shortcut for a single-process run on one machine. It is
empty by default, so an unconfigured boot serves JSON and the socket only.

## Run

```sh
cp config.example.yaml config.yaml   # then put your map keys in config.yaml only
go build -o bin/aredn-map .
./bin/aredn-map                    # reads config.yaml from ./ or ../
./bin/aredn-map -config /path/to/config.yaml
./bin/aredn-map -once              # one full sweep, print the summary, exit
```

`AREDNSCAN_CONFIG` names the file as an environment variable. A missing file is not an
error: the defaults in `internal/infra/config/model.go` describe a working scanner pointed
at `localnode.local.mesh`.

## Configuration

| key | meaning |
| --- | --- |
| `server.port`, `server.host` | listen address |
| `server.allow_origins` | frontend origins allowed to read this API cross-origin; empty = any |
| `server.static_dir` | optional frontend bundle to serve from this process; empty by default |
| `scan.seeds` | hostnames or IPs to start from, usually the access point |
| `scan.cron`, `scan.cron_zone` | when to re-sweep; 5- or 6-field, or `@every` |
| `scan.run_on_startup` | sweep once at boot |
| `scan.max_hops` | how far to expand, `-1` for as far as the mesh goes |
| `scan.max_nodes` | ceiling on nodes held, `0` for none |
| `scan.worker_goroutines` | page fetches in flight |
| `scan.detail_concurrency` | concurrent `/a/sysinfo` and device-page reads |
| `scan.node_timeout`, `scan.sweep_timeout` | per-node and per-sweep budgets |
| `scan.defer_detail_beyond_hops` | beyond this hop count, list nodes without fetching pages |
| `scan.absorb_mesh_hosts` | register names from each supernode's host table, without scanning them |
| `scan.relazy_after` | how stale a node must be to re-read its page |
| `scan.revisit_after` | wait before retrying a host that failed; `0` disables the lane |
| `http.*` | timeouts, retries and user agent for page fetches |
| `icmp.*` | probe count, intervals, concurrency and mode |
| `logging.*` | level, console mirroring, and rotating file |
| `map.*` | basemap keys and the initial view the frontend starts from |

## Layout

```
main.go                     bootstrap: config, logger, resources, cron, HTTP
internal/infra/config       typed config, defaults, validation
internal/infra/logger       zap core, console and rotating file sinks
internal/meta               version and user agent
internal/model              the wire contract: node, link, band rules, payloads
internal/coord              WGS-84 to GCJ-02 and Maidenhead grids
internal/repository/aredn   page and JSON reads from one AREDN node
internal/repository/ping    ICMP probes and the resolution they need
internal/repository/store   in-memory tables, merge rules, change ledger
internal/service/scanner    the sweep: discovery, expansion, probes, details, revisits
internal/cron               the gocron scheduler and its registered job
internal/controller         request handlers by domain, and the shared deps
internal/handler            Fiber app, route table, WebSocket push, static bundle
```

## HTTP

| | |
| --- | --- |
| `GET /api/health` | liveness |
| `GET /api/status` | counters, sweep state, band and link-type totals |
| `GET /api/nodes` | the node table, with `q`, `band` and `state` filters |
| `GET /api/links` | the link table, with `type` and `node` filters |
| `GET /api/topology` | graph vertices and edges, one row per wire |
| `GET /api/lookup?ip=` | which node owns a mesh, service or alias address |
| `GET /api/config` | what the frontend must ask at runtime instead of baking in |
| `GET /api/ws` | push: one frame per change, carrying only rows written since the last |
| `POST /api/scan/{enable,run,incremental,stop}` | operator control over sweeps |

## How a sweep runs

1. Read the seed's own status page: its local ties become the first frontier.
2. Read each node's neighbour tables; every neighbour becomes a node and a link.
3. Probe the frontier with ICMP. No answer parks the host for `scan.revisit_after`
   instead of being reported as down.
4. Fetch each node's pages for hardware, firmware, coordinates and published services.
5. Park failures with a reason, and revisit parked hosts once they are due.

Names that only ever appear in another node's host table are registered so the mesh reads
complete, but they are not scanned: their page has never been fetched, so they are shown as
unscanned rather than as an unknown radio or an offline host.
