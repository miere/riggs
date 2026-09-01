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
| `pull_request/main.py review-queue` | job, every 3m | `git.pr.bulk` (was `git.pr.fetch-reviews`, §12c) |
| `pull_request/main.py approve` | rule `pr-approve` | `git.pr.approve` |
| `pull_request/main.py approve --action-id approve_merge` | rule `pr-approve-merge` | `git.pr.approve-merge` |
| `quick_coding_tasks/main.py poll` | job, every 3m | `jira.tickets.bulk` (was `jira.tickets.poll`, §8d) |
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
                internal/github        internal/jira    internal/slackmd
                (REST + ETags)         (REST v3)        (GH md -> mrkdwn)
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
  notify/                        # the card ledger (§9) + the item ledger (§9b)
  bulk/                          # the digest rotation engine (§9b)
  ask/                           # the "hand this to somebody" tag (§7bb)
  github/                        # REST client, ETag cache (§8)
  jira/                          # external seam
  slackmd/                       # GitHub Markdown -> Slack mrkdwn (§7d)
  apphome/                       # the App Home tab and its Update button (§7e)
  updates/                       # release lookup + binary self-update (§7e)
  version/                       # the build version, stamped by the release workflow
```

Rules:

- Each tool lives in its own package under `internal/tools/`.
- External-SDK wrappers and cross-cutting helpers live under
  `internal/<domain>/`, never inside a tool package.
- **A domain package owns what its items ARE and how they DRAW; nothing else.**
  `pullrequest` and `ticket` each supply a `bulk.Domain` and stop there. The
  rotation between messages is not theirs to reimplement, and neither is the
  guarantee that an ask names both people.

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
GitHub token of its own — but Riggs owns the HTTP calls (§8). The AI harness
behind the two Run options (§7bb) is shelled out to in full, and holds its own
auth the same way. Both are reached through an injected runner, never a raw
`exec.Command` at the call site, so every loop that touches them is fakeable.

The harness is configured, not discovered: there is no default command, because
a machine quietly shelling out to whatever happened to be on its PATH is worse
than one that offers no Run option at all.

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
- **EVERY request is acked, first, whatever it is** — not only the ones the
  dispatch switch understands. Slack marks a control with a ⚠ when it gets no
  response, and an earlier version acked only inside the interactive arm with no
  default, so anything else was dropped in total silence: no ack, so Slack
  warned; no log line, so nothing recorded it. A link button raises an
  interaction even though Slack itself opens the URL.
- **Callbacks are handled on their own goroutine.** Slack expects an ack within three seconds; approving a pull request
  makes several GitHub calls with retries and legitimately takes longer, so
  acking after the work would have Slack re-sending an interaction already being
  acted on.
- **A handler error is logged and swallowed.** One failed click must not take the
  connection down: the next click is separate work, and an exit here needs a
  human to notice before any button works again.
- **The scheduler stays in Murtaugh.** The daemon owns reactions, not timing. So
  a reconcile pass (CLI) and a click (daemon) are separate processes writing the
  same ledger — which is what its WAL and busy timeout were chosen for (§9).
- **The log level is `$RIGGS_LOG_LEVEL`.** It was fixed at Info, which is how a
  callback the daemon chose to ignore left no trace at all.
- **An unroutable click is reported, not an error.** A retired control whose
  message is still in the channel is an ordinary occurrence, and the daemon logs
  what it could not place.

## 7bb. The digests' actions

Two digests, one pattern. On the pull-request digest, four options can render on
a live row and three are answered by the daemon:

| Option | Intent | Handler |
| --- | --- | --- |
| ⧉ Open on Browser | `open_browser` | none — the option carries a `url` and Slack opens it |
| ✎ Ask for Code Review | `ask_review` | `pullrequest.Asker` — tags a person, starts nothing |
| ▸ Run Code Review | `run_review` | `ai.Runner` — runs the local harness |
| ✓ Approve and Merge | `approve_merge` | `pullrequest.Approver`, rebase-only (§8) |

On the ticket digest, three:

| Option | Intent | Handler |
| --- | --- | --- |
| ⧉ Open on Browser | `open_browser` | none — same as above |
| ✎ Ask for SME Assistance | `ask_assist` | `ticket.Asker` — tags a person, starts nothing |
| ▸ Run AI Assistance | `run_assist` | `ai.Runner` — runs the local harness |

### Asking is not running

The two are separate verbs, and for a long time they were one option that read
as the wrong one. "Ask for AI Assistance" tagged a colleague and started nothing:
everyone who read the label expected an agent to pick the ticket up, and what
actually happened was that a person got mentioned in a thread. The label was the
bug. The pull-request side had the same shape with an honest name — "Ask for
Code Review" does ask — but no way to do the work either.

So each pair is now two options, and both halves of each pair are optional:

- **Ask** needs somebody to ask (`review-request.user-id`, `sme-assistance.user-id`).
- **Run** needs a harness to run (`ai.command`).

**Neither has a fallback.** The asks used to fall back to the admin, which was
defensible while asking was the only verb on the row — asking yourself at least
reaches somebody. It is not defensible beside Run: a menu whose first entry
quietly means "send myself a card" reads as a bug next to one that does the work.
An unanswered installer question turns the option off, which is also the only
reading under which the two are honestly distinct.

**The option is not rendered when its setting is absent**, which is the rule this
surface applies everywhere else — a control that cannot act is worse than one
that was never there, because it invites a click and then explains why it will
not work. `riggs capabilities` reports all four and names the setting behind each.

**`RowActions` is passed to the digest engine AND to the completer.** The
completer redraws a digest from the ledger after an approval; given a different
answer about what may be offered, it would silently add or remove options from
every row in a message nobody touched.

**The intent token `ask_assist` keeps its original spelling** even though its
label no longer says "AI". Digests already sitting in Slack carry it in their
option values, and renaming it would turn every one of those menus into a button
the router does not answer.

### Running one

`internal/ai` is the harness: a configured command line, a working directory, a
timeout, and the process seam every external binary in this codebase goes behind
(§11). The package name is the one Phase 21 retired — that `internal/ai` shelled
out to `claude -p` for a one-paragraph card summary, and was removed because a
summary is not worth 8.6 seconds on the click path, a hard dependency on a local
binary, and output that changed between renders (§7d). None of those objections
survives here: this **is** the work, nothing is waiting on a render, and a
machine without the binary is told the option is off rather than shown one that
cannot fire.

- **A known harness gets its own prompt flag; anything else is handed the prompt
  as its first argument.** The list has one entry, `claude`, and is short on
  purpose: guessing another tool's calling convention from its name is how a
  review prompt ends up being read as a filename. The program is matched on its
  base name, so `/opt/homebrew/bin/claude` is the same harness as `claude`.
- **The command is split on whitespace, with no quote handling.** An invocation
  needing more than that wants a wrapper script, which is one line and legible
  from the outside — unlike a quoting dialect invented here.
- **The working directory is asked for, never assumed.** It is the setting that
  decides whether the feature works at all: Claude Code reads the project it is
  standing in — its CLAUDE.md, its permissions, its git remote — and a launch
  agent inherits a working directory of `/`. A harness started in the wrong place
  is not slightly worse; it is a review of nothing.
- **The prompt's subject is guaranteed, the wording is not.** `{ref}`/`{key}` and
  `{url}` place them; a prompt naming neither still gets both appended on their
  own line. This is the same guarantee `internal/ask` makes about its two
  mentions and it is made for the same reason: a wording edited to drop the
  reference fails *silently*. The harness still starts, still runs, and still
  reports success — having reviewed whatever it found in the working directory.
- **One status line, updated in place.** Posted before the harness starts,
  because a run takes minutes and an option that shows nothing for four of them
  reads as one that did not work. Rewritten with the outcome when it finishes.
  Three messages saying "started", "still going", "finished" would bury the
  digest under its own progress report.
- **A failure quotes the tail of the output**, last twelve lines and 1200
  characters, fenced. The end rather than the beginning: a harness that failed
  says why last, after however much progress it narrated first.
- **The run is bounded** (`ai.timeout`, default 15m). This runs under a daemon
  nobody is watching, where a hung harness is indistinguishable from one thinking
  hard right up until the machine runs out of them. A timeout and a failure get
  different words, because one is a review that went wrong and the other may
  still have been going.
- **The same item cannot run twice at once.** That is a double-click, and the
  second run is pure waste that would also race the first to comment. Different
  items run concurrently, and there is deliberately **no global cap**: these are
  started by hand, one click at a time, and a limit that silently refused the
  second is a worse failure than two processes on a machine that can afford them.
  The claim lives in the runner, which is therefore the one handler built once
  and held rather than per click — a runner rebuilt per click could not tell that
  the same pull request is already being reviewed.
- **Nothing about the run reaches GitHub or Jira from Riggs.** The harness's own
  output goes wherever the prompt sent it, under the admin's own credentials, and
  the Common Rule applies to the prompt as it does to everything else.

- **Both action ids are distinct** (`pr_bulk_overflow`, `jira_bulk_overflow`), so
  one dispatch table answers both without either digest having to know the
  other exists.
- **"Assign to Me" is not implemented, and so is not rendered** — the same call
  Approve got. The verb behind it (`jira.tickets.assign`) already exists and is
  still reachable; what is missing is the option and its route, which is where
  it should stay until somebody wants it.

- **Approve is not implemented.** It is specified for later, and it is not
  rendered: a button that silently does nothing is worse than one that is not
  there.
- **Approve and Merge renders only on a Dependabot-authored pull request.**
- **A done row keeps only the link.** There is nothing left to approve or ask
  about on a pull request that has been reviewed, merged or closed.
- **Ask for Code Review posts a CARD and stops.** It is the legacy container
  shape (§12c is why that renderer was kept), with two differences: no overflow
  — the reviewer is being asked one question, and a menu of alternatives is a
  worse way to ask it — and approving from it leaves **no comment** on GitHub,
  because that approval is the reviewer's own and a body would be words they did
  not write. Riggs then tags the reviewer in the card's own thread, so the card
  reads as the subject and the ask as the message about it. Nothing is delegated
  and no review is started. Destination is `review-request` in the config: a
  channel, or a DM when none is named.
- **The ask card is tracked in the ledger** (`git.pr.ask:<ref>`), so an
  approval can find and rewrite it. It was not, at first: "an ask is a one-off,
  not a card to maintain" held right up until an approval needed to change one.
  Asking twice therefore updates the card rather than posting a second, which is
  the better answer anyway.
- **An approval settles it**: the Approve button goes and the container
  collapses. A card still offering Approve for a merged pull request is worse
  than no card — it invites a click that can only fail. The link stays, because
  "where was that again" outlives the review. Settling is independent of the
  digest row: a pull request can be in a digest, have an ask card, both, or
  neither, and the same approval settles whichever exist.
- **The ask card has its own `action_id`** (`pr_ask_review`), not the legacy
  card's `approve_only`, so Riggs' dispatch table and Murtaugh's still-live
  rules never have to agree about a name.
- **Handlers build their dependencies per click** and close them again. Holding
  a ledger handle and a GitHub client open all day for the seconds a week anyone
  spends clicking would also mean holding them across the reconcile pass that
  runs in a different process.

### The ask

Both digests can hand one item to a person, and both do it the same way: post a
card about the thing, then tag them in its thread. `internal/ask` owns the one
part that must not differ. Neither of these starts anything — that is the pair
of Run options above, and the whole reason they are separate.

The wording is configuration. The two mentions in it are not:

- the person being asked is mentioned — prefixed if the prompt did not;
- the person who asked is copied in — appended as `c/c` if the prompt did not.

Those are the point of the feature, and a prompt edited to drop one would fail
*silently*: the message still posts, still reads fine, and simply never reaches
anybody. `{user}` and `{requester}` place them; `{reviewer}` stays an alias for
`{user}` because a live config still spells it that way. A prompt that places
`{requester}` inline — as the ticket default does — renders `somebody` when the
click carried no user, because an empty mention reaches Slack as a literal
`<@>`.

The ticket ask carries **no verb at all** — only the link. A review request asks
somebody to look at code that exists and offers Approve; this asks a
subject-matter expert whether work that does not exist yet is ready to be picked
up, and there is nothing on that card for them to press.

### Glyphs

Status markers are TEXT-PRESENTATION characters, named in `blockkit/glyphs.go`.
That is a choice, not a setting.

A text object's `emoji: false` governs **one** thing: whether Slack parses
`:shortcode:` sequences. It has no effect on a literal codepoint Unicode gives
emoji presentation — Slack renders those as a colour image regardless, and
normalises them back to a shortcode in the message's fallback text. Which is how
`⏺` (U+23FA) shipped in a status line on a block that already had
`emoji: false`, while `✓` (U+2713) on the very next line rendered correctly.

The two are indistinguishable in an editor, so the rule is enforced by a test
that parses every `.go` file under `internal/` and rejects an
emoji-presentation rune in any string literal. Comments are exempt, which is
what lets this paragraph name the characters it is warning about.

### The Common Rule

**Nothing Riggs sends anywhere may refer to Riggs.** Not the Slack messages, not
the GitHub review bodies, not a Jira comment. Every one of them is submitted
with the admin's own credentials and reads as the admin's own words, because it
is the admin's own decision that produced it — naming the automation that
carried it says something about their process to everyone who reads the pull
request, and that is not the tool's to volunteer.

The approval body was `"Approved via Riggs."` until Phase 9. It is now
`"Approved."`, and `assertNoSelfReference` in the tests holds the line.

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
- **Titles are elided at 50 runes**, cut to 47 plus an ellipsis. One
  untruncated title in a list of ten pushes the reference onto a wrap.
- **Each digest family has its own icon, title and subtitle consts**, not the
  legacy card's and not each other's. Sharing one would mean a change to it
  silently re-rendering every message of the others — the same reason the option
  type is duplicated. `blockkit.Digest` itself is shared by both families: it is
  the *shape*, and the shape genuinely is identical.
- **Titles are escaped.** A title containing `&` or `<` would
  otherwise re-open the row's own bold run and garble every row after it.
- **An empty digest is deleted, not rendered.** A header with nothing under it
  reads as "nothing needs you" while occupying the space of when something did.
  `Empty()` reports it; §9b acts on it.

## 7d. GitHub Markdown → Slack mrkdwn

`internal/slackmd` translates GitHub-flavoured Markdown into Slack's mrkdwn. It
is a shared component on purpose: the pull-request card body needs it now, the
Home tab's release notes will, and Jira descriptions are the same problem in
different clothing.

The two dialects look alike and are not, and text moved across without
translation does not fail — it renders wrong, quietly.

| GitHub | Slack | Why it matters |
| --- | --- | --- |
| `**bold**` | `*bold*` | a single `*` means the **opposite** thing in each |
| `*italic*` | `_italic_` | copying emphasis across turns bold into italic |
| `~~struck~~` | `~struck~` | one tilde, not two |
| `## Heading` | `*Heading*` | mrkdwn has no heading; a `#` renders literally |
| `[t](url)` | `t [N]` | links do not render on every surface these strings reach |
| ` ```kotlin ` | ` ``` ` | Slack prints the language as text in the block |
| `- item` | `•  item` | mrkdwn renders no list markup |

Rules:

- **Bold is parked behind a NUL sentinel before italics are touched.** In the
  obvious order `**x**` becomes `*x*`, and the italic rule then matches its own
  predecessor's output and turns every bold run into an italic one. This is the
  single easiest thing to get wrong here.
- **Images are taken before links.** An image is a link with a `!` in front, so
  the link pattern would claim it and leave a stray exclamation mark.
- **Suppressed links are recorded, not dropped.** `Result.Links` carries them,
  and `WithFootnotes()` prints the list — release notes want it, a two-line card
  body does not.
- **Code fences pass through untouched** apart from the fence and escaping.
  Emphasis inside a code block is not emphasis.
- **`&`, `<`, `>` are escaped**, because Slack reads them as markup.
- **It is a simplified converter, not a Markdown implementation.** One line at a
  time, no nested structure. That is enough for a description or a release note,
  and stopping there is what keeps it readable in one sitting.

### Card bodies

Ticket cards use the same rule, over the flattened ADF description. With that,
**`internal/ai` is gone** and nothing in Riggs shells out to an LLM.

A pull-request card shows the first two paragraphs of the description, converted
— **not** an LLM summary. That call cost ~8.6s on the click path with a human
waiting, bound the card to a local `claude` binary being present and
authenticated, and returned non-deterministic text, so the same card rendered
differently on two passes and could not be honestly fingerprinted. A description
is also not obviously improved by being summarised: the author wrote it to
explain the change.

An HTML comment (the template nobody deletes) and a badges-only paragraph are
skipped rather than counted, because both are routinely the first thing in a
body and neither says anything.

## 7e. The App Home tab

`internal/apphome` renders the one surface Riggs has that is about *Riggs*
rather than about somebody's pull requests, and `internal/updates` is what makes
its single control mean anything.

The view, top to bottom: the portrait, the running version with a controls menu
beside it, then — behind a divider — the four editable prompts, and then, behind
another, the latest release's notes with an **Update** button beside them.

### The audience split is the design

Everyone in the workspace can open the app and see the portrait and the version.
**Everything from the divider onwards is the admin's alone**, and so is the
controls menu above it. A non-admin is not shown a greyed-out button; they are
shown nothing, because a control you cannot use is worse than one that was never
there — it invites a click and then explains why it will not work.

The gate is `admin.slack-user-id`. An **unset** admin matches *nobody*, not
everybody: the other reading of an empty setting hands a binary swap to the whole
workspace.

It is also why the release lookup runs for the admin only. For everyone else it
is not merely wasted work — it is a GitHub request per curious colleague, against
a 60-an-hour unauthenticated quota, for a section they will never be shown.

### Versioning

Riggs versions itself the way Murtaugh does, and for the same reason this tab
needs: **a pushed tag is the release**. `.github/workflows/release.yml` stamps
the tag into `internal/version.Version` via ldflags and publishes one asset per
platform, named `riggs-<tag>-<goos>-<goarch>`.

That name is a **contract** between the workflow and `updates.AssetName`, which
is the function the Update button looks up. It lives in one place because a
mismatch produces a release that installs on nobody's machine — and nothing
fails until somebody clicks.

The release job re-reads the built binary's own `version` output before
publishing. It catches the single mistake the workflow can make unaided: an
ldflags path that no longer matches the package, which builds perfectly and
reports `dev` forever.

### A dev build must always be able to take the latest stable

This is the **one place Riggs deliberately diverges from Murtaugh**. Murtaugh
refuses to update a `dev` binary, on the grounds that overwriting a local
checkout would surprise the developer. Riggs is specified the other way, and the
reason is situational rather than philosophical: the daemon under launchd is
routinely running a binary built from a working tree, so refusing it would remove
the button precisely on the machine that needs it.

So a running version that is not a release — `dev`, or the bare VCS revision
`version.String()` falls back to — counts as *behind* any published release.
Two real versions are still compared by semver, and an older tag is never
offered: that is a re-tagged or yanked release, and a silent downgrade is worse
than doing nothing.

### The notes are converted, not copied

The release body is GitHub-flavoured Markdown and the Home tab is Slack mrkdwn.
It goes through `internal/slackmd` (§7d) — the same converter the card bodies
use, reused rather than re-implemented — so headings flatten to a bold line,
`**bold**` becomes `*bold*` rather than an italic run, a fence loses its language
tag, and links become `text [N]`.

Footnotes **are** appended here and deliberately are not on a digest row. Release
notes are read on purpose, and a note whose every "see the PR" has been flattened
to "see [4]" with no list to resolve it against has lost the thing it pointed at.
A two-line card excerpt is read at a glance and would be buried by the same list.
The release's own page is appended last, because the one link a reader reliably
wants is never in the body.

### Publishing

`app_home_opened` arrives over the **Events API**, not as an interaction, so the
daemon's `Listener` delivers into a `Handlers` struct with a field for each. They
are genuinely different kinds: a click is routed by `(action_id, intent)`, while
this is a user simply looking at the app, with no control and no message behind
it. The same event also fires for the *messages* tab, which is a DM being read
and has nothing to publish.

The rendered view is fingerprinted and a publish that would change nothing is
skipped. `app_home_opened` fires on every glance at the app, and republishing an
identical view is a Slack call bought for nothing. `Publish` reports which
happened, so the daemon's log can say so — "app home published" on a call that
was skipped is worse than no line at all.

### The controls menu

The version line is a `section` with an `overflow` accessory (`app_menu`), whose
one option today is **Restart** (`restart`).

It sits on the version line rather than below the divider with the update,
because it is not *about* a release: there is something to restart whether or
not there is anything to install. The two gates differ accordingly — the update
section needs a release AND an installer; the menu needs only the admin gate and
a supervisor to restart through. With no supervisor wired the menu is not drawn
at all, which is the same rule as everywhere else on this surface: never render
a control that cannot act.

The restart handler re-checks the admin gate, and reports the outcome BEFORE
asking launchd — afterwards there is nobody left to say it. The one failure
worth narrating is launchd declining, because that means the daemon is *not*
coming back on its own and the admin is otherwise left watching a tab that will
never change.

The menu is `app_menu` rather than a second `home_*` id because it is Riggs'
own controls, as opposed to the Update button, which belongs to a release. New
operations go in here as options; a bare token value each, so the routing table
keeps matching them exactly.

### The jobs

The Jobs section is what replaces going and reading another tool's database to
find out what Riggs is running. A schedule you cannot see is one you assume is
working, and the two jobs Riggs took over were invisible unless you went and
looked somewhere else entirely.

It sits **above** the prompts, because it answers the question somebody opens
this tab to ask. A prompt is read when you are about to change it; a schedule is
read when you are wondering whether anything is running at all.

Structurally it is the prompt rows again: one section per job, the job's name in
the `block_id`, one overflow per row whose option values are bare tokens. That
is not duplication to be factored out — it is the pattern this surface has, and
a shared renderer would need a flag for every place the two diverge, starting
with the confirmation on Delete.

- **Each row carries what it is, what it runs, and how it went**: the name and
  cadence, the command line, and a status line — `✓ ran 2m ago in 1.4s · next in
  58s`, or the failure with its reason. The status is rendered by
  `internal/apphome`, not by `blockkit`: it is arithmetic on a clock, and a
  package that lays out JSON has no business holding one.
- **A running job says so, before anything else on the line.** A job that takes
  minutes is otherwise indistinguishable from one that is not firing — including
  when it has been disabled mid-run, which is why "running" is checked first.
- **A disabled job shows no next-run time**, because there is not one. "Disabled
  · next in 40s" is the kind of detail that makes a reader doubt the whole panel.
- **Delete carries Slack's own confirmation**, on the option itself. It is the
  one control on this surface that destroys something and cannot be undone, an
  overflow gives no second chance of its own, and "I meant to press Disable" is
  one row's distance away. Nothing else is confirmed: a control that asks twice
  is one people learn to click through.
- **Disable keeps the definition and the history.** "Off for now" and "deleted"
  are different intentions, and only one of them is recoverable.
- **Run now takes the same code path a tick does.** A "Run now" that ran things
  differently would be a way to prove the wrong thing works. It redraws the tab
  before running as well as after, because a control that shows nothing for two
  minutes reads as one that did nothing.
- **New job… sits on the controls menu, directly under Restart.** It has nowhere
  else to live: there is no row to hang it off when there are no jobs yet, which
  is exactly when somebody goes looking for it.
- **An empty schedule renders an empty STATE, not an empty section.** "Nothing is
  scheduled" is a fact worth saying, and it points at the control that fixes it;
  a bare header reads as a section that failed to load. A build with no ledger at
  all draws no section, which is a different fact again.

### The prompts

Riggs sends four pieces of prose on the admin's behalf: the wording of each ask
(§7bb) and the instruction handed to the harness for each Run. All four are
editable here, admin-only like everything else past the divider.

They live on the Home tab rather than in the config file alone because a prompt
is judged by its output. The install is not when anybody knows what it should
say; the moment after reading a review that missed the point is. Making that a
file edit and a daemon restart means it does not happen.

- **One row per prompt, each with its own overflow.** Which prompt a click is
  about rides in the row's `block_id`, exactly as a pull request rides in a
  digest row's — an overflow click reports its own block_id and not its
  siblings' values (§7b), so it is the only place a per-row identity can travel.
  The options are bare tokens (`edit`, `reset`) that the router matches exactly.
- **Reset is only drawn on a prompt that has an override.** There is nothing to
  reset otherwise, and an option that does nothing is the mistake this surface
  keeps not making. A prompt running on its default says so, because a default
  and an override that happens to match it are otherwise indistinguishable —
  and only one of them follows a later change to the default.
- **The row shows the wording in force**, cut at 220 runes and escaped like a
  digest row: a prompt is prose somebody typed, and an `&` in it would re-open
  the row's own bold run and garble everything after it.
- **The registry is `config.Prompts`**, not a table in this package. A second
  description of the config — ids, labels, defaults, getters, YAML paths — is
  one that has to be kept in step, and nothing would warn when it drifted.

### The editor

`views.open` is the one modal Riggs opens, and a view submission is the one
inbound callback that is not a click. Both go through the machinery that already
exists rather than beside it:

- **A submission is routed by the same table.** Its callback_id becomes the
  `action_id` and its `private_metadata` becomes the item, which is exactly the
  split a click already makes. It is the same kind of thing — a control Riggs
  rendered, operated by a human, delivered to the app that drew it — and that it
  arrived from a modal rather than a message changes where it came from, not what
  dispatching it means. `internal/daemon` needed no new event path at all.
- **The prompt id is in `private_metadata`, not the callback_id.** The router
  matches the callback_id exactly, and a table cannot match a value that varies
  per prompt — the same constraint that keeps every option value a bare token.
- **The trigger id lives about three seconds.** The edit handler opens the modal
  and does nothing else first. That is also why the socket listener acknowledges
  before it dispatches (§7b): a modal opened after a ledger read is a modal that
  does not open, and the only symptom is `expired_trigger_id` in a log nobody is
  reading.
- **The input is required**, which is Slack's default and is left that way. An
  empty submission would have to mean either "reset" or "a prompt that says
  nothing", and Reset is already an option on the row — so Slack refuses the
  empty box before it reaches the handler, and the handler refuses it again.
- **A submission has no channel**, so a failure is DMed rather than posted: the
  click reporter reads an empty channel as "this person, privately", which for a
  modal is the only place left to reach them.

### Writing a prompt back

`config.SetPrompt` is the only path that modifies the config file, and both of
its rules come from the file it is editing.

**Nothing is marshalled.** The file holds `${ENV}` *references* and the loaded
`Config` holds their expanded values, so writing the struct back over the file
would put live bot tokens into it, in plain text, as a side effect of somebody
rewording a prompt.

**The edit is textual, guided by the parser.** A `yaml.Node` round-trip keeps the
comments but silently drops the blank lines between sections, reflowing a
carefully laid-out file a little further on every save. So the parser locates the
value and its `Line`/`Column` drive a replacement of those lines alone: the
touched span changes and the rest of the file is byte-identical. An entry's
extent is its own line plus every following line indented deeper than its key,
which is the extent of a block scalar, a nested mapping or a wrapped plain scalar
without having to tell them apart.

The rest follows from that:

- **The value is always double-quoted and always on one line.** A prompt can hold
  a colon, a leading brace, a `#`, or a newline from the multiline input, and
  every one of those changes what a plain scalar means. Quoting unconditionally
  means the writer never has to be right about which of them needed it.
- **Reset deletes the key** rather than writing the default's own words, so
  "never overridden" stays distinguishable from "overridden to whatever the
  default said that day" — and a later change to the default reaches the machine.
- **A missing key is inserted as the section's first**, not appended after its
  last: appending would have to decide whether a trailing comment block belongs
  to this section or heads the next one, and that is not decidable from the text.
  A missing section is appended whole; a section declared with nothing under it
  has its own line rewritten, because a second `ai:` would make the file
  unloadable.
- **The result is re-parsed before it is kept**, then written through a temporary
  file in the same directory at mode 0600 and renamed. A bug in the surgery is
  caught here rather than at the next start-up, by which time the daemon has
  exited and the file it cannot read is the only copy.
- **The in-memory value is updated only after the file takes it**, so the daemon
  never acts on a prompt that vanishes at the next restart. That in-memory write
  is why `Config` has a mutex: those four fields are the only ones that change
  after load, and it covers them and nothing else.

### The Update button

Its `action_id`/value pair is `home_update`/`update` — **bare tokens**, like every
other control Riggs renders, so the daemon's routing table can match them exactly
(§7b). The release tag is deliberately *not* the value. It would make the value
vary, which a table cannot match on, and a Home tab published on Tuesday and
clicked on Friday would install Tuesday's idea of the latest release. The handler
re-resolves what to install at click time.

The admin gate is re-checked in the handler. The button is only ever rendered for
the admin, but an `action_id` and a value are just strings in a payload, and this
one replaces a binary.

The swap is **staged, verified, then renamed**:

1. the asset is written beside the target and made executable;
2. it is run once (`riggs version`) to prove it is a working Riggs — a truncated
   download, an HTML error page or the wrong architecture all fail here, with the
   original still in place and the daemon still running it;
3. the old binary is **copied** aside as `.backup` (not renamed — renaming the
   running binary out of the way is legal on Unix and leaves the daemon running
   from a path that no longer holds what it says it does);
4. the new file is renamed over it, within one directory, so there is no window
   in which the path does not resolve.

A symlinked binary has its **target** replaced; renaming over the link turns it
into a regular file and silently strands anything else pointing at the target.

Then `launchctl kickstart -k` — not "exit and trust `KeepAlive`", even though the
plist sets it. Exiting is a request that the agent be restarted *if* someone is
watching; a Riggs started by hand for an afternoon's debugging would simply
vanish. Asking launchd directly also means it can *say* when launchd is not in
charge, and the admin is told to restart it themselves rather than waiting for a
daemon that is never coming back.

The outcome is DMed to the admin, because the interesting half of it lands after
this process has gone. On the success path the tab is deliberately **not**
republished: the view this process would draw is the old version's, and the
restarted daemon publishes the new one on the next open.

### Slack app configuration

The Home tab is not code alone. The app at api.slack.com needs **App Home → Home
Tab enabled** and an **`app_home_opened` event subscription**, plus the
`views.publish` capability the bot token already carries. Without them the daemon
runs perfectly and the tab stays empty, with nothing in the log to say why —
because the event never arrives.

## 8. GitHub access

Riggs talks to GitHub's **REST** API over its own HTTP client, with ETag
conditional requests. It does not shell `gh` for data.

It uses GraphQL for exactly **one field**: `reviewDecision`. REST does not
expose it, and it cannot be reconstructed from the review list — two attempts
to derive it were wrong in opposite directions, and the parity check (§11)
caught both against live data:

| Shape | GitHub says |
| --- | --- |
| one approval from another reviewer, branch unprotected, we are still requested | `APPROVED` |
| the same, plus an older dismissed review | `null` |

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
- **Discovery excludes archived repositories** (`archived:false`). Archiving a
  repository makes it read-only: GitHub locks every pull request in it and
  answers a review with `422 lock prevents review`, so nothing there could ever
  be acted on. The query is only half of it — `scope()` also carries tracked
  pull requests, so `Detail.Archived` (read free from `base.repo.archived`)
  collapses a card that was adopted before the repository was archived.
  The case that prompted this: an archived repository holding eleven green
  Dependabot pull requests, every one of them permanently unapprovable.
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
- **There is no idle nudge.** The Python re-pinged the admin on cards that had
  sat unclaimed past a 24h threshold, on a weekday cron. It is retired (§8c):
  re-pinging an unclaimed ticket does not make anyone pick it up, it only makes
  the queue louder the longer it goes unread. A ticket claimed outside Slack
  still gets its card collapsed — that was the nudge's other job, and the poll
  already did it on the same pass.
- **Dry runs never summarise.** Shelling `claude -p` per ticket turned a
  preview into minutes of work for a value nobody acts on; the title stands in.
  The same applies to the pull-request loop.

## 8c. The idle nudge, and why it is gone

The Python re-pinged the admin in-thread on any ticket card that had sat
unclaimed past a 24h threshold, on a weekday 09/12/14/17 cron. Riggs carried it
over verbatim during the migration (phase 4) and has now retired it.

A reminder that fires on a timer rather than on a change tells the reader
nothing they did not already know: the ticket is still there, which is exactly
what the card already says. What it does do is make the queue louder the longer
it goes unread, which trains the reader to skip it — and a queue you have learnt
to skip is worse than one that says its piece once.

The nudge's *other* job survives. It re-checked Jira before pinging, so a ticket
quietly claimed since the last poll got its card collapsed rather than a bogus
reminder. `Poll` already does that on the same pass, for every tracked card, and
now carries the test that used to live on the nudge.

What went with it:

- `jira.tickets.nudge` and `ticket.Engine.Nudge`, and the
  `quick-coding-tasks-nudge` job the installer registered.
- `notify.MinGap` — the rate-limited latch policy. The nudge was its only user,
  so `Latch` is now just a name and `latchOpen` is "has it fired".
- The import's nudge-clock carry-over. It existed so cutover would not ping
  every stale card at once; with nothing to ping, `last_nudge_ts` is read by
  nobody.

**Operator note:** the installer no longer *registers* the job, which does
nothing about the one already in Murtaugh's config. It is still there, and once
this build is installed it will fire at a tool the binary no longer exposes —
four times a weekday, failing silently into the job log. Remove it as part of
deploying this:

```sh
murtaugh cfg job delete --name quick-coding-tasks-nudge
```

## 8d. The ticket digest

`jira.tickets.bulk` mirrors the same query as a bulk digest instead of a card
each, on the shared rotation engine (§9b). The card loop is untouched and still
reachable; it is simply no longer what the schedule runs.

The domain supplies two things and nothing else: which tickets a pass is
responsible for, and what a row says.

Rules:

- **Candidates are (matches the query) ∪ (already in the digest).** A tracked
  ticket that has fallen out of the query has been handled by somebody, so its
  row is struck through rather than quietly dropped — the reader was shown it
  advertised and is owed the outcome.
- **A ticket that cannot be READ is left exactly as it is**, on §8b's rule. It
  is omitted from the pass entirely, so the engine redraws it from what the
  ledger last stored rather than from a stub.
- **FIFO is by `created`**, which is why the Jira read now asks for that field.
  Ordering by `updated` would put a ticket somebody edited this morning behind
  one raised last month, which is the opposite of a queue.
- **The row names the REPORTER**, not an assignee: an unclaimed ticket has no
  assignee, and the reporter is who you go to about scope.
- **The cooldown is a rolling three hours**, the same as the pull-request
  digest. The specification asked for "no more than once per period, in 6h
  blocks"; a rolling window rather than calendar blocks, for the reason §9b
  already gives — a block boundary makes the gap between two announcements
  anything from a minute to a whole block depending on where in it the ticket
  appeared, and nobody reading the channel can tell which they got. It remains
  its own constant: the two queues are tuned independently and happen to agree.
- **Nothing replaces the idle nudge, and nothing needs to.** §8c retired it on
  its own argument, before this existed. What the digest adds is not a louder
  reminder but a *quieter* one: an unclaimed ticket rejoins a fresh message once
  its cooldown expires, which says the same thing the nudge did without a second
  message under the first.
- **The digest must be posted through Riggs' own Slack profile.** A click is
  delivered to the app that posted the message, so a digest sent as Murtaugh —
  which is how the ticket cards are posted today — renders a menu Riggs' daemon
  never hears about. The installer asks for the profile for exactly this reason.

## 9. The notification ledger

Every notification is stateful. A threaded reply can only go onto a message
that was already posted, so "post" and "update" and "thread" are one mechanism,
not three programs.

`internal/notify` owns a ledger keyed by `<tool>:<identity>` (e.g.
`git.pr:acme/monolith#20069`), storing `{profile, channel, ts,
fingerprint, latches, updated_at}`. Two operations cover both automations:

- **`Upsert(key, card)`** — post if the key is new; update **only** if the
  card's fingerprint changed; re-post and reset latches if the stored `ts` went
  stale (the message was deleted).
- **`Thread(key, msg, latch)`** — post into the stored message's thread, or
  no-op if there is no such message. One latch policy is left:
  *once-per-episode* (the review queue's `tagged` flag, reset on leaving the
  reviewable state). A rate-limited *min-gap* latch existed for the nudge's 1h
  floor and went with it — a one-valued policy enum explaining itself is worse
  than no enum.

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

## 9c. The schedule

`internal/schedule` is Riggs' own cron. It replaces Murtaugh's, which is the
last half of the "always the callee" invariant §1 opened with: Phase 6 took the
inbound half when Riggs got its own Slack app, and this takes the rest.

The schedule lives **inside the daemon**. That is the decision everything else
follows from, and it was chosen against the obvious alternative — one launchd
agent or systemd timer per job — for reasons that are all about the seam
between Riggs and an init system:

- **The calendar dialects do not map.** launchd offers `StartInterval` (an
  integer of seconds) or `StartCalendarInterval` (a dict with no ranges and no
  steps: you cannot write `*/5`, you enumerate all twelve values). systemd
  offers `OnCalendar`/`OnUnitActiveSec`, a third syntax again. A cron expression
  translates cleanly into neither, so per-job units would mean owning a
  translation layer with real semantic holes.
- **systemd user units stop at logout** unless lingering is enabled, and
  enabling it needs root. The failure is the worst kind: works all afternoon,
  gone by morning, nothing in the log.
- **Not every Linux has systemd.** Alpine, WSL1, plenty of containers. "Out of
  the box on Linux" cannot rest on it.
- **The daemon has to be up anyway.** If it is down, every button in every digest
  is dead and the jobs not firing is the least of the problem. So the one real
  cost of an in-process scheduler — jobs pause when the daemon does — is a cost
  that was already paid.

What is left is one supervised process on each platform (§12b), and above it a
single code path that is identical on both.

### What a job is

A name, an argument list, a schedule, a timeout and an enabled flag — stored in
the **ledger**, not in `config.yaml`. Half of every record is what *happened*
(when it last ran, for how long, whether it worked, what it said when it did
not), and that has no place in a hand-edited file whose comments are the reason
it can be fixed. The definition and its outcome are one row because the Home tab
draws them as one line.

- **A job runs THIS binary again, as a child process.** Not an in-process call
  through the tool registry, which would be cheaper. The argv is byte-identical
  to what Murtaugh ran, so the migration changes when a job fires and nothing
  about what it does; a job that hangs or crashes cannot take the daemon with
  it; and a timeout is a signal to a process rather than a goroutine politely
  noticing a cancelled context, which a blocked syscall never does. The tools
  also assume they own the process — they open the ledger, do one thing and
  exit — and running twenty of them inside a long-lived daemon is a different
  set of assumptions from the ones they were written under.
- **The child inherits the daemon's environment verbatim**, which is
  load-bearing: that environment is the one the supervisor gave it, captured
  PATH and resolved `env-file` included (§12b). A job started with less would
  fail on `gh` not being found, having worked perfectly when run by hand.
- **`--config-file` is passed only when the config is not where Riggs would look
  anyway**, the same rule the installer followed.

### One field, two dialects

`3m` is an interval; `0 9 * * 1-5` is a calendar expression. They are told apart
by shape rather than by a second setting, because a job has one schedule and a
form with "kind" and "value" invites the pair where kind says interval and value
holds a cron expression. Murtaugh spelled these as two flags and its jobs only
ever used the first.

The cron parser is hand-written. The supported grammar — `*`, `n`, `a-b`, `*/s`,
`a-b/s` and comma-separated lists — is the whole of what anybody writes, and
small enough to be read and tested in one sitting. Names (`MON`, `JAN`) are
deliberately absent: they are the half of cron syntax that varies between
implementations, and a job that runs on the wrong day because two parsers
disagreed about `SUN` is precisely the bug this must not have. The
day-of-month/day-of-week **union** IS reproduced, wart and all, because somebody
pasting an expression out of a crontab has to get what the crontab did.

Calendar expressions are evaluated in **local time**: "09:00 on weekdays" means
the operator's morning, and a job that drifts an hour twice a year because it
was pinned to UTC is one nobody trusts.

### The loop

A fifteen-second tick, against a minimum interval and a calendar resolution of a
minute. It bounds how late a job can be, and costs one read of a table with a
handful of rows.

- **Missed runs are SKIPPED, not caught up.** Due times are held in memory, not
  stored, which is what makes that true by construction: a daemon that was down
  over nine o'clock comes back and waits for tomorrow rather than firing a
  morning report in the afternoon. The digest jobs lose nothing by it — they are
  governed by a three-hour cooldown (§9b), not by their tick.
- **An interval job is due immediately on first sight; a calendar job waits.**
  `every 3m` after a restart means the digest should be current now. `0 9 * * *`
  said when it wants to run, and a restart at 14:00 is not it.
- **A job that overruns its own cadence is skipped, not doubled up.** Two passes
  of the same digest race each other to write the same ledger rows. The claim
  lives in the scheduler, which is why it is the one long-lived thing here: a
  runner rebuilt per click could not tell that a job is already running.
- **There is no global concurrency cap.** Different jobs run at once, which is
  what a busy morning looks like.
- **A panicking job does not take the daemon down.** It is the first thing in
  this process that runs unattended, and there is nobody to notice.
- **Only the last run is kept.** A full history belongs in a log; the question
  this table answers is "is this working?", and that is answered by the most
  recent answer to it.
- **An unreadable stored schedule is reported and skipped**, not fatal — the
  alternative is a job that silently never runs.

### The standard jobs

`riggs jobs seed` creates the two jobs `riggs install` has always set up, from
`schedule.Standard` — one declaration, so the installer and the command cannot
drift apart. The names are kept, so a machine that had them under another
scheduler recognises them rather than gaining two lookalikes. An existing job is
left alone: running it twice must not undo an edit made in between.

**It is a seed, not an import, and it was briefly named as though it were one.**
The argument gave that away — a genuine import would already know the GitHub
login, because the login is *inside* the job being imported. Having to pass it
is proof that nothing is being read.

A real import was possible: Murtaugh exposes `cfg job list`, `cfg job show` and
`cfg export`. It was rejected because it would couple Riggs to another tool's
output format at the exact moment the dependency on that tool is being removed —
and because `cfg job list` has no JSON output, so the coupling would be to
human-readable text.

**Neither the seed nor the installer asserts what another scheduler holds.**
Riggs cannot see another tool's configuration, and a warning describing a state
it has not checked is how a tool teaches people to ignore its output. If the
same jobs are defined in two places they will both run, and both will race to
write the same ledger rows; `riggs jobs list` says what Riggs runs, and the
other scheduler says what it runs.

## 9b. The item ledger (bulk digests)

§9's unit is a **card**: one message about one entity, keyed by that entity
forever. A digest breaks that assumption — one message carries many items, and
which items it carries moves underneath it. So `items` records the membership
`cards` never needed:

```
items(key, stream, post_key, position, status, done, posted_at, updated_at)
```

`cards` keeps its job as the *post* table; a digest's row there is keyed
`<stream>:post:<n>`, allocated by `NextPostKey`.

**The rotation is one implementation, in `internal/bulk`.** It began inside
`internal/pullrequest`, as the only digest there was; the ticket queue wanted
the same five rules and none of the same rendering, so the rules moved out and
the rendering stayed. A domain supplies what its items ARE (`Source`) and how
they DRAW (`Renderer`), and nothing between.

That is the opposite call to the one blockkit made about its two card shapes
(§7c), and for the opposite reason. There, two things that *looked* alike had
diverging lifecycles, so they were kept apart. Here, two things that look
nothing alike — a pull request and a Jira ticket — have provably the same
lifecycle: announce, hold, rotate, strike through, purge. Duplicating it would
mean two copies of the one piece of logic in Riggs where an off-by-one silently
loses somebody's row.

`Renderer` is split from `Source` because half the callers have no upstream at
all: completing a row after a click rebuilds the message from the ledger alone,
and must not need a GitHub or Jira client to do it.

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

**Rows render from the ledger.** `items` stores each row's title, author and URL,
not just its status — so a post can be rebuilt with no upstream read at all.
Without that, acting on one row wrecked the others: any row the pass could not
refetch collapsed to its bare reference with a dead link.

That was true of the click path from the start and, until the rotation moved
here, **not** of the reconcile path. One branch still rebuilt a held row as a
bare id: an item the source could not report this pass — a transient 502, a lost
permission, a pull request turned draft — was redrawn without its title or its
link, and then written back, losing the real values permanently for something
that was only briefly unreadable. It now renders from the ledger like every
other row.

**An approval completes the row immediately** (`Completer`). The reconcile pass
would reach the same conclusion on its own, but up to three minutes later, and a
button that visibly does nothing for three minutes reads as one that did not
work. A failure is posted in the **digest's** thread — where the row still sits
waiting, and where somebody looking for the outcome will look — never swallowed.
Neither is allowed to mask the approval itself: a row that fails to redraw is
still an approved pull request.

Rules:

- **Rolling cooldown, not calendar blocks** — 3h on both digests today, but each
  family names its own constant and its own environment override
  (`RIGGS_BULK_MAX_ITEMS`, `RIGGS_JIRA_BULK_MAX_ITEMS`): raising one queue's cap
  says nothing about the other's, and two settings that agree are not one
  setting.
- **FIFO by pull request age** — oldest waiting first, not by when Riggs noticed
  it.
- **The cap holds, it does not drop.** Anything past its cooldown that misses
  the cap stays exactly where it is and leads the queue next pass. A row removed
  with nowhere to go would simply vanish.
- **A done row does not rotate.** It stays struck through where it is until its
  cooldown expires, then it is purged. That is what eventually empties a post —
  and it means a pull request that goes green again comes back as new, which is
  the same non-stickiness §9's cards have.
- **An emptied post is deleted, not blanked** (§7c), **unless somebody replied
  in its thread.** Deleting a Slack message deletes its whole thread with it. An
  emptied digest is tidiness; a colleague's reply under it is work, and tidiness
  loses. Such a post is left in Slack and dropped from the ledger: the row exists
  only to update or delete that message, and this decides we will never do
  either again.
- **Riggs' own replies do not count as a conversation.** It posts into a
  digest's own thread on two paths — narrating an approval, and reporting a
  failed click — so a plain "does this thread have replies" would keep every
  digest that ever saw a click. The check compares each reply against the bot's
  own user id, read once per token from `auth.test`.
- **A thread that cannot be read blocks the delete.** Not knowing whether a
  conversation is there is not permission to destroy one, so the failure is
  reported rather than falling through. `conversations.replies` needs a history
  scope (`channels:history` and its private, DM and group-DM siblings); the app
  already holds all four.
- **A vanished digest stays vanished.** Unlike a card, a digest whose message is
  gone is not re-posted: its items come back on their own cooldown, and
  resurrecting a message the reader dismissed is the wrong answer.
- **A post that cannot be read from GitHub is not treated as deleted.** A failed
  read is skipped and retried; only the cooldown purges.
- **Rebuilding is idempotent.** The fingerprint gate means a pass that changes
  nothing makes no Slack call, which is what keeps a per-minute schedule
  affordable.

## 10. Configuration file

`internal/config` owns the admin identity, the Slack profiles, the two ask
sections and the AI harness. It is loaded once, in the composition root — and,
uniquely among them, its four prompts can be written back (§7e).

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
- **`review-request` and `sme-assistance` are independent, and neither is ever
  defaulted from the other.** They are the same three settings — channel, user,
  prompt — for two actions that look identical and answer different questions:
  one asks a human to review code that exists, the other asks a subject-matter
  expert whether work that does not exist yet is ready to be picked up. In
  practice they are pointed at different channels and different people, so a
  shared setting would mean changing one silently moved the other. The installer
  asks for both, separately, for the same reason.
- **An unset user disables its action; it does not fall back to the admin**
  (§7bb).
- **`ai-assistance` is the retired name for `sme-assistance`,** and is parsed as
  an alias rather than refused. Unlike `admin.github-login` — which was refused
  by name because leaving it in place would silently steer the review queue at
  somebody else — this key means exactly what it always meant, so refusing to
  boot over the spelling would cost a working install for nothing. `riggs
  capabilities` reports the deprecation. A file carrying both spellings keeps the
  new one's values field by field and fills only what it left blank: the other
  reading would let a forgotten old key silently override a deliberate new one.
- **`ai` is one command and two prompts.** One command because it is one harness;
  two prompts because reviewing code that exists and scoping work that does not
  are different instructions, and that is the same reason the two sections above
  are separate. Its `timeout` is validated at load — `20` parses as nothing,
  falls back to fifteen minutes, and the operator who wrote it believes runs are
  capped at twenty.
- **`ai.command` and `ai.workdir` are `${ENV}`-expanded; the prompts are not.** A
  prompt is prose the admin wrote, and a `$` in it is a dollar sign.
- **The four prompts are the only fields that change after load**, written back
  in place by the App Home tab (§7e). They are also the only ones behind the
  `Config` mutex; everything else is written once during `Load` and read for the
  life of the process.

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
- The installer **seeds Riggs' own schedule** (§9c) into the ledger it is about
  to hand the daemon. It used to register the jobs with Murtaugh through its
  CLI; there is no longer a second tool in the chain, and therefore no longer a
  second place for the two of them to disagree about what is running.
- Cadences are carried over unchanged. A migration that also changes the
  schedule makes it impossible to attribute a behaviour difference.
- An **existing job is left alone**. Re-running the installer must not undo a
  schedule somebody has since edited from the Home tab.
- It **says how to retire Murtaugh's copies** and why. Nothing here can remove
  them — that is Murtaugh's config, not ours — and two schedulers driving one
  digest is noise rather than redundancy.
- Each job is asked for a channel and a Slack profile. The profile is not
  cosmetic: a click is delivered to the app that POSTED the message, so a digest
  sent through the wrong one renders a menu the daemon never hears about.
- **The digest job names its GitHub user on the command**, and `admin.github-login`
  no longer exists. The job says who it is for: a config edit cannot repoint the
  queue at a different person, and reading the job answers the question without a
  second lookup. The installer still asks for the handle — it just bakes it into
  the command instead of persisting it. With none given the job is skipped and
  reported, not registered to fail every three minutes.
  A config still carrying the key is **refused at load**, by name, with a message
  saying what to do instead. Refusing rather than ignoring is the point: silently
  dropping a setting someone believes is steering the review queue is the very
  trap the removal closes. The tools have no fallback either — `git.pr.bulk`,
  `git.pr.fetch-reviews` and `git.pr.check-parity` all require the login as an
  argument.
- **The installer collects Riggs' own Slack app**, app-level token included.
  Riggs does not share Murtaugh's app: a click is delivered to whichever app
  posted the message, so the daemon can only ever answer buttons on its own.
  Without an app token the install still completes, and says that the digest's
  buttons will not respond until one exists.
- **The Jira tenant is asked for, never guessed** (§13's removal of the default).
  A config written without it leaves the `jira.*` tools unregistered.
- **Three questions decide the row actions** (§7bb): who assists with pull
  requests, who assists with tickets, and which command runs an AI review. Each
  may be left empty, which turns that option off — there is no fallback, and an
  empty answer writes no section rather than an empty one for somebody to fill in
  and wonder why nothing changed. The two people are resolved to Slack ids here,
  for the reason the reviewer always was: a handle that matches nobody would
  otherwise not be discovered until someone pressed the button.
- **The harness invocation is echoed back** (`will run: claude -p <prompt>`), and
  its binary is probed on PATH. The difference between a known harness and a
  custom one is invisible in the answer just typed and decides whether the prompt
  arrives behind a flag or as a bare argument.
- **The working directory is asked for**, defaulting to `$HOME`. It is what makes
  the feature work at all (§7bb), and a directory that does not exist yet is
  noted rather than refused — it may well be created before the first click.
- **A prompt left at its default is not written.** An answer equal to the default
  is stored as empty, so a later change to the default reaches the machine
  instead of being pinned to whatever it said the day of the install.

### 12c. Decommissioning the card job

The digest and the per-PR cards mirror the same review queue. Running both
means every pull request is announced twice, which is noise rather than
redundancy — so the digest **replaces** the card job rather than joining it.

The mechanism is the job *name*: `github-review-queue` is redefined to run
`git.pr.bulk` at 3m. Redefining replaces the old definition, so there is
nothing left over. Registering the digest under a new name would have left the
card job running, and nothing in the install path removes a job.

**The card renderer is retained.** `blockkit.Card`, `pullrequest.Card` and the
`git.pr.fetch-reviews` tool are untouched and still registered — the shape is
about to be reused for something else. What changed is only that nothing is on
a schedule to drive it; run `riggs git pr --fetch-reviews` and the cards work
exactly as before.

3m rather than 1m because the digest's governing timescale is the 3h cooldown
(§9b). The tick rate only bounds how quickly a new pull request first appears,
and two minutes of latency on a code review is not worth 20 wakeups an hour.
The card loop was at 1m for a different reason — there, posting the card the
moment checks go green *is* the notification.

> **Cards already in Slack stop updating.** They were posted by Murtaugh's app
> and are maintained by its workflow rules, which still work; they simply will
> not collapse to their final state any more. Nothing deletes them.

### 12b. Supervising the daemon

`riggs service <install|uninstall|status|restart>` (in `internal/service`) runs
`riggs daemon` under whichever init this machine has: a **launch agent** on
macOS, labelled `io.riggs.daemon`, or a **systemd user unit** on Linux, called
`riggs-daemon.service`. `riggs launchd` is the former name and still works; it
prints one line on stderr and forwards.

Everything else Riggs does is a one-shot. The daemon is the part that has to
*keep running* — across a crash, a logout and a reboot — and since §9c it also
carries the schedule, so keeping it up stopped being a macOS convenience and
became the one piece of setup the whole design rests on.

`internal/launchd` is unchanged and is wrapped rather than rewritten: the plist
it writes, the bootout-first install, the captured PATH and the XML escaping are
all still exactly right, and reimplementing a working supervisor to fit a new
interface is how a working supervisor stops working.

#### The Linux half

A **user** unit, not a system one. Everything Riggs touches belongs to one
person — their Slack tokens, their `gh` login, their AI harness, their home
directory — and a system unit would run as root and then have to be told how to
become them, which is a lot of ceremony and one more way to end up with a daemon
that cannot read its own config.

The cost of that choice is the one thing about systemd that surprises people:

- **A user unit stops when the user logs out**, unless lingering is enabled.
  `riggs service install` checks `loginctl show-user --property=Linger` and
  prints the exact `sudo loginctl enable-linger` command when it is off. It
  checks rather than fixes: enabling it needs polkit or root, and a tool that
  silently escalated to change a login-manager setting would be doing something
  nobody asked for.
- **A machine with no systemd is refused, not half-installed.** Both
  `/run/systemd/system` and the `systemctl` binary are required — the first
  because systemd may be installed without being PID 1, which is the state
  inside many containers and under WSL1, where every systemctl call fails with a
  message about D-Bus that explains nothing. The refusal says what to do instead.
- **`Restart=always`, not `on-failure`** — the daemon exits *cleanly* when its
  socket closes, exactly the reasoning behind launchd's unconditional
  `KeepAlive`. `RestartSec` is launchd's `ThrottleInterval`.
- **`Environment=PATH`**, for the same reason the plist carries one.
- **Every ExecStart argument is quoted**, unconditionally: systemd splits on
  whitespace, and a home directory with a space in it is unusual and entirely
  legal.
- **`XDG_CONFIG_HOME` is honoured**, because systemd honours it — installing to
  `~/.config` on a machine that has moved it writes a file systemd never reads,
  and the only symptom is a unit that does not exist.
- **`disable --now` before the file is removed.** Disabling a unit systemd can no
  longer read leaves the symlink in `.wants` behind, and every later
  daemon-reload complains about it.

The Home tab's **Restart** control goes through this too, so it works on Linux.
It was launchd-only, which was defensible while the daemon was a macOS
convenience and is not now that it carries the schedule.

Rules:

- **`KeepAlive` is unconditional**, not `SuccessfulExit=false`. The daemon exits
  *cleanly* when its socket closes, so restarting only on failure would leave a
  disconnected daemon down until somebody noticed. `launchctl bootout` still
  stops it.
- **`ThrottleInterval` is set.** An agent that cannot start — bad token, no
  network — otherwise respawns as fast as launchd can fork it.
- **The plist carries a `PATH`.** launchd's default is
  `/usr/bin:/bin:/usr/sbin:/sbin` — no Homebrew, no `~/.local/bin`. Riggs shells
  out to `gh` for its GitHub token, so without this
  the daemon connects perfectly and then fails on the *first click* with
  "executable file not found in $PATH". `install` captures the PATH of the shell
  that ran it, and reports any of those tools it cannot resolve — because the
  alternative is finding out from a log nobody is watching.
- **The plist names the config path explicitly.** A launch agent inherits none
  of the shell's environment, so `$RIGGS_CONFIG` never reaches it and the
  precedence chain (§10) would resolve somewhere else entirely.
- **Install is idempotent**: it boots out any previous incarnation first, so
  re-running after changing the profile or upgrading the binary picks the change
  up instead of leaving the old agent running.
- **The log directory is created.** launchd will not, and a missing one makes
  the agent fail to spawn with a message only `launchctl print` reveals.
- **Values are XML-escaped.** A home directory with an `&` in it is unusual and
  entirely legal, and would otherwise produce a plist launchd silently refuses.
- **macOS is checked at runtime, not by build tag**, so the command exists on
  Linux and explains itself rather than vanishing from the usage line.

#### `env-file`

The same "no inherited environment" problem breaks the tokens: every
`${SLACK_...}` in the config would expand to empty and the daemon would start up
connected to nothing.

So `config.yaml` gains `env-file`, a dotenv file loaded **before** expansion.
It uses the parser Murtaugh already uses, which is the point: one `.env` can
serve both, and quoting behaves the same in each.

Where it looks, first hit wins:

1. `env-file` in the config — an absolute answer, `~` expanded, `${VAR}`
   expanded.
2. `.env` **beside the config file**, which in the default case is
   **`~/.config/riggs/.env`**. The ledger already follows the config file
   (§10); the environment does too, so moving a config moves all of its state.

That default holds even when there is **no config file at all** — the location
one *would* live at still decides, which is the state a fresh machine and
`riggs capabilities` are both in.

- **The file wins over the ambient environment** (`godotenv.Overload`), which
  inverts standard dotenv precedence on purpose. Murtaugh's gateway exports its
  own `SLACK_BOT_TOKEN` into every job it spawns, so under normal precedence the
  *same profile* would resolve to Murtaugh's app when scheduled and to Riggs'
  own when started by launchd — one identity posting the digest, another
  listening for its clicks, failing silently. Riggs' dotenv defines Riggs'
  identity whoever spawned the process. The cost: overriding one variable for a
  single run means editing the file, not exporting it.
- A **missing conventional** file is not an error — Riggs is still invoked from
  Murtaugh with the variables already exported, and refusing to start there
  would be a regression.
- A **named** file that cannot be read **is** an error, the same rule
  `--config-file` follows.
- **The resolved path is reported by `riggs capabilities`**, loaded or not. The
  symptom of the wrong one is an empty token, which surfaces as "profile has no
  bot-token" — a message that says nothing about where the token was looked
  for, and the first question anyone debugging a launch agent asks.

## 13. Roadmap

| Phase | Contents | Status |
| --- | --- | --- |
| 0 | Skeleton, both frontends, config + profiles, `ping`, `capabilities` | done |
| 1 | Slack domain (live client), Block Kit cards, `notify` ledger, `slack.send-msg` | done |
| 2 | GitHub REST client + ETag cache (§8), PR state derivation, `git.pr.fetch-reviews` + parity gate | done |
| 3 | `git.pr.approve` / `--approve-merge` | done |
| 4 | Jira domain, `jira.tickets.*` poll/nudge/assign/dismiss/import | done (nudge retired, phase 25) |
| 5 | Repoint Murtaugh jobs and rules, retire the Python | done (applies on gateway restart) |
| 6 | Riggs' own Slack app: `riggs daemon`, Socket Mode, interaction router (§7b) | done |
| 7 | The bulk digest block (§7c) | done |
| 8 | The item ledger and the digest reconcile loop (§9b) | done |
| 9 | The digest's actions: ask-review, approve-and-merge | done |
| 10 | Supervising the daemon: `riggs launchd`, `env-file` (§12b) | done |
| 11 | Decommission the per-PR card job, keeping the renderer (§12c) | done |
| 12 | Pin the default dotenv location; report it (§12b) | done |
| 13 | `jira.base-url` becomes required configuration; no default tenant | done |
| 14 | Explicit identity: login on the command (setting removed), own Slack app, dotenv wins | done |
| 15 | Give the launch agent a PATH (§12b) | done |
| 16 | Digest polish: own icon, 50/47 title | done |
| 17 | Ask for Code Review posts a card (§7bb) | done |
| 18 | Acknowledge every socket request (§7b) | done |
| 19 | Complete the row on approval; report failures in-thread (§9b) | done |
| 20 | slackmd converter; card bodies from the description (§7d) | done |
| 21 | Ticket bodies too; `internal/ai` decommissioned | done |
| 22 | Track and settle the ask-review card (§7bb) | done |
| 23 | Text-presentation glyphs, enforced by a source scan | done |
| 24 | The App Home tab, versioning, and self-update (§7e) | done |
| 25 | Retire the idle nudge (§8c) | done |
| 26 | The Home tab's controls menu: Restart (§7e) | done |
| 27 | The ticket digest: rotation extracted to `internal/bulk`, `jira.tickets.bulk`, Ask for AI Assistance (§8d) | done |
| 28 | Asking split from running: `internal/ai` revived, `sme-assistance`, editable prompts on the Home tab (§7bb, §7e) | done |
| 29 | Riggs owns the schedule: `internal/schedule` in the daemon, jobs on the Home tab, `riggs service` for launchd and systemd (§9c, §12b) | done |

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

- **unreleased** — Phase 29. Riggs owns its own schedule (§9c). Murtaugh held
  the cron and invoked `riggs git pr --bulk` every three minutes; now a ticker
  inside the daemon does, and Murtaugh stops being a dependency at all — the
  other half of the "always the callee" invariant §1 opened with, the first half
  of which Phase 6 already reversed.

  The schedule lives IN the daemon rather than in one launchd agent or systemd
  timer per job, and that is the whole architectural claim: launchd's
  `StartCalendarInterval` has no ranges and no steps, systemd's `OnCalendar` is a
  third syntax again, a cron expression maps onto neither, systemd user units
  stop at logout without lingering, and some Linuxes have no systemd at all. A
  ticker in a Go process has none of those problems and is the same code on both
  platforms. The one cost — jobs pause when the daemon does — was already paid:
  a daemon that is down has no working buttons either.

  A job runs THIS binary again as a child process, with byte-identical argv to
  what Murtaugh ran, so the migration changes when a job fires and nothing about
  what it does. Missed runs are skipped rather than caught up; a job that
  overruns its cadence is skipped rather than doubled; a panicking one does not
  take the daemon with it. Schedules are one field in two dialects (`3m`, or
  `0 9 * * 1-5`) with a hand-written cron parser that reproduces the
  day-of-month/day-of-week union wart on purpose.

  The Home tab grows a **Jobs** section above the prompts — one row per job with
  its command, its cadence and how the last run went, an overflow carrying Edit,
  Run now, Disable and a confirmed Delete — and **New job…** under Restart on the
  controls menu, opening Riggs' second modal. `riggs jobs` is the terminal half:
  list, add, rm, enable, disable, run and import.

  `riggs launchd` becomes `riggs service` (§12b) and gains a systemd user unit,
  so Linux is supported out of the box: it checks for lingering and prints the
  fix, refuses a machine where systemd is not PID 1 rather than half-installing,
  and quotes every ExecStart argument. The Home tab's Restart goes through it
  too, and so works on Linux for the first time. `riggs launchd` still forwards.

- **unreleased** — Phase 28. Asking somebody and doing the work stop being the
  same option (§7bb). "Ask for AI Assistance" tagged a colleague and started
  nothing, which is the one thing its label promised; it is now "Ask for SME
  Assistance", and **Run AI Assistance** beside it starts a local harness that
  actually does it. The pull-request row gains the same pair. `internal/ai`
  returns — the package Phase 21 retired for summarising cards, now running the
  work itself — with the process seam, the prompt-subject guarantee, the in-place
  status line and the per-item claim.

  Every one of the four verbs is now optional and none of them falls back to the
  admin: an unanswered installer question turns the option off and it is not
  rendered. `RowActions` carries that decision to the digest engine and to the
  completer, so a redraw cannot disagree with the pass that drew it.

  The config gains `ai` (one command, two prompts, a working directory and a
  bound) and renames `ai-assistance` to `sme-assistance`, keeping the old key as
  a working alias — that section always meant a person, so refusing to boot over
  the spelling would cost a working install for nothing. `riggs capabilities`
  reports all four actions, the harness binary, and the deprecation.

  The four prompts become editable from the App Home tab (§7e), which brings
  Riggs' first modal and its first view submission — routed by the *same* table
  as a click, since the callback_id is the control and the private_metadata is
  the item. `config.SetPrompt` writes one value back into the YAML by textual
  surgery guided by the parser: a marshal would put the expanded `${ENV}` tokens
  on disk in plain text, and a `yaml.Node` round-trip would quietly eat the blank
  lines out of a file whose comments are the reason it can be fixed.

- **unreleased** — Phase 27. The ticket queue becomes a bulk digest (§8d).
  The rotation moves out of `internal/pullrequest` into `internal/bulk` (§9b),
  parameterised by a domain's `Source` and `Renderer`; the pull-request digest
  becomes a thin adapter over it and its eighteen tests pass unchanged, which is
  the whole evidence that the move was behaviour-preserving. Adds
  `jira.tickets.bulk`, `ticket.Asker` ("Ask for AI Assistance", §7bb), the
  `ai-assistance` config section (§10), `internal/ask` for the tag both digests
  share, and `created` to the Jira read so FIFO orders by how long a ticket has
  actually waited.

  It also fixes a latent bug the extraction surfaced: a tracked item the source
  could not report on a given pass was redrawn as a bare id — no title, no link
  — and that stub was then written back to the ledger, destroying the real
  values for something that was only briefly unreadable. §9b claimed rows always
  render from the ledger; on that one branch they did not.

  The installer now registers `quick-coding-tasks-poll` against the digest, and
  asks it for a Slack profile: the ticket cards were posted through Murtaugh's
  app, and a digest posted that way would render a menu Riggs' daemon never
  hears about.

- **unreleased** — Phase 26. The Home tab's version line becomes a `section`
  with an `overflow` accessory (`app_menu`), carrying **Restart**. It lives
  above the divider, not with the update, because restarting has nothing to do
  with a release — there is something to restart whether or not anything is out
  of date. Admin-only like everything else that operates Riggs, and not drawn at
  all when no supervisor is wired: a Restart option on a Riggs that cannot
  restart is the same mistake as a disabled button. The outcome is DMed before
  launchd is asked, since afterwards this process is gone.

- **unreleased** — Phase 25. The idle nudge is retired (§8c). A reminder on a
  timer tells the reader nothing the card did not already say, and only makes
  the queue louder the longer it goes unread. `jira.tickets.nudge`,
  `ticket.Engine.Nudge` and the `quick-coding-tasks-nudge` job are gone, along
  with `notify.MinGap` — the nudge was its only user, so `Latch` collapses to a
  name and `latchOpen` to "has it fired". The nudge's other job, collapsing a
  card whose ticket was claimed outside Slack, was always also `Poll`'s, and the
  test moved there. An already-installed nudge job must be removed from
  Murtaugh's config by hand; this build no longer exposes the tool it calls.

- **unreleased** — Phase 24. The App Home tab (§7e). Riggs gains a version
  (`internal/version`, stamped by a tagged release), a release lookup
  (`internal/updates`) and a Home surface (`internal/apphome`) that shows the
  portrait and the running version to everyone, and the latest release's notes
  plus an Update button to the admin alone. The daemon grows its first Events
  API subscription, so `Listener` now delivers into a `Handlers` struct rather
  than a bare interaction callback. `internal/slackmd` is reused for the notes —
  with footnotes, unlike a card excerpt. Two GitHub Actions workflows arrive
  with it: CI (fmt, vet, build, `-race`) and a tag-triggered release that
  publishes the four platform binaries the Update button downloads. Diverges
  from Murtaugh on one rule, deliberately: a `dev` build IS offered the latest
  stable, because the launchd daemon routinely runs one.

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
- **unreleased** — Phase 9. The digest's actions (§7bb): `ask_review` drops a
  message tagging a configured reviewer in a configured channel or DM, and
  `approve_merge` reuses the existing rebase-only approver. Also states the
  Common Rule and fixes the one place that broke it — the GitHub approval body
  said "Approved via Riggs." and now says "Approved."
- **unreleased** — Phase 10. `riggs launchd install|uninstall|status` (§12b)
  supervises the daemon as a macOS launch agent. Adds `env-file` to the config,
  because a launch agent inherits no environment and every `${SLACK_...}` would
  otherwise expand to empty — a daemon connected to nothing.
- **unreleased** — Phase 11. The digest replaces the per-PR card job (§12c) by
  reusing its name, so the review queue has exactly one notifier again. The card
  renderer and `git.pr.fetch-reviews` are retained and still registered — only
  the schedule is gone.
- **unreleased** — Phase 12. Pins the default dotenv location at
  `~/.config/riggs/.env` (`config.DefaultEnvPath`) and reports the resolved path
  from `riggs capabilities`, loaded or not — the failure mode is an empty token,
  whose error message names neither the file nor the directory.
- **unreleased** — Phase 13. `jira.base-url` becomes real configuration. It was
  the one Atlassian setting `expand()` never touched, so a `${VAR}` reference
  reached the HTTP client verbatim and every request went to
  `${ATLASSIAN_BASE_URL}/rest/api/3/...`. It now expands, falls back to
  `$ATLASSIAN_BASE_URL` like the credentials beside it, and must be an absolute
  http(s) URL. **The default tenant is removed**: `jira.DefaultBaseURL` used to
  hard-code an Atlassian instance, so a machine that configured no tenant
  silently talked to whichever one this source named. With none configured the
  `jira.*` tools are now simply absent (§6), like any other capability gap.
- **unreleased** — Phase 14. Four corrections to how identity is supplied.
  The digest job names its GitHub user on the command line, and
  `admin.github-login` is removed outright — a config that still carries it is
  refused at load rather than ignored. The installer collects Riggs' own Slack app including
  the app-level token, and asks for the Jira tenant §13 stopped defaulting.
  And `env-file` now *overrides* the ambient environment, so Riggs resolves to
  the same Slack app whether Murtaugh spawned it or launchd did.
- **unreleased** — Phase 15. The launch agent gets a `PATH`. Phase 10 solved
  the environment problem for *tokens* (`env-file`) and missed *executables*:
  the daemon could not find `gh`, so every click that needed GitHub failed with
  "executable file not found in $PATH" while the connection looked healthy.
  `launchd install` now bakes the installing shell's PATH into the plist and
  warns about any tool it cannot resolve.
- **unreleased** — Phase 17. "Ask for Code Review" becomes a card (§7bb): the
  legacy container shape minus the overflow, with an Approve that leaves no
  comment on GitHub, and the reviewer tagged in the card's own thread with the
  requester copied in. `review-request.user-id` accepts a handle and resolves it
  against the workspace; the installer collects and resolves it at setup.
- **unreleased** — Phase 16. Digest polish: its own icon const, and row titles
  cut at 50 runes to 47 plus an ellipsis.
- **unreleased** — Phase 18. The daemon acknowledges every socket request, not
  just the interactive ones it recognises (§7b). A link button raises an
  interaction Riggs does nothing with, and dropping it unacked put a ⚠ on the
  control; the same missing branch meant the click was never logged either.
  Adds `$RIGGS_LOG_LEVEL`, and an `action_id` on link buttons so their clicks
  are identifiable rather than blank.
- **unreleased** — Phase 19. An approval completes its digest row at once
  (§9b), and a failed one is reported in the digest's thread. `items` gains the
  row's title, author and URL so a post rebuilds from the ledger alone — which
  also fixes a latent bug: a row the pass could not refetch used to collapse to
  its bare reference with a dead link.
- **unreleased** — Phase 20. `internal/slackmd` (§7d): a shared GitHub-Markdown
  to Slack-mrkdwn converter. The pull-request card body becomes the first two
  paragraphs of the description, converted — replacing the `claude -p` summary,
  which cost ~8.6s on the click path, bound the card to a local binary, and was
  non-deterministic.
- **unreleased** — Phase 22. The ask-review card is tracked in the ledger, and
  an approval collapses it and removes its Approve button (§7bb).
- **unreleased** — Phase 21. Ticket card bodies come from the description too,
  and **`internal/ai` is deleted** — nothing in Riggs shells out to an LLM any
  more. `claude` leaves the launch agent's PATH requirement and the
  capabilities report with it.
- **unreleased** — Phase 23. Status markers become text-presentation glyphs
  (§7bb). `emoji: false` never governed this: `⏺` rendered as an image on a
  block that already had the flag set. Enforced by a source scan, since the
  right and wrong characters are indistinguishable in an editor.
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
