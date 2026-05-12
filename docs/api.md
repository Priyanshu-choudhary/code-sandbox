# goboxd HTTP API

All endpoints return JSON. Bodies are encoded as UTF-8.

## `POST /run`

Compiles (if needed) and runs the supplied source against zero or more test
cases inside an isolated nsjail sandbox.

### Request body

```jsonc
{
  "language":     "cpp",                  // required, must be a registered language
  "source":       "int main(){}",         // required, capped by GOBOXD_MAX_SOURCE_BYTES
  "build_flags":  ["-O2", "-std=c++17"],  // optional, each must be in the language's allowed_flags
  "run_flags":    [],                     // optional, each must be in the language's allowed_flags
  "test_cases": [
    {
      "input":           "1 2\n",
      "expected_output": "3\n",
      "time_limit_s":    1,        // optional override, must be <= language default
      "memory_kb":       65536     // optional override, must be <= language default
    }
  ],
  "time_limit_s": 5,                      // optional default for the smoke-run case
  "memory_kb":    262144                  // optional default for the smoke-run case
}
```

If `test_cases` is empty the program is executed once with empty stdin
("smoke" mode) and the result is reported as a single test entry.

### Response body

```jsonc
{
  "status": "accepted",                   // top-level rollup, see below
  "build": {                              // present iff the language has a build phase
    "status":      "ok",                  // ok | failed | internal_error
    "stdout":      "",
    "stderr":      "",
    "duration_ms": 1230
  },
  "tests": [
    {
      "status":      "accepted",
      "stdout":      "3\n",
      "stderr":      "",
      "exit_code":   0,
      "signal":      "",                  // present only when the child was signalled
      "duration_ms": 12,
      "memory_kb":   1024                 // peak RSS in KB; approximate
    }
  ]
}
```

### Status vocabulary

Per-test:

| status                       | meaning                                                   |
|------------------------------|-----------------------------------------------------------|
| `accepted`                   | program exited 0 and stdout matches expected exactly      |
| `output_whitespace_mismatch` | tokens match after whitespace normalisation               |
| `wrong_output`               | stdout differs from expected even after normalisation     |
| `time_exceeded`              | wall-time or CPU-time rlimit was hit (SIGKILL or SIGXCPU) |
| `memory_exceeded`            | child SIGKILLed before wall time, treated as OOM          |
| `runtime_error`              | program exited non-zero or was killed by another signal   |
| `not_executed`               | build failed or earlier failure prevented running         |
| `internal_error`             | the sandbox itself or the harness failed                  |

Build:

| build.status     | meaning                                            |
|------------------|----------------------------------------------------|
| `ok`             | compiler exited 0                                  |
| `failed`         | compiler exited non-zero or hit a resource limit   |
| `internal_error` | nsjail/exec failure (rare, surfaces as 500)        |

Top-level rollup rule:

1. If `build` is present and `build.status != ok` → `build_failed`.
2. Else: walk `tests` in order; the first non-`accepted` status becomes the top-level status.
3. Else → `accepted`.

### Errors

`POST /run` returns HTTP 400 with `{ "error": "...", "message": "..." }`
for client-side problems:

| error              | when                                                     |
|--------------------|----------------------------------------------------------|
| `bad_request`      | malformed JSON, missing language/source                  |
| `unknown_language` | the language is not registered                           |
| `source_too_large` | body exceeds `GOBOXD_MAX_SOURCE_BYTES`                   |
| `too_many_tests`   | test count exceeds `GOBOXD_MAX_TEST_CASES`               |
| `disallowed_flag`  | a flag is not in the language's `allowed_flags` allowlist|
| `internal_error`   | unexpected server-side failure (HTTP 500)                |

User code that crashes or times out is **never** a 5xx — those return 200 with
the appropriate per-test status. HTTP 5xx is reserved for sandbox/harness bugs.

---

## `GET /healthz`

Liveness probe. Always returns `200 {"status":"ok"}` when the process is up.

## `GET /readyz`

Readiness probe. Returns `200` when:

- the configured `nsjail` binary exists and is executable, and
- every registered language's compiler / interpreter is on disk.

For compiled languages the compiler (`build.command`) is probed, since the
run command points at a binary that won't exist until a job runs. Returns
`503` with the same shape on partial failure:

```json
{
  "nsjail": "ok",
  "languages": {
    "python":  "ok",
    "verilog": "missing: /usr/bin/vvp"
  }
}
```

## `GET /info`

Server metadata. Useful for clients enumerating capabilities.

```json
{
  "version":         "0.1.0-mvp",
  "go":              "go1.23.12",
  "nsjail_path":     "/usr/local/bin/nsjail",
  "languages":       ["python", "cpp", "java", "..."],
  "max_concurrency": 8,
  "limits": {
    "max_source_bytes": 262144,
    "max_output_bytes": 1048576,
    "max_test_cases":   100
  }
}
```

---

## Resource limits and overrides

Each language YAML declares the maximum `time_limit_s` and `memory_kb` that
the run phase will use. Clients can pass a smaller `time_limit_s` /
`memory_kb` on the request or per test case, but never one larger - requests
that try to bump the limit return `disallowed_flag` style 400s.

The sandbox additionally enforces:

- `--rlimit_cpu` = `time_limit_s + 1`
- `--rlimit_fsize` = 64 MB
- `--rlimit_nofile` = 64
- `--rlimit_nproc` = language-specific `max_processes`
- captured stdout and stderr each truncated to `GOBOXD_MAX_OUTPUT_BYTES`

---

## Plug-and-play languages

Drop a file `<name>.yaml` into `GOBOXD_LANGUAGES_DIR` (default
`/etc/goboxd/languages`) and restart. No Go code changes.

```yaml
name: rust
version: "rustc 1.75"
source_file: main.rs
artifact: main
build:
  command: /usr/bin/rustc
  args: ["-O", "{{source}}", "-o", "{{artifact}}"]
  allowed_flags: ["-O", "--edition=2021"]
  time_limit_s: 30
  memory_kb: 524288
  max_processes: 64
run:
  command: /sandbox/main
  time_limit_s: 5
  memory_kb: 262144
  max_processes: 64
```

Template tokens are substituted into `args`:

- `{{source}}` → `source_file`
- `{{artifact}}` → `artifact` (empty for interpreted languages)
