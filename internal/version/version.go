// Package version exposes the Riggs build version.
package version

import "runtime/debug"

// Version is set at build time via
//
//	-ldflags "-X github.com/miere/riggs-mcp/internal/version.Version=<v>"
//
// When unset (e.g. a plain `go build` / `go install`), String() derives it from
// the module's embedded VCS build info, falling back to "dev".
var Version string

// String returns the best available version string: the ldflags-injected value
// if present, else the VCS revision embedded by the Go toolchain, else "dev".
func String() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			return rev + dirty
		}
	}
	return "dev"
}
