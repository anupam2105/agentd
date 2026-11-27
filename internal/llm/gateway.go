package llm

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Gateway routes requests to configured providers.
type Gateway struct {
	providers map[string]Provider
	Pricing   Pricing
}

// NewGateway builds a gateway; the mock provider is always available.
func NewGateway(pricing Pricing, providers ...Provider) *Gateway {
	g := &Gateway{providers: map[string]Provider{}, Pricing: Pricing{"mock*": {}}}
	for k, v := range pricing {
		g.Pricing[k] = v
	}
	g.providers["mock"] = Mock{}
	for _, p := range providers {
		g.providers[p.Name()] = p
	}
	return g
}

// Providers lists configured provider names.
func (g *Gateway) Providers() []string {
	out := make([]string, 0, len(g.providers))
	for k := range g.providers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve picks the provider for a request: explicit provider, else by model
// name ("mock*" → mock, "claude*" → anthropic, otherwise openai).
func (g *Gateway) Resolve(req *Request) (Provider, error) {
	name := req.Provider
	if name == "" {
		switch {
		case strings.HasPrefix(req.Model, "mock"):
			name = "mock"
		case strings.HasPrefix(req.Model, "claude"):
			name = "anthropic"
		default:
			name = "openai"
		}
	}
	p, ok := g.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %q is not configured (configured: %s)", name, strings.Join(g.Providers(), ", "))
	}
	return p, nil
}

// Complete sends the request to its provider.
func (g *Gateway) Complete(ctx context.Context, req *Request) (*Response, error) {
	p, err := g.Resolve(req)
	if err != nil {
		return nil, &Error{Msg: err.Error()}
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	resp.Provider = p.Name()
	if resp.Model == "" {
		resp.Model = req.Model
	}
	return resp, nil
}
