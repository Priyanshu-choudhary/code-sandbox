# Security holes closed

All seven vulnerability classes called out in the spec are closed. This file
gives the file/line reference, the threat model, and a way to reproduce the
attempted attack so a reviewer can verify each fix.

The live attack suite at `agent_chat/smoke_security.sh` runs every test below
against a running container and reports pass/fail.

---

## 1. Path traversal

**Threat.** A language YAML — or, in a future feature, a request field —
declares a `source_file` like `../../etc/passwd`. When the executor calls
`filepath.Join(jobDir, source_file)` the path resolves *above* the job dir
and the harness writes user-controlled bytes into a privileged location.

**Where it is closed.**

- [`internal/executor/executor.go:21`](../internal/executor/executor.go) —
  `safeName = regexp.MustCompile("^[a-zA-Z0-9._-]+$")`.
- [`internal/executor/executor.go:63`](../internal/executor/executor.go) —
  `lang.SourceFile` is rejected at request time if it fails `safeName`.
- [`internal/executor/executor.go:66`](../internal/executor/executor.go) —
  `lang.Artifact` is rejected the same way.
- File paths are only ever built with `filepath.Join(jobDir, lang.SourceFile)`
  ([executor.go:93](../internal/executor/executor.go)); no other field flows
  into a path.

**Why this is the minimum.** No request body field — `language`, `source`,
`build_flags`, `run_flags`, `test_cases[].input` — ever becomes a filename or
a path component. Only the YAML-controlled `source_file` and `artifact`
become filenames, and both are validated. The set of accepted characters
excludes `/`, `..`, and any shell metacharacter.

**Reproduce.** Unit-tested at
[`internal/executor/executor_test.go:34`](../internal/executor/executor_test.go)
(`TestSafeNameRegex`). Bad inputs `"../etc/passwd"`, `"main py"`, `"main.py;rm -rf /"`,
`"main.py\n"`, `""`, `"main$"` are all rejected; good ones pass.

---

## 2. Shell command injection

**Threat.** Calls of the form `exec.Command("sh", "-c", "gcc " + source)`
allow user input to break out of the intended argv. A `source` value of
`x.c; rm -rf /` would execute the second command.

**Where it is closed.**

- [`internal/sandbox/sandbox.go:108`](../internal/sandbox/sandbox.go) —
  the only process exec in the codebase is
  `exec.CommandContext(hardCtx, spec.NsjailPath, nsArgs...)`. Each nsjail
  argument is a separate Go string, never concatenated.
- Compiler / interpreter invocations are passed through nsjail's `--` argv,
  also as separate strings, so no shell ever parses them.
- The YAML templates (`{{source}}`, `{{artifact}}`) are substituted into
  individual argv elements
  ([`executor.go:228`](../internal/executor/executor.go)), not into a single
  concatenated string.

**Reproduce.** A `source` field containing shell metacharacters is treated as
literal program text. Demo in `smoke_security.sh::shell_meta_in_source`:
sending `source = "import os; os.system('echo PWNED')"` only executes inside
the sandbox (no network, no host fs, no /tmp escape).

---

## 3. Compiler / runtime flag injection

**Threat.** A user-supplied `build_flags: ["-o", "/etc/passwd"]` redirects
the compiler output, or `run_flags: ["--allow-write"]` (Deno-style) escalates
permissions.

**Where it is closed.**

- [`internal/executor/executor.go:74-83`](../internal/executor/executor.go) —
  on every request, build and run flags are checked against the language's
  per-phase `allowed_flags` allowlist before any sandbox spawn.
- [`internal/executor/executor.go:259`](../internal/executor/executor.go) —
  `filterFlags` rejects anything not in the allowlist with
  `ErrDisallowedFlag`.
- An **empty** allowlist (as for Java's run phase) rejects every user-supplied
  flag — there is no "default-allow" path.
- [`internal/api/api.go:124`](../internal/api/api.go) — the API layer maps
  `ErrDisallowedFlag` to HTTP 400, never to a 5xx.

**Reproduce.** Unit-tested at
[`internal/executor/executor_test.go:11`](../internal/executor/executor_test.go)
(`TestFilterFlags`). Live: see `smoke_phase3.sh::disallowed flag returns 400`
and `smoke_security.sh::flag_redirects_output`.

---

## 4. Unbounded request sizes

**Threat.** A 4 GB `source` field exhausts server memory; a request with
1 000 000 test cases stalls the worker for hours.

**Where it is closed.**

- [`internal/api/api.go:100`](../internal/api/api.go) —
  `http.MaxBytesReader(w, r.Body, s.Cfg.MaxSourceBytes*4)` caps the raw HTTP
  body before JSON decoding even starts.
- [`internal/executor/executor.go:59`](../internal/executor/executor.go) —
  `len(req.Source) > MaxSourceBytes` rejected.
- [`internal/executor/executor.go:62`](../internal/executor/executor.go) —
  `len(req.TestCases) > MaxTestCases` rejected.
- Decoder uses `DisallowUnknownFields` at
  [`internal/api/api.go:106`](../internal/api/api.go) so attempting to bury
  large payloads in unknown fields fails fast.

**Defaults** (overridable via env): source ≤ 256 KB, body ≤ 1 MB,
test cases ≤ 100, captured output ≤ 1 MB.

**Reproduce.** `smoke_security.sh::huge_body` posts a 5 MB body and asserts
HTTP 400.

---

## 5. UID / job directory collisions

**Threat.** Two concurrent jobs share the same working directory and leak
each other's source code, or one job overwrites another's compiled artifact.

**Where it is closed.**

- [`internal/executor/executor.go:86`](../internal/executor/executor.go) —
  `jobID := uuid.NewString()` gives 122 bits of randomness per job.
- [`internal/executor/executor.go:87`](../internal/executor/executor.go) —
  `jobDir := filepath.Join(e.Cfg.JobRoot, "job-"+jobID)` is unique per job.
- `os.MkdirAll(jobDir, 0o755)` runs as the goboxd user; nsjail enters the
  jail as uid `65534` (nobody)
  ([`sandbox.go:80`](../internal/sandbox/sandbox.go)) and can only write to
  the bind-mounted `/sandbox`, which is *this* job's dir.

Inside the sandbox the jail user is always `nobody`. Because every job has
its own mount namespace and its own bind-mounted writable directory, two
"nobody" processes cannot see each other's files.

**Reproduce.** `smoke_security.sh::concurrent_isolation` fires two
overlapping jobs that each write a unique sentinel to a file in `/sandbox`
and assert they don't see each other's sentinel.

---

## 6. Unbounded child output

**Threat.** A program prints to stdout forever, exhausting server memory or
filling the response body until the harness OOMs.

**Where it is closed.**

- [`internal/sandbox/sandbox.go:113-114`](../internal/sandbox/sandbox.go) —
  both stdout and stderr go to `&cappedBuffer{max: spec.MaxOutput}`.
- [`internal/sandbox/sandbox.go:180`](../internal/sandbox/sandbox.go) —
  `cappedBuffer.Write` discards bytes once the cap is reached and sets
  `capped = true`. There is no path that allocates beyond `MaxOutput`.
- The Result surfaces `OutputCapped` so clients (and tests) can detect
  truncation rather than silently believing they got everything.

**Default** `GOBOXD_MAX_OUTPUT_BYTES=1048576` (1 MB) per stream.

**Reproduce.** `smoke_security.sh::output_bomb` runs `while True: print('x')`
under a 1-second time limit and asserts the response stdout is ≤ 1 MB and
the status is `time_exceeded`.

---

## 7. Stale directories

**Threat.** A crash or a panic between `MkdirAll` and a manual cleanup
leaves `/tmp/goboxd/job-*` directories behind, leaking source code and
eventually filling the disk.

**Where it is closed.**

- [`internal/executor/executor.go:91`](../internal/executor/executor.go) —
  `defer os.RemoveAll(jobDir)` runs even on panic / early return.
- [`cmd/goboxd/main.go:28`](../cmd/goboxd/main.go) — at startup
  `sweepStaleJobs(cfg.JobRoot, time.Hour)` removes any `job-*` directory
  older than one hour, so a process that gets SIGKILLed mid-flight cannot
  leak indefinitely.
- [`cmd/goboxd/main.go:67-84`](../cmd/goboxd/main.go) — the sweeper only
  touches names that start with `job-`, never anything else in the root.

**Reproduce.** `smoke_security.sh::cleanup_after_failure` runs a job that
forces a runtime error and checks `/tmp/goboxd/` is empty after the
request returns.

---

## Bonus hole found during the audit (not on the spec list)

While building the live attack suite I noticed that `/etc/shadow` was readable
from inside the sandbox even with `--user 65534 --group 65534`. The reason:

- nsjail clones a new user namespace by default and maps the host's
  invoking uid (root) to the configured inside uid.
- With `--user 65534`, host root's files (uid=0 on disk) appeared as
  owned by uid 65534 inside the namespace - the same uid the jail process
  was running as.
- The kernel's permission check used the *owner* bits of mode 640 and
  granted read access.

Fix at [`internal/sandbox/sandbox.go:70-79`](../internal/sandbox/sandbox.go):

- `--disable_clone_newuser` - run in the real uid namespace, no remapping.
- `--user 99999 --group 99999` - a real uid distinct from any system user
  and from 65534 (overflow).
- Job dirs are chown'd to `99999:99999` in
  [`internal/executor/executor.go:97-103`](../internal/executor/executor.go)
  so the jail can still write its build artefacts and scratch files.

With this in place `/etc/shadow` is correctly denied
(`smoke_security.sh::read_host_secret` proves it live).

---

## Defence-in-depth (beyond the seven)

These aren't on the spec list but are part of the same shield:

- **No network.** nsjail clones a fresh network namespace and brings
  loopback DOWN (`--iface_no_lo` in
  [`sandbox.go:79`](../internal/sandbox/sandbox.go)). The sandbox cannot
  reach the host, the container's siblings, or the internet.
- **No /proc.** `--disable_proc` ([sandbox.go:78](../internal/sandbox/sandbox.go))
  hides host process metadata from the jail.
- **`rlimit_fsize 64MB`**, **`rlimit_nofile 64`** stop fork/file-descriptor
  exhaustion attacks beyond what the OOM/time killer already catches.
- **`-XX:CompressedClassSpaceSize`-friendly memory_kb** in
  `configs/languages/java.yaml` so a malicious payload cannot get classified
  as `internal_error` by starving the JVM at startup.
- **`DisallowUnknownFields`** on the JSON decoder
  ([`api.go:106`](../internal/api/api.go)) rejects payloads with extra fields
  that might exploit a future, larger struct.

---

## Coverage matrix

| # | Hole                       | Fix file:line                                             | Test                                            |
|---|----------------------------|-----------------------------------------------------------|-------------------------------------------------|
| 1 | Path traversal             | `internal/executor/executor.go:21,63,66`                  | `executor_test.go::TestSafeNameRegex`           |
| 2 | Shell injection            | `internal/sandbox/sandbox.go:108`                         | `smoke_security.sh::shell_meta_in_source`       |
| 3 | Flag injection             | `internal/executor/executor.go:74-83,259`                 | `executor_test.go::TestFilterFlags`, smoke      |
| 4 | Unbounded body             | `internal/api/api.go:100`, `executor.go:59,62`            | `smoke_security.sh::huge_body`                  |
| 5 | UID / dir collisions       | `internal/executor/executor.go:86-87`                     | `smoke_security.sh::concurrent_isolation`       |
| 6 | Unbounded output           | `internal/sandbox/sandbox.go:113-114,180`                 | `sandbox_test.go`, `smoke_security.sh::output_bomb` |
| 7 | Stale directories          | `internal/executor/executor.go:91`, `cmd/goboxd/main.go:28,67` | `smoke_security.sh::cleanup_after_failure` |
