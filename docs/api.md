
---

# goboxd API Specification

All direct endpoints return JSON. Bodies are encoded as UTF-8. Timestamps are represented as epoch milliseconds.

---

## 1. Asynchronous Ingress Engine (FastAPI Deployment)

This is the public-facing gateway hosted on Vercel that decouples client connections from execution compute resources.

### `POST /submissions`

Accepts a single code file, multiple test constraints, and non-blocking scheduling requests.

#### Request Headers

```http
Content-Type: application/json

```

#### Request Body

```json
{
  "sourceCode": "import java.util.*;\npublic class Main {\n public static void main(String[] args) {\n System.out.println(\"[[0, 0, 0]]\");\n }\n}",
  "language": "java",
  "type": "submit",
  "testCases": [
    {
      "input": "0 0 0",
      "expectedOutput": "[[0, 0, 0]]\n"
    },
    {
      "input": "1 2 3 4",
      "expectedOutput": "[]\n"
    }
  ],
  "timeLimitMs": 2000, 
  "memoryLimitMb": 256  
}

```

#### Payload Rules & Properties

* **`type`**: String (`"run"` or `"submit"`). Optional, defaults to `"run"`. Set to `"submit"` to evaluate all distinct test cases.
* **`timeLimitMs`**: Integer. System defaults to `2000` ($2\text{ seconds}$). Converted inside the engine to floating-point seconds (`timeLimitSeconds`).
* **`memoryLimitMb`**: Integer. System defaults to `256` ($\text{MB}$). Converted inside the engine to Kilobytes (`memoryLimitKb`).

#### Response Body (`HTTP 202 Accepted`)

```json
{
  "submissionId": "85b8a8b8-7481-4b55-acad-e60e3f62efcb",
  "status": "QUEUED"
}

```

---

### `GET /submissions/{submission_id}`

Polls the distributed execution state. It automatically merges raw execution metrics with historical tracking data stored during the ingress window.

#### Response Body (`HTTP 200 OK` - Processing In-Flight)

```json
{
  "submissionId": "85b8a8b8-7481-4b55-acad-e60e3f62efcb",
  "status": "QUEUED",
  "stdout": null,
  "stderr": null,
  "runtime": null,
  "memory": null
}

```

#### Response Body (`HTTP 200 OK` - Completed Execution Run)

```json
{
  "jobId": "85b8a8b8-7481-4b55-acad-e60e3f62efcb",
  "status": "DONE",
  "updatedAt": "1781774677468",
  "result": {
    "build": {
      "duration_ms": 957,
      "status": "ok",
      "stderr": "",
      "stdout": ""
    },
    "status": "accepted",
    "tests": [
      {
        "duration_ms": 101,
        "exit_code": 0,
        "memory_kb": 38500,
        "signal": "",
        "status": "accepted",
        "stderr": "",
        "stdin": "-1 0 1 2 -1 -4\n",
        "expectedOutput": "[[-1, -1, 2], [-1, 0, 1]]\n",
        "stdout": "[[-1, -1, 2], [-1, 0, 1]]\n"
      },
      {
        "duration_ms": 90,
        "exit_code": 0,
        "memory_kb": 38384,
        "signal": "",
        "status": "accepted",
        "stderr": "",
        "stdin": "0 0 0\n",
        "expectedOutput": "[[0, 0, 0]]\n",
        "stdout": "[[0, 0, 0]]\n"
      }
    ]
  }
}

```

---

## 2. Distributed Queue Message Protocol (AWS SQS)

When a client submits code via the asynchronous endpoint, the FastAPI layer processes conversions and publishes an internal payload format to AWS SQS.

### Message Attributes & Body Schema

The Go Worker consumes messages containing this JSON-serialized schema:

```json
{
  "jobId": "85b8a8b8-7481-4b55-acad-e60e3f62efcb",
  "type": "submit",
  "sourceCode": "public class Main {\npublic static void main(String[] args) {\nSystem.out.println(\"[[0, 0, 0]]\");\n}\n}",
  "language": "java",
  "stdin": "0 0 0\n",
  "testCases": {
    "0 0 0\n": "[[0, 0, 0]]\n",
    "1 2 3 4\n": "[]\n"
  },
  "timeLimitSeconds": 2.0,
  "memoryLimitKb": 262144,
  "userId": "system-user",
  "queuedAt": 1781774675
}

```

#### Mapping Architecture Rules

* **`testCases` Struct Conversion:** The incoming sequential list array from Python is transformed into a deterministic `map[string]string` format inside Go. The unique key is the processed execution inputs (`stdin`), and the mapping value holds the comparison ground truth (`expectedOutput`).
* **`stdin` Parameter Compatibility:** Emits the input from index `[0]` to provide backward compatibility for legacy Go execution paths.

---

## 3. Data Cache Schema Structure (Redis Cache)

To maintain clean decoupling, the stateless Go worker writes directly to Redis, and the Python API reads directly from it.

### Hash Layout: `job:{submission_id}`

This cache item is generated dynamically by the Go worker upon completing sandbox testing.

| Field Name | Storage Data Type | Technical Context Details |
| --- | --- | --- |
| `jobId` | String | Unique tracking UUID. |
| `status` | String | Global process flag (`QUEUED`, `PROCESSING`, `DONE`). |
| `updatedAt` | String (Timestamp) | Epoch millisecond timestamp of completing run tracking. |
| `result` | Stringified JSON Object | The serialized compilation and execution outcome matrix. |

---

## 4. Synchronous High-Speed Playground API (Go Core)

Direct worker playground pipeline bypassing SQS buffers.

### `POST /run`

Runs the source code synchronously across defined array cases.

#### Request Body

```json
{
  "language": "cpp",
  "source": "int main(){}",
  "build_flags": ["-O2", "-std=c++17"],
  "run_flags": [],
  "test_cases": [
    {
      "input": "1 2\n",
      "expected_output": "3\n",
      "time_limit_s": 1,
      "memory_kb": 65536
    }
  ]
}

```

#### Response Body

```json
{
  "status": "accepted",
  "build": {
    "status": "ok",
    "stdout": "",
    "stderr": "",
    "duration_ms": 1230
  },
  "tests": [
    {
      "status": "accepted",
      "stdout": "3\n",
      "stderr": "",
      "exit_code": 0,
      "signal": "",
      "duration_ms": 12,
      "memory_kb": 1024
    }
  ]
}

```

---

## 5. System Verdict & Status Vocabulary

### Core Execution Status Keys

These tokens apply to both synchronous test items and asynchronous response properties:

| Status Key String Value | Structural Context Meaning |
| --- | --- |
| `accepted` | Program exited with code `0` and `stdout` matches the `expectedOutput` exactly. |
| `wrong_output` | Output string differs from verification criteria targets. |
| `time_exceeded` | Process breached designated wall time limits (triggers an underlying host `SIGKILL`). |
| `memory_exceeded` | Process tripped sandbox resource allocation caps before timing out. |
| `runtime_error` | Code exited with non-zero codes or encountered anomalous signal crashes (e.g., SigSegv). |
| `build_failed` | Code compilation step failed. Remaining test runs match as `not_executed`. |

---

## 6. System Diagnostics and Metrics

### `GET /healthz`

Liveness check. Always returns `200 OK` with `{"status":"ok"}`.

### `GET /readyz`

Readiness validation probe. Validates host system integrity dependencies (`nsjail` execution bindings and toolchain binary paths on the local file system). Returns `200 OK` on success, or `503 Service Unavailable` on failures.

#### Response Body Example

```json
{
  "nsjail": "ok",
  "languages": {
    "python": "ok",
    "java": "ok",
    "verilog": "missing: /usr/bin/vvp"
  }
}

```