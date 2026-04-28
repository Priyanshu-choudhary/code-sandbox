package executor

// Status vocabulary documented in /docs/api.md.
const (
	StatusAccepted             = "accepted"
	StatusWrongOutput          = "wrong_output"
	StatusWhitespaceMismatch   = "output_whitespace_mismatch"
	StatusTimeExceeded         = "time_exceeded"
	StatusMemoryExceeded       = "memory_exceeded"
	StatusRuntimeError         = "runtime_error"
	StatusBuildFailed          = "build_failed"
	StatusNotExecuted          = "not_executed"
	StatusInternalError        = "internal_error"

	BuildOK            = "ok"
	BuildFailed        = "failed"
	BuildInternalError = "internal_error"
)

// Request is the JSON body of POST /run.
type Request struct {
	Language   string     `json:"language"`
	Source     string     `json:"source"`
	BuildFlags []string   `json:"build_flags,omitempty"`
	RunFlags   []string   `json:"run_flags,omitempty"`
	TestCases  []TestCase `json:"test_cases,omitempty"`
	TimeLimitS int        `json:"time_limit_s,omitempty"`
	MemoryKB   int        `json:"memory_kb,omitempty"`
}

type TestCase struct {
	Input          string `json:"input"`
	ExpectedOutput string `json:"expected_output"`
	TimeLimitS     int    `json:"time_limit_s,omitempty"`
	MemoryKB       int    `json:"memory_kb,omitempty"`
}

// Response is the JSON body of POST /run.
type Response struct {
	Status string       `json:"status"`
	Build  *BuildPhase  `json:"build,omitempty"`
	Tests  []TestResult `json:"tests,omitempty"`
}

type BuildPhase struct {
	Status     string `json:"status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
}

type TestResult struct {
	Status     string `json:"status"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	MemoryKB   int    `json:"memory_kb,omitempty"`
}
