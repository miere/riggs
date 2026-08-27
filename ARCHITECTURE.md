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
| `quick_coding_tasks/main.py poll` | job, every 3m | `jira.tickets.poll` |
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
  notify/                        # the card ledger (§9)
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

## 7bb. The digest's actions

Three options render on a live row; two of them are answered by the daemon.

| Option | Intent | Handler |
| --- | --- | --- |
| ⧉ Open on Browser | `open_browser` | none — the option carries a `url` and Slack opens it |
| ✎ Ask for Code Review | `ask_review` | `pullrequest.Asker` |
| ✓ Approve and Merge | `approve_merge` | `pullrequest.Approver`, rebase-only (§8) |

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
- **The digest has its own icon const**, not the legacy card's. Sharing it would
  mean a change to one silently re-rendering every card of the other — the same
  reason the option type is duplicated.
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

The view, top to bottom: the portrait, the running version with a controls
menu beside it, and then — behind a divider — the latest release's notes with an
**Update** button beside them.

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

## 9. The notification ledger

Every notification is stateful. A threaded reply can only go onto a message
that was already posted, so "post" and "update" and "thread" are one mechanism,
not three programs.

`internal/notify` owns a ledger keyed by `<tool>:<identity>` (e.g.
`git.pr:UpsideRealty/upside#20069`), storing `{profile, channel, ts,
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

**Rows render from the ledger.** `items` stores each row's title, author and URL,
not just its status — so a post can be rebuilt with no upstream read at all.
Without that, acting on one row wrecked the others: any row the pass could not
refetch collapsed to its bare reference with a dead link.

**An approval completes the row immediately** (`Completer`). The reconcile pass
would reach the same conclusion on its own, but up to three minutes later, and a
button that visibly does nothing for three minutes reads as one that did not
work. A failure is posted in the **digest's** thread — where the row still sits
waiting, and where somebody looking for the outcome will look — never swallowed.
Neither is allowed to mask the approval itself: a row that fails to redraw is
still an approved pull request.

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
- Ticket job cadences are carried over unchanged (3m and the weekday cron). A
  migration that also changes the schedule makes it impossible to attribute a
  behaviour difference. The review job is the one exception, and it is not a
  migration any more — see §12c.
- A job whose tool this build does not expose is **skipped and reported**, not
  installed. Registering `jira.tickets` before phase 4 would mean a scheduled
  failure every three minutes.
- The job passes `--config-file` only when the config is not where Riggs would
  look anyway, so the common case stays readable.
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

`riggs launchd <install|uninstall|status>` (in `internal/launchd`) runs
`riggs daemon` as a macOS launch agent, labelled `io.riggs.daemon`.

Everything else Riggs does is a one-shot Murtaugh starts and waits for. The
daemon is the first part that has to *keep running* — across a crash, a logout
and a reboot — and nothing in the design had an opinion about that.

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
