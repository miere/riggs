# riggs-mcp — Architecture

This document is the canonical reference for the structural decisions in
riggs-mcp. Drift between code and this document is treated as a bug. Any change
that adds or modifies an architectural element MUST update this file in the
same PR.

Riggs takes its structure from `mcp-techops`. Where it departs from that
blueprint, the departure is called out and justified.

## 1. What Riggs is for

Riggs replaces the Python layer under Murtaugh's automations with a single Go
binary. Murtaugh keeps owning the schedule and the Slack gateway; it invokes
Riggs either as a CLI (from a job or a workflow rule) or over MCP. Riggs never
opens a Socket Mode connection and never receives a Slack event — it is always
the callee.

The automations being replaced:

| Today | Trigger | Becomes |
| --- | --- | --- |
| `pull_request/main.py review-queue` | job, every 1m | `git.pr.fetch-reviews` |
| `pull_request/main.py approve` | rule `pr-approve` | `git.pr.approve` |
| `pull_request/main.py approve --action-id approve_merge` | rule `pr-approve-merge` | `git.pr.approve-merge` |
| `quick_coding_tasks/main.py poll` | job, every 3m | `jira.tickets` |
| `quick_coding_tasks/main.py nudge` | job, weekdays 09/12/14/17 | `jira.tickets` (nudge latch) |
| `quick_coding_tasks/main.py action` | rules `quick-coding-tasks-*` | `jira.tickets` action verbs |
| `repository_manager/main.py` | — | to be scoped |

## 2. High-level architecture

```
                       ┌──────────────────────┐
                       │ cmd/riggs            │
                       └──────────┬───────────┘
                                  │
                       ┌──────────▼───────────┐
                       │ internal/app         │  ← composition root
                       └──────────┬───────────┘
                                  │ builds Registry, picks Mode
                  ┌───────────────┴───────────────┐
         ┌────────▼─────────┐           ┌─────────▼────────┐
         │ frontends/cli    │           │ frontends/mcp    │
         │ (human stdin/out)│           │ (MCP stdio JSON) │
         └────────┬─────────┘           └─────────┬────────┘
                  └──────────────► Tool ◄─────────┘
                                   (internal/tools)
                                      │
              ┌───────────────────────┼───────────────────────┐
       internal/slack          internal/notify           internal/config
       (where it goes)         (what was already said)    (who we are)
                                      │
                      ┌───────────────┼───────────────┐
                internal/github   internal/jira   internal/ai
                (REST + ETags)    (REST v3)       (shells `claude -p`)
```

- `internal/app` is the only place tools are wired into the registry.
- Frontends know nothing about each other and reach tools only through
  `tools.Tool`.
- Tool packages depend on the domain packages, never the reverse.

## 3. The `Tool` contract

```go
type Tool interface {
    Name() string
    Description() string
    InputSchema() *jsonschema.Schema
    Invoke(ctx context.Context, args map[string]any) (any, error)
}
```

Identical to the blueprint. Semantics:

- **`Name`** is the registry key. A `.`-separated name declares a namespace.
  Riggs uses up to three segments (`git.pr.approve`); see §4.
- **`InputSchema`** MUST have `Type: "object"`. `nil` means the tool takes no
  parameters.
- **`Invoke`** receives args keyed by the schema's property names. The CLI maps
  `--kebab-case` to `snake_case` before invocation, so tools never see flag
  spellings.

One optional extension exists:

```go
type VerbTool interface {
    Tool
    PrimaryArg() string
}
```

`PrimaryArg` names the schema property that a verb flag's value binds to (§4).
It is consumed **only** by the CLI frontend; over MCP the property is passed
like any other argument, so both frontends stay on exactly the same schema.

## 4. Frontend conventions

### CLI (`internal/frontends/cli`)

Command lines resolve against the registry in three forms, most literal first:

| Form | Example | Resolves to |
| --- | --- | --- |
| flat | `riggs ping` | `ping` |
| dotted | `riggs jira tickets --query …` | `jira.tickets` |
| verb flag | `riggs git pr --approve <ref>` | `git.pr.approve` |

The verb-flag form is a **deliberate divergence from the blueprint**, which has
two-segment names only. It exists because the operation reads better as a flag
than as a third positional token, and because it lets the operation carry its
primary argument as the flag's own value — which *preserves* the blueprint's
"every flag has a value, no positional arguments" parser rule rather than
breaking it.

Rules:

- A verb is matched against the **registry**, not a list of spellings:
  `<prefix>.<verb>` must be a registered tool. An ordinary parameter flag can
  therefore never be mistaken for a verb.
- The longest namespace prefix is tried first, so `git pr` wins over `git`.
- A verb flag's value is optional. `--fetch-reviews` with no value (or followed
  by another flag) binds nothing, letting the tool apply its own default — that
  is how the reviewer falls back to `admin.github-login`.
- **stdout** is reserved for tool output; **stderr** is for diagnostics
  (`riggs: <error>`). Exit code is `1` on any tool or usage error.
- **Result rendering** dispatches by type via `cli.Render`: `string` and
  `[]string` are written as-is, `fmt.Stringer` uses `String()`, everything else
  falls back to `%v`. `--json-output` switches to the same JSON shape the MCP
  frontend emits, so Murtaugh's workflow rules can parse either frontend.

### MCP (`internal/frontends/mcp`)

- **stdout** is reserved for MCP protocol traffic. No logs, no raw text.
- Every registered tool is exposed; its `InputSchema()` is published verbatim
  (an empty `{"type":"object"}` schema is substituted when it returns `nil`).
- Tool results are JSON-marshalled into a single `TextContent` block. A plain
  string result is passed through as-is so trivial tools stay uncluttered.
- Tool errors return `CallToolResult{IsError: true, …}`, never a transport
  error.

## 5. Package layout

```
cmd/riggs/                       # main; --config-file extraction, mode parsing
internal/
  app/                           # composition root + Registry wiring
  frontends/{cli,mcp}/
  tools/
    tool.go                      # Tool + VerbTool + Registry
    <tool>/                      # flat tool (ping, capabilities)
    <ns>/<cmd>/                  # namespaced tool
  config/                        # config file, admin identity, Slack profiles
  slack/                         # profile → delivery Target resolution
  notify/                        # the card ledger (§9)
  github/                        # REST client, ETag cache (§8)
  jira/ ai/                      # external seams
```

Rules:

- Each tool lives in its own package under `internal/tools/`.
- External-SDK wrappers and cross-cutting helpers live under
  `internal/<domain>/`, never inside a tool package.

## 6. Credentials and capability gating

Credentials are read from the environment; the config file holds `${ENV}`
references, never secrets.

| Variable | Used by |
| --- | --- |
| `SLACK_BOT_TOKEN` etc. | referenced by name from a Slack profile (§7) |
| `ATLASSIAN_JIRA_EMAIL` / `ATLASSIAN_JIRA_TOKEN` | Jira REST v3 (Basic auth) |

The names are the ones Murtaugh's `.env` already uses, so nothing needs
re-provisioning at cutover.

GitHub credentials come from the authenticated `gh` CLI — Riggs never stores a
GitHub token of its own — but Riggs owns the HTTP calls (§8). Card summaries
shell out to `claude -p`. Both `gh` and `claude` are reached through an
injected runner, never a raw `exec.Command` at the call site, so every loop
that touches them is fakeable.

Rules:

- **A missing credential disables a feature; it never fails the boot.** `riggs
  ping` and `riggs mcp` work on a machine with nothing configured.
- A **broken** config file, by contrast, IS fatal: an unknown key or malformed
  YAML means the operator wrote something they believe is in effect.
- Because a disabled tool is simply absent, "why is my tool missing?" is not
  answerable by reading the source. `riggs capabilities` answers it instead,
  naming the exact setting or binary that is missing. It is the analogue of the
  blueprint's `catalogue` decisions, and it never echoes a configured value.

## 7. Slack delivery

Riggs delivers to Slack and nowhere else. There is no output-sink abstraction,
because there is no second sink.

```yaml
admin:
  slack-user-id: U0B20G0ET9T
  jira-email: miere@nurturecloud.com
  github-login: miere
slack:
  profiles:
    default:
      bot-token: ${SLACK_BOT_TOKEN}
      user-token: ${SLACK_USER_TOKEN}
    nc:
      bot-token: ${SLACK_NC_BOT_TOKEN}
```

`internal/slack` resolves `(profile, channel)` into a `Target`. Resolution is
pure and total: it yields a fully-determined destination or a typed
`ErrNotConfigured` naming what is missing. Nothing opens a socket to find out.

Rules:

- `--slack-profile` selects the account; absent, it is `default`. A profile
  that is not defined — **including `default`** — is an error, because silently
  not notifying is worse than a loud failure.
- `--slack-channel` selects the conversation; absent, the notification is a DM
  to `admin.slack-user-id`. That in turn requires the admin to be configured.
- Both are declared as ordinary schema properties by every notifying tool,
  rather than as global frontend flags. On the CLI this is indistinguishable
  from a global flag; over MCP it is the only way a caller can express them at
  all.
- `admin` exists because that identity was previously spread across five
  settings in three files (`REVIEWER_HANDLE`, `REVIEWER_SLACK_ID`,
  `nudge_user_id`, `allowed_users`, `slack_to_jira_email`) and could drift.
- `app-token` is accepted on a profile but unused: Riggs never opens a Socket
  Mode connection (§1). The field exists so a profile can be described in full
  without the loader rejecting the key.

## 8. GitHub access

Riggs talks to GitHub's **REST** API over its own HTTP client, with ETag
conditional requests. It does not shell `gh` for data, and it does not use
GraphQL.

This is a departure from the blueprint, which delegates GitHub entirely to
`gh`. The reason is measured, not assumed. The review-queue loop runs every
minute across every tracked repo, and `gh pr list --json` is a GraphQL query
billed by node count:

| Path | Cost | Projected at every-1m |
| --- | --- | --- |
| `gh pr list --limit 200` × 7 repos (the shipped loop) | 164 points/tick | ~9,840/hr against a **5,000/hr** quota |
| same at `--limit 30` (applied to the Python as a stopgap) | 64 points/tick | ~3,840/hr |
| REST list + `If-None-Match`, nothing changed | **0** | **0** |

A conditional GET that returns `304 Not Modified` does not count against the
rate limit — verified directly against the live token, where two consecutive
304s left `x-ratelimit-used` unmoved. The GraphQL bucket has no equivalent: its
cost is the same whether or not anything changed.

The REST bucket is also a *different* 5,000/hr allowance, and the loop
currently consumes **zero** of it while the GraphQL bucket runs over budget.

Rules:

- The token is obtained by shelling `gh auth status --show-token`, so `gh`
  remains the credential provider and Riggs needs no GitHub environment
  variable of its own. (`gh auth token` is not available on the installed
  gh 2.14.7.)
- Every ETag is stored in the ledger (§9) beside the entry it belongs to, so
  the cache survives between invocations — which is what makes a per-minute
  poll from a short-lived process nearly free.
- **`updated_at` is not a sufficient change signal.** A PR's `updated_at` does
  not move when a check run completes, and "the PR went green" is the entire
  trigger for the review queue. Tracked open PRs therefore need their own
  ETag'd poll of `/commits/{sha}/check-runs`; driving refresh off `updated_at`
  alone would silently stop cards from ever appearing.
- Assembling one PR's state takes several endpoints (`/pulls`,
  `/pulls/{n}/reviews`, `/commits/{sha}/check-runs`, `/commits/{sha}/status`)
  where GraphQL took one query. That is the accepted cost: each is
  individually cacheable, and the steady state of this loop is "nothing
  changed".
- Discovery (`review-requested:<user>`) uses the search API, which has its own
  much tighter bucket (30 requests/**minute**). One call per tick is fine;
  fanning search out is not.
- Concurrency is bounded. GitHub's secondary rate limits trigger on burst
  concurrency independently of the quota, so the fan-out over tracked PRs is
  deliberately modest.

## 9. The notification ledger

*(Designed; implemented in phase 2.)*

Every notification is stateful. A nudge can only be threaded onto a message
that was already posted, so "post" and "update" and "thread" are one mechanism,
not three programs.

`internal/notify` owns a ledger keyed by `<tool>:<identity>` (e.g.
`git.pr:UpsideRealty/upside#20069`), storing `{profile, channel, ts,
fingerprint, latches, updated_at}`. Two operations cover both automations:

- **`Upsert(key, card)`** — post if the key is new; update **only** if the
  card's fingerprint changed; re-post and reset latches if the stored `ts` went
  stale (the message was deleted).
- **`Thread(key, msg, latch)`** — post into the stored message's thread, or
  no-op if there is no such message. Two latch policies cover every case in the
  Python: *once-per-episode* (the review queue's `tagged` flag, reset on
  leaving the reviewable state) and *min-gap* (the nudge's 1h floor under a 24h
  idle threshold).

Rules:

- The ledger is SQLite, not JSON. The every-1m reconcile and a button press can
  now write the same card from different processes; the Python's
  read-modify-write JSON file has no answer for that.
- It lives beside the config file, under the same base name:
  `~/.config/riggs/config.yaml` → `~/.config/riggs/config.db`. Moving the
  config with `--config-file` moves the state with it.
- Deriving the desired card is a **pure function** of the upstream data, with
  no I/O mixed in. That is what makes "update only when it actually changed"
  correct, and what makes running a tick twice a no-op.
- The existing Python state (`github_review_queue.json`, ~231 KB of live cards,
  and `tickets.json`) is imported at cutover. Losing it re-posts a hundred
  cards into `#nc-code-reviews`, so the import is a hard requirement.

## 10. Configuration file

`internal/config` owns the admin identity and the Slack profiles. It is loaded
once, in the composition root.

Precedence, first hit wins:

1. `--config-file <path>` — a reserved frontend flag, stripped in `cmd/riggs`
   before mode parsing, so it may appear anywhere on the command line. A
   missing explicit path is an **error**.
2. `$RIGGS_CONFIG`
3. `$XDG_CONFIG_HOME/riggs/config.yaml`
4. `~/.config/riggs/config.yaml`

Rules:

- There is **no embedded default**, which is a divergence from the blueprint.
  A default policy for "which Slack account is yours" would be a fiction. A
  missing conventional file is therefore not an error: it yields an empty
  config whose `Path` still reports where the file would live, so the ledger
  has a well-defined home before the config exists.
- Decoding uses `KnownFields(true)`. A mistyped key is a silent behaviour
  change, so it is refused rather than ignored.
- Structural problems are reported **all at once**, so fixing a config takes
  one edit rather than one round-trip per mistake.
- An empty token is a capability gap, not a config error (§6).

## 11. Testing conventions

- Any external process or SDK is wrapped behind a small interface inside its
  domain package; tests substitute a fake. No live external calls in tests.
- Probes for the ambient machine (`exec.LookPath`, `os.Getenv`) are injected,
  so a test never depends on what happens to be installed on the box running
  it.
- Each tool ships its own `_test.go` covering happy-path invocation, its input
  schema, and both output shapes.
- **The parity gate for phase 2**: a dry run against the *real* 231 KB review
  queue state file must produce zero Slack calls. If the port derives one state
  differently from the Python, that test fails loudly instead of spamming the
  channel.

## 12. Roadmap

| Phase | Contents | Status |
| --- | --- | --- |
| 0 | Skeleton, both frontends, config + profiles, `ping`, `capabilities` | done |
| 1 | Slack domain (live client), Block Kit cards, `notify` ledger | next |
| 2 | GitHub REST client + ETag cache (§8), PR state derivation, `git.pr.fetch-reviews` + parity gate | |
| 3 | `git.pr.approve` / `--approve-merge` | |
| 4 | Jira domain, `jira.tickets` poll/nudge/action | |
| 5 | State import, repoint Murtaugh jobs and rules, retire the Python | |

## 13. Change log

- **unreleased** — GitHub access moves to REST + ETag conditional requests
  (§8), replacing the blueprint's "delegate everything to `gh`". Driven by
  measurement: the shipped review-queue loop costs 164 GraphQL points/tick,
  ~9,840/hr against a 5,000/hr quota, while the REST bucket sits unused at
  zero. `gh` stays as the credential provider only.
- **0.0.0** — Phase 0. Go MCP + CLI skeleton over a shared registry, with
  verb-flag command resolution, the config file (admin identity + Slack
  profiles + derived ledger path), Slack target resolution, and the `ping` and
  `capabilities` tools.
