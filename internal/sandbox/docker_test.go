package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDockerIntegration runs real containers. Enable with AGENTD_DOCKER_TESTS=1
// (uses AGENTD_DOCKER_HOST, DOCKER_HOST, or the default socket).
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("AGENTD_DOCKER_TESTS") != "1" {
		t.Skip("set AGENTD_DOCKER_TESTS=1 to run against a real Docker daemon")
	}
	host := os.Getenv("AGENTD_DOCKER_HOST")
	if host == "" {
		host = os.Getenv("DOCKER_HOST")
	}
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	d, err := NewDocker(host)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := d.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	tool, err := NewTool("shell", &ToolConfig{Image: "alpine:3.22", Command: []string{"sh", "-c", "{{.command}}"}, Timeout: "3s"})
	if err != nil {
		t.Fatal(err)
	}
	run := func(cmd string) *Result {
		t.Helper()
		in := map[string]any{"command": cmd}
		spec, err := tool.Spec("itest", cmd, 1, in, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		spec.Name += "-" + time.Now().Format("150405.000000")
		res, err := d.Run(ctx, spec)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return res
	}

	res := run("echo out; echo err >&2; id -u; exit 3")
	if res.ExitCode != 3 || !strings.Contains(res.Stdout, "out") || !strings.Contains(res.Stdout, "65534") || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("basic: %+v", res)
	}
	if res := run("touch /etc/x 2>&1; echo rc=$?"); !strings.Contains(res.Stdout, "rc=1") {
		t.Fatalf("root filesystem must be read-only: %+v", res)
	}
	if res := run("wget -q -T 2 -O- http://example.com >/dev/null 2>&1; echo rc=$?"); strings.Contains(res.Stdout, "rc=0") {
		t.Fatalf("network must be disabled: %+v", res)
	}
	if res := run("echo hi > /tmp/f && cat /tmp/f"); strings.TrimSpace(res.Stdout) != "hi" {
		t.Fatalf("/tmp must be writable: %+v", res)
	}
	if res := run("sleep 30"); !res.TimedOut {
		t.Fatalf("timeout not enforced: %+v", res)
	}
}
