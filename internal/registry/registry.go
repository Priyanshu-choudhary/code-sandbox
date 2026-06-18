package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Phase is a single command stage (build or run).
type Phase struct {
	Command      string   `yaml:"command"`
	Args         []string `yaml:"args"`
	AllowedFlags []string `yaml:"allowed_flags"`
	TimeLimitS   int      `yaml:"time_limit_s"`
	MemoryKB     int      `yaml:"memory_kb"`
	MaxProcesses int      `yaml:"max_processes"`
}

// Language describes how to compile and run a single language.
// Loaded from YAML at startup. No Go code changes required to add new languages.
type Language struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	SourceFile string `yaml:"source_file"`
	Artifact   string `yaml:"artifact"`
	Build      *Phase `yaml:"build"`
	Run        *Phase `yaml:"run"`
}

type Registry struct {
	mu        sync.RWMutex
	languages map[string]Language
}

func New() *Registry {
	return &Registry{languages: make(map[string]Language)}
}

// LoadDir reads every *.yaml/*.yml file in dir and registers it.
// Validation errors short-circuit the load.
func (r *Registry) LoadDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read languages dir %q: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			continue
		}
		path := filepath.Join(dir, name)
		if err := r.loadFile(path); err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
	}
	return nil
}

func (r *Registry) loadFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var lang Language
	if err := yaml.Unmarshal(raw, &lang); err != nil {
		return fmt.Errorf("yaml decode: %w", err)
	}
	if err := validate(lang); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.languages[lang.Name] = lang
	return nil
}

func validate(l Language) error {
	if l.Name == "" {
		return fmt.Errorf("missing name")
	}
	if l.SourceFile == "" {
		return fmt.Errorf("missing source_file")
	}
	if l.Run == nil {
		return fmt.Errorf("missing run phase")
	}
	if l.Run.Command == "" {
		return fmt.Errorf("run.command empty")
	}
	if l.Build != nil && l.Build.Command == "" {
		return fmt.Errorf("build.command empty")
	}
	return nil
}

func (r *Registry) Get(name string) (Language, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.languages[name]
	return l, ok
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.languages))
	for k := range r.languages {
		out = append(out, k)
	}
	return out
}
