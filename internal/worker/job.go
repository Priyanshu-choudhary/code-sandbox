package worker

// Job is the Go mirror of com.cfc.platform.Pojo.execution.ExecutionJob.
// Both sides must agree on this JSON shape — any change here must be mirrored
// in ExecutionJob.java.
//
// Jackson (Java) serialises with camelCase by default; the json tags below
// must match exactly.
type Job struct {
	JobID            string            `json:"jobId"`
	Type             string            `json:"type"`     // "run" | "submit"
	SourceCode       string            `json:"sourceCode"`
	Language         string            `json:"language"`
	Stdin            string            `json:"stdin,omitempty"`
	TestCases        map[string]string `json:"testCases,omitempty"`  // input → expectedOutput
	TimeLimitSeconds *float64          `json:"timeLimitSeconds,omitempty"`
	MemoryLimitKb    *int              `json:"memoryLimitKb,omitempty"`
	UserID           string            `json:"userId,omitempty"`
	QueuedAt         int64             `json:"queuedAt"`
}
