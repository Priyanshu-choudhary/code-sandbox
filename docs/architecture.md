# goboxd architecture

A short tour of how a request becomes a verdict.

## Goals (in priority order)

1. **Safety first.** Untrusted code should never escape the sandbox.
2. **Plug-and-play languages.** Add a language with one YAML file, zero Go changes.
3. **Predictable tail latency.** Bounded queue, fail-fast 429 over a soft queue.
4. **Stateless single binary.** No DB, no Redis, no shared queue - easy to scale horizontally later.

## High-level request flow

```
            HTTP                      go routine                    Linux
   client ─────► chi router ─► api.run ─► executor.Run ─► sandbox.Run ──► nsjail child
     ▲                            │            │              │              │
     │                            │            │              │              ▼
     │                            │            │              │      compiler / interpreter
     │                            │            │              │              │
     │                            │            ▼              ▼              │
     │                            │     job dir   ◄───────  bindmount /sandbox
     │                            │   /tmp/goboxd/job-<uuid>
     │                            │
     └───────────── 200 / 400 / 429 ◄────── rollup status, per-test results
```

### Layers in order

| Layer                       | Source                                       | Responsibility                                                                 |
|-----------------------------|----------------------------------------------|--------------------------------------------------------------------------------|
| HTTP                        | `internal/api/api.go`                        | Body cap, JSON decode, request log, status mapping (400/429/500)               |
| Validation + concurrency    | `internal/executor/executor.go`              | safeName, flag allowlist, override bounds, sem acquire, queue gating           |
| Per-language template       | `internal/registry/registry.go`              | YAML load, `{{source}}` / `{{artifact}}` substitution                          |
| Sandbox process             | `internal/sandbox/sandbox.go`                | nsjail argv build, capped buffers, exit-status classification                  |
| OS isolation                | nsjail + Linux namespaces + cgroups          | mount/pid/ipc/uts/net namespaces; rlimits; uid 99999 in real-uid ns            |

## Why nsjail (and not isolate / containers / firejail)

| Option              | Pro                                          | Con                                                                  |
|---------------------|----------------------------------------------|----------------------------------------------------------------------|
| Spawn a container per request | Strongest isolation                  | ~500 ms cold start; OOM-prone with hundreds of submissions           |
| Plain `setuid + chroot`       | Tiny binary                          | No mount / pid / net isolation; weak against curious code            |
| `isolate` (used by Judge0)    | Battle-tested                        | Tied to Judge0's Ruby model; harder to embed into a Go HTTP service  |
| **nsjail**                    | All the namespace primitives + cgroups in one command; trivial to drive from Go's `exec.Command` | Needs `--privileged` or `CAP_SYS_ADMIN` on the container |

nsjail is invoked once per phase (build, then per test case). Each call is
a fresh process tree in a fresh mount/pid/net namespace - no state carries
between requests.

## Concurrency model

```
  POST /run ──► executor.Run
                  │
                  ├── trySemAcquire (cap = MaxConcurrency + QueueDepth)
                  │       └── fail-fast 429 if no slot available
                  │
                  ├── stats.IncInFlight + observe latency
                  │
                  ├── build phase  ────► sandbox.Run (nsjail)
                  │
                  ├── for each test ───► sandbox.Run (nsjail, stdin=test.Input)
                  │
                  └── rollup + JSON response
```

The semaphore is a buffered channel; the loaded capacity (`MaxConcurrency`)
matches CPU count by default. `QueueDepth` is extra buffered slots to
absorb micro-bursts; anything past that is shed. There is **no async**
work in flight after the response is sent - everything is owned by one
goroutine per request.

### Why fail-fast 429 over an infinite queue

Under burst, an unbounded queue degrades p95/p99 for everyone: a request
that arrives at second 0 might wait 20 s because the queue was already
deep. Fail-fast 429 keeps the served population's p99 inside its SLO and
lets the caller decide whether to retry.

## Plug-and-play language registry

A language is a YAML in `configs/languages/`:

```yaml
name: rust
source_file: main.rs
artifact: main
build:
  command: /usr/bin/rustc
  args: ["-O", "{{source}}", "-o", "{{artifact}}"]
  allowed_flags: ["-O", "--edition=2021"]
  time_limit_s: 30
  memory_kb: 1048576
run:
  command: /sandbox/main
  time_limit_s: 5
  memory_kb: 262144
```

- `registry.go::LoadDir` reads every `*.yaml` on startup; missing fields fail boot loudly.
- `{{source}}` and `{{artifact}}` are substituted at request time into individual argv entries (never concatenated into a single shell string).
- `allowed_flags` is an **explicit allowlist** per phase; an empty list means "no user flags".

To add Kotlin tomorrow: drop `configs/languages/kotlin.yaml`, install `kotlinc` in the Dockerfile, restart. Zero Go changes, zero rebuild of the binary.

## Filesystem and uid model

```
host root (uid 0)
  └─ container root (uid 0, privileged: true)
       └─ goboxd process (uid 0)               ◄── creates /tmp/goboxd/job-<uuid>, chowns to 99999
            └─ nsjail (uid 0)                  ◄── --disable_clone_newuser
                 └─ jail child (uid 99999)     ◄── setuid(99999), CapEff=0
                      └─ /sandbox writable
                         /, /usr, /etc, /lib visible read-only via chroot=/
```

Key choices:

- **uid 99999** is deliberately not 65534 (kernel overflow uid) and not any system uid. With the user namespace **disabled** (`--disable_clone_newuser`), file permissions evaluate against the **real on-disk uid**, so root-owned files like `/etc/shadow` correctly deny our jail user.
- **Job dir is chowned to 99999 after mkdir** so the jail user can write its build artefact and scratch files. The dir is `defer`-removed and a startup sweeper deletes any `job-*` older than 1 h.

## Status state machine

```
                          ┌──────────────┐
                          │  POST /run   │
                          └──────┬───────┘
                                 │
                ┌────────────────┴────────────────┐
                │  validate language, flags...   │
                ├── unknown_language ────► 400    │
                ├── source_too_large  ────► 400   │
                ├── disallowed_flag   ────► 400   │
                ├── override_too_big  ────► 400   │
                ├── overloaded        ────► 429   │
                ▼                                  
        sem acquired
                │
                ▼
        ┌─────────────┐
        │  build?     │── (none) ─────┐
        └──────┬──────┘                │
               │ yes                   │
               ▼                       │
        sandbox.Run(build)             │
               │                       │
        ┌──────┴──────┐                │
        │ build_failed│ tests = [not_executed × N]
        └──────┬──────┘                │
               │ ok                    │
               └───────────────────────┘
                          │
                          ▼
            for each test_case:
              sandbox.Run(run, stdin=tc.Input)
                          │
                          ▼
              classify: accepted / output_whitespace_mismatch /
                        wrong_output / time_exceeded /
                        memory_exceeded / runtime_error
                          │
                          ▼
                rollup(tests, build) → top-level status
```

`internal_error` is reserved for `nsjail exec failed` etc. - the user's program never causes 5xx.

## Bounds enforced

| Resource             | Default               | Where                                   |
|----------------------|-----------------------|-----------------------------------------|
| HTTP body            | `4 * MaxSourceBytes`  | `api.go:MaxBytesReader`                 |
| Source bytes         | 256 KB                | `executor.go`                           |
| Test count           | 100                   | `executor.go`                           |
| Captured stdout/stderr | 1 MB each             | `sandbox.go:cappedBuffer`               |
| Wall time            | language YAML         | nsjail `--time_limit`                   |
| CPU time             | wall + 1 s            | nsjail `--rlimit_cpu`                   |
| Virtual memory       | language YAML         | nsjail `--rlimit_as`                    |
| Max processes        | language YAML         | nsjail `--rlimit_nproc`                 |
| File descriptors     | 64                    | nsjail `--rlimit_nofile`                |
| File size            | 64 MB                 | nsjail `--rlimit_fsize`                 |
| Concurrent jobs      | `NumCPU()`            | executor semaphore                      |
| Queue depth          | `4 * NumCPU()`        | executor semaphore extra slots          |

## Observability

- **`/healthz`** - process alive.
- **`/readyz`** - nsjail binary exists and every registered compiler / interpreter is on disk.
- **`/info`** - build version, registered languages, limits.
- **`/metrics`** - `total / rejected / in_flight / p50 / p95 / p99 / max_ms`, JSON, ring-buffer backed.
- **Structured access log** - one slog JSON line per request: `req_id`, `method`, `path`, `status`, `bytes_out`, `duration_ms`, `remote`.

## Failure modes and the chosen response

| Cause                              | We respond            |
|------------------------------------|-----------------------|
| Malformed JSON                     | 400 `bad_request`     |
| Unknown language                   | 400 `unknown_language`|
| Body > limit                       | 400 `source_too_large`|
| Bad flag                           | 400 `disallowed_flag` |
| Test-case override above default   | 400 `override_too_big`|
| Server saturated                   | 429 `overloaded`      |
| nsjail itself failed               | 500 `internal_error`  |
| Child crashed / timed out / OOM    | **200** with the right per-test status (not a 5xx) |

The last row is the most important contract: **user code crashing is normal, expected, and a 200**.
