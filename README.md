# riggs-mcp

A single Go binary that carries Murtaugh's automations: mirroring GitHub review
requests into Slack, approving and merging PRs from a button, and advertising
`ai-able` Jira tickets. It replaces the Python layer under
`~/.config/murtaugh/automations/`.

Riggs is always the callee. Murtaugh keeps owning the schedule and the Slack
gateway, and invokes Riggs as a CLI (from a job or a workflow rule) or over MCP.
It never opens a Socket Mode connection.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the structural decisions.

## Install

```sh
go build -o ~/.local/bin/riggs ./cmd/riggs
```

## Configure

The quickest route is the installer:

```sh
riggs install
```

It asks where the config should live, collects the credentials without echoing
them, sends a **real** test card for a **real** pull request to your Slack DM —
failing the install if that does not work — and then, if Murtaugh is installed,
registers the scheduled jobs through `murtaugh cfg job set`. Jobs whose tool
this build does not yet expose are skipped and reported rather than installed
to fail on a schedule.

It needs a terminal, and will refuse to run without one rather than echo a
pasted token into your scrollback.

To configure by hand instead, copy the example and edit it:

```sh
mkdir -p ~/.config/riggs
cp config.example.yaml ~/.config/riggs/config.yaml
```

Tokens may be literal values or `${ENV}` references; the file is written mode
0600 either way, and the installer prefers a reference whenever the variable is
already set. The ledger database lives beside the config file under the same base name
(`config.yaml` → `config.db`), so `--config-file` moves both.

Check what's live:

```sh
riggs capabilities
```

It names the exact setting or binary behind anything that is disabled. A
missing credential disables a feature; it never stops the binary.

## Use

```sh
riggs ping                                          # pong
riggs capabilities                                  # what's enabled, what's missing
riggs capabilities --json-output                    # the same, machine-readable
riggs --config-file /path/to/other.yaml ping        # anywhere on the line
riggs mcp                                           # MCP stdio server
```

Commands come in three spellings, depending on the tool:

```sh
riggs ping                                          # flat
riggs jira tickets --query "project = NYX ..."      # namespaced
riggs git pr --approve acme/monolith#20069    # namespaced + verb flag
```

A verb flag names the operation and carries its primary argument as the flag's
own value. The argument is optional where the tool has a sensible default —
`riggs git pr --fetch-reviews` falls back to `admin.github-login`.

Every tool that posts to Slack accepts two more flags:

- `--slack-profile <name>` — which Slack account. Defaults to the profile named
  `default`; if that is not configured, the call fails rather than silently not
  notifying.
- `--slack-channel <id>` — where. Absent, the notification is a DM to
  `admin.slack-user-id`.

## Develop

```sh
go test ./...
go vet ./...
```

Anything that touches an external process or SDK sits behind an injected seam,
so the tests make no live calls and do not depend on what is installed on the
machine running them.
