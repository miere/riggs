package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// EnvFileName is the conventional dotenv file, looked for beside the config
// file when `env-file` names none.
const EnvFileName = ".env"

// DefaultEnvPath is ~/.config/riggs/.env — where Riggs looks for its
// environment when nothing points it elsewhere.
//
// It is derived from DefaultPath rather than spelled out again, so the two
// cannot drift: the rule is "beside the config file", and this is what that
// resolves to in the default case.
func DefaultEnvPath() string {
	return filepath.Join(filepath.Dir(DefaultPath()), EnvFileName)
}

// EnvPath is the dotenv file this config read, or would have read. It is
// reported by `riggs capabilities`, because the symptom of the wrong one is an
// empty token — which surfaces as "profile has no bot-token", a message that
// says nothing about where the token was looked for.
func (c *Config) EnvPath() string { return c.envPath }

// EnvLoaded reports whether that file existed and was loaded.
func (c *Config) EnvLoaded() bool { return c.envLoaded }

// loadEnvFile loads the dotenv file into the process environment, so the
// ${VAR} references in the config resolve from it.
//
// Rules, all of them chosen so a launchd agent and a shell behave the same:
//
//   - An already-set variable wins. That is standard dotenv precedence, and it
//     is what lets an operator override one token for a single run without
//     editing a file.
//   - A missing file is not an error. The variables may come from the real
//     environment, which is exactly the case when Riggs is invoked from
//     Murtaugh's gateway — and refusing to start there would be a regression.
//   - An *explicitly named* file that cannot be read IS an error: asking for a
//     specific file and silently getting none is never what the caller meant,
//     the same rule --config-file follows.
//
// godotenv is the parser Murtaugh already uses, which matters more than the
// dependency costs: the operator points both at one .env and quoting behaves
// identically in each.
func (c *Config) loadEnvFile(configPath string) error {
	path, explicit := c.envFilePath(configPath)
	c.envPath = path
	if path == "" {
		return nil
	}

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) && !explicit {
			return nil
		}
		return fmt.Errorf("env-file %s: %w", path, err)
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("env-file %s: %w", path, err)
	}
	c.envLoaded = true
	return nil
}

// envFilePath resolves which dotenv file to read, and whether the operator
// asked for it by name.
func (c *Config) envFilePath(configPath string) (path string, explicit bool) {
	if c.EnvFile != "" {
		return expandHome(os.ExpandEnv(c.EnvFile)), true
	}
	if configPath == "" || configPath == NoFilePath {
		return "", false
	}
	return filepath.Join(filepath.Dir(configPath), EnvFileName), false
}

// expandHome resolves a leading ~ so a config file can name a path the way an
// operator would write it.
func expandHome(path string) string {
	if path != "~" && !hasHomePrefix(path) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func hasHomePrefix(path string) bool {
	return len(path) >= 2 && path[0] == '~' && (path[1] == '/' || path[1] == filepath.Separator)
}
