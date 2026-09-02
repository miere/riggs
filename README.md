# riggs

A single Go binary that carries Murtaugh's automations: mirroring GitHub review
requests into Slack, approving and merging PRs from a button, running a local AI
review on demand, and advertising `ai-able` Jira tickets. It replaces the Python
layer under `~/.config/murtaugh/automations/`.

Every digest row offers up to four verbs, and the split between them is the
point: **Ask for Code Review** and **Ask for SME Assistance** tag a person and
stop, while **Run Code Review** and **Run AI Assistance** start a harness on this
machine that does the work. Each is optional, and an option whose setting is
absent is not rendered at all.

Riggs runs its own schedule and its own Slack app. `riggs daemon` holds a Socket
Mode connection open, answers the clicks on the messages it posted, and ticks the
jobs it owns — so Murtaugh is no longer in the chain.

See [ARCHITECTURE.md](ARCHITECTURE.md) for the structural decisions.

## Install

```sh
go build -o ~/.local/bin/riggs ./cmd/riggs
```

## Supervise it

The daemon has to keep running: it holds the Slack connection AND the schedule,
so a Riggs that is down has neither working buttons nor firing jobs.

```sh
riggs service install
```

One command, both platforms — a launch agent on macOS, a systemd **user** unit on
Linux. It reports the two things that otherwise fail silently: tools missing from
the supervised PATH, and systemd lingering being off, which stops a user unit the
moment you log out (`sudo loginctl enable-linger $USER`). `riggs service status`
says what the supervisor thinks; `riggs launchd` is the old name and forwards.

## Jobs

Riggs schedules its own work. Jobs live in the ledger, are edited from the **App
Home tab** — a Jobs section with Edit, Run now, Disable and Delete on each row,
and **New job…** under Restart in the controls menu — and from the terminal:

```sh
riggs jobs list
riggs jobs add review-queue 3m git pr --bulk <github-login>
riggs jobs add nightly "0 9 * * 1-5" jira tickets --bulk
riggs jobs run nightly
```

A schedule is one field in either of two dialects: an interval (`3m`) or a
five-field calendar expression (`0 9 * * 1-5`, local time). A job runs the riggs
binary again as a child process, so what it does is exactly what typing the same
command would do. Missed runs are skipped rather than caught up, and a job that
overruns its own cadence is skipped rather than started twice.

Riggs ships with **nothing scheduled**: neither the installer nor any command
creates a job for you. If the same job is also defined in another scheduler,
remove it there — both would run, racing each other to write the same rows.

## Configure

The quickest route is the installer:

```sh
riggs install
```

It asks where the config should live, collects the credentials without echoing
them, asks who assists with pull requests, who assists with tickets and which
command runs an AI review, and sends a **real** test card for a **real** pull
request to your Slack DM — failing the install if that does not work.

It creates no jobs. The schedule is yours to fill in (see **Jobs** below).

Every one of those three answers may be left empty, which turns the corresponding
option off rather than falling back to something.

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

It names the exact setting or binary behind anything that is disabled, including
each of the four row actions and the AI harness it would shell out to. A missing
credential disables a feature; it never stops the binary.

## Prompts

The four wordings Riggs sends on your behalf — the two asks and the two harness
instructions — are editable from the **App Home tab**, admin only. Each has its
own row with an Edit control, and a Reset that drops the override so the built-in
default applies again. An edit is written back to `config.yaml` in place, keeping
its comments and its `${ENV}` references, and takes effect on the next click
rather than the next restart.

## Use

Riggs schedules two things, so it has two commands:

```sh
riggs git pr --bulk <github-login>                  # the pull-request digest
riggs jira tickets --bulk "project = NYX AND ..."   # the ticket digest
```

Both take the same optional flags:

- `--slack-profile <name>` — which Slack account. Defaults to the profile named
  `default`; if that is not configured, the call fails rather than silently not
  notifying.
- `--slack-channel <id>` — where. Absent, the notification is a DM to
  `admin.slack-user-id`.
- `--dry-run` — report what would change without sending anything.
- `--max-items <n>`, `--cooldown <duration>` — the digest's size and rolling
  window.
- `--json-output` — machine-readable output, anywhere on the line.

**These spellings are a contract.** Stored jobs invoke them as literal argv and
nothing validates that before the process starts, so renaming one would fail in
a job rather than at build time.

Everything else operates this machine rather than doing work on a schedule:

```sh
riggs capabilities                                  # what's enabled, what's missing
riggs jobs list                                     # the schedule
riggs service status                                # the supervisor
riggs daemon                                        # the Slack connection + scheduler
riggs install                                       # provision
riggs version
riggs --config-file /path/to/other.yaml capabilities   # anywhere on the line
```

## Develop

```sh
go test ./...
go vet ./...
```

Anything that touches an external process or SDK sits behind an injected seam,
so the tests make no live calls and do not depend on what is installed on the
machine running them.
