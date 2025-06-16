# Release Orchestrator

A Go release controller (`relctl`) that deploys versioned services to a Nomad
cluster running Docker, and rolls back to the previous signed release
automatically when anything goes wrong. Releases are signed manifests that pin
every image by digest and every schema migration by checksum. Bash preflight
checks block a release before rollout if the cluster, database or release is in
a state where it could fail.

Everything here runs on one Docker host: a 3-node Nomad cluster (each node a
Linux container with its own Docker daemon), MySQL, a registry, and an ops box
that runs `relctl`, the preflight checks and the tests.

```
 build machine                           Docker host (Linux, or Docker Desktop's VM)
 ─────────────────────────────           ───────────────────────────────────────────────────────
 make dist / images / release            relorch-ops (relctl, bash preflight, mysql, nomad CLI)
   go build (reproducible)                 │  deploy / rollback / reconcile
   relctl image build ──push──────────▶ relorch-registry  ◀──pull by digest──┐
   relctl manifest create + sign           │                                  │
                                           ▼                                  │
                                         relorch-nomad-server ──schedules──▶ node1  node2  node3
                                           ▲                                (Nomad client + dockerd each)
                                           │                                  │  catalog-api ×3
                                         relorch-mysql ◀──────── app DB ───────┤  order-worker ×3
                                           app (services) + relctl (state)    │  edge-gateway ×3
```

## Results

Measured on one Linux machine (Ubuntu, 20 CPUs, 61 GB RAM) with
Nomad 2.1.1, Docker 29, MySQL 8.4.13 and Go 1.27.3. The raw output is in
[`results/`](results/); `scripts/results-md.sh` renders it.

| | Result |
|---|---|
| Automatic rollback, release that crashes on start (n=10) | previous release healthy **6.6s** median, **7.1s** max after the bad rollout began |
| Automatic rollback, release that never becomes healthy (n=10) | **26.6s** median, **27.3s** max (20s of that is Nomad's health deadline) |
| Slowest automatic rollback in the recovery scenarios | **34.7s** (R09: two services upgraded, the second never healthy, both rolled back) |
| Preflight checks | **20** Bash checks; **22 of 22** injected faults blocked before rollout, nothing changed |
| Recovery scenarios | **25 of 25** pass |
| Reproducible build | **13** artifacts byte-identical when built on node1, node2, node3 and the host |

What the numbers do and do not show:

- "Rollback" is timed by the controller from the moment the bad version was
  submitted to the moment every service of the previous release is healthy
  again on every node and verified. Once a failure is detected the rollback
  itself takes 5–6s. The rest is detection, which depends on how the failure
  looks: a crash is seen in about a second, a version that never turns healthy
  only at the 20s health deadline (`RELCTL_HEALTHY_DEADLINE`).
- The three nodes are containers on one machine, not separate machines. They
  have their own Docker daemons, image caches and Nomad clients, so node loss,
  drains and partial failures are real at the Nomad level, but they share one
  kernel and one disk.
- The reproducibility check above builds inside the three node containers and
  on the host they run on, so it shows the build does not depend on the build
  environment's paths, users or timing. The CI workflow builds on three
  separate GitHub runners (two Ubuntu releases, two CPU architectures) and
  compares their checksums before signing the manifest.

### Preflight fault suite: 22 of 22 faults caught before rollout

Each fault is injected for real, then `relctl deploy` is run. A fault counts as caught only if the deploy is blocked (exit 3) by the expected check, no Nomad job version, schema row or release record changed, and the same release passes preflight again once the fault is removed. `20` distinct checks were exercised.

| Fault | Injected | Caught by | What the check reported |
|---|---|---|---|
| F01 | Manifest edited after signing (catalog-api digest swapped) | ✅ PF01 | manifest changed after signing: sha256 f5a8fc9c6f6bf9fac0ea47780862a8116f89d7352d6c7f1eb42c52c78729e6cb, signature covers 9fbe3c72995a7b8b3ee53aa38e2d |
| F02 | Manifest signed by a key that is not trusted | ✅ PF01 | signed by key 9316e0b6ee20a22f, which is not in the trust store |
| F03 | Service dependency cycle (catalog-api <-> order-worker) | ✅ PF02 | relctl: dependency cycle: cannot order catalog-api, edge-gateway, order-worker; |
| F04 | Migration file modified after signing | ✅ PF03 | migrations/0004_add_items_stock.up.sql sha256 a61303f52aaa != signed 39e6435a5db6;  |
| F05 | Image digest in manifest was never pushed | ✅ PF04 | not in registry: catalog-api catalog-api@95f02d8e4b57 -> HTTP 404;  |
| F09 | Service asks for more memory than any node has | ✅ PF07 | catalog-api catalog-api: 1 unplaceable (memory=3);  |
| F16 | Breaking migration: running release could not be rolled back to | ✅ PF14 | release migrates to schema 6 but the running catalog-api v1.1.0 reads schema <= 5,order-worker v1.1.0 reads schema <= 5,edge-gateway v1.1.0 reads sche |
| F17 | Service needs a schema this release does not migrate to | ✅ PF15 | schema target 4 outside supported range: catalog-api v1.1.0 needs 5-6 |
| F22 | Release older than the running one (accidental downgrade) | ✅ PF20 | 0.0.1 is older than running 1.1.17; use relctl rollback, or --allow-downgrade |
| F11 | Migration user lost ALTER on the app schema | ✅ PF09 | relctl lacks ALTER on app; migrations would fail part way |
| F12 | Earlier migration failed part way (dirty flag set) | ✅ PF10 | migration 4 add_items_stock failed part way and left the schema dirty; repair it before deploying |
| F13 | Applied migration differs from the one in the release | ✅ PF11 | migration 2 applied as 9b8d3a228af8, release has 01d33840bb09;  |
| F14 | Database already migrated past this release's schema | ✅ PF12 | database is at schema 5 but this release targets 4 |
| F15 | Column added by hand, outside any migration | ✅ PF13 | live schema fingerprint e694937889d4 differs from 629fe2cd5777 recorded after migration 4: someone changed the schema outside a release |
| F18 | Long-running transaction holding locks a migration would wait on | ✅ PF16 | blocking: thread 1303 open 13s, 7 rows locked  |
| F21 | Earlier release interrupted mid-rollout, never reconciled | ✅ PF19 | release 1.1.18 (id 76) stopped at rollout:catalog-api; run relctl reconcile first |
| F08 | A node is ineligible: only 2 of 3 required nodes | ✅ PF06 | 2 of 3 required nodes ready+eligible with Docker [node1,node2]; not usable: [node3=ready/ineligible] |
| F19 | Running release degraded: a catalog-api instance is failing checks | ✅ PF17 | degraded in 3 samples: catalog-api d4d3c60f on node1: failure;  |
| F20 | Database credentials variable for edge-gateway deleted | ✅ PF18 | missing db_dsn in nomad/jobs/edge-gateway: tasks would block waiting for the template |
| F06 | Registry unreachable | ✅ PF04 | curl: (6) Could not resolve host: registry registry http://registry:5000 is not reachable |
| F07 | Nomad server down | ✅ PF05 | Nomad API http://nomad-server:4646 unreachable: curl: (6) Could not resolve host: nomad-server |
| F10 | MySQL down | ✅ PF08 | cannot connect to mysql:3306: ERROR 2005 (HY000): Unknown MySQL server host 'mysql' (-2) |

### Recovery scenarios: 25 of 25 passed

Each scenario breaks something during an upgrade and passes only if the cluster ends on one consistent release: every service healthy, the deploy lock free, no release left in progress, and every allocation on every node running the exact binary its signed manifest names.

Automatic rollbacks: previous release healthy 6.2s (median) / 21.3s (max) after the failure was detected, and 18.4s / 34.7s after the bad rollout began (n=15).

| # | Scenario | Result | Outcome | Recovery | Notes |
|---|---|---|---|---|---|
| R01 | New catalog-api version crashes on start | ✅ | rolled_back | 7.3s | failure: task catalog-api on node3 restarted: Terminated: Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"; rollback 5.7s after  |
| R02 | New version runs but never passes its health check | ✅ | rolled_back | 27.5s | failure: allocation ba4826ae on node3 marked unhealthy: Alloc Unhealthy: Task not running for min_healthy_time of 3s by healthy_deadline of 20s; rollback 6.2s a |
| R03 | New version starts slower than the health deadline | ✅ | rolled_back | 25.8s | failure: allocation 885889af on node3 marked unhealthy: Alloc Unhealthy: Task not running for min_healthy_time of 3s by healthy_deadline of 20s; rollback 5.2s a |
| R04 | New version passes checks, then crashes mid-rollout | ✅ | rolled_back | 14.6s | failure: task catalog-api on node3 restarted: Terminated: Exit Code: 5, Exit Message: "Docker container exited with non-zero exit code: 5"; rollback 5.8s after  |
| R05 | New version exceeds its memory limit and is OOM-killed | ✅ | rolled_back | 6.8s | failure: task catalog-api on node3 restarted: Terminated: Exit Code: 137, Exit Message: "OOM Killed"; rollback 5.2s after detection, 6.8s after the bad rollout  |
| R06 | Binary refuses the live schema although its manifest accepts it | ✅ | rolled_back | 15.1s | failure: task edge-gateway on node3 restarted: Terminated: Exit Code: 1, Exit Message: "Docker container exited with non-zero exit code: 1"; rollback 13.6s afte |
| R07 | New version fails on one node only (partial failure) | ✅ | rolled_back | 13.6s | failure: task catalog-api on node2 restarted: Terminated: Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"; rollback 5.8s after  |
| R08 | Last service in dependency order fails; all three roll back in reverse | ✅ | rolled_back | 22.9s | failure: task order-worker on node3 restarted: Terminated: Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"; rollback 21.3s afte |
| R09 | Middle service fails; services after it are never touched | ✅ | rolled_back | 34.7s | failure: allocation 3945854b on node3 marked unhealthy: Alloc Unhealthy: Task not running for min_healthy_time of 3s by healthy_deadline of 20s; rollback 13.6s  |
| R10 | Image deleted from the registry after preflight, before its service rolls out | ✅ | rolled_back | 18.4s | deleted edge-gateway@ae75fe7e4c30 while catalog-api was rolling out; failure: task edge-gateway on node3 restarted: Driver Failure: Failed to pull `registry:500 |
| R11 | Registry goes down mid-rollout; rollback works from node image caches | ✅ | rolled_back | 19.4s | registry stopped 0.5s into the catalog-api rollout; rolled back: task catalog-api on node2 restarted: Driver Failure: Failed to pull `registry:5000/catalog-api@ |
| R12 | A node dies mid-rollout | ✅ | succeeded |  | node3 killed during the catalog-api rollout; release completed on the two remaining nodes |
| R13 | A node is drained mid-rollout | ✅ | succeeded |  | node2 drained during the catalog-api rollout; deploy succeeded |
| R14 | Nomad server restarts mid-rollout | ✅ | succeeded |  | Nomad server restarted during the catalog-api rollout; deploy succeeded |
| R15 | MySQL restarts mid-rollout | ✅ | succeeded |  | MySQL restarted during the catalog-api rollout; deploy succeeded |
| R16 | Controller killed mid-rollout; reconcile restores the last good release | ✅ | reconciled | 25.9s | controller SIGKILLed during the edge-gateway rollout (catalog-api already upgraded); a new deploy was refused (locked) |
| R17 | Controller killed while rolling back; reconcile finishes the job | ✅ | reconciled | 21.5s | controller SIGKILLed as soon as it started rolling back; back on 1.1.13 21.8s after reconcile started |
| R18 | Two controllers deploy at once; the lock admits one | ✅ | succeeded |  | second deploy refused: another release holds the deploy lock (owner 401dadce0ecc/34678/e89bb660) |
| R19 | Migration fails half way; release aborts before any service changes, then repair | ✅ | aborted |  | aborted: migration: migration 5 create_audit_log failed, schema left dirty: Error 1067 (42000): Invalid default value for 'broken'; no service job changed |
| R20 | A brand-new service fails its first deploy; it is removed, the rest untouched | ✅ | rolled_back | 2.1s | failure: task report-worker on node2 restarted: Terminated: Exit Code: 3, Exit Message: "Docker container exited with non-zero exit code: 3"; rollback 0.6s afte |
| R21 | Operator rolls back a release that deployed fine | ✅ | succeeded | 18.3s | all three services back on 1.1.15 in 18.3s |
| R22 | Rolling back to the release already running changes nothing | ✅ | noop |  | two `rollback --to 1.1.15` calls were no-ops; no job version changed |
| R23 | All three nodes restart at once; dependents become ready only after catalog-api | ✅ | recovered | 16.1s | all services healthy and verified 16s after the restart, without operator action; first catalog-api ready 03:26:31.024; first of 6 dependents ready 03:26:32.639 |
| R24 | catalog-api briefly unavailable during an order-worker rollout; no false rollback | ✅ | succeeded |  | all 3 catalog-api instances failing health checks for 6s |
| R25 | Dependents roll out only after catalog-api is healthy | ✅ | succeeded |  | all 6 dependent instances started after catalog-api was healthy at 03:25:38.281 |

### Rollback benchmark (crash_on_start, n=10)

| | min | p50 | p95 | max |
|---|---|---|---|---|
| failure detected → previous release healthy | 5.1s | 5.6s | 6.1s | 6.1s |
| bad rollout began → previous release healthy | 6.1s | 6.6s | 7.1s | 7.1s |

### Rollback benchmark (unhealthy, n=10)

| | min | p50 | p95 | max |
|---|---|---|---|---|
| failure detected → previous release healthy | 5.1s | 5.7s | 6.2s | 6.2s |
| bad rollout began → previous release healthy | 25.9s | 26.6s | 27.3s | 27.3s |

### Reproducible build: identical on node1, node2, node3, host

Go 1.27.3, linux/amd64, SOURCE_DATE_EPOCH=0. 13 artifacts: relctl, two versions of each service binary, and the OCI image digest of each.

| sha256 | artifact |
|---|---|
| `94578b053ae78965…` | bin/catalog-api-1.0.0 |
| `ebbd95999011d892…` | bin/catalog-api-1.1.0 |
| `5ebe6cf744c184c5…` | bin/edge-gateway-1.0.0 |
| `9cb0e3274a48c451…` | bin/edge-gateway-1.1.0 |
| `d2a912d9c5cdcaa1…` | bin/order-worker-1.0.0 |
| `c7076f57e04afe0d…` | bin/order-worker-1.1.0 |
| `4b78ae15dc0450d2…` | bin/relctl |
| `57119cc771614188…` | image:catalog-api:1.0.0 |
| `3dcdad015df10411…` | image:catalog-api:1.1.0 |
| `d9890175e8302561…` | image:edge-gateway:1.0.0 |
| `9138e1f03abcfdf5…` | image:edge-gateway:1.1.0 |
| `9218b1aa5e0d575e…` | image:order-worker:1.0.0 |
| `eb48c2bef299edda…` | image:order-worker:1.1.0 |

## Quick start

Needs Docker (Desktop is fine), Go 1.27, `jq` and `shellcheck`. The cluster
uses about 2 GB of RAM.

```bash
make up                                             # cluster, images, signing key, secrets
make release VERSION=1.0.0 SVC_VERSION=1.0.0 TARGET=3
make deploy VERSION=1.0.0                           # first release, schema 0 -> 3
make release VERSION=1.1.0 SVC_VERSION=1.1.0 TARGET=4
make deploy VERSION=1.1.0                           # rolling upgrade, schema 3 -> 4
make status
```

A release that crashes on start, to watch an automatic rollback:

```bash
dist/bin/relctl manifest create --clean --version 1.2.0 --service-version 1.1.0 \
  --env catalog-api:FAULT_MODE=crash_on_start --out releases/out/1.2.0
dist/bin/relctl manifest sign --bundle releases/out/1.2.0 --key keys/release.key
make deploy VERSION=1.2.0
```

The test suites (all run against the live cluster from the ops box):

```bash
make test        # unit tests
make faults      # preflight fault suite (about 2 minutes)
make recovery    # recovery scenarios (about 30 minutes); RUN=R07 for one
make bench N=10  # time N automatic rollbacks
bash scripts/repro-nodes.sh   # build on node1, node2, node3 and the host; compare
make down        # remove the cluster and its volumes
```

The Nomad UI is at http://localhost:4646. MySQL and the registry listen on
127.0.0.1:13306 and 127.0.0.1:5050. The passwords in `deploy/` are local
development values.

## How a release runs

1. **Lock.** `relctl deploy` takes a lease on a row in MySQL and renews it
   every 5s. A second controller is refused (exit 6). If the holder dies, the
   lease expires after 15s and `relctl reconcile` can take over.
2. **Preflight.** 20 Bash checks (below) run against the bundle, the
   cluster and the database. Any failure blocks the release before anything
   changes (exit 3).
3. **Verify.** The Go side verifies the ed25519 signature and every artifact
   checksum again, using the bytes it is about to use.
4. **Migrate.** Schema migrations run first, recorded with their checksums.
   A migration is marked dirty before it runs and clean after, so one that
   fails half way (MySQL DDL cannot be rolled back) blocks every later
   release until it is repaired. Migrations are expand-only: the running
   release must accept the new schema (PF14), so rolling services back never
   needs a schema rollback.
5. **Roll out in dependency order.** Services whose spec did not change are
   skipped. Each changed service is registered as a Nomad job pinned to its
   image digest and updated one allocation at a time. The controller watches
   every new allocation and calls the rollout failed on the first crash,
   restart, OOM kill, failed image pull or unhealthy verdict, without waiting
   for Nomad's deadlines.
6. **Verify at runtime.** Every allocation on every node is asked which
   binary it runs (`GET /version` returns the sha256 of its own executable).
   It must match the signed manifest.
7. **Roll back on failure.** Every service this release touched goes back to
   its spec in the previous release, in reverse dependency order, with all bad
   allocations replaced at once. A service that had no previous version is
   stopped. Old images are still cached on the nodes, so this works with the
   registry down.

## Preflight checks

`preflight/checks/*.sh`, run by `preflight/run.sh` (embedded in `relctl`;
`relctl preflight --bundle DIR` runs them alone). A check whose prerequisite
failed is skipped, so one root cause shows up as one failure.

| ID | Check | Fails when |
|---|---|---|
| PF01 | Manifest signature | the ed25519 signature (checked with openssl) does not verify, the manifest changed after signing, or the key is not trusted |
| PF02 | Manifest is well-formed | fields, versions or digests are malformed, migrations are not contiguous, or services have a dependency cycle |
| PF03 | Bundle artifact checksums | a migration file in the bundle differs from the checksum the manifest signs |
| PF04 | Image digests resolvable | the registry is down or an image digest the manifest pins does not exist |
| PF05 | Nomad has a leader | the Nomad API is unreachable or has no leader |
| PF06 | Enough ready Docker nodes | fewer than 3 nodes are ready, eligible and have a healthy Docker driver |
| PF07 | Nomad can place every service | a dry-run plan of any job has unplaceable allocations (CPU, memory, constraints) |
| PF08 | MySQL reachable | the database does not answer |
| PF09 | Migration user privileges | the controller's MySQL user lacks a privilege a migration needs (CREATE, ALTER, INDEX, DROP...) |
| PF10 | No dirty migration | an earlier migration failed part way |
| PF11 | Applied migrations match release | an applied migration's checksum differs from the same migration in this release |
| PF12 | Schema not ahead of release | the database is already past this release's schema, or applied migrations have gaps |
| PF13 | No out-of-band schema change | the live schema's fingerprint differs from the one recorded after the last migration |
| PF14 | Rollback stays possible | the running release cannot read the schema this release migrates to, so a rollback would break it |
| PF15 | Services accept target schema | a new service's supported schema range does not include the release's target |
| PF16 | No long transactions or table locks | a transaction older than 10s or a table lock would block the migration's DDL |
| PF17 | Running release healthy | the running release has missing allocations or failing checks in 3 samples, 1s apart |
| PF18 | Service secrets present | a service's database credentials are missing from its Nomad Variable |
| PF19 | No interrupted release | an earlier release was interrupted and not reconciled |
| PF20 | Release is newer than running | the release is not newer than the running one (unless --allow-downgrade) |

## Layout

| Path | What it is |
|---|---|
| `cmd/relctl` | The CLI: build, sign, verify, preflight, deploy, rollback, reconcile, status |
| `internal/controller` | Release orchestration, rollback, reconcile |
| `internal/nomadx` | Nomad job rendering and the rollout watcher |
| `internal/manifest` | Manifest format, ed25519 signing, bundle checksums, dependency order |
| `internal/ociimage` | Reproducible single-binary OCI images, registry push |
| `internal/migrate` | Checksummed, dirty-flagged schema migrations and schema fingerprint |
| `internal/store` | Release history, deploy lease and schema snapshots in MySQL |
| `internal/demosvc`, `cmd/{catalog-api,order-worker,edge-gateway}` | The services being deployed, with fault modes |
| `preflight/` | The Bash preflight checks |
| `migrations/` | Schema migrations for the app database |
| `deploy/` | Docker Compose cluster: Nomad server, 3 DinD Nomad clients, MySQL, registry, ops box |
| `test/faults/run.sh` | Preflight fault suite |
| `test/integration/` | Recovery scenarios (Go, build tag `integration`) |
| `scripts/` | Reproducible build, cross-node reproducibility check, rollback benchmark |
| `.github/workflows/ci.yml` | CI: tests, 3-runner reproducible build, signed manifest, integration |
| `results/` | Output of the last local runs |
