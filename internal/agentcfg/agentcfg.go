// Package agentcfg loads the agentd YAML config file (pricing, tools, policy).
package agentcfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/anupam2105/agentd/internal/llm"
	"github.com/anupam2105/agentd/internal/policy"
	"github.com/anupam2105/agentd/internal/sandbox"
)

// File is the parsed config file.
type File struct {
	Pricing llm.Pricing                   `yaml:"pricing"`
	Tools   map[string]sandbox.ToolConfig `yaml:"tools"`
	Policy  policy.Config                 `yaml:"policy"`
}

// Load reads path. An empty path yields an empty config (no tools; default policy).
func Load(path string) (*File, error) {
	f := &File{}
	if path == "" {
		return f, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // path comes from operator config
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(b)
}

// Parse decodes config bytes, rejecting unknown fields so typos surface early.
func Parse(b []byte) (*File, error) {
	f := &File{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return f, nil
}
