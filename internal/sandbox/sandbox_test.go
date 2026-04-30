package sandbox

import (
	"strings"
	"testing"
)

func TestCappedBufferUnderLimit(t *testing.T) {
	b := &cappedBuffer{max: 1024}
	_, _ = b.Write([]byte("hello"))
	if string(b.Bytes()) != "hello" {
		t.Fatalf("want hello got %q", string(b.Bytes()))
	}
	if b.capped {
		t.Fatalf("must not be capped")
	}
}

func TestCappedBufferTruncates(t *testing.T) {
	b := &cappedBuffer{max: 5}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defgh"))
	if string(b.Bytes()) != "abcde" {
		t.Fatalf("want abcde got %q", string(b.Bytes()))
	}
	if !b.capped {
		t.Fatalf("expected capped")
	}
}

func TestCappedBufferUnlimited(t *testing.T) {
	b := &cappedBuffer{max: 0}
	huge := strings.Repeat("x", 10_000)
	_, _ = b.Write([]byte(huge))
	if len(b.Bytes()) != len(huge) {
		t.Fatalf("unlimited buffer must keep everything")
	}
}
