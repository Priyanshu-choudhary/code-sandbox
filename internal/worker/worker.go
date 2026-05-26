// Package worker implements the SQS consumer that drives async code execution.
//
// Flow:
//   Spring Boot publishes ExecutionJob JSON to SQS
//   → Worker.Start() polls the queue (long-poll, 20s wait)
//   → For each message: write RUNNING to Redis, call executor.Run(), write DONE/ERROR to Redis
//   → Delete the SQS message
//
// Redis key schema (must stay in sync with ExecutionJobService.java):
//
//	Key    : job:{jobId}           (Hash, TTL = 3600s)
//	Fields : jobId, status, updatedAt, result (JSON string), error
package worker

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/redis/go-redis/v9"

	"github.com/Priyanshu-choudhary/code-sandbox/internal/executor"
)

const (
	jobTTL            = time.Hour // mirrors JOB_TTL_SECONDS in ExecutionJobService.java
	sqsWaitSeconds    = 20       // long-poll: reduce empty-receive API calls
	sqsVisibilityS    = 90       // hide message while we process it (max exec ~30s + headroom)
	sqsMaxMessages    = 10       // batch size per receive call
	redisKeyPrefix    = "job:"
)

// Worker polls SQS and executes code jobs, writing results back to Redis.
type Worker struct {
	sqsClient *sqs.Client
	queueURL  string
	rdb       *redis.Client
	exec      *executor.Executor
	workers   int // number of concurrent polling goroutines
}

// New creates a Worker.
//   - queueURL  : SQS queue URL (required)
//   - redisAddr : host:port of the Redis/Valkey endpoint (required)
//   - redisTLS  : true when the Redis endpoint requires TLS (ElastiCache Serverless)
//   - workers   : number of parallel SQS polling goroutines
func New(queueURL, redisAddr string, redisTLS bool, exec *executor.Executor, workers int) (*Worker, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}

	rdbOpts := &redis.Options{
		Addr:         redisAddr,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	}
	if redisTLS {
		rdbOpts.TLSConfig = &tls.Config{
			// ElastiCache Serverless (Valkey) has a valid AWS-signed cert.
			InsecureSkipVerify: false, //nolint:gosec
		}
	}
	rdb := redis.NewClient(rdbOpts)

	if workers < 1 {
		workers = 1
	}

	return &Worker{
		sqsClient: sqs.NewFromConfig(cfg),
		queueURL:  queueURL,
		rdb:       rdb,
		exec:      exec,
		workers:   workers,
	}, nil
}

// Start launches worker goroutines and blocks until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	slog.Info("sqs worker starting",
		"queue_url", w.queueURL,
		"workers", w.workers,
	)

	done := make(chan struct{}, w.workers)
	for i := range w.workers {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			w.pollLoop(ctx, id)
		}(i)
	}
	// Wait for all goroutines to finish (ctx cancelled).
	for range w.workers {
		<-done
	}
	slog.Info("sqs worker stopped")
}

// pollLoop is one SQS long-poll loop. Each iteration receives up to
// sqsMaxMessages messages and processes them concurrently.
func (w *Worker) pollLoop(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		out, err := w.sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            aws.String(w.queueURL),
			MaxNumberOfMessages: sqsMaxMessages,
			WaitTimeSeconds:     sqsWaitSeconds,
			VisibilityTimeout:   sqsVisibilityS,
			MessageAttributeNames: []string{"All"},
		})
		if err != nil {
			if ctx.Err() != nil {
				return // shutdown
			}
			slog.Error("sqs receive error", "worker", id, "err", err)
			// Back off before retrying to avoid a tight error loop.
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			continue
		}

		for _, msg := range out.Messages {
			// Process each message in its own goroutine so one slow job
			// doesn't block others in the same batch.
			go func(m types.Message) {
				w.handleMessage(ctx, m)
			}(msg)
		}
	}
}

// handleMessage parses one SQS message, runs the job, and writes the result
// to Redis. It always deletes the message at the end (even on error) to avoid
// reprocessing — the error is surfaced via the Redis "error" field.
func (w *Worker) handleMessage(ctx context.Context, msg types.Message) {
	if msg.Body == nil {
		w.deleteMessage(ctx, msg)
		return
	}

	var job Job
	if err := json.Unmarshal([]byte(*msg.Body), &job); err != nil {
		slog.Error("sqs message parse failed", "err", err, "body", *msg.Body)
		w.deleteMessage(ctx, msg)
		return
	}

	log := slog.With("jobId", job.JobID, "lang", job.Language, "type", job.Type)
	log.Info("job received")

	// ── 1. Mark RUNNING ─────────────────────────────────────────────────────
	if err := w.writeRedis(ctx, job.JobID, "RUNNING", nil, ""); err != nil {
		log.Error("redis write RUNNING failed", "err", err)
		// Continue anyway — the client will see QUEUED a bit longer.
	}

	// ── 2. Build executor.Request ────────────────────────────────────────────
	req, err := buildRequest(job)
	if err != nil {
		log.Error("cannot build executor request", "err", err)
		_ = w.writeRedis(ctx, job.JobID, "ERROR", nil, err.Error())
		w.deleteMessage(ctx, msg)
		return
	}

	// ── 3. Execute ───────────────────────────────────────────────────────────
	// Use a per-job timeout slightly shorter than the SQS visibility window.
	execCtx, cancel := context.WithTimeout(ctx, time.Duration(sqsVisibilityS-10)*time.Second)
	defer cancel()

	resp, execErr := w.exec.Run(execCtx, req)

	// ── 4. Write result to Redis ─────────────────────────────────────────────
	if execErr != nil {
		log.Error("executor error", "err", execErr)
		_ = w.writeRedis(ctx, job.JobID, "ERROR", nil, execErr.Error())
	} else {
		// Determine top-level status to propagate.
		redisStatus := "DONE"
		if resp.Status == executor.StatusTimeExceeded {
			redisStatus = "TIMEOUT"
		}

		resultMap := responseToMap(resp)
		if err := w.writeRedis(ctx, job.JobID, redisStatus, resultMap, ""); err != nil {
			log.Error("redis write DONE failed", "err", err)
		} else {
			log.Info("job done", "status", redisStatus)
		}
	}

	// ── 5. Delete SQS message ────────────────────────────────────────────────
	w.deleteMessage(ctx, msg)
}

// buildRequest converts an ExecutionJob (from CFC backend) into an
// executor.Request (goboxd's native format).
func buildRequest(job Job) (executor.Request, error) {
	if job.SourceCode == "" {
		return executor.Request{}, fmt.Errorf("sourceCode is empty")
	}
	if job.Language == "" {
		return executor.Request{}, fmt.Errorf("language is empty")
	}

	req := executor.Request{
		Language: job.Language,
		Source:   job.SourceCode,
	}

	// Optional limit overrides — convert from Java doubles to ints.
	if job.TimeLimitSeconds != nil {
		req.TimeLimitS = int(math.Round(*job.TimeLimitSeconds))
	}
	if job.MemoryLimitKb != nil {
		req.MemoryKB = *job.MemoryLimitKb
	}

	switch job.Type {
	case "run":
		// Playground: single run with stdin, no expected output comparison.
		req.TestCases = []executor.TestCase{
			{Input: job.Stdin},
		}
	case "submit":
		// Judge: one test case per (input, expectedOutput) pair.
		// Java Map iteration order is undefined, but order doesn't matter for
		// verdict — all cases are evaluated.
		req.TestCases = make([]executor.TestCase, 0, len(job.TestCases))
		for input, expected := range job.TestCases {
			req.TestCases = append(req.TestCases, executor.TestCase{
				Input:          input,
				ExpectedOutput: expected,
			})
		}
	default:
		return executor.Request{}, fmt.Errorf("unknown job type: %q", job.Type)
	}

	return req, nil
}

// responseToMap converts executor.Response into a map[string]any suitable for
// JSON-encoding and storing in Redis. The Java side reads it with:
//
//	objectMapper.readValue(resultJson, new TypeReference<Map<String, Object>>() {})
func responseToMap(resp executor.Response) map[string]any {
	m := map[string]any{
		"status": resp.Status,
	}
	if resp.Build != nil {
		m["build"] = map[string]any{
			"status":      resp.Build.Status,
			"stdout":      resp.Build.Stdout,
			"stderr":      resp.Build.Stderr,
			"duration_ms": resp.Build.DurationMS,
		}
	}
	if len(resp.Tests) > 0 {
		tests := make([]map[string]any, len(resp.Tests))
		for i, t := range resp.Tests {
			tests[i] = map[string]any{
				"status":      t.Status,
				"stdout":      t.Stdout,
				"stderr":      t.Stderr,
				"exit_code":   t.ExitCode,
				"signal":      t.Signal,
				"duration_ms": t.DurationMS,
				"memory_kb":   t.MemoryKB,
			}
		}
		m["tests"] = tests
	}
	return m
}

// writeRedis writes (or updates) the job:{jobId} hash in Redis, matching the
// schema in ExecutionJobService.java exactly.
func (w *Worker) writeRedis(ctx context.Context, jobID, status string, result map[string]any, errMsg string) error {
	key := redisKeyPrefix + jobID
	fields := map[string]any{
		"jobId":     jobID,
		"status":    status,
		"updatedAt": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	if errMsg != "" {
		fields["error"] = errMsg
	}
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fields["result"] = string(b)
	}

	pipe := w.rdb.Pipeline()
	pipe.HSet(ctx, key, fields)
	pipe.Expire(ctx, key, jobTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// deleteMessage removes a processed message from SQS.
func (w *Worker) deleteMessage(ctx context.Context, msg types.Message) {
	if msg.ReceiptHandle == nil {
		return
	}
	if _, err := w.sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(w.queueURL),
		ReceiptHandle: msg.ReceiptHandle,
	}); err != nil {
		slog.Error("sqs delete message failed", "err", err)
	}
}
