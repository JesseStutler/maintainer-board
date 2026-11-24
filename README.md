# Maintainer Board

A self-hosted kanban board for open-source maintainer work. It sits on top of
a local agent-orchestrator (`ao`) daemon and the `gh` CLI, and brings three
things into one page:

- every Copilot and Codex session that AO manages, as a card with an embedded
  chat,
- the pull requests and issues you are reviewing, with incremental update
  tracking ("what changed since I last looked, and how many times"),
- your own notes, summaries, tags and workflow columns.

It is one Go binary with the web UI embedded. There is no Node or npm build
step, no external service, and AO is used through its HTTP API without being
forked or modified.

## Contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Access through an SSH tunnel](#access-through-an-ssh-tunnel)
- [Command line flags](#command-line-flags)
- [How update tracking works](#how-update-tracking-works)
- [Columns and automation roles](#columns-and-automation-roles)
- [Agent sessions](#agent-sessions)
- [Summaries](#summaries)
- [Architecture](#architecture)
- [HTTP API](#http-api)
- [Running in the background](#running-in-the-background)
- [Security](#security)
- [Development and tests](#development-and-tests)
- [Troubleshooting](#troubleshooting)
- [Known limitations](#known-limitations)

## Features

**Board**

- Customizable columns: add, rename, recolor, reorder and delete in the
  Columns dialog. Six defaults: Inbox, In Progress, Waiting, Updates, Ready,
  Done.
- Drag and drop across and within columns, with a drop indicator. The card
  drawer also has a column dropdown for keyboard use.
- Cards carry a category (PR Review, Issue, Dev, Design, Triage), free-form
  tags, a summary, Markdown notes and an audit timeline.
- Search (title, summary, notes, `#number`, tags, session ID), category and
  agent filters, `Updates Only`, `Show Done`, `Show Orchestrator`.
- Dark and light themes. English UI; user content in any language renders with
  CJK font fallbacks.

**GitHub tracking**

- Link a card with `#6247`, `6247`, `owner/repo#123` or a pull request or
  issue URL. Whether the number is a PR or an issue is detected through the
  API.
- A background sync (every 120 s by default) compares the live state with the
  baseline you last acknowledged and reports new commits, force-pushes, new
  comments and reviews, CI results, review decisions and state changes.
- Cards move automatically: new activity goes to **Updates**, approved PRs
  with green CI go to **Ready**, merged or closed items go to **Done**.
- `Mark Caught Up` and `Reviewed -> Waiting` acknowledge the current state in
  one click.
- The browser tab title shows `(N) Maintainer Board` while N cards have unread
  updates. Optional browser notifications (off until you enable them).

**Agent sessions**

- Every live AO worker session gets a card automatically. A `#number` in the
  session name is linked to the default repository.
- The drawer shows the conversation (Markdown, code blocks with a copy
  button, tables), polls while it is open, and lets you send messages, stop a
  running turn, resume an imported session and terminate a session.
- Start a new Copilot or Codex session from any card, or link an existing one.
- Import historical Copilot and Codex sessions through AO, with search and
  filters.

## Requirements

- Go 1.26 (to build).
- A running AO daemon, by default at `http://127.0.0.1:3001`.
- `gh`, authenticated (`gh auth status`), for GitHub tracking.
- Optional: `copilot` and/or `codex` on `PATH` for `Summarize with AI`.

The board keeps working when AO or GitHub is unreachable: it reports the
outage in the header and retries.

## Quick start

```sh
cd /root/tools/maintainer-board
./run.sh
```

`run.sh` builds the binary when sources changed and starts it with the
defaults. The equivalent manual steps:

```sh
go build -o maintainer-board ./cmd/maintainer-board
./maintainer-board \
  --addr 127.0.0.1:3012 \
  --db /root/tools/maintainer-board/data/board.db \
  --ao-url http://127.0.0.1:3001 \
  --default-repo volcano-sh/volcano \
  --sync-interval 120s
```

Open <http://127.0.0.1:3012>. On first start the board creates the database,
the six default columns, and one card per live AO worker session.

## Access through an SSH tunnel

The server listens on loopback only. From a Windows or other client machine,
forward the port:

```sh
ssh -L 3012:127.0.0.1:3012 user@linux-host
```

Then open <http://127.0.0.1:3012> in the local browser. Use `127.0.0.1` or
`localhost` as the host name: other host names are rejected (see
[Security](#security)). To keep the tunnel in your SSH config:

```
Host linux-host
    LocalForward 3012 127.0.0.1:3012
```

## Command line flags

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | `127.0.0.1:3012` | Listen address. |
| `--db` | `/root/tools/maintainer-board/data/board.db` | SQLite database path. The directory is created if needed. |
| `--ao-url` | `http://127.0.0.1:3001` | Base URL of the AO daemon. |
| `--ao-project` | first project AO reports | AO project used when a session is started from the board. |
| `--default-repo` | `volcano-sh/volcano` | Repository for bare `#123` references. |
| `--sync-interval` | `120s` | GitHub background sync period (minimum 10 s). |
| `--ao-sync-interval` | `5s` | AO session sync period. AO events trigger an immediate sync in between. |
| `--gh-path` | `gh` | Path of the `gh` CLI. |
| `--ci-ignore-contexts` | `tide` | Comma-separated status contexts and check names left out of the CI rollup. |
| `--prow-approval-labels` | `true` | Treat the `lgtm` + `approved` label pair as an approval. |
| `--allowed-hosts` | none | Extra `Host` header values to accept besides loopback. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |

## How update tracking works

Each linked card stores three snapshots of its pull request or issue:

- **live** (`gh*` fields): what the last sync saw,
- **acknowledged** (`ack*` fields): what you last marked as reviewed,
- **previous poll**: what the sync before saw.

A snapshot is the state (`OPEN`, `DRAFT`, `MERGED`, `CLOSED`), the CI rollup,
the review decision, the head SHA and the commit, comment and review comment
counts.

On every sync the live snapshot is compared with the acknowledged one:

| Change since the baseline | Shown as |
| --- | --- |
| More commits | `+2 commits` |
| Same commit count, different head SHA | `Force-pushed` |
| More comments, inline review comments or review summaries | `+3 comments` |
| CI settled on a result different from the baseline | `CI passed` / `CI failed` |
| Review decision changed | `Approved`, `Changes requested`, `Approval dismissed`, `Review required` |
| Draft toggled, or reopened | `Ready for review`, `Converted to draft`, `Reopened` |

If anything changed, the card is flagged unread, `unreadUpdates` is the number
of new commits plus new comments (at least 1), and `updateSummary` joins the
parts, for example `+1 commit, +3 comments`.

`updateCycles` answers "how many separate times was this updated since I last
looked": it goes up only when a poll differs from the previous poll, so
re-polling the same state never double-counts, while two separate pushes show
as `Updated 2x`.

Automatic moves (when `Auto-move` is on for the card) happen only on a new
cycle:

1. Merged or closed: move to the column with the **Done** role and clear the
   unread markers. Later comments on a finished item do not bring it back; a
   reopen does.
2. Otherwise, approved, CI green, not a draft and no unresolved review
   threads: move to **Ready**.
3. Otherwise, if the card is in a column with the Inbox, In Progress, Waiting
   or Ready role: move to **Updates**.

Details worth knowing:

- Linking records the current state as the baseline and never moves the card.
- Because only a new cycle moves a card, you can drag an unread card out of
  Updates (for example to In Progress while you re-review) and it stays there
  until something new happens.
- Cards in a column without a role are never moved automatically.
- CI counts as an update only when it settles on success or failure. A
  re-run that is still pending, or one that ends with the same result, is not
  news.
- `tide` (Prow's always-pending merge status) is left out of the CI rollup;
  change this with `--ci-ignore-contexts`.
- With `--prow-approval-labels` (default), a PR carrying both `lgtm` and
  `approved` counts as approved even when GitHub's own review decision still
  says review required.
- The background sync skips merged pull requests and re-checks closed items
  every 30 minutes. `Sync GitHub` and a card's `Sync Now` always fetch.

`Mark Caught Up` copies the live snapshot into the acknowledged one and
clears `hasUnreadUpdates`, `unreadUpdates`, `updateCycles` and
`updateSummary`. `Reviewed -> Waiting` does the same and moves the card to
the column with the Waiting role.

## Columns and automation roles

A column has a title, an accent color and an optional role. Roles are how
automation finds its columns, so you can rename or reorder columns freely:

| Role | Meaning |
| --- | --- |
| `inbox` | New cards land here, including cards created from AO sessions. |
| `in_progress`, `waiting` | Cards here move to Updates when new activity arrives. |
| `updates` | Target for cards with new GitHub activity. |
| `ready` | Target for approved pull requests with green CI. |
| `done` | Target for merged and closed items. |
| none | A manual column. Its cards are never moved automatically. |

Each role belongs to one column at a time; assigning it elsewhere takes it
from the previous column. Deleting a column moves its cards to the Inbox-role
column, or to the first remaining column. The last column cannot be deleted.

## Agent sessions

- The board polls `GET /api/v1/sessions` every 5 seconds and also follows
  AO's event stream, so session state changes appear within a moment.
- A card is created for each live session of kind `worker`. Orchestrator
  sessions are skipped unless `Show Orchestrator` is on or you link one
  manually.
- The card title follows AO renames until you edit the title yourself.
- Deleting a card, or unlinking its session, tells the board not to create a
  new card for that session. Link it again from another card whenever you
  want.
- Imported sessions (`needsResume`) show their history immediately. The
  agent has to be resumed before it accepts messages. If AO answers
  `SESSION_OPEN_ELSEWHERE`, close the session in the other terminal and
  resume again.
- The conversation is polled every 2.5 seconds while the agent is working
  and every 8 seconds while it is idle, only while the drawer is open.
- This is the same session AO desktop shows: messages sent from either side
  appear in both.

## Summaries

- **AUTO**: extracted from the latest substantial assistant message of the
  session, stripped of Markdown, at most about 180 characters. It costs no
  tokens and refreshes when the session sees new activity.
- **AI**: `Summarize with AI` sends the recent turns and the linked pull
  request or issue status to the local `copilot` CLI (`copilot -s
  --no-ask-user -p ...`, 45 s timeout), falling back to `codex exec`. The
  result is two lines, `Objective:` and `Status:`. The CLIs run in an empty
  scratch directory with shell and write tools denied (Copilot) or a
  read-only sandbox (Codex).
- **MANUAL**: anything you type. Manual and AI summaries are never
  overwritten by extraction; clearing the summary hands control back to AUTO.

## Architecture

```
 Browser (vanilla ES modules, no build step)
     |  JSON over HTTP + server-sent events
     v
 maintainer-board (single Go binary)
     |-- internal/server   HTTP API, embedded UI, sync loops, SSE hub
     |-- internal/db       SQLite: columns, cards, card_events, settings
     |-- internal/ao       typed client for the AO daemon API
     |-- internal/github   gh wrapper, update detection, auto-move rules
     |-- internal/summary  summary extraction and the AI summarizer
     |-- web/              index.html, styles.css, *.js (go:embed)
     |
     |-- HTTP --> AO daemon (http://127.0.0.1:3001)
     |-- exec --> gh (pull requests and issues)
     '-- exec --> copilot / codex (AI summaries only)
```

Layout:

```
cmd/maintainer-board/main.go   flags, wiring, graceful shutdown
internal/db/                   schema, migrations, CRUD, ordering
internal/ao/                   AO client: sessions, conversation, spawn, import, events
internal/github/               reference parsing, gh client, decision engine
internal/summary/              extraction, prompt, copilot/codex runner
internal/server/               handlers, AO sync, GitHub sync, security middleware
web/                           the single-page UI
scripts/                       optional browser tests (headless Chromium)
```

State lives in one SQLite file (`modernc.org/sqlite`, no cgo). The UI keeps
only view preferences (theme, filters, drawer width, notification toggle) in
`localStorage`.

AO endpoints used: `GET/POST /api/v1/sessions`, `PATCH /api/v1/sessions/{id}`,
`GET /api/v1/sessions/{id}/conversation`,
`POST .../conversation/messages`, `POST .../conversation/interrupt`,
`POST .../resume-agent`, `POST .../kill`, `GET /api/v1/projects`,
`GET/POST /api/v1/session-import`, `GET /api/v1/events`.

`gh` commands used: `gh api repos/{repo}/issues/{n}` (PR or issue),
`gh pr view {n} --repo {repo} --json ...`, `gh api repos/{repo}/pulls/{n}`
(exact counts), `gh issue view {n} --repo {repo} --json ...`, and
`gh api graphql` for unresolved review threads, only for PRs that are
approved with green CI.

## HTTP API

All bodies are JSON. Errors have the shape
`{"error": {"code": "...", "message": "..."}}`. Field names are camelCase.

| Method and path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check. |
| `GET /api/board` | Columns, cards (with their AO session), AO and GitHub status, settings, unread count. |
| `GET /api/events` | Server-sent events; a `board` event after every change. |
| `PATCH /api/settings` | `{"showOrchestrator": true}` |
| `GET /api/columns`, `POST /api/columns` | List; create `{"title", "color", "role"}`. |
| `PATCH /api/columns/{id}`, `DELETE /api/columns/{id}` | Update title, color, role; delete. |
| `POST /api/columns/reorder` | `{"ids": ["inbox", "..."]}` left to right. |
| `GET /api/cards`, `POST /api/cards` | List; create `{"title", "category", "columnId", "tags", "notes", "summary", "githubRef", "aoSessionId", "agent": {"harness", "prompt", "projectId"}}`. |
| `GET /api/cards/{id}`, `PATCH /api/cards/{id}`, `DELETE /api/cards/{id}` | Read; update `title`, `category`, `tags`, `summary`, `notes`, `autoMove`, `columnId`, `aoSessionId`; delete. |
| `PATCH /api/cards/{id}/move` | `{"columnId", "position"}` (0-based). |
| `GET /api/cards/{id}/events` | Audit timeline, newest first. |
| `POST /api/cards/{id}/link-github` | `{"ref": "#6247"}` |
| `POST /api/cards/{id}/unlink-github` | Remove the link and tracking state. |
| `POST /api/cards/{id}/sync-github` | Fetch now; returns the card, `newUpdate`, `moved`. |
| `POST /api/cards/{id}/ack` | Mark caught up; optional `{"moveToColumnId": "waiting"}`. |
| `POST /api/cards/{id}/summarize` | AI summary; returns the card, `summary`, `engine`. |
| `POST /api/cards/{id}/session` | Start an agent session: `{"harness": "copilot" or "codex", "prompt", "projectId"}`. |
| `POST /api/github/sync` | Sync every linked card; returns a report. |
| `GET /api/sessions` | AO sessions with the ID of their card, if any. |
| `GET /api/sessions/{id}/conversation` | `?limit=60&beforeSequence=N` |
| `POST /api/sessions/{id}/messages` | `{"text", "clientMessageId"}`, answers 202. |
| `POST /api/sessions/{id}/interrupt`, `/resume`, `/kill` | Session control. AO error codes such as `SESSION_OPEN_ELSEWHERE` are passed through. |
| `PATCH /api/sessions/{id}` | Rename in AO: `{"displayName"}`. |
| `GET /api/import/candidates`, `POST /api/import` | List history; import `{"ids": [...]}` and create cards. |

Example:

```sh
curl -s http://127.0.0.1:3012/api/board | head -c 400
curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"title": "Review CSI change", "githubRef": "#6247", "columnId": "waiting"}' \
  http://127.0.0.1:3012/api/cards
```

## Running in the background

With systemd (user service), create
`~/.config/systemd/user/maintainer-board.service`:

```ini
[Unit]
Description=Maintainer Board
After=network.target

[Service]
WorkingDirectory=/root/tools/maintainer-board
ExecStart=/root/tools/maintainer-board/maintainer-board --addr 127.0.0.1:3012 --db /root/tools/maintainer-board/data/board.db --ao-url http://127.0.0.1:3001 --default-repo volcano-sh/volcano --sync-interval 120s
Environment=PATH=/root/.local/bin:/usr/local/bin:/usr/bin:/bin
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now maintainer-board
journalctl --user -u maintainer-board -f
```

Without systemd:

```sh
cd /root/tools/maintainer-board
nohup ./run.sh > data/maintainer-board.log 2>&1 &
```

The process shuts down cleanly on `SIGINT` and `SIGTERM`. The database is
safe to copy while the server runs (`sqlite3 data/board.db ".backup backup.db"`).

## Security

The board can start agents and send them prompts, so it is built to be
reachable only by you:

- It binds to loopback by default. Do not expose it on a network interface
  without an authenticating reverse proxy in front.
- Requests with a `Host` header other than `127.0.0.1`, `localhost` or
  `[::1]` are rejected, which blocks DNS rebinding. Add names with
  `--allowed-hosts` if you front it with a proxy.
- State-changing requests must be same-origin and carry
  `Content-Type: application/json`, which blocks cross-site form posts.
- Pages are served with a strict Content Security Policy (same-origin
  scripts and styles only, no inline code).
- Conversation text, titles and notes are rendered as text nodes; Markdown
  links are limited to `http`, `https` and `mailto`. Images are not loaded.
- `gh`, `copilot` and `codex` are executed without a shell; references are
  validated before they reach `gh`.

## Development and tests

```sh
go vet ./...
go test -race ./...            # unit and handler tests
node --test web/tests/         # Markdown parser tests (needs Node 18+, optional)
```

The Go tests cover the schema and ordering, reference parsing, the update
detection and auto-move state machine, the `gh` output parsing, the AO client
against a fake daemon, every HTTP handler against fake AO and GitHub
backends, and a guard that keeps emoji out of the repository.

Two optional browser tests drive a headless Chromium through the DevTools
protocol (no npm packages; set `CHROMIUM` if the binary is not
`chromium-browser`):

```sh
node scripts/ui-smoke.mjs http://127.0.0.1:3012        # read-only walkthrough, screenshots
node scripts/ui-flow.mjs  http://127.0.0.1:3913        # creates and deletes cards and columns
```

`ui-flow.mjs` modifies the board, so run it against a scratch instance:

```sh
./maintainer-board --addr 127.0.0.1:3913 --db data/scratch/board.db
```

Neither script starts, messages, resumes or terminates an AO session.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Header shows `AO Offline` | Is the daemon up? `curl -s http://127.0.0.1:3001/api/v1/projects`. The board reconnects on its own. |
| Header shows `Server Offline` | The board process or the SSH tunnel is down. |
| `403 FORBIDDEN_HOST` | Open the board as `127.0.0.1` or `localhost`, or add the name with `--allowed-hosts`. |
| A card says `Last sync failed` | `gh auth status`, network access to github.com, and API rate limits. |
| CI never turns green | A permanently pending status context; add it to `--ci-ignore-contexts`. |
| `Summarize with AI` fails | `copilot` or `codex` must be on `PATH` and logged in. The error names the CLI that failed. |
| A deleted card's session should come back | Create a task and choose `Existing Session`, or link it from a card's Agent Session section. |
| Resume answers "open in another terminal" | Exit the agent in that terminal (or in AO desktop), then resume again. |

## Known limitations

- Tool approval prompts of an agent cannot be answered from the board; use
  AO desktop for those. Plain questions can be answered by sending a message.
- Review comment counts come from GitHub's totals. An edited comment is not
  detected as new activity.
- Each `Summarize with AI` run through Copilot is stored by the Copilot CLI
  as a session of its own. The board hides those runs in its Import History
  list; other tools will still show them.
- One board per database file. There are no user accounts; anyone who can
  reach the port has full access.
