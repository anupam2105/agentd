// Package sandbox runs tool calls in locked-down Docker containers.
package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiVersion = "v1.41"

// Docker is a minimal Docker Engine API client (unix or tcp). Talking to the
// API directly keeps the distroless image free of a docker CLI.
type Docker struct {
	host   string // for URL building: "http://docker" or "http://host:port"
	client *http.Client
}

// NewDocker connects to dockerHost ("unix:///var/run/docker.sock" or "tcp://host:2375").
func NewDocker(dockerHost string) (*Docker, error) {
	if dockerHost == "" {
		dockerHost = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(dockerHost)
	if err != nil {
		return nil, fmt.Errorf("parse docker host: %w", err)
	}
	tr := &http.Transport{MaxIdleConns: 16, IdleConnTimeout: 90 * time.Second}
	d := &Docker{client: &http.Client{Transport: tr}}
	switch u.Scheme {
	case "unix":
		sock := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", sock)
		}
		d.host = "http://docker"
	case "tcp", "http":
		d.host = "http://" + u.Host
	default:
		return nil, fmt.Errorf("unsupported docker host scheme %q (want unix:// or tcp://)", u.Scheme)
	}
	return d, nil
}

func (d *Docker) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var rdr io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := d.host + "/" + apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return d.client.Do(req)
}

func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return &APIError{Status: resp.StatusCode, Msg: e.Message}
	}
	return &APIError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
}

// APIError is a non-2xx Docker API response.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker API %d: %s", e.Status, e.Msg) }

func isNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (d *Docker) expect(ctx context.Context, method, path string, query url.Values, body, out any, ok ...int) error {
	resp, err := d.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	for _, code := range ok {
		if resp.StatusCode == code {
			if out != nil {
				return json.NewDecoder(resp.Body).Decode(out)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil
		}
	}
	return apiError(resp)
}

// Ping checks the daemon is reachable.
func (d *Docker) Ping(ctx context.Context) error {
	return d.expect(ctx, http.MethodGet, "/_ping", nil, nil, nil, http.StatusOK)
}

func (d *Docker) ensureImage(ctx context.Context, image string) error {
	err := d.expect(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, nil, http.StatusOK)
	if err == nil || !isNotFound(err) {
		return err
	}
	name, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	resp, err := d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	// The pull streams JSON progress; failures arrive in-band.
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.Error != "" {
			return fmt.Errorf("pull %s: %s", image, msg.Error)
		}
	}
	return sc.Err()
}

// Run executes spec in a fresh container and always removes it afterwards.
func (d *Docker) Run(ctx context.Context, spec *Spec) (*Result, error) {
	if err := d.ensureImage(ctx, spec.Image); err != nil {
		return nil, fmt.Errorf("image: %w", err)
	}
	networkMode := "none"
	if spec.Network {
		networkMode = "bridge"
	}
	create := map[string]any{
		"Image":           spec.Image,
		"Cmd":             spec.Command,
		"Env":             spec.Env,
		"User":            spec.User,
		"WorkingDir":      "/tmp",
		"Labels":          spec.Labels,
		"NetworkDisabled": !spec.Network,
		"HostConfig": map[string]any{
			"NetworkMode":    networkMode,
			"ReadonlyRootfs": true,
			"Tmpfs":          map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"},
			"Memory":         spec.MemoryBytes,
			"MemorySwap":     spec.MemoryBytes,
			"NanoCpus":       int64(spec.CPUs * 1e9),
			"PidsLimit":      spec.PidsLimit,
			"CapDrop":        []string{"ALL"},
			"SecurityOpt":    []string{"no-new-privileges"},
			"Init":           true,
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.expect(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, create, &created, http.StatusCreated); err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	// Remove with a fresh context so cleanup survives a cancelled caller.
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = d.expect(rmCtx, http.MethodDelete, "/containers/"+created.ID, url.Values{"force": {"1"}}, nil, nil, http.StatusNoContent, http.StatusNotFound)
	}()

	start := time.Now()
	if err := d.expect(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil, nil, http.StatusNoContent, http.StatusNotModified); err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}

	res := &Result{ExitCode: -1}
	waitCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	var waited struct {
		StatusCode int64 `json:"StatusCode"`
	}
	err := d.expect(waitCtx, http.MethodPost, "/containers/"+created.ID+"/wait", nil, nil, &waited, http.StatusOK)
	switch {
	case err == nil:
		res.ExitCode = int(waited.StatusCode)
	case errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
		killCtx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = d.expect(killCtx, http.MethodPost, "/containers/"+created.ID+"/kill", nil, nil, nil, http.StatusNoContent, http.StatusConflict)
		kcancel()
	default:
		return nil, fmt.Errorf("wait container: %w", err)
	}
	res.DurationMs = time.Since(start).Milliseconds()

	logCtx, lcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer lcancel()
	resp, err := d.do(logCtx, http.MethodGet, "/containers/"+created.ID+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}}, nil)
	if err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("logs: %w", apiError(resp))
	}
	stdout, stderr, truncated, err := demux(resp.Body, spec.MaxOutputBytes)
	if err != nil {
		return nil, fmt.Errorf("read logs: %w", err)
	}
	res.Stdout, res.Stderr, res.Truncated = stdout, stderr, truncated
	return res, nil
}

// demux splits Docker's multiplexed log stream (8-byte frame headers),
// keeping at most limit bytes per stream.
func demux(r io.Reader, limit int) (stdout, stderr string, truncated bool, err error) {
	var out, errb bytes.Buffer
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return "", "", false, err
		}
		size := int64(binary.BigEndian.Uint32(hdr[4:]))
		dst := &out
		if hdr[0] == 2 {
			dst = &errb
		}
		room := int64(limit - dst.Len())
		if room < 0 {
			room = 0
		}
		take := min(size, room)
		if _, err := io.CopyN(dst, r, take); err != nil {
			return "", "", false, err
		}
		if size > take {
			truncated = true
			if _, err := io.CopyN(io.Discard, r, size-take); err != nil {
				return "", "", false, err
			}
		}
	}
	return out.String(), errb.String(), truncated, nil
}
