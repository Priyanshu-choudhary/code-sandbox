package config

import (
	"os"
	"runtime"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr        string
	NsjailPath      string
	LanguagesDir    string
	JobRoot         string
	MaxConcurrency  int
	QueueDepth      int           // additional buffered slots before 429
	RequestTimeout  time.Duration
	MaxSourceBytes  int64
	MaxOutputBytes  int64
	MaxTestCases    int
	// Real-uid namespace identity the sandbox enters as. We deliberately
	// avoid 65534 (kernel overflow uid) so unmapped/foreign-owned files
	// can't accidentally match the jail process's uid.
	JailUID int
	JailGID int

	// Async SQS worker — set SQSQueueURL to enable.
	SQSQueueURL  string // e.g. https://sqs.ap-south-1.amazonaws.com/936344984906/cfc-execution-jobs
	RedisAddr    string // e.g. localhost:6379 or valkey-endpoint:6379
	RedisPassword string
	RedisTLS     bool   // true for ElastiCache Serverless (enforces TLS)
	SQSWorkers   int    // number of parallel SQS polling goroutines (default 2)
}

func Load() Config {
	return Config{
		HTTPAddr:        envOr("GOBOXD_HTTP_ADDR", ":8080"),
		NsjailPath:      envOr("GOBOXD_NSJAIL", "/usr/local/bin/nsjail"),
		LanguagesDir:    envOr("GOBOXD_LANGUAGES_DIR", "/etc/goboxd/languages"),
		JobRoot:         envOr("GOBOXD_JOB_ROOT", "/tmp/goboxd"),
		MaxConcurrency:  envIntOr("GOBOXD_MAX_CONCURRENCY", runtime.NumCPU()),
		QueueDepth:      envIntOr("GOBOXD_QUEUE_DEPTH", runtime.NumCPU()*4),
		RequestTimeout:  time.Duration(envIntOr("GOBOXD_REQUEST_TIMEOUT_S", 60)) * time.Second,
		MaxSourceBytes:  int64(envIntOr("GOBOXD_MAX_SOURCE_BYTES", 256*1024)),
		MaxOutputBytes:  int64(envIntOr("GOBOXD_MAX_OUTPUT_BYTES", 1024*1024)),
		MaxTestCases:    envIntOr("GOBOXD_MAX_TEST_CASES", 100),
		JailUID:         envIntOr("GOBOXD_JAIL_UID", 99999),
		JailGID:         envIntOr("GOBOXD_JAIL_GID", 99999),

		SQSQueueURL: envOr("SQS_QUEUE_URL", ""),
		RedisAddr:   envOr("REDIS_ADDR", "localhost:6379"),
		RedisPassword: os.Getenv("REDIS_PASSWORD"),
		RedisTLS:    envOr("REDIS_TLS", "false") == "true",
		SQSWorkers:  envIntOr("GOBOXD_SQS_WORKERS", 2),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
