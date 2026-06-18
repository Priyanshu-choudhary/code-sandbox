
---

# goboxd architecture

A short tour of how a request becomes a verdict within a dual-mode code execution engine.

## Execution Models

`goboxd` operates across two highly decoupled interaction paradigms designed for massive multi-tenant scale:

1. **Synchronous REST Engine (`type: "run"`):** Direct client-to-worker connection over a single HTTP request lifecycle. Primarily optimized for code playgrounds or rapid development iterations where low-latency text evaluation is desired.
2. **Asynchronous SQS/Redis Queue Engine (`type: "submit"`):** A production-grade distributed execution pipeline. State data is managed externally by a Python/FastAPI Serverless tier (Vercel) feeding decoupled Go workers via Amazon SQS message queues and caching execution telemetry inside a Redis state layer.

---

## High-Level Execution Pipelines

### 1. Asynchronous Queue Architecture (Distributed Judge)

```
 [ Client App ]
        │
        ▼ (POST /submissions)
┌─────────────────────────────────┐
│ FastAPI Serverless Tier (Vercel)│
└───────┬─────────────────┬───────┘
        │                 │
        │ (Push Message)  │ (Set original tracking array)
        ▼                 ▼
 ┌─────────────┐   ┌─────────────┐
 │   AWS SQS   │   │ Redis Cache │
 │ Code Queue  │   │  Metadata   │
 └──────┬──────┘   └──────┬──────┘
        │                 ▲
        │ (Poll Job)      │ (HSET Result & Status: DONE)
        ▼                 │
┌─────────────────────────┴───────┐
│    Go Code Executor Worker      │
│  - Pull SQS message             │
│  - Loop through all test cases   │
│  - Execute within Nsjail        │
│  - Compute global verdict       │
└─────────────────────────────────┘

```

### 2. Synchronous Direct Request Flow (HTTP Playground)

```
            HTTP                      go routine                    Linux
   client ─────► chi router ─► api.run ─► executor.Run ─► sandbox.Run ──► nsjail child
     ▲                            │            │            │              │
     │                            │            │            │              ▼
     │                            │            │            │       compiler / interpreter
     │                            │            ▼            ▼              │
     │                            │         job dir   ◄───────      bindmount /sandbox
     │                            │   /tmp/goboxd/job-<uuid>
     │                            │
     └───────────── 200 / 400 / 429 ◄────── rollup status, per-test results

```

---

## Architectural Layers in Order

| Layer | Source Component | Responsibility |
| --- | --- | --- |
| **API Gateway / Ingress** | `api/index.py` (Python) / `internal/api/api.go` (Go) | Validates payload limits, standardizes input newline constraints, handles authentication mitigation, routes traffic. |
| **Message Queue Broker** | AWS SQS (`QueueUrl`) | Buffers peak usage spikes, provides dead-letter durability, guarantees decoupling of compute constraints from REST latency. |
| **State Cache Engine** | Redis DB | High-throughput distributed data store tracking pipeline processing metrics (`status: DONE`, `result` maps, processing times). |
| **Validation + Concurrency** | `internal/executor/executor.go` | Sanitizes code properties, enforces user flags allowlist, claims structural thread concurrency semaphores. |
| **Per-Language Configuration** | `internal/registry/registry.go` | Extends runtime platforms dynamically via declarative YAML template substitution strings (`{{source}}`, `{{artifact}}`). |
| **Sandbox Isolation Control** | `internal/sandbox/sandbox.go` | Spawns, monitors, and evaluates targeted runtime behaviors via deterministic argument arrays. |
| **OS Isolation Jail** | `nsjail` + Linux namespaces + cgroups | Restricts low-level system execution boundary privileges (mounts, process trees, network context, cgroup limits). |

---

## Core Infrastructure Deep Dive

### The Asynchronous Lifecycle Lifecycle

When a script is passed down into the distributed system infrastructure using the asynchronous workflow, execution transitions through distinct stages across Python, AWS, Go, and Redis:

#### A. Ingress and Serialized Dispatching (Python State Preparation)

1. The serverless FastAPI runtime processes an unauthenticated payload containing multiple test elements (`[{input: "...", expectedOutput: "..."}]`).
2. The layer sanitizes line termination formatting rules to avoid evaluation drift across system boundaries (`tc.input.endswith("\n")`).
3. To protect evaluation tracking data, the API generates a unique tracking sequence mapped to a Redis structure (`metadata:<submission_id>`) storing exact item positioning parameters.
4. Concurrently, a flattened request representation maps string-to-string keys (`map[string]string`), formats explicit resource configuration overrides, and outputs an opaque structural package directly to AWS SQS before cutting execution connection costs to the client.

#### B. Go Worker Polling and Queue Consuming

1. Disconnected, distributed Go workers dynamically poll the SQS endpoint looking for work.
2. Upon processing an available message payload, the internal handler strips out execution data and switches routing context fields based on incoming tracking tags:
* **`type: "run"`:** Evaluates against the first test instance, returning a playground execution structure.
* **`type: "submit"`:** Dictates strict verification metrics where **every individual test block** executes in sequential isolation against its own isolated container environment.



#### C. Verification, Stitching, and Result Delivery

1. During execution iteration, raw execution metrics are captured and paired alongside the corresponding request indices (`req.TestCases[i]`).
2. The executor maps the structural data layout natively inside `responseToMap(req, resp)`, embedding exact parameter keys (`stdin`, `expectedOutput`, `stdout`) back into the combined output result block.
3. This completely parsed structural representation is encoded into an optimized JSON hash schema and pushed to Redis using a status key updates pattern (`HSET job:<id> status DONE`).
4. Subsequent user polls via `GET /submissions/{id}` read directly from the Redis data cache, fetching structural outcomes cleanly with sub-millisecond lookups.

---

## Why nsjail (and not isolate / containers / firejail)

| Option | Pro | Con |
| --- | --- | --- |
| **Spawn a container per request** | Strongest filesystem boundary separation. | ~500ms cold starts; OOM-prone with hundreds of parallel concurrent submissions. |
| **Plain `setuid + chroot**` | Lightweight, fast execution profiling. | Lacks isolation for virtual tables (`/proc`, system buses, network limits); weak against multi-tenant exploit vectors. |
| **`isolate` (used by Judge0)** | Battle-tested, specialized algorithmic design matrix. | Strongly coupled to underlying state models; challenging to cleanly integrate inside a modular Go microservice architecture. |
| **`nsjail`** | **Utilizes standard Linux namespaces and cgroup limits natively via a single optimized command vector.** | Requires high host capabilities (`CAP_SYS_ADMIN` / privileged status) within runtime container nodes. |

---

## Concurrency Model

### Direct REST Mode vs Worker SQS Scaling

```
 [ Synchronous Mode ]                         [ Asynchronous Mode ]
  POST /submissions                           SQS Message Ingestion
         │                                               │
         ▼                                               ▼
  executor.Run                                    worker.handleMessage
         │                                               │
  trySemAcquire (MaxConcurrency)                  Scale out Worker Pods
   └── (Fail-fast 429 Shedding)                    └── (Elastic Horizontal Autoscaling)

```

### Why fail-fast 429 over an infinite queue in Sync Mode

Under sudden burst conditions, an unbounded in-memory structure introduces system-wide degradation. Requests waiting long periods cause cascading timeouts across frontend clients. Fail-fast mechanics drop overflow traffic cleanly via `HTTP 429`, maintaining predictable p99 processing bounds for accepted work and shifting retry logic handling to upstream load-balancing infrastructure.

---

## Filesystem and User Security Privileges

```
host root (uid 0)
  └─ container root (uid 0, privileged: true)
       └─ goboxd process (uid 0)             ◄── Creates /tmp/goboxd/job-<uuid>, chowns to 99999
            └─ nsjail (uid 0)                 ◄── Passes flag --disable_clone_newuser
                 └─ jail child (uid 99999)     ◄── Enforces setuid(99999), CapEff=0
                     └─ /sandbox writable
                         /, /usr, /etc, /lib visible read-only via chroot=/

```

### Key Security Protocols

* **Explicit System Identity Matching:** The system drops sandboxed operations down to dedicated unprivileged system users (`uid 99999`). Disabling explicit nested user namespace maps (`--disable_clone_newuser`) forces file permission tests to execute against underlying system tables. This blocks access to protected administrative system assets (like `/etc/shadow`) while preserving standard application execution tracking.
* **Transient Ephemeral Workspace Mounts:** Temporary generation environments are isolated via uniquely named runtime structures (`/tmp/goboxd/job-<uuid>`). Following processing termination, cleanup cycles run natively using scoped program teardowns (`defer os.RemoveAll`), and a fallback background cleanup daemon purges workspace structures exceeding a 1-hour lifecycle window.

---

## Status State Machine

```
                          ┌──────────────┐
                          │   Incoming   │
                          └──────┬───────┘
                                 │
                ┌────────────────┴────────────────┐
                │ Validations (Lang, Flags, Size) │
                ├── unknown_language ────► Error ─┼─► [Sync: 400 JSON]
                ├── source_too_large  ────► Error ─┤   [Async: Saved to Redis]
                ├── disallowed_flag   ────► Error ─┤
                └── overloaded        ────► 429 ──┘
                                 │
                         [ Token Granted ]
                                 │
                                 ▼
                        ┌────────────────┐
                        │ Build Phase    │── (None) ───┐
                        └────────┬───────┘             │
                                 │ Yes                 │
                                 ▼                     │
                        sandbox.Run(Build)             │
                                 │                     │
                        ┌────────┴───────┐             │
                        │  build_failed  │             │
                        └────────┬───────┘             │
                                 │ Pass                │
                                 └─────────────────────┘
                                         │
                                         ▼
                               ┌─────────────────┐
                               │ Test Loop       │
                               │ sandbox.Run(Run)│
                               └────────┬────────┘
                                        │
                                        ▼
                               ┌─────────────────┐
                               │ Output Verdict  │
                               │ Classification  │
                               └────────┬────────┘
                                        │
                                        ▼
                              [ Global Rollup State ]
                     accepted / wrong_output / time_exceeded

```

---

## Enforced Resource Limits

| Resource Layer | System Limit | Constraint Location Mechanism |
| --- | --- | --- |
| **HTTP Request Payload** | `4 * MaxSourceBytes` | `api.go:MaxBytesReader` |
| **Source Payload Workspace** | 256 KB | `executor.go` Validation Blocks |
| **Maximum Batch Iterations** | 100 Test Cases | `executor.go` Limit Checkers |
| **Standard Output Streams** | 1 MB Maximum Limit Each | `sandbox.go:cappedBuffer` Pipes |
| **Execution Wall Clock Time** | Evaluated via Runtime Configuration | Nsjail Explicit Flag `--time_limit` |
| **Host System Processor Time** | Allowed execution bound $+ 1\text{ s}$ | Nsjail Enforced Limit `--rlimit_cpu` |
| **Virtual Memory Sandbox Envelope** | Specified via Configuration Map | Nsjail Enforced Limit `--rlimit_as` |
| **Parallel Execution Thread Tree** | Specified via Configuration Map | Nsjail Enforced Limit `--rlimit_nproc` |

---

## Production Error Resolution Strategies

| Execution Root Trigger Context | Global System Pipeline Response |
| --- | --- |
| **Malformed JSON Structural Request** | `400 bad_request` / Pipeline processing halt. |
| **Unsupported Execution Engine Request** | `400 unknown_language` / Dispatches immediate abort. |
| **Payload Payload Size Bound Breach** | `400 source_too_large` / Rejects processing space. |
| **Prohibited Argument Parameter Interception** | `400 disallowed_flag` / Blocks execution tree. |
| **Sandbox Execution System Failure** | `500 internal_error` / Isolation platform notification. |
| **Application Crash / OOM Exception / Timeout** | **`HTTP 200 DONE`** containing granular per-test failure indicators (`runtime_error`, `time_exceeded`, `memory_exceeded`). |

