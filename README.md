# Riggs

Riggs runs an AI agent on your machine and connects it to a chat gateway over
[RAX](https://github.com/miere/rax-protocol). It supports Claude Code and ACP agents.

## Installing

Releases carry a `riggs` binary for Apple silicon (`aarch64-apple-darwin`), Intel Macs
(`x86_64-apple-darwin`) and Linux (`x86_64-unknown-linux-gnu`). The repository is private, so
download with `gh`:

```sh
target=aarch64-apple-darwin
gh release download --repo miere/riggs --pattern "*-$target.tar.gz*"
shasum -a 256 -c riggs-*-$target.tar.gz.sha256
tar -xzf riggs-*-$target.tar.gz
install -m 0755 riggs-*-$target/riggs ~/.local/bin/
```

`~/.local/bin` is on the PATH of the LaunchAgent that `riggs launchd` writes. To build from source
instead: `cargo install --locked --git ssh://git@github.com/miere/riggs riggs`.

## Running Riggs

Riggs reads one TOML file. By default it lives at `~/.config/riggs/default/riggs.toml`, and
`--config PATH` points somewhere else. Relative paths in it are resolved against its folder.

```toml
[gateway]
urls = ["wss://gateway.example.com"]   # more addresses are fallbacks, tried in order
token_file = "node-token"   # the default; must be mode 0600

[agent]
kind = "claude_code"        # or "acp"
command = "claude"
args = []
workdir = "~/work"          # defaults to the config folder
env = { ANTHROPIC_MODEL = "claude-opus-4-1" }

[agent.sandbox]
mode = "seatbelt"           # or "off" (the default); macOS only
write = ["scratch"]         # writable beyond the workspace, temp dirs and Claude Code's own state
deny_read = []              # unreadable; unset blinds the agent to ~/.ssh, ~/.aws, ~/.config/gcloud,
                            # ~/.config/gh and ~/.netrc

[sessions]
durable = true              # keep sessions across restarts
dir = "sessions"
retain = "30d"

[log]
level = "info"              # trace, debug, info, warn or error
format = "text"             # or "json"
```

Files a person shares in the conversation are fetched from the gateway when the prompt arrives and
saved under `files/` beside the config, one folder per session. The agent gets the local path.
They are deleted with their session, including when an unused session is pruned.

The sandbox confines the agent alone. Signing in and refreshing the Claude Code credential run
outside it on purpose: a boxed `claude` can read its credential but cannot write a refreshed one
back, and because Anthropic rotates refresh tokens, a refresh that cannot be saved destroys the
credential. The node's own token is always denied to the agent, whatever `deny_read` lists.

An ACP agent uses `kind = "acp"` and may also set `interruptible`, `startup_timeout`,
`cancel_grace_period` and `permission_timeout`. An optional top-level `env_file = ".env"` adds
variables to the agent's environment. A value in `agent.env` may read one back with `${NAME}`,
which resolves against Riggs' own environment first and then `env_file`, so a secret stays out of
the TOML; a name nothing sets is an error rather than an empty value. Unknown keys are an error,
so typos never pass silently.

The gateway identifies this node only by its token. Put the token you minted on the gateway in the
token file, and make sure only you can read it:

```sh
chmod 600 ~/.config/riggs/default/node-token
riggs validate
riggs run
```

`riggs validate` checks the config and the token file without connecting. `riggs run` prints a
short banner, then serves the agent until it gets SIGTERM or SIGINT. A second signal stops it at
once.

To rotate a token, write the new one beside the old file and `mv` it into place. If the gateway
rejected the old token, Riggs notices the new file within a few seconds and reconnects. It does
not restart, and it keeps its sessions. Agents run as your user, so they can read the token file
too. Do not copy one token to two machines; the gateway would keep swapping them.

On macOS, `riggs launchd` manages a LaunchAgent that starts Riggs at login and restarts it if it
stops:

```sh
riggs launchd install     # write the LaunchAgent
riggs launchd start       # hand it to launchd
riggs launchd status      # is it loaded, is it running, what did it exit with
riggs launchd restart     # shut Riggs down cleanly, then start it again
riggs launchd stop        # take it off launchd
riggs launchd uninstall   # stop it and delete the LaunchAgent
```

`status` is the one to reach for when launchd is being unhelpful — it digs the pid, the last exit
code and the log paths out of `launchctl print`, and tells "never installed" apart from "installed
but not loaded":

```
riggs.default is running (pid 4812).
  LaunchAgent: /Users/you/Library/LaunchAgents/riggs.default.plist
  last exit:   0
  out log:     /Users/you/Library/Logs/riggs/riggs.default.out.log
  err log:     /Users/you/Library/Logs/riggs/riggs.default.err.log
```

`--alias NAME` picks which Riggs you mean: it names both the job (`riggs.<alias>`) and the
configuration at `~/.config/riggs/<alias>/riggs.toml`. It is global, so it goes with `run` and
`validate` too, and it defaults to `default`.

`install` never replaces an existing plist unless you pass `--update-existing`, and it is the only
one of the six that reads a configuration — the others act on the job `--alias` names, and refuse
`--config` rather than quietly ignoring it.

`stop` and a plain `restart` unload the job, which asks Riggs to shut down cleanly: launchd sends
SIGTERM and waits out `ExitTimeOut` (20 seconds unless you set it in the plist) before killing it.
A turn that is still running when that clock expires gets killed with it, so pass
`riggs launchd restart --force` only when you want the process gone now.

Logs go to `~/Library/Logs/riggs/`. `riggs version --check` tells you whether a newer release exists; it
never downloads anything. The repository is private, so set `GH_TOKEN` (for example
`GH_TOKEN=$(gh auth token)`).

## Releasing

Push a tag such as `v0.1.0`. The release workflow builds every target, stamps the version from the
tag, and publishes the archives with their SHA-256 sums. Both workflows need a `RAX_READ_TOKEN`
secret that can read `miere/rax-rs`, since Cargo fetches the RAX crates from that private
repository.

## Benchmarks

```sh
cargo test -p riggs-e2e --test benchmark -- --ignored --nocapture
```

The benchmark runs the real `riggs` binary against the RAX gateway simulator and times a fixed
set of turns for each agent kind: plain text, held tool calls, a question, a 10 MiB attachment, a
turn whose socket is cut twice, and a restart that resumes a saved session. It repeats each one
twenty times, or `RIGGS_BENCH_RUNS` times, prints a table and writes the numbers to
`target/riggs-bench/`. The agents are the scripted fakes, so what it measures is the time Riggs
and RAX add, never how fast a model answers.

## Crates

- `riggs`: the daemon. It loads the config, reads the node token, dials the gateway and serves
  the agent.
- `riggs-node`: serves RAX sessions onto one agent backend. It handles requests, turns, the tool
  gate, prompts, sign-ins, credential reports and the durable session store.
- `riggs-claude-code`: runs Claude Code as that backend. Each session gets one `claude` process,
  and every tool call waits at a PreToolUse hook until the gateway rules on it.
- `riggs-process`: starts each agent in its own process group and kills it with everything it
  spawned, including processes that left the group.
- `riggs-e2e`: end-to-end tests. They run the node, and the real `riggs` binary, against the RAX
  gateway simulator, with scripted `fake-claude` and `fake-acp` agents.
