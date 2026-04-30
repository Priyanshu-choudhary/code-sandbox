package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/Priyanshu-choudhary/code-sandbox/internal/registry"
)

func TestFilterFlags(t *testing.T) {
	cases := []struct {
		name    string
		user    []string
		allow   []string
		wantErr bool
	}{
		{"empty user", nil, []string{"-O2"}, false},
		{"all allowed", []string{"-O2", "-Wall"}, []string{"-O2", "-Wall"}, false},
		{"reject unknown", []string{"-fbad"}, []string{"-O2"}, true},
		{"empty allow rejects anything", []string{"-x"}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := filterFlags(c.user, c.allow)
			if c.wantErr {
				if !errors.Is(err, ErrDisallowedFlag) {
					t.Fatalf("want ErrDisallowedFlag, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}

func TestSafeNameRegex(t *testing.T) {
	good := []string{"main.py", "Main.java", "main_1.cpp", "a-b.c", "x.y.z"}
	bad := []string{"../etc/passwd", "main.py;rm -rf /", "main py", "main.py\n", "", "main$"}
	for _, g := range good {
		if !safeName.MatchString(g) {
			t.Errorf("expected %q to be safe", g)
		}
	}
	for _, b := range bad {
		if safeName.MatchString(b) {
			t.Errorf("expected %q to be rejected", b)
		}
	}
}

func TestCompareOutput(t *testing.T) {
	cases := []struct {
		actual, expected string
		want             outputCmp
	}{
		{"42\n", "42\n", outputExact},
		{"42", "42\n", outputExact},                          // trailing newline tolerated
		{"42 \n", "42\n", outputExact},                       // trailing whitespace tolerated
		{"4 2", "42", outputMismatch},                        // different tokens
		{"43", "42", outputMismatch},
		{"hello world\n", "hello\tworld", outputWhitespaceOnly}, // tab vs space mid-line
		{"hello  world\n", "hello world\n", outputWhitespaceOnly}, // collapsed spaces
		{"foo\nbar\n", "foo bar", outputWhitespaceOnly},      // newline vs space between tokens
	}
	for _, c := range cases {
		if got := compareOutput(c.actual, c.expected); got != c.want {
			t.Errorf("compareOutput(%q,%q) = %v want %v", c.actual, c.expected, got, c.want)
		}
	}
}

func TestRollup(t *testing.T) {
	cases := []struct {
		name string
		r    Response
		want string
	}{
		{"all good", Response{Tests: []TestResult{{Status: StatusAccepted}, {Status: StatusAccepted}}}, StatusAccepted},
		{"first fails", Response{Tests: []TestResult{{Status: StatusWrongOutput}, {Status: StatusAccepted}}}, StatusWrongOutput},
		{"build fail", Response{Build: &BuildPhase{Status: BuildFailed}}, StatusBuildFailed},
		{"tle wins over wrong", Response{Tests: []TestResult{{Status: StatusTimeExceeded}, {Status: StatusWrongOutput}}}, StatusTimeExceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rollup(c.r); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestBoundOverrides(t *testing.T) {
	lang := registry.Language{
		Run: &registry.Phase{TimeLimitS: 5, MemoryKB: 262144},
	}
	if err := boundOverrides(lang, Request{TimeLimitS: 3, MemoryKB: 100000}); err != nil {
		t.Fatalf("under-limit should pass: %v", err)
	}
	if err := boundOverrides(lang, Request{TimeLimitS: 10}); !errors.Is(err, ErrOverrideTooBig) {
		t.Fatalf("over time should fail")
	}
	if err := boundOverrides(lang, Request{MemoryKB: 999999}); !errors.Is(err, ErrOverrideTooBig) {
		t.Fatalf("over memory should fail")
	}
	if err := boundOverrides(lang, Request{TestCases: []TestCase{{TimeLimitS: 100}}}); !errors.Is(err, ErrOverrideTooBig) {
		t.Fatalf("over per-test time should fail")
	}
}

func TestRenderArgsSubstitutes(t *testing.T) {
	lang := registry.Language{SourceFile: "main.cpp", Artifact: "main"}
	got := renderArgs([]string{"-o", "{{artifact}}", "{{source}}"}, lang, []string{"-O2"})
	want := []string{"-o", "main", "main.cpp", "-O2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v want %v", got, want)
	}
}
