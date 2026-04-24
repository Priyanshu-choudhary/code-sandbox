package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// RunSpec describes one sandboxed process invocation.
type RunSpec struct {
	NsjailPath   string        // path to the nsjail binary
	Cwd          string        // job working dir (mounted as /sandbox inside the jail)
	Command      string        // absolute path to the program to run (inside the jail)
	Args         []string      // already-validated args
	Stdin        []byte        // bytes piped to stdin
	WallTime     time.Duration // hard wall-time cap
	MemoryKB     int           // RLIMIT_AS in KB
	MaxProcesses int           // RLIMIT_NPROC
	MaxOutput    int64         // bytes captured from each of stdout/stderr
	UID          int           // real uid the sandbox enters as
	GID          int           // real gid the sandbox enters as
}

// Result holds what came out of the sandbox.
type Result struct {
	Stdout       []byte
	Stderr       []byte
	ExitCode     int           // child exit code (>=128 + signal if signaled)
	Signal       string        // killing signal name, empty when normal exit
	Duration     time.Duration // wall-clock time spent in nsjail
	MaxRSSKB     int           // peak resident set size of the child (kilobytes)
	TimedOut     bool          // wall_time or CPU time was exceeded
	OOMKilled    bool          // child SIGKILLed but wall_time not yet reached
	OutputCapped bool          // stdout or stderr was truncated to MaxOutput
}

// Exit codes we recognise on Linux. exit = 128 + signum when the kernel
// signaled the child.
const (
	exitSIGKILL = 128 + 9  // 137 - OOM / explicit kill / wall-time
	exitSIGABRT = 128 + 6  // 134 - assert/abort
	exitSIGSEGV = 128 + 11 // 139 - segfault
	exitSIGXCPU = 128 + 24 // 152 - rlimit_cpu exceeded
)

// Run invokes nsjail synchronously and returns the captured result.
//
// Security notes:
//   - We never use `sh -c`. Args are already validated at the registry layer
//     and go straight to exec.Command as separate argv entries.
//   - Stdout/stderr are bounded via cappedBuffer to MaxOutput bytes.
//   - A hard context deadline is added on top of nsjail's own --time_limit
//     to ensure the call returns even if nsjail itself gets stuck.
func Run(ctx context.Context, spec RunSpec) (Result, error) {
	if spec.NsjailPath == "" {
		return Result{}, errors.New("sandbox: empty nsjail path")
	}
	if spec.Command == "" {
		return Result{}, errors.New("sandbox: empty command")
	}

	// Build nsjail args.
	//
	// Filesystem strategy:
	//   --chroot / exposes the host root read-only (so /usr, /lib, /bin etc.
	//   are visible to compilers/interpreters) and --bindmount writes the
	//   job dir to /sandbox as the only writable location. Explicit
	//   --bindmount_ro paths are intentionally avoided - they would
	//   attempt to remount already-visible mountpoints on top of themselves
	//   (kernel returns EINVAL).
	//
	// Network strategy:
	//   Default (no flag) clones a new empty network namespace -> no net.
	//   --iface_no_lo additionally brings loopback DOWN.
	nsArgs := []string{
		"--mode", "once",
		"--really_quiet",
		"--log", "/dev/null",
		"--chroot", "/",
		"--cwd", "/sandbox",
		"--bindmount", spec.Cwd + ":/sandbox",
		// We deliberately do NOT use a user namespace. With CLONE_NEWUSER on,
		// nsjail maps host uid 0 to our jail uid, which made host-root-owned
		// files (e.g. /etc/shadow) appear owned by the jail user and grant
		// owner-mode read access. With clone_newuser disabled, --user is a
		// plain setuid() to 99999 in the *real* uid namespace - the kernel
		// then evaluates file permissions against the genuine on-disk uids,
		// so /etc/shadow (root:shadow 640) is correctly denied.
		// Disabling the user namespace requires euid==0 on the parent, which
		// the container provides.
		"--disable_clone_newuser",
		"--user", strconv.Itoa(spec.UID),
		"--group", strconv.Itoa(spec.GID),
		"--hostname", "goboxd",
		"--disable_proc",
		"--iface_no_lo",
		"--rlimit_as", strconv.Itoa(spec.MemoryKB / 1024),
		"--env", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"--env", "TMPDIR=/sandbox",
		"--env", "HOME=/sandbox",
		"--env", "LANG=C.UTF-8",
		"--rlimit_cpu", strconv.Itoa(int(spec.WallTime.Seconds()) + 1),
		"--rlimit_fsize", "65536",
		"--rlimit_nofile", "64",
		"--rlimit_nproc", strconv.Itoa(spec.MaxProcesses),
		"--time_limit", strconv.Itoa(int(spec.WallTime.Seconds())),
	}
	nsArgs = append(nsArgs, "--", spec.Command)
	nsArgs = append(nsArgs, spec.Args...)

	hardCtx, cancel := context.WithTimeout(ctx, spec.WallTime+2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(hardCtx, spec.NsjailPath, nsArgs...)
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}

	outBuf := &cappedBuffer{max: spec.MaxOutput}
	errBuf := &cappedBuffer{max: spec.MaxOutput}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := Result{
		Stdout:       outBuf.Bytes(),
		Stderr:       errBuf.Bytes(),
		Duration:     elapsed,
		OutputCapped: outBuf.capped || errBuf.capped,
	}

	// getrusage Maxrss is in KB on Linux. Approximate - reflects the direct
	// child (nsjail) which forks the user program, but for short-lived
	// programs it tracks closely.
	if cmd.ProcessState != nil {
		if ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && ru != nil {
			res.MaxRSSKB = int(ru.Maxrss)
		}
	}

	if hardCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}

	if err == nil {
		return res, nil
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return res, fmt.Errorf("nsjail exec: %w", err)
	}

	res.ExitCode = exitErr.ExitCode()
	classifyExit(&res, spec, exitErr, elapsed)
	return res, nil
}

// classifyExit maps the WaitStatus to our OOM/TLE/runtime flags.
func classifyExit(res *Result, spec RunSpec, exitErr *exec.ExitError, elapsed time.Duration) {
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			res.Signal = ws.Signal().String()
			res.ExitCode = 128 + int(ws.Signal())
		}
	}

	switch res.ExitCode {
	case exitSIGKILL:
		// SIGKILL came from the kernel or nsjail. If wall-clock time is
		// (close to) the limit, it was the time guard; otherwise treat as OOM.
		if elapsed >= spec.WallTime-100*time.Millisecond {
			res.TimedOut = true
		} else {
			res.OOMKilled = true
		}
	case exitSIGXCPU:
		res.TimedOut = true
	}
}

// cappedBuffer is an io.Writer that drops bytes past `max`.
type cappedBuffer struct {
	max    int64
	buf    bytes.Buffer
	capped bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.max <= 0 {
		return c.buf.Write(p)
	}
	remaining := c.max - int64(c.buf.Len())
	if remaining <= 0 {
		c.capped = true
		return len(p), nil
	}
	if int64(len(p)) > remaining {
		c.capped = true
		c.buf.Write(p[:remaining])
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }

var _ io.Writer = (*cappedBuffer)(nil)
