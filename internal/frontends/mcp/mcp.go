// Package mcp implements the MCP (stdio) frontend. It exposes every tool in
// the shared registry as an MCP tool, and runs the JSON-RPC server over
// stdin/stdout. Stdout is reserved for protocol messages; no logs or raw text
// are written to it.
//
// Output convention: a tool's result is JSON-marshalled and wrapped in a
// single TextContent block. A plain string result is passed through as-is so
// trivial tools (e.g. ping) don't need a struct.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/riggs-mcp/internal/tools"
	"github.com/miere/riggs-mcp/internal/version"
)

// ServerName is advertised to MCP clients.
const ServerName = "riggs-mcp"

// ServerVersion is advertised to MCP clients. It mirrors `riggs version`.
var ServerVersion = version.String()

// Frontend is the MCP adapter.
type Frontend struct {
	registry *tools.Registry
}

// New constructs an MCP Frontend backed by the given registry.
func New(reg *tools.Registry) *Frontend {
	return &Frontend{registry: reg}
}

// Server builds an *mcpsdk.Server with every registered tool wired in. It is
// exposed so tests can inspect the resulting server without touching stdio.
func (f *Frontend) Server() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, nil)
	for _, t := range f.registry.All() {
		registerTool(s, t)
	}
	return s
}

// Serve runs the MCP server over a stdio transport. It blocks until the
// connected client disconnects or ctx is cancelled.
func (f *Frontend) Serve(ctx context.Context) error {
	return f.Server().Run(ctx, &mcpsdk.StdioTransport{})
}

// registerTool wires a single tools.Tool into the MCP server using the
// low-level Server.AddTool, so we can publish the tool's own InputSchema and
// dispatch dynamic map[string]any arguments.
func registerTool(s *mcpsdk.Server, t tools.Tool) {
	schema := t.InputSchema()
	if schema == nil {
		schema = emptyObjectSchema()
	}
	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return errorResult(err), nil
		}
		result, err := t.Invoke(ctx, args)
		if err != nil {
			return errorResult(err), nil
		}
		text, err := renderJSON(result)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, nil
	}
	s.AddTool(&mcpsdk.Tool{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: schema,
	}, handler)
}

// emptyObjectSchema returns the canonical {"type":"object"} schema the SDK
// requires for tools that take no parameters.
func emptyObjectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object"}
}

// decodeArgs unmarshals raw JSON arguments into a map. An empty payload yields
// a nil map so tools don't need to nil-check.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return m, nil
}

// renderJSON encodes a tool result for transport. Strings are passed through to
// keep trivial tools' output uncluttered; everything else is JSON.
func renderJSON(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// errorResult builds the CallToolResult shape used to surface tool errors, per
// the MCP convention of returning IsError=true with a TextContent payload.
func errorResult(err error) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
	}
}
