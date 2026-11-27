package sandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func frame(stream byte, s string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(s))) //nolint:gosec // test frames are tiny
	return append(h, s...)
}

func TestDemux(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(frame(1, "hello "))
	buf.Write(frame(2, "oops"))
	buf.Write(frame(1, "world, this is long"))
	out, errOut, truncated, err := demux(&buf, 11)
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world" || errOut != "oops" || !truncated {
		t.Fatalf("got %q %q %v", out, errOut, truncated)
	}
}

func newShell(t *testing.T) *Tool {
	t.Helper()
	tool, err := NewTool("shell", &ToolConfig{
		Image: "alpine", Command: []string{"sh", "-c", "{{.command}}"}, Env: map[string]string{"B": "2", "A": "1"},
		InputSchema: map[string]any{"type": "object", "required": []any{"command"},
			"properties": map[string]any{"command": map[string]any{"type": "string"}, "n": map[string]any{"type": "integer"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestToolSpec(t *testing.T) {
	tool := newShell(t)
	in := map[string]any{"command": "echo hi"}
	raw, _ := json.Marshal(in)
	spec, err := tool.Spec("run1", "2:tool:shell", 1, in, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(spec.Command, "|") != "sh|-c|echo hi" || spec.User != "65534:65534" || spec.Network || spec.MemoryBytes != 256<<20 {
		t.Fatalf("spec defaults: %+v", spec)
	}
	if spec.Env[len(spec.Env)-2] != "A=1" || spec.Labels["io.agentd.run"] != "run1" {
		t.Fatalf("env/labels: %v %v", spec.Env, spec.Labels)
	}
	again, _ := tool.Spec("run1", "2:tool:shell", 1, in, raw)
	if again.Name != spec.Name || !strings.HasPrefix(spec.Name, "agentd-") {
		t.Fatalf("container name should be deterministic: %s %s", spec.Name, again.Name)
	}
	if _, err := tool.Spec("run1", "k", 1, map[string]any{}, []byte("{}")); err == nil {
		t.Fatal("missing template key should fail")
	}
}

func TestValidateInput(t *testing.T) {
	tool := newShell(t)
	ok := []map[string]any{{"command": "x"}, {"command": "x", "n": float64(3)}, {"command": "x", "extra": true}}
	for _, in := range ok {
		if err := tool.ValidateInput(in); err != nil {
			t.Errorf("%v: %v", in, err)
		}
	}
	bad := []map[string]any{{}, {"command": 1.0}, {"command": "x", "n": 1.5}}
	for _, in := range bad {
		if err := tool.ValidateInput(in); err == nil {
			t.Errorf("%v: expected error", in)
		}
	}
}

func TestNewToolValidation(t *testing.T) {
	bad := []ToolConfig{
		{Command: []string{"x"}},
		{Image: "a"},
		{Image: "a", Command: []string{"x"}, Timeout: "1h"},
		{Image: "a", Command: []string{"x"}, Memory: "1k"},
		{Image: "a", Command: []string{"{{"}},
	}
	for i, c := range bad {
		if _, err := NewTool("t", &c); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}
