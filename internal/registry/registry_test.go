package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func writeYAML(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoadDirValidLanguage(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "py.yaml", `
name: py
source_file: main.py
run:
  command: /usr/bin/python3
  args: ["{{source}}"]
  time_limit_s: 5
  memory_kb: 1024
`)
	r := New()
	if err := r.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	lang, ok := r.Get("py")
	if !ok {
		t.Fatal("missing py")
	}
	if lang.Run.Command != "/usr/bin/python3" {
		t.Fatalf("run.command = %q", lang.Run.Command)
	}
}

func TestLoadDirRejectsBadConfig(t *testing.T) {
	cases := map[string]string{
		"no name": `
source_file: main.py
run: {command: /usr/bin/python3}
`,
		"no source": `
name: x
run: {command: /usr/bin/python3}
`,
		"no run": `
name: x
source_file: main.py
`,
		"empty run cmd": `
name: x
source_file: main.py
run: {command: ""}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeYAML(t, dir, "bad.yaml", body)
			if err := New().LoadDir(dir); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLoadDirSkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, dir, "README.md", "not yaml")
	writeYAML(t, dir, "ok.yaml", `
name: ok
source_file: f
run: {command: /bin/true}
`)
	r := New()
	if err := r.LoadDir(dir); err != nil {
		t.Fatalf("err: %v", err)
	}
	if names := r.Names(); len(names) != 1 || names[0] != "ok" {
		t.Fatalf("got %v want [ok]", names)
	}
}
