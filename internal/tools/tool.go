// Package tools defines the Tool abstraction that every Riggs capability
// implements, together with a small in-memory registry shared by the
// frontends.
package tools

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
)

// Tool is the protocol-agnostic contract that every capability exposed by
// Riggs must satisfy. Frontends (CLI, MCP) consume tools through this
// interface and never reach into a tool's concrete type.
//
// Args passed to Invoke are keyed by the property names declared in the tool's
// InputSchema. A nil InputSchema means the tool takes no parameters; frontends
// must invoke such tools with a nil or empty map.
type Tool interface {
	Name() string
	Description() string
	InputSchema() *jsonschema.Schema
	Invoke(ctx context.Context, args map[string]any) (any, error)
}

// VerbTool is the optional extension a tool implements to be selectable by a
// verb flag on the CLI — `riggs git pr --approve <ref>` resolving to the tool
// registered as "git.pr.approve".
//
// PrimaryArg names the schema property the verb flag's value binds to ("pr"
// for the example above). Returning "" means the verb is a bare selector that
// takes no value.
//
// This exists only for the CLI frontend. Over MCP the tool is addressed by its
// registered name and the property is passed like any other argument, so the
// two frontends stay on exactly the same schema.
type VerbTool interface {
	Tool
	PrimaryArg() string
}

// Registry holds the set of tools available to the application. It is
// constructed by the composition root and handed to each frontend.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t to the registry. Registering two tools with the same name is
// treated as a programming error and panics; callers control the input set.
func (r *Registry) Register(t Tool) {
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		panic("tools: duplicate tool registration: " + name)
	}
	r.tools[name] = t
	r.order = append(r.order, name)
}

// Get returns the tool registered under name, if any.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns the registered tools in registration order.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}
