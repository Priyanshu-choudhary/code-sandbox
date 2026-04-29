package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Priyanshu-choudhary/code-sandbox/internal/config"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/registry"
	"github.com/Priyanshu-choudhary/code-sandbox/internal/sandbox"
)

// safeName guards against path traversal in source_file / artifact YAML values.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

type Executor struct {
	Cfg config.Config
	Reg *registry.Registry
	// sem caps concurrent executions at MaxConcurrency. Capacity is
	// MaxConcurrency + QueueDepth so callers wait briefly when the workers
	// are busy but a sustained burst beyond capacity returns 429 fast.
	sem chan struct{}

	stats *Stats
}

func New(cfg config.Config, reg *registry.Registry) *Executor {
	return &Executor{
		Cfg:   cfg,
		Reg:   reg,
		sem:   make(chan struct{}, cfg.MaxConcurrency+cfg.QueueDepth),
		stats: NewStats(),
	}
}

// Stats returns a snapshot for /metrics handlers.
func (e *Executor) Stats() StatsSnapshot { return e.stats.Snapshot(e.Cfg.MaxConcurrency, e.Cfg.QueueDepth) }

var (
	ErrUnknownLanguage = errors.New("unknown language")
	ErrSourceTooLarge  = errors.New("source too large")
	ErrTooManyTests    = errors.New("too many test cases")
	ErrDisallowedFlag  = errors.New("disallowed flag")
	ErrOverrideTooBig  = errors.New("per-test override exceeds language default")
	ErrOverloaded      = errors.New("server overloaded; retry later")
)

func (e *Executor) Run(ctx context.Context, req Request) (Response, error) {
	// Non-blocking semaphore acquire. If the buffered slot can't be taken
	// instantly we reject with 429 (caller may retry). This keeps tail
	// latency bounded under bursts.
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	default:
		e.stats.IncRejected()
		return Response{Status: StatusInternalError}, ErrOverloaded
	}

	e.stats.IncInFlight()
	defer e.stats.DecInFlight()
	start := time.Now()
	defer func() { e.stats.Observe(time.Since(start)) }()

	lang, ok := e.Reg.Get(req.Language)
	if !ok {
		return Response{Status: StatusInternalError}, ErrUnknownLanguage
	}
	if int64(len(req.Source)) > e.Cfg.MaxSourceBytes {
		return Response{Status: StatusInternalError}, ErrSourceTooLarge
	}
	if len(req.TestCases) > e.Cfg.MaxTestCases {
		return Response{Status: StatusInternalError}, ErrTooManyTests
	}
	if !safeName.MatchString(lang.SourceFile) {
		return Response{Status: StatusInternalError}, fmt.Errorf("unsafe source_file in registry")
	}
	if lang.Artifact != "" && !safeName.MatchString(lang.Artifact) {
		return Response{Status: StatusInternalError}, fmt.Errorf("unsafe artifact in registry")
	}
	if lang.Build != nil {
		if err := filterFlags(req.BuildFlags, lang.Build.AllowedFlags); err != nil {
			return Response{Status: StatusInternalError}, err
		}
	}
	if lang.Run != nil {
		if err := filterFlags(req.RunFlags, lang.Run.AllowedFlags); err != nil {
			return Response{Status: StatusInternalError}, err
		}
	}

	// Bound per-test overrides to language defaults (users can lower limits,
	// never raise them above what the YAML allows).
	if err := boundOverrides(lang, req); err != nil {
		return Response{Status: StatusInternalError}, err
	}

	jobID := uuid.NewString()
	jobDir := filepath.Join(e.Cfg.JobRoot, "job-"+jobID)
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		return Response{Status: StatusInternalError}, fmt.Errorf("mkdir job: %w", err)
	}
	defer os.RemoveAll(jobDir)

	srcPath := filepath.Join(jobDir, lang.SourceFile)
	if err := os.WriteFile(srcPath, []byte(req.Source), 0o644); err != nil {
		return Response{Status: StatusInternalError}, fmt.Errorf("write source: %w", err)
	}

	// Hand ownership of the job dir + source file to the in-jail user so it
	// can write build artefacts, scratch files, etc. The sandbox runs at
	// uid:gid = e.Cfg.JailUID:e.Cfg.JailGID (default 99999:99999) in the
	// real uid namespace; without this chown the bind-mounted /sandbox would
	// be unwritable.
	if err := os.Chown(jobDir, e.Cfg.JailUID, e.Cfg.JailGID); err != nil {
		return Response{Status: StatusInternalError}, fmt.Errorf("chown job dir: %w", err)
	}
	if err := os.Chown(srcPath, e.Cfg.JailUID, e.Cfg.JailGID); err != nil {
		return Response{Status: StatusInternalError}, fmt.Errorf("chown source: %w", err)
	}

	resp := Response{}

	// ----- Build phase (optional) -----
	if lang.Build != nil {
		b, err := e.runBuild(ctx, lang, jobDir, req.BuildFlags)
		resp.Build = &b
		if err != nil {
			resp.Status = StatusInternalError
			return resp, err
		}
		if b.Status != BuildOK {
			// Mark every test case as not_executed so clients see the full list.
			if len(req.TestCases) > 0 {
				resp.Tests = make([]TestResult, len(req.TestCases))
				for i := range resp.Tests {
					resp.Tests[i] = TestResult{Status: StatusNotExecuted}
				}
			}
			resp.Status = StatusBuildFailed
			return resp, nil
		}
	}

	// ----- Run phase -----
	if len(req.TestCases) == 0 {
		tr := e.runOne(ctx, lang, jobDir, req.RunFlags, "", req.TimeLimitS, req.MemoryKB)
		// Without expected output we cannot mark wrong_output / mismatch;
		// downgrade those to accepted on the smoke path.
		if tr.Status == StatusWrongOutput || tr.Status == StatusWhitespaceMismatch {
			tr.Status = StatusAccepted
		}
		resp.Tests = []TestResult{tr}
	} else {
		resp.Tests = make([]TestResult, 0, len(req.TestCases))
		for _, tc := range req.TestCases {
			tr := e.runTest(ctx, lang, jobDir, req.RunFlags, tc)
			resp.Tests = append(resp.Tests, tr)
		}
	}

	resp.Status = rollup(resp)
	return resp, nil
}

func (e *Executor) runBuild(ctx context.Context, lang registry.Language, jobDir string, userFlags []string) (BuildPhase, error) {
	phase := lang.Build
	args := renderArgs(phase.Args, lang, userFlags)
	timeLimit := time.Duration(phase.TimeLimitS) * time.Second
	if timeLimit <= 0 {
		timeLimit = 30 * time.Second
	}

	res, err := sandbox.Run(ctx, sandbox.RunSpec{
		NsjailPath:   e.Cfg.NsjailPath,
		Cwd:          jobDir,
		Command:      phase.Command,
		Args:         args,
		WallTime:     timeLimit,
		MemoryKB:     phase.MemoryKB,
		MaxProcesses: phase.MaxProcesses,
		MaxOutput:    e.Cfg.MaxOutputBytes,
		UID:          e.Cfg.JailUID,
		GID:          e.Cfg.JailGID,
	})
	if err != nil {
		return BuildPhase{Status: BuildInternalError, Stderr: err.Error()}, err
	}

	status := BuildOK
	if res.ExitCode != 0 || res.TimedOut || res.OOMKilled {
		status = BuildFailed
	}
	return BuildPhase{
		Status:     status,
		Stdout:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		DurationMS: res.Duration.Milliseconds(),
	}, nil
}

func (e *Executor) runTest(ctx context.Context, lang registry.Language, jobDir string, userFlags []string, tc TestCase) TestResult {
	tr := e.runOne(ctx, lang, jobDir, userFlags, tc.Input, tc.TimeLimitS, tc.MemoryKB)
	if tr.Status != StatusAccepted {
		return tr
	}
	switch compareOutput(tr.Stdout, tc.ExpectedOutput) {
	case outputExact:
		// keep accepted
	case outputWhitespaceOnly:
		tr.Status = StatusWhitespaceMismatch
	case outputMismatch:
		tr.Status = StatusWrongOutput
	}
	return tr
}

func (e *Executor) runOne(ctx context.Context, lang registry.Language, jobDir string, userFlags []string, stdin string, tlOverride, memOverride int) TestResult {
	phase := lang.Run
	args := renderArgs(phase.Args, lang, userFlags)

	timeLimitS := phase.TimeLimitS
	if tlOverride > 0 {
		timeLimitS = tlOverride
	}
	memKB := phase.MemoryKB
	if memOverride > 0 {
		memKB = memOverride
	}

	res, err := sandbox.Run(ctx, sandbox.RunSpec{
		NsjailPath:   e.Cfg.NsjailPath,
		Cwd:          jobDir,
		Command:      phase.Command,
		Args:         args,
		Stdin:        []byte(stdin),
		WallTime:     time.Duration(timeLimitS) * time.Second,
		MemoryKB:     memKB,
		MaxProcesses: phase.MaxProcesses,
		MaxOutput:    e.Cfg.MaxOutputBytes,
		UID:          e.Cfg.JailUID,
		GID:          e.Cfg.JailGID,
	})
	if err != nil {
		return TestResult{Status: StatusInternalError, Stderr: err.Error()}
	}

	status := StatusAccepted
	switch {
	case res.TimedOut:
		status = StatusTimeExceeded
	case res.OOMKilled:
		status = StatusMemoryExceeded
	case res.ExitCode != 0:
		status = StatusRuntimeError
	}

	return TestResult{
		Status:     status,
		Stdout:     string(res.Stdout),
		Stderr:     string(res.Stderr),
		ExitCode:   res.ExitCode,
		Signal:     res.Signal,
		DurationMS: res.Duration.Milliseconds(),
		MemoryKB:   res.MaxRSSKB,
	}
}

func renderArgs(template []string, lang registry.Language, userFlags []string) []string {
	out := make([]string, 0, len(template)+len(userFlags))
	for _, a := range template {
		a = strings.ReplaceAll(a, "{{source}}", lang.SourceFile)
		a = strings.ReplaceAll(a, "{{artifact}}", lang.Artifact)
		out = append(out, a)
	}
	out = append(out, userFlags...)
	return out
}

func filterFlags(user, allowed []string) error {
	if len(user) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	for _, u := range user {
		if _, ok := set[u]; !ok {
			return fmt.Errorf("%w: %q", ErrDisallowedFlag, u)
		}
	}
	return nil
}

func boundOverrides(lang registry.Language, req Request) error {
	maxT := lang.Run.TimeLimitS
	maxM := lang.Run.MemoryKB
	if req.TimeLimitS > maxT {
		return fmt.Errorf("%w: time_limit_s", ErrOverrideTooBig)
	}
	if req.MemoryKB > maxM {
		return fmt.Errorf("%w: memory_kb", ErrOverrideTooBig)
	}
	for i, tc := range req.TestCases {
		if tc.TimeLimitS > maxT {
			return fmt.Errorf("%w: test_cases[%d].time_limit_s", ErrOverrideTooBig, i)
		}
		if tc.MemoryKB > maxM {
			return fmt.Errorf("%w: test_cases[%d].memory_kb", ErrOverrideTooBig, i)
		}
	}
	return nil
}

// outputCmp classifies the relationship between actual and expected output.
type outputCmp int

const (
	outputExact outputCmp = iota
	outputWhitespaceOnly
	outputMismatch
)

func compareOutput(actual, expected string) outputCmp {
	if actual == expected {
		return outputExact
	}
	if normalizeStrict(actual) == normalizeStrict(expected) {
		return outputExact
	}
	if normalizeLoose(actual) == normalizeLoose(expected) {
		return outputWhitespaceOnly
	}
	return outputMismatch
}

// normalizeStrict only converts CRLF -> LF and trims trailing whitespace.
// Used to be tolerant of trailing newlines from print() calls.
func normalizeStrict(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimRight(s, " \t\n")
}

// normalizeLoose collapses any run of whitespace to a single space and
// trims. Used to detect "would be correct except for spacing" answers.
func normalizeLoose(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func rollup(r Response) string {
	if r.Build != nil && r.Build.Status == BuildFailed {
		return StatusBuildFailed
	}
	for _, t := range r.Tests {
		if t.Status != StatusAccepted {
			return t.Status
		}
	}
	return StatusAccepted
}
