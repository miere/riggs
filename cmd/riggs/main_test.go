package main

import (
	"reflect"
	"slices"
	"testing"
)

// --config-file is a frontend flag: it may appear anywhere on the line and is
// stripped before the mode and the tool name are parsed.
func TestExtractConfigFlag(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantPath string
		wantRest []string
	}{
		{"absent", []string{"ping"}, "", []string{"ping"}},
		{"leading", []string{"--config-file", "/tmp/a.yaml", "ping"}, "/tmp/a.yaml", []string{"ping"}},
		{"trailing", []string{"git", "pr", "--approve", "o/r#1", "--config-file", "/tmp/a.yaml"},
			"/tmp/a.yaml", []string{"git", "pr", "--approve", "o/r#1"}},
		{"equals form", []string{"--config-file=/tmp/b.yaml", "mcp"}, "/tmp/b.yaml", []string{"mcp"}},
		{"last wins", []string{"--config-file", "/tmp/a.yaml", "--config-file", "/tmp/b.yaml", "ping"},
			"/tmp/b.yaml", []string{"ping"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, rest, err := extractConfigFlag(tc.args)
			if err != nil {
				t.Fatalf("extractConfigFlag: %v", err)
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("rest = %v, want %v", rest, tc.wantRest)
			}
		})
	}
}

func TestExtractConfigFlagRequiresPath(t *testing.T) {
	if _, _, err := extractConfigFlag([]string{"--config-file"}); err == nil {
		t.Error("--config-file with no value = nil error, want a failure")
	}
	if _, _, err := extractConfigFlag([]string{"--config-file="}); err == nil {
		t.Error("--config-file= with an empty value = nil error, want a failure")
	}
}

// extractConfigFlag must not alias its input's backing array.
func TestExtractConfigFlagDoesNotAliasInput(t *testing.T) {
	args := []string{"--config-file", "/tmp/a.yaml", "ping", "extra"}
	original := slices.Clone(args)
	if _, _, err := extractConfigFlag(args); err != nil {
		t.Fatalf("extractConfigFlag: %v", err)
	}
	if !reflect.DeepEqual(args, original) {
		t.Errorf("input mutated: %v, want %v", args, original)
	}
}

// `version` must work before any configuration is loaded, so a broken config
// can still be diagnosed.
func TestVersionNeedsNoConfig(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if err := run(args); err != nil {
			t.Errorf("run(%v) = %v, want success", args, err)
		}
	}
}
