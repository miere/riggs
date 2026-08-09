// Package ping provides the smallest possible tool: proof that the registry,
// both frontends and the binary itself are wired correctly. It takes no
// parameters and needs no credentials, so it answers on a machine with nothing
// provisioned.
package ping

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
)

// Tool is the no-op health check.
type Tool struct{}

// New constructs the ping tool.
func New() *Tool { return &Tool{} }

// Name is the registry key.
func (t *Tool) Name() string { return "ping" }

// Description is the one-line hint shown to MCP clients.
func (t *Tool) Description() string { return "Health check; always replies \"pong\"." }

// InputSchema is nil: ping takes no parameters.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Invoke returns the fixed reply.
func (t *Tool) Invoke(context.Context, map[string]any) (any, error) { return "pong", nil }
