# Activity, usage and cost

Pilothouse reads the transcripts Claude Code and Codex already write, and
OpenCode's database, so the web app, the phone and the CLI can show what a
session is doing, how many tokens it used and what that would cost at API
prices. Nothing is sent anywhere: transcripts are
read on this machine, by the daemon, and only what you open is shown.

- [What is read](#what-is-read)
- [The activity feed](#the-activity-feed)
- [The session report](#the-session-report)
- [Catch me up](#catch-me-up)
- [Usage and cost](#usage-and-cost)
- [Plans](#plans)
- [Daily budget](#daily-budget)
- [Insights](#insights)
- [CLI](#cli)
- [Limits](#limits)

## What is read

| Agent | Transcript | Found by |
|-------|------------|----------|
| Claude Code | `~/.claude/projects/<project>/<session>.jsonl`, its subagents in `<session>/subagents/agent-*.jsonl` and its workflow agents in `<session>/subagents/workflows/<run>/agent-*.jsonl` (also under `$CLAUDE_CONFIG_DIR/projects`) | the `transcript_path` its hooks send, else the session id |
| Codex | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-*-<thread>.jsonl` (default `~/.codex/sessions`) | the `transcript_path` its hooks send; Codex sends none until the file exists, so the daemon then looks for the rollout by thread id |
| OpenCode | its SQLite database, `~/.local/share/opencode/opencode.db` (`$OPENCODE_DB`, `$OPENCODE_DATA_HOME`, `$XDG_DATA_HOME`), read-only: the session's messages and those of the subagent sessions it started | the session id its plugin sends |

The transcript path comes from a hook payload, which the agent controls. So it
is trusted only after it resolves, symlinks included, to a **regular `.jsonl`
file inside that agent's own folder**. Anything else (a file elsewhere, a
symlink out, a FIFO or device, another agent's folder) is never opened, and the
session page says why. Both formats are undocumented and change between agent
versions: records Pilothouse doesn't know are skipped, never fatal.

Reads are incremental (each file's offset is remembered) and bounded:

- A file seen for the first time is parsed in full only for its last 64 MB.
  Before that, a cheap pass counts usage and turns only (up to 1 GB more), so
  totals stay complete for very long sessions. Past that, the session page says
  the transcript was read part way.
- A single line over 8 MB (huge tool output) is skipped without being buffered.
- A session's feed keeps its newest 1,000 items; prompt and reply text are
  capped at 2 KB and tool output at its last 1 KB.
- The 64 most recently written subagent transcripts of a Claude Code session
  add their steps to the feed. Older ones, and workflow agents, only count
  usage, up to 4,096 files a session.
- A transcript that is truncated, replaced or rewritten starts the feed over.
- Compressed (`.jsonl.zst`) Codex rollouts, which Codex makes of old sessions,
  are not read.

## The activity feed

Each session's feed lists, oldest first:

| kind | what |
|------|------|
| `prompt` | what you typed (`human`), a reply sent through Pilothouse (`pilothouse`), or a message from another Claude session (`peer`) |
| `text` | what the agent wrote to you |
| `tool` | a tool call, summarised like approval cards ("Run: npm test", "Edit: src/app.ts"), with the end of its output and whether it failed; `pending` while it runs |
| `subagent` | a subagent the agent started; the subagent's own steps follow as sidechain items |
| `error` | an API error worth seeing on its own |
| `system` | compaction, the end of a turn, an interrupt |

Files changed counts lines added and removed per file from the edits the agent
made (Claude Code's Edit, MultiEdit and Write; Codex's `apply_patch`), using
the agent's own report of the change when it logs one. A refused or failed edit
doesn't count. Changes made through shell commands (`sed -i`, a formatter)
aren't seen.

The feed is read with a cursor: the first read returns the newest items (200
by default, up to 1,000), later reads return only items added or changed since
— a tool call shows up again when its result arrives. After the daemon restarts
or a transcript is rewritten, an old cursor gets the whole feed again.

## The session report

Opening a session in the web app shows a report of its latest turn, read off
the same feed. Nothing in it is written by a model; it is counts, commands
and numbers the transcript already holds.

- **You asked**: the turn's prompt, marked when it came from Pilothouse or
  from another agent.
- **\<Agent\> says**: what the agent last wrote, folded when it is long.
- **Did it check its work?**: a plain verdict on the turn, then the last run of
  each check (tests, lint, build) behind it, with how many tests passed when the
  output says. *Checked after the last edit* means everything that ran passed on
  the latest code. Otherwise it names what failed, says the code changed after
  the last check, says *Not checked* when the turn changed files and ran nothing,
  or says *Result not shown* when Pilothouse can't tell. When the agent has
  stopped and something is missing, a button drafts a reply asking it to fix or
  re-check; you read it and send it yourself.

  Agents often trim a check's output (`go test ./... | tail -5`), and then the
  exit status is `tail`'s, not the test run's. So Pilothouse reads the run's own
  output instead, and a run it can't confirm is never shown as a pass. Any
  failure in a command counts, even when a later run in the same command passed,
  so an agent that breaks code on purpose to prove a test catches it shows as
  failed until it runs the tests again. Edits made by shell commands such as
  `sed -i` aren't seen as edits, so a check after one of those can still read as
  *after the last edit*.

  Also the files changed (+N/−M), the commit the turn made, and calls that were
  blocked or failed. Commands are matched on what actually runs, so
  `echo "npm test"` or a heredoc mentioning a test runner is not a test run. A
  turn that only searched and read says so.
- **How it got there**: the turn step by step in plain words, with reads
  folded together and subagents grouped; earlier turns below it.
- **Changes**: every file changed with its diff.

The API returns this as `receipt` on `GET /api/sessions/{id}/activity`
(fields in `internal/api/v02.go`). Reading it with `peek=1` (the Today list
does) doesn't count as you having looked at the session.

## Catch me up

"Catch me up" (`pilothouse activity <id> --catch-up`) summarises a session since you last opened it, in short
sentences, most important first, for example:

```
Waiting for you: Run: git push origin main
Hit 2 errors (last: Run: npm run build)
Tests: npm test passed (last run 2 min ago)
Edited 5 files (+120/−30)
Ran 14 tool calls
Asked you 1 question
Needed your approval 2 times
Last said: “The login bug was a stale session cookie; fixed and tested.”
```

The same feed always gives the same sentences. "Since you last looked" is when
a person last opened the session's feed (on any device or `pilothouse
activity`); a view that stays open keeps its summary. After a daemon restart
the first look summarises the whole feed.

## Usage and cost

Tokens come from the transcripts: Claude Code logs each response's usage
(repeated on every content block, so Pilothouse counts each response once);
Codex logs per-response token counts. Usage is kept per session, local day and
model, includes subagents, and is stored in the database, so it survives the
agent cleaning up old transcripts. Input never includes cached tokens: cache
reads and writes are counted on their own (OpenAI counts cached tokens inside
input; Pilothouse moves them out).

**Cost is what the tokens would cost at public pay-per-token API prices**,
standard speed, USD per million tokens. It is not what a subscription such as
Claude Max charges: a plan covers this usage for a flat price, so list it under
[Plans](#plans) to see both. Credits, discounts and negotiated rates are not
known to Pilothouse. OpenCode records a cost for each message itself, from its
own prices for the provider; Pilothouse uses that. Built-in prices:

- **Claude** — Anthropic API list prices (as of 2026-10-06): Fable 5.1 and
  Fable 5 $10 / $50, Opus 5.5 $4 / $20, Opus 5 and Opus 4.5–4.8 $5 / $25,
  Sonnet 5.5 and Sonnet 5 $2 / $10, Sonnet 4.x $3 / $15, Haiku 5.5 $0.10 /
  $0.50 ($0.50 / $2.50 above 100K prompt tokens), Haiku 4.5 $1 / $5 (input /
  output). Cache writes cost 1.25× input (5-minute) or 2× (1-hour); cache reads
  0.1× input, except Fable 5.1 ($0.25) and Opus 5.5 ($0.20). Fast mode is
  counted at 2×.
- **Codex** — the models on Codex's token rate card (as read on 2026-10-23),
  converted from credits at $0.04 per credit: GPT-6 Astra, GPT-6.1 Sol, GPT-6
  Sol, GPT-6 Luna, GPT-5.6 Sol, Terra and Luna, Daybreak Blue and Red. The rate
  card prices ChatGPT-plan usage; if you use Codex with an API key, check
  OpenAI's API prices and override them.

Model ids are matched with their date or provider spelling
(`claude-haiku-4-5-20251001`, `claude-opus-4-8[1m]`,
`us.anthropic.claude-opus-5-5-v1:0`), but a built-in price never applies to a
newer model that merely starts the same way: an unknown model costs $0 and its
tokens are still counted. Add or replace prices in `config.yaml`; a key also
covers ids it is a prefix of (`my-model` covers `my-model-q4`, `gpt-7*` covers
`gpt-7-anything`):

```yaml
usage:
  prices:
    claude-opus-5-5: { input: 4, output: 20, cache_read: 0.2, cache_write: 5 }
    my-local-model:  { input: 0, output: 0 }
```

Each live session's usage refreshes every 10 seconds and is stored with the
session (so `pilothouse ls --json`, the TUI and the phone see it), at most every
30 seconds per session.

## Plans

```yaml
usage:
  plans:
    - name: Claude Max        # anything you like, up to 60 characters
      agent: claude           # claude, codex, opencode or grok
      monthly_usd: 200
    - name: OpenCode Go
      agent: opencode
      monthly_usd: 10
```

Each plan covers one agent. Insights and `pilothouse usage` then show what you
pay a month, that price over the range shown (a month is 365.25 / 12 days),
and next to each plan what its agent used at API prices, so you can see how
much a flat plan covers. The breakdown by agent names each agent's plan.
Pilothouse doesn't know a plan's usage limits. After changing plans, restart
the daemon (`pilothouse service restart`, or `pilothouse stop` and
`pilothouse start`).

## Daily budget

```yaml
usage:
  daily_budget_usd: 20
```

The budget is in API-price dollars, so on a flat plan it works as an alarm for
heavy use rather than a spending limit. When today's estimated spend (local
time, all sessions) reaches 80 % and again at 100 % of the budget, Pilothouse sends one notification each, to your desktop
and devices, once per day — also across restarts. It never stops an agent.

## Insights

The insights page and `pilothouse usage` cover the last N days (7 by default,
up to 90):

- Cost at API prices and tokens per day, and by agent, model and repo, with
  session counts.
- Your plans: what you pay, and what each plan's agent used at API prices.
- Today's spend against the budget, and the burn rate over the last hour.
- What the rules did: calls **auto-allowed** by rules and "allow for session"
  grants (grants also counted on their own), requests that **asked** a person,
  calls **blocked** by rules, answers from a **device** (phone, Telegram) or at
  the **terminal** (the agent's own dialog, the CLI, the TUI), requests that
  **expired**, the **median time to answer**, the **minutes agents waited** on a
  person, the rules that decided most and the commands blocked most.

Rule statistics come from the event log, which keeps decisions for
`retention` (30 days by default): over longer ranges they cover only the
retained part. Usage is kept indefinitely.

## CLI

```sh
pilothouse activity s_3fq9                 # the newest 50 items, usage and files changed
pilothouse activity s_3fq9 -f              # keep printing; running calls get a "finished" line
pilothouse activity s_3fq9 --catch-up      # start with the catch-up summary
pilothouse activity s_3fq9 -n 200 --json
pilothouse usage                           # last 7 days
pilothouse usage --days 30 --json
```

## Limits

- Estimates, not invoices: API list prices only (OpenCode: its own), and
  Codex prices are derived from its credit rate card.
- Usage is counted while the daemon runs. On start it also counts sessions
  that ended in the last week (up to 100); sessions older than that, or never
  seen by Pilothouse, aren't counted.
- Turns are prompts the model answered (a slash command that reaches no model
  isn't one).
- The transcript folders are those of the daemon's environment. If you set
  `CLAUDE_CONFIG_DIR` or `CODEX_HOME` only in your shell, the daemon (started
  by launchd or systemd) doesn't see it and won't read transcripts there.
- Claude Code's transcript format is undocumented; a new version can change
  what the feed shows until Pilothouse catches up. Usage counting depends only
  on the per-response `usage` block, which has been stable.
