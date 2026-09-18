# Riggs

Riggs runs an AI agent on your machine and connects it to a chat gateway over
[RAX](https://github.com/miere/rax-protocol). It supports Claude Code and ACP agents.

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

An ACP agent uses `kind = "acp"` and may also set `interruptible`, `startup_timeout`,
`cancel_grace_period` and `permission_timeout`. An optional top-level `env_file = ".env"` adds
variables to the agent's environment. Unknown keys are an error, so typos never pass silently.

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

On macOS, `riggs launchd` writes a LaunchAgent that starts Riggs at login and restarts it if it
stops:

```sh
riggs launchd --alias default
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/riggs.default.plist
```

It never replaces an existing plist unless you pass `--update-existing`. Logs go to
`~/Library/Logs/riggs/`. `riggs version --check` tells you whether a newer release exists; it
never downloads anything. The repository is private, so set `GH_TOKEN` (for example
`GH_TOKEN=$(gh auth token)`).

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
