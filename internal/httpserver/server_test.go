package httpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anupam2105/agentd/internal/httpserver"
	"github.com/anupam2105/agentd/internal/observability"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := httpserver.New(":0", "test", logger, observability.NewMetrics())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doGet(t *testing.T, url string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func TestEndpoints(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	cases := []struct {
		path, want string
		status     int
	}{
		{"/healthz", `"status":"ok"`, http.StatusOK},
		{"/readyz", `"status":"ready"`, http.StatusOK},
		{"/v1/version", `"version":"test"`, http.StatusOK},
		{"/does-not-exist", "", http.StatusNotFound},
	}
	for _, c := range cases {
		status, body := doGet(t, ts.URL+c.path)
		if status != c.status || !strings.Contains(body, c.want) {
			t.Errorf("GET %s = %d %q, want %d containing %q", c.path, status, body, c.status, c.want)
		}
	}
}

func TestMetricsUseBoundedRouteLabels(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	doGet(t, ts.URL+"/healthz")
	doGet(t, ts.URL+"/random-scanner-path")

	_, body := doGet(t, ts.URL+"/metrics")
	if !strings.Contains(body, `agentd_http_requests_total{method="GET",route="/healthz",status="200"} 1`) {
		t.Errorf("missing /healthz series in:\n%s", body)
	}
	if !strings.Contains(body, `route="unknown"`) || strings.Contains(body, "random-scanner-path") {
		t.Errorf("unmatched paths must be labelled unknown")
	}
}
