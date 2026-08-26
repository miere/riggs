# riggs-mcp — Architecture

This document is the canonical reference for the structural decisions in
riggs-mcp. Drift between code and this document is treated as a bug. Any change
that adds or modifies an architectural element MUST update this file in the
same PR.

Riggs takes its structure from `mcp-techops`. Where it departs from that
blueprint, the departure is called out and justified.

## 1. What Riggs is for

Riggs replaces the Python layer under Murtaugh's automations with a single Go
binary. Murtaugh keeps owning the **schedule**: it invokes Riggs as a CLI (from
a job) or over MCP to run a reconcile pass.

Riggs owns its own **Slack app**, and therefore its own inbound half. It posts
as itself and answers the clicks on its own messages directly, over a Socket
Mode connection it holds open (`riggs daemon`, §7b). Its responsibility is
exactly two things: send to Slack, and react to its own blocks.

> **This reverses the original design.** Riggs began as a pure callee — Murtaugh
> owned the connection, received every interaction, and invoked Riggs through a
> workflow rule to act on one. That indirection existed only because Riggs
> posted using Murtaugh's bot token, so the events landed in Murtaugh's gateway.
> Once Riggs has its own app they land here, and routing them back out through
> another process's config would be a detour with nothing at the end of it.
>
> Messages posted under Murtaugh's app before the switch stay Murtaugh's:
> `chat.update` and `chat.delete` require the token of the app that posted, so
> the old per-PR cards keep being served by the existing workflow rules and are
> deliberately not migrated.

The automations being replaced:

| Today | Trigger | Becomes |
| --- | --- | --- |
| `pull_request/main.py review-queue` | job, every 1m | `git.pr.fetch-reviews` |
| `pull_request/main.py approve` | rule `pr-approve` | `git.pr.approve` |
| `pull_request/main.py approve --action-id approve_merge` | rule `pr-approve-merge` | `git.pr.approve-merge` |
| `quick_coding_tasks/main.py poll` | job, every 3m | `jira.tickets.poll` |
| `quick_coding_tasks/main.py nudge` | job, weekdays 09/12/14/17 | `jira.tickets.nudge` |
| `quick_coding_tasks/main.py action` | rules `quick-coding-tasks-*` | `jira.tickets.assign` / `.dismiss` |
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
        ┌──────────────┼───────────────┬───────────────┐
┌───────▼──────────┐   │   ┌───────────▼──────┐  ┌─────▼──────────┐
│ frontends/cli    │   │   │ frontends/mcp    │  │ daemon         │
│ (human stdin/out)│   │   │ (MCP stdio JSON) │  │ (Socket Mode)  │
└───────┬──────────┘   │   └───────────┬──────┘  └─────┬──────────┘
        └──────────────┴────► Tool ◄───┘               │
                            (internal/tools)     Router → Handler
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
- Tool names are **normalised at this boundary**: every `.` and `-` in the
  registry name becomes `_`, so `slack.send-msg` is published as
  `slack_send_msg` and `git.pr.fetch-reviews` as `git_pr_fetch_reviews`. The
  convention matches Murtaugh's, and exists because some providers reject a
  `.` in a function name. It is a translation, not a rename: the registry key
  keeps its dots and the CLI keeps its spaces and hyphens, so this can never
  move a command.
- Normalisation collapses two characters into one, so distinct registry names
  could in principle collide (`a.b-c` and `a-b.c` both become `a_b_c`). That is
  refused at server construction rather than allowed to shadow a tool silently
  — the same treatment `Registry` gives a duplicate registration.
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
  slack/                         # profile → Target resolution; inbound decode (§7b)
  daemon/                        # Socket Mode listener + interaction router (§7b)
  notify/                        # the card ledger (§9)
  github/                        # REST client, ETag cache (§8)
  jira/ ai/                      # external seams
```

Rules:

- Each tool lives in its own package under `internal/tools/`.
- External-SDK wrappers and cross-cutting helpers live under
  `internal/<domain>/`, never inside a tool package.

## 6. Credentials and capability gating

Credentials come from the config file, which may hold either a literal token
or a `${ENV}` reference. The file is written mode 0600.

An earlier rule here said the config never holds secrets. `riggs install`
changed that: an installer that asks for a token has to be able to persist it,
and a machine with no pre-existing `.env` had nowhere to put one. The
`${ENV}` form remains, and the installer prefers it whenever the corresponding
variable is already populated — so an existing machine keeps its secrets in the
environment, and a fresh one is provisioned in one pass.

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
- `app-token` is the xapp- token `riggs daemon` opens its Socket Mode
  connection with (§7b). It is required only on the profile the daemon listens
  as, and ignored on every other: a profile Riggs only posts through needs a bot
  token and nothing more.
- The client speaks **HTTP directly**, not through `slack-go` (which Murtaugh
  uses). Riggs needs three endpoints — `chat.postMessage`, `chat.update`,
  `conversations.open` — and the cards are `container` blocks, a type the
  typed SDK does not model, so the payload would be hand-built JSON either
  way. Owning the request also means owning 429 handling, which matters for a
  per-minute job.
- Slack reports application errors with **HTTP 200 and `"ok": false`**, so the
  status code alone never tells you whether a post succeeded. Every response is
  checked on `ok`. `message_not_found` is translated into a typed error,
  because to the ledger it means "re-post", not "fail".

## 7b. The daemon (inbound)

`riggs daemon` holds a Socket Mode connection open and dispatches the
interactions Slack pushes down it. `internal/daemon` owns the connection, the
routing table and nothing else.

```
Slack ──ws──► SocketListener ──► Daemon ──► Router ──► Handler
                (slack-go)         (decode)   (dispatch)
```

- **Routing is on `(action_id, intent)`, matched exactly.** `intent` is the
  selected option's value for an overflow, or the button's value for a button.
  This is inherited from the workflow rules it replaces, and it is why every
  option's value is a bare token (`approve_merge`): a value that varied per row
  could not be matched by a table.
- **The per-row reference rides in the `block_id`**, because an overflow click's
  payload reports its own block_id but not its siblings' values. That is the
  only place a per-row identity can travel.
- **slack-go is used inbound only.** An inbound callback is a payload Slack
  composed, its shape is Slack's to change, and the SDK already models it.
  Outbound stays hand-built ordered-struct JSON — that is what makes blockkit's
  fingerprint stable, and therefore what makes the ledger's "update only when it
  actually changed" mean anything (§9). A map-backed encoder would quietly
  break it.
- **Every callback is acked before it is handled**, and handled on its own
  goroutine. Slack expects an ack within three seconds; approving a pull request
  makes several GitHub calls with retries and legitimately takes longer, so
  acking after the work would have Slack re-sending an interaction already being
  acted on.
- **A handler error is logged and swallowed.** One failed click must not take the
  connection down: the next click is separate work, and an exit here needs a
  human to notice before any button works again.
- **The scheduler stays in Murtaugh.** The daemon owns reactions, not timing. So
  a reconcile pass (CLI) and a click (daemon) are separate processes writing the
  same ledger — which is what its WAL and busy timeout were chosen for (§9).
- **An unroutable click is reported, not an error.** A retired control whose
  message is still in the channel is an ordinary occurrence, and the daemon logs
  what it could not place.

## 7c. The bulk block

`internal/blockkit` now renders two shapes, and they are separate on purpose.

| | `Card` (card.go) | `Digest` (bulk.go) |
| --- | --- | --- |
| Carries | one entity | many items |
| Slack shape | `container` + `actions` row | `card` header + `section` rows |
| Controls | buttons, link button, overflow | one `overflow` accessory per row |
| Identity | `actions` block_id | each row's block_id |
| Changes when | that entity changes | membership changes |

They look alike and are not the same feature. A `Card` is one entity's
self-updating container; a `Digest` is a list whose membership moves underneath
it. Collapsing them would couple two lifecycles that are about to diverge, so
only what is *provably* identical is shared: the fingerprint rule
(`fingerprint.go`) and the primitive text/icon objects. Everything else is
duplicated deliberately.

That includes the menu option type. A digest option can carry a `url` — which is
how "Open on Browser" costs no interaction — and a container card's overflow
never has; giving the shared type a URL field would have changed the bytes every
existing card renders, and the fingerprint with them.

Rules:

- **A row's block_id is its identity.** Same constraint as §7b: the overflow
  click reports its own block_id and not its siblings' values.
- **Option values are bare intent tokens**, identical on every row, so the
  router's table can match them exactly.
- **Only the title is struck through** on a done row. The reference and author
  stay legible, because they are what you read to find the thing again.
- **Titles are escaped and elided.** A title containing `&` or `<` would
  otherwise re-open the row's own bold run and garble every row after it; one
  untruncated title in a list of ten pushes the reference onto a wrap.
- **An empty digest is deleted, not rendered.** A header with nothing under it
  reads as "nothing needs you" while occupying the space of when something did.
  `Empty()` reports it; §9b acts on it.

## 8. GitHub access

Riggs talks to GitHub's **REST** API over its own HTTP client, with ETag
conditional requests. It does not shell `gh` for data.

It uses GraphQL for exactly **one field**: `reviewDecision`. REST does not
expose it, and it cannot be reconstructed from the review list — two attempts
to derive it were wrong in opposite directions, and the parity check (§11)
caught both against live data:

| Pull request | Shape | GitHub says |
| --- | --- | --- |
| `gcp-jsm-bridge#80` | one approval from another reviewer, branch unprotected, we are still requested | `APPROVED` |
| `nct-intelligence-beholder#1315` | the same, plus an older dismissed review | `null` |

Whatever separates those is undocumented, so it is asked rather than guessed.
One query per candidate pull request, against 164 points per tick for the
`gh pr list` this replaces.

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
- **Writes are a separate path.** Mutations never go through the conditional
  GET helper: they are not cached, not conditional, and **never retried**. A
  retried approval could approve twice; a retried merge could act on something
  already merged. One attempt, and the error carries GitHub's own words so a
  merge conflict reaches Slack as "Base branch was modified", not "HTTP 405".
- **Merging is rebase, always.** Our repositories do not allow squash and have
  no auto-merge. The method is not a parameter — making it one would invite a
  caller to pass "squash", and a merge that fails *after* an approval has
  landed is the worst outcome of this flow.
- **Reads are ordered cheapest-first.** Discovery matches team-based review
  requests too, so a tick sees far more pull requests than it acts on — 48
  against 8 on the live queue. Resolving all of them fully cost *more* GraphQL
  than the path being replaced. So the detail read (cached, conditional) comes
  first and settles both cheap exclusions; checks come before the decision;
  and an untracked pull request that is not green is answered without asking
  for a decision at all.
- Measured steady state, live queue: **73 requests per tick of which 64 are
  304s, no measurable REST quota, ~30–41 GraphQL points**. The remaining cost
  is one `reviewDecision` query per candidate; batching them into a single
  aliased query is the obvious next reduction.

## 8b. The ticket queue

`internal/ticket` advertises a Jira query as claimable cards. The JQL is a
parameter, not a constant, so one tool serves any queue rather than only the
`ai-able` board the Python was pinned to.

Rules:

- **The query is the source of truth for "still up for grabs".** A tracked card
  whose ticket no longer matches has been handled elsewhere and collapses. A
  ticket that cannot be *read*, though, is left alone: collapsing on a
  transient failure would claim it was handled when it may not have been.
- **Claiming means assign *and* transition.** A ticket assigned but left in
  Ready is re-advertised by the next poll, so a failed transition is reported
  rather than printed and ignored — which is what the Python did.
- **Dismissing does not touch Jira.** It means "not for me", not "handled"; the
  ticket stays exactly as it is for anyone else.
- **Only the configured admin may act.** A card is visible to a whole channel,
  and a button anyone could press would assign work to someone who never asked
  for it. An unattributed click is refused too.
- The nudge re-checks Jira before pinging, so a quietly claimed ticket gets its
  card collapsed instead of a bogus reminder. Age is measured from when the
  card was advertised, not from any Jira date.
- **Dry runs never summarise.** Shelling `claude -p` per ticket turned a
  preview into minutes of work for a value nobody acts on; the title stands in.
  The same applies to the pull-request loop.

## 9. The notification ledger

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
- A threaded reply goes to the **card's own channel**, read from the ledger,
  not to the caller's current default — otherwise moving a default would
  strand replies away from the card they belong to.
- The existing Python state (`github_review_queue.json`, ~231 KB of live cards,
  and `tickets.json`) is imported at cutover. Losing it re-posts a hundred
  cards into `#nc-code-reviews`, so the import is a hard requirement.

Tables: `cards` (key → profile, channel, ts, fingerprint), `latches`
(key, name → fired_at) and `http_cache` (url → etag, body) for §8.

## 9b. The item ledger (bulk digests)

§9's unit is a **card**: one message about one entity, keyed by that entity
forever. A digest breaks that assumption — one message carries many items, and
which items it carries moves underneath it. So `items` records the membership
`cards` never needed:

```
items(key, stream, post_key, position, status, done, posted_at, updated_at)
```

`cards` keeps its job as the *post* table; a digest's row there is keyed
`git.pr.bulk:post:<n>`, allocated by `NextPostKey`.

**`posted_at` is the cooldown anchor, and it moves only on entry to a NEW post.**
An in-place status refresh deliberately does not touch it. Otherwise a busy pull
request — checks flipping red and green all morning — would keep resetting its
own clock and could never age out of the message it was first announced in.

One pass, per pull request:

| State | Action |
| --- | --- |
| untracked, actionable | candidate to join the next digest |
| untracked, not actionable | ignored — never announced (same dead-on-arrival rule as §9) |
| tracked, within cooldown | stays; row refreshed in place |
| tracked, cooled, still open | moves: out of its old post, into the new one |
| tracked, cooled, done | purged |

Then every existing post is rebuilt from the items that remain in it, and one
new post is created from the selection.

Rules:

- **Rolling 3h cooldown**, not calendar blocks.
- **FIFO by pull request age** — oldest waiting first, not by when Riggs noticed
  it.
- **The cap holds, it does not drop.** Anything past its cooldown that misses
  the cap stays exactly where it is and leads the queue next pass. A row removed
  with nowhere to go would simply vanish.
- **A done row does not rotate.** It stays struck through where it is until its
  cooldown expires, then it is purged. That is what eventually empties a post —
  and it means a pull request that goes green again comes back as new, which is
  the same non-stickiness §9's cards have.
- **An emptied post is deleted, not blanked** (§7c).
- **A vanished digest stays vanished.** Unlike a card, a digest whose message is
  gone is not re-posted: its items come back on their own cooldown, and
  resurrecting a message the reader dismissed is the wrong answer.
- **A post that cannot be read from GitHub is not treated as deleted.** A failed
  read is skipped and retried; only the cooldown purges.
- **Rebuilding is idempotent.** The fingerprint gate means a pass that changes
  nothing makes no Slack call, which is what keeps a per-minute schedule
  affordable.

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

## 12. Installation

`riggs install` (in `internal/installer`) provisions a working setup. It is
interactive, so it lives outside the tool registry and is never exposed over
MCP — the same treatment the blueprint gives its `auth` command.

The flow: config location, admin identity, credentials, a live smoke test, then
Murtaugh's jobs.

Rules:

- It **refuses to run without a terminal**, rather than echoing a pasted token
  into the scrollback of a piped session.
- The GitHub login is discovered, not asked for: `gh auth status --show-token`
  reports it alongside the token. Note that gh writes both to **stderr**, and
  exits non-zero when any configured host is unhealthy — so both streams are
  parsed and the exit code is not what decides success.
- The smoke test posts a **real card for a real PR** to the admin's DM. A
  stand-in message would prove less than it appears to. Any failure aborts the
  install, because the alternative is a setup that looks finished and fails
  later, unattended.
- Zero PRs awaiting review is not a failure: a confirmation card is sent
  instead, and the console says which happened.
- Murtaugh is configured **only** through its CLI — `murtaugh jobs define`,
  never a write to its database: the command re-validates the whole assembled
  config and rolls back a change that would leave it invalid, which a
  hand-written row would bypass. Note `cfg job set` is documented as
  equivalent but rejects `--args`, which would leave the job invoking Riggs
  with no verb at all.
- Job cadences are carried over unchanged from the existing definitions
  (1m, 3m, and the weekday cron). A migration that also changes the schedule
  makes it impossible to attribute a behaviour difference.
- A job whose tool this build does not expose is **skipped and reported**, not
  installed. Registering `jira.tickets` before phase 4 would mean a scheduled
  failure every three minutes.
- The job passes `--config-file` only when the config is not where Riggs would
  look anyway, so the common case stays readable.

## 13. Roadmap

| Phase | Contents | Status |
| --- | --- | --- |
| 0 | Skeleton, both frontends, config + profiles, `ping`, `capabilities` | done |
| 1 | Slack domain (live client), Block Kit cards, `notify` ledger, `slack.send-msg` | done |
| 2 | GitHub REST client + ETag cache (§8), PR state derivation, `git.pr.fetch-reviews` + parity gate | done |
| 3 | `git.pr.approve` / `--approve-merge` | done |
| 4 | Jira domain, `jira.tickets.*` poll/nudge/assign/dismiss/import | done |
| 5 | Repoint Murtaugh jobs and rules, retire the Python | done (applies on gateway restart) |
| 6 | Riggs' own Slack app: `riggs daemon`, Socket Mode, interaction router (§7b) | done |
| 7 | The bulk digest block (§7c) | done |
| 8 | The item ledger and the digest reconcile loop (§9b) | done |
| 9 | The digest's actions: ask-review, approve-and-merge | done |

## 13b. Cutover

Performed 2026-08-10. Three jobs and four workflow rules now point at
`~/.local/bin/riggs`; `fetch-important-comms` stays on Python (out of scope)
and `pr-run-local-review` stays a `delegate-to-agent` trigger (Riggs is not
involved).

The Murtaugh runtime loads its config once at startup, so **the change takes
effect when the gateway is restarted**, not when it is written. Until then the
Python continues to run — which makes the staged state safe rather than
half-migrated.

Before the cutover, one thing was verified rather than assumed: that a job run
by Murtaugh inherits the environment Riggs needs. A throwaway
`riggs capabilities --json-output` job, run through `murtaugh jobs run`,
confirmed both Slack tokens, Jira and the `gh`/`claude` binaries were visible.
The whole migration rests on that, and it is not obvious — the Python needed
only `ATLASSIAN_JIRA_*`, because it reached Slack by shelling `murtaugh`
rather than holding a token.

Rollback: the previous job and rule definitions are captured under
`/tmp/riggs-cutover-backup/` and can be restored with the same commands.

## 14. Change log

- **unreleased** — Phase 6. Riggs gets its own Slack app and its own inbound
  half: `riggs daemon` (§7b), a Socket Mode listener and an
  `(action_id, intent)` router. slack-go joins as the first Slack dependency,
  used to *decode* callbacks only — outbound stays hand-built JSON, because the
  ledger's fingerprint depends on a stable encoding. Reverses §1's
  "always the callee" invariant and puts `app-token` (§7) to work.
- **unreleased** — Phase 7. `blockkit.Digest` (§7c): the bulk block, a `card`
  header over `section` rows with `overflow` accessories. Deliberately not a
  refactor of `Card` — the two shapes share only the fingerprint rule and the
  primitive text objects, because a per-entity card and a list whose membership
  moves are about to evolve apart.
- **unreleased** — Phase 8. The item ledger (§9b) and the digest reconcile
  loop: `items` records which post each pull request is shown in and when its
  3h cooldown started, `chat.delete` joins the Slack client so an emptied
  digest can be removed rather than blanked, and `git.pr.bulk` schedules the
  pass. Sibling of `git.pr.fetch-reviews`, not a replacement — both read the
  same GitHub and write the same ledger in different streams.
- **unreleased** — Phase 5, the cutover (§13b). Also fixes the installer,
  which built its job command from `cfg job set` — documented as equivalent to
  `jobs define`, but it rejects `--args`.
- **unreleased** — Phase 4. `internal/jira` (REST v3, ADF flattening) and
  `internal/ticket`. Adds `jira.tickets.poll`, `.nudge`, `.assign`,
  `.dismiss` and `.import-state`. Verified against live Jira: 16 tickets
  matched, and after importing the Python's 106 entries the poll recognises
  all of them as already advertised.
- **unreleased** — Phase 3. `git.pr.approve` and `git.pr.approve-merge`, with
  the approval guard (a standing approval is not resubmitted; a dismissed one
  is), verification with retries through GitHub's replication lag, and honest
  outcome messages — the Python posted "Approved" unconditionally. Adds a dry
  run the Python had and the first cut here did not.
- **unreleased** — Phase 2. `internal/github` gains conditional requests and
  the pull-request reads; `internal/pullrequest` ports the state derivation;
  `internal/ai` produces the card summaries. Adds `git.pr.fetch-reviews`,
  `git.pr.import-state` and `git.pr.check-parity`. The parity check passes 8/8
  against the live queue, having first caught two wrong derivations of
  `reviewDecision` (§8).
- **unreleased** — MCP tool names are normalised: `.` and `-` become `_` at
  the MCP boundary only (§4). Registry keys and CLI spellings are unchanged.
- **unreleased** — `riggs install` (§12), plus the first slice of
  `internal/github`: the token/login from `gh`, and the review-requested
  search the smoke test posts a card for. Adds a `jira` config section so the
  installer can persist the Atlassian credentials it asks for.
- **unreleased** — Phase 1. The live Slack client (plain HTTP, §7), the
  `blockkit` card renderer shared by both automations, and the `notify` ledger
  (SQLite: cards, latches, HTTP cache). Adds `slack.send-msg`, registered only
  when a Slack profile exists, and `capabilities` now reports the ledger
  without creating it.
- **unreleased** — GitHub access moves to REST + ETag conditional requests
  (§8), replacing the blueprint's "delegate everything to `gh`". Driven by
  measurement: the shipped review-queue loop costs 164 GraphQL points/tick,
  ~9,840/hr against a 5,000/hr quota, while the REST bucket sits unused at
  zero. `gh` stays as the credential provider only.
- **0.0.0** — Phase 0. Go MCP + CLI skeleton over a shared registry, with
  verb-flag command resolution, the config file (admin identity + Slack
  profiles + derived ledger path), Slack target resolution, and the `ping` and
  `capabilities` tools.
