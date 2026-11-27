package agentcfg

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
)

func TestExampleConfigIsValid(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "agentd.example.yaml")
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.NewRegistry(f.Tools, nil); err != nil {
		t.Fatalf("tools: %v", err)
	}
	if _, err := policy.New(f.Policy); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if _, ok := f.Tools["shell"]; !ok {
		t.Fatal("example config should define the shell tool")
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte("tools:\n  x:\n    imagee: alpine\n")); err == nil {
		t.Fatal("expected unknown field error")
	}
	f, err := Parse(nil)
	if err != nil || f == nil {
		t.Fatalf("empty config should parse: %v", err)
	}
}

func TestLoadEmptyPath(t *testing.T) {
	f, err := Load("")
	if err != nil || len(f.Tools) != 0 {
		t.Fatalf("got %+v %v", f, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
