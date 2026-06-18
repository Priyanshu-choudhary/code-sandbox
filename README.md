Here is a completely overhauled, high-energy, battle-tested `README.md` that takes this project from a "boring academic sandbox" to an elite, high-performance, remote-judging beast. Copy-paste this straight into your repository to turn it into an absolute showstopper.

---

```markdown
<div align="center">

# ⚡ GOBOXD ⚡

**An ultra-secure, hyper-decoupled, dual-engine code execution judge built for massive scale.**

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8.svg?logo=go&logoColor=white)](https://go.dev)
[![FastAPI](https://img.shields.io/badge/FastAPI-Serverless-009688.svg?logo=fastapi&logoColor=white)](https://fastapi.tiangolo.com)
[![AWS SQS](https://img.shields.io/badge/AWS_SQS-Distributed-FF9900.svg?logo=amazon-aws&logoColor=white)](https://aws.amazon.com/sqs/)
[![Redis](https://img.shields.io/badge/Redis-Cache-DC382D.svg?logo=redis&logoColor=white)](https://redis.io)
[![Docker](https://img.shields.io/badge/Docker-Nsjail_Jail-2496ED.svg?logo=docker&logoColor=white)](https://www.docker.com)

<p align="center">
  <a href="#-the-architecture-the-wow-factor">Architecture</a> •
  <a href="#-key-capabilities">Features</a> •
  <a href="#-dual-engine-execution-flows">Execution Flows</a> •
  <a href="#-quick-start">Quick Start</a> •
  <a href="#-api-payload-glance">API Contracts</a>
</p>

</div>

---

## 🤯 The Architecture (The "WOW" Factor)

Most code executors are fragile scripts that crash under a single loop or lag out completely when hundreds of users submit code. **`goboxd` handles this differently.** We designed a **hybrid dual-engine architecture** that completely separates the user-facing web requests from raw execution computing resources.

1. **The Ingress Gateway (Python FastAPI Serverless via Vercel):** An ultra-lean, highly scalable ingress layer. It accepts incoming payloads, normalizes execution line-breaks, registers persistent sequence maps in Redis, dumps the job into AWS SQS, and instantly terminates the request loop in milliseconds. No idling. No server locks.
2. **The Execution Core (Stateless Go Worker via Docker + Nsjail):** A cluster of isolated Go daemons pulling work from SQS asynchronously. It provisions custom file spaces inside volatile `/tmp/goboxd/job-<uuid>` paths, restricts low-level process tokens to an unprivileged system user (`uid 99999`), handles multiple test loops concurrently, and stores a unified hash layout back to Redis before deleting all file traces.

---

## 🚀 Key Capabilities

* **⚡ Zero-Overhead Dual Pipelines:** Run lightweight code playgrounds instantly using **Synchronous REST (`type: "run"`)** or scale multi-case verification challenges using **Asynchronous Distributed Queues (`type: "submit"`)**.
* **🔒 Military-Grade OS Isolation:** Runs unprivileged execution wrappers via **Nsjail, Linux Namespaces (Mount/PID/Net/IPC), and strict cgroups controllers**. Untrusted scripts can consume all the memory they want; they can never crash the host cluster.
* **🔌 Instant Plug-and-Play Languages:** Want to add Kotlin, Rust, or Zig tomorrow? Don't rewrite Go code. Simply drop a structured `<lang>.yaml` rule file into your registry configs, install the compiler toolchain inside the system Dockerfile, and restart. Done.
* **📈 Bulletproof Fail-Fast Throttling:** Protects synchronous processing via specialized in-memory semaphores. If thread bounds are tripped under burst conditions, the engine sheds load gracefully via `HTTP 429` rather than introducing tail-latency decay.
* **🛡️ Failures as Data:** Application runtime crashes, infinite loops, and stack segmentation faults are treated as expected telemetry data (`HTTP 200 DONE`), never escalating to server infrastructure errors (`5xx`).

---

## 🔄 Dual-Engine Execution Flows

### 1. Asynchronous Distributed Engine (`type: "submit"`)
Optimized for production algorithmic judges (e.g., LeetCode-style platforms).

```text
 [ Client App ]
        │
        ▼ (POST /submissions to Vercel Ingress)
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
        │ (Poll Job)      │ (HSET Result Status: DONE)
        ▼                 │
┌─────────────────────────┴───────┐
│   Go Containerized Executor     │
│  - Pull SQS message block       │
│  - Sequential Nsjail Isolation  │
│  - Inject Input/Output Context  │
└─────────────────────────────────┘

```

### 2. Synchronous REST Playground Engine (`type: "run"`)

Optimized for live text evaluations and quick execution playgrounds.

```text
client ──► chi router ──► api.run ──► executor.Run ──► sandbox.Run ──► nsjail child
                                                                            │
                                                                            ▼
                                                                     compiler/runtime

```

---

## 🛠️ Project Structure

```text
.
├── api/index.py      # Vercel FastAPI Ingress Gateway
├── cmd/goboxd/       # Synchronous/Asynchronous Go Worker Entrypoint
├── configs/          # Declarative Language Registry Definitions (*.yaml)
├── internal/         # Core application logic (sandbox execution, parsing)
│   ├── api/          # Go HTTP router implementation
│   ├── executor/     # Concurrency validation and job scheduling logic
│   └── sandbox/      # Low-level Nsjail argument bindings & pipes
├── vercel.json       # Serverless edge orchestration configuration
└── requirements.txt  # Python serverless dependency graph

```

---

## 🏁 Quick Start

### 1. Local Development Sandbox (Go Engine only)

Prerequisites: Docker & Docker Compose v2.

```bash
# Clone the repository
git clone [https://github.com/Priyanshu-choudhary/code-sandbox.git](https://github.com/Priyanshu-choudhary/code-sandbox.git)
cd code-sandbox

# Spin up the container cluster
docker compose up --build

```

Your local core engine router is now waiting for synchronous connection bursts at: `http://localhost:8080`

### 2. Launching to Production (FastAPI Serverless via Vercel)

Ensure you set your secure environment variables inside the Vercel project management panel (**NEVER commit your `.env` directly to Git!**):

* `SQS_QUEUE_URL` — Your AWS Simple Queue Service target route endpoint.
* `AWS_REGION` — Active zone for infrastructure credentials (e.g., `ap-south-1`).
* `REDIS_URL` — Secure production caching endpoint cluster link.

```bash
# Deploy instantly using Vercel CLI
vercel --prod

```

---

## 📝 API Payload Glance

### `POST /submissions` (Asynchronous Gateway)

Submit untrusted batch verification runs effortlessly:

```json
{
  "sourceCode": "public class Main { public static void main(String[] args) { System.out.println(\"[[0, 0, 0]]\"); } }",
  "language": "java",
  "type": "submit",
  "testCases": [
    { "input": "0 0 0", "expectedOutput": "[[0, 0, 0]]\n" }
  ]
}

```

#### Response (`HTTP 202 Accepted`)

```json
{
  "submissionId": "2e18e27f-36f8-4bf2-a867-60fbb85124ea",
  "status": "QUEUED"
}

```

### `GET /submissions/{id}` (Poll Verification Verdict)

When processing completes, the unified pipeline serves complete diagnostics natively from Redis:

```json
{
  "jobId": "2e18e27f-36f8-4bf2-a867-60fbb85124ea",
  "status": "DONE",
  "updatedAt": "1781775412387",
  "result": {
    "build": { "status": "ok", "duration_ms": 1053 },
    "status": "accepted",
    "tests": [
      {
        "duration_ms": 88,
        "exit_code": 0,
        "memory_kb": 38624,
        "status": "accepted",
        "stdin": "0 0 0\n",
        "expectedOutput": "[[0, 0, 0]]\n",
        "stdout": "[[0, 0, 0]]\n"
      }
    ]
  }
}

```

---

## ⚖️ License

Distributed under the **GNU General Public License v3.0**. Read [LICENSE](https://www.google.com/search?q=LICENSE) for the full text.

```

```