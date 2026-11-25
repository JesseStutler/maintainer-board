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
- [The card face](#the-card-face)
- [How update tracking works](#how-update-tracking-works)
- [Your own review status](#your-own-review-status)
- [Search](#search)
- [Columns and automation roles](#columns-and-automation-roles)
- [Agent sessions](#agent-sessions)
- [The card workspace](#the-card-workspace)
- [Models](#models)
- [Summaries](#summaries)
- [Import History](#import-history)
- [Deleting cards and sessions](#deleting-cards-and-sessions)
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
- The card face shows what matters without opening it: the pull request or
  issue with its state and author, the review decision, your own review, CI,
  activity counts, the unread delta, and the agent with its live state. See
  [The card face](#the-card-face).
- Search across card fields, AO session IDs and the text of session
  conversations, plus an `AI Search` by meaning. See [Search](#search).
- Category, agent and author filters (`All Authors`, `Mine`, each author on
  the board), `Updates Only`, `Show Done`, `Show Orchestrator`. All filters
  combine.
- Dark and light themes, sized to be read without browser zoom (14px base).
  English UI; user content in any language renders with CJK font fallbacks.

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
- Your own review is tracked separately from the overall decision: a card
  says `You: Approved` or `Re-review needed` instead of a bare
  `Review Required` after you already looked. See
  [Your own review status](#your-own-review-status).
- The browser tab title shows `(N) Maintainer Board` while N cards have unread
  updates. Optional browser notifications (off until you enable them).

**Agent sessions**

- Every live AO worker session gets a card automatically. A `#number` in the
  session name is linked to the default repository.
- The card workspace shows the conversation (Markdown, code blocks with a
  copy button, tables), polls while it is open, and lets you send messages,
  stop a running turn, resume an imported session and terminate a session.
  It is a resizable side drawer that expands to a full-width, two-pane
  workspace. See [The card workspace](#the-card-workspace).
- Start a new Copilot or Codex session from any card, or link an existing one.
- Choose the model and reasoning effort when starting a session, and switch
  them for a running one. See [Models](#models).
- Import historical Copilot and Codex sessions through AO. The history is
  searchable by session ID (native UUID or AO ID), by title, folder and
  branch, by what was said in the conversation, and by meaning with
  `AI Search`. See [Import History](#import-history).
- Deleting a card asks whether its AO session should be deleted from Agent
  Orchestrator too or kept there. See
  [Deleting cards and sessions](#deleting-cards-and-sessions).
- Slow operations (resume, AI summary, AI search, sending, sync, delete)
  show a spinner with a label on their button and a status row with the
  elapsed time where the result will appear.

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
| `--gh-user` | the account `gh` is logged in as | Your GitHub login, for the `MINE` tag, the `Mine` filter and your own review status. Detected with `gh api user`; when that fails at startup the board runs without it and asks again before every GitHub sync. |
| `--summary-model` | `gemini-3.8-flash` | Model the Copilot CLI uses for `Summarize with AI` and both AI searches. Empty uses the CLI's default model. |
| `--summary-effort` | `low` | Reasoning effort passed with `--summary-model` (`--reasoning-effort`). Empty passes none. A model that rejects the effort is asked again without it. |
| `--copilot-path` | `copilot` | Path of the `copilot` CLI used by the AI features. |
| `--codex-path` | `codex` | Path of the `codex` CLI, the last fallback of the AI features. |
| `--history-home` | your home directory | Home directory whose `.copilot` and `.codex` session stores enrich and search Import History. They are only read. |
| `--ci-ignore-contexts` | `tide` | Comma-separated status contexts and check names left out of the CI rollup. |
| `--prow-approval-labels` | `true` | Treat the `lgtm` + `approved` label pair as an approval. |
| `--allowed-hosts` | none | Extra `Host` header values to accept besides loopback. |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error`. |

## The card face

From top to bottom:

1. **Top row**: category badge, then one pill with the linked item and its
   state (`PR #6247 | OPEN`, `Issue #6100 | CLOSED`), then the author
   (`@login`). Items you opened carry a `MINE` tag.
2. **Title** and a two-line **summary**.
3. **Status strip** (linked cards only):
   - the review decision of the pull request: `Review Required`, `Approved`
     or `Changes Req`;
   - your own review, in teal when it is current (`You: Approved`,
     `You: LGTM`, `You: Changes Req`, `You: Commented`, `You: Reviewed`), in
     amber when the item changed afterwards (`Re-review needed`), or a muted
     `Not reviewed` on an open pull request of somebody else;
   - CI: `CI Passing`, `CI Failing` or `CI Pending`;
   - activity counts such as `4 commits / 7 comments`.
4. **Unread bar**, when there is activity you have not acknowledged:
   `+2 commits, +1 comment (2x)`, where `(2x)` is the number of separate
   updates.
5. **Tags**.
6. **Footer**: the agent with its brand mark (Copilot or Codex), name and
   live state (`Working`, `Idle`, `Needs Input`, `Resume needed`), and the
   time of the last activity.

The AO session ID (`standalone-1`, `volcano-2`) is not printed on the card.
It is in the tooltip of the agent badge and in the header of the card
workspace, and it is searchable.

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

## Your own review status

The review decision GitHub reports is about the pull request as a whole. The
board also tracks where you stand, in three card fields:

| Field | Meaning |
| --- | --- |
| `ghMyReviewState` | `APPROVED`, `LGTM`, `CHANGES_REQUESTED`, `COMMENTED` (read from GitHub), `REVIEWED` (marked on the board), or empty. |
| `ghMyReviewedAt` | Time of your latest review, Prow command or mark. |
| `ghIsStaleReview` | True when the item changed after that time. |

How the state is read from GitHub on every sync:

- A submitted review counts as GitHub counts it: your latest `APPROVED` or
  `CHANGES_REQUESTED` review stays in force until a later one replaces it or
  it is dismissed; a plain review comment does not cancel an approval.
- Prow commands count too, in a comment or in a review body, when they stand
  alone on a line: `/approve` gives `APPROVED`, `/lgtm` gives `LGTM`, and
  `/lgtm cancel`, `/remove-lgtm`, `/approve cancel` and `/remove-approve`
  take them back.
- Any other review activity of yours is `COMMENTED`. Plain conversation
  comments without a command are not a review.
- Nothing is recorded on items you opened yourself.

`Mark Reviewed` in the GitHub section of a card (or `Mark Caught Up` when
there are unread updates) records a review you did without leaving a trace on
GitHub, for example after reading the change locally, or for an issue. With
no review on GitHub the state becomes `REVIEWED`; with one, its state is kept
and only the reference time moves to now. `Clear` removes a `REVIEWED` mark.

The review is **stale** (`Re-review needed`) when, after `ghMyReviewedAt`,

- the head commit moved past the one you reviewed (new commits or a
  force-push; for a Prow command, a commit dated later), or
- somebody else commented or submitted a review.

Your own later comments, CI results and comments of bots (accounts ending in
`-bot`, `-robot` or `[bot]`, `app/...` accounts, and a few known names such as
`github-actions` and `dependabot`) never make it stale. Merged and closed
items are never stale.

## Search

The search box filters the board as you type. Every word has to match, in
any of these places:

- the card itself: title, summary, notes, tags, category, the linked item's
  title, repository, `#number`, labels and `@author`, the agent name and the
  session branch;
- the **AO session ID**, in full or in part. The ID is not printed on the
  card, so a card found through it shows `Matched ID: volcano-2`;
- the **text of the session conversation**. A card found that way shows
  `In chat:` with the passage around the match.

Conversation text is kept in a local index (the `content_index` table). The
board fills it in the background after each AO sync, fetching only what is
new, so the search is instant and also covers imported sessions that were
not resumed. Matching is a case-insensitive substring match, which works for
Chinese as well. `GET /api/search?q=...` returns the same result with a
`matchedBy` list (`card`, `sessionId`, `conversation`), a reason and the
snippet.

**AI Search** (the button next to the search box, or `Ctrl+Enter` in it)
finds cards by meaning. The board describes every card to the local
`copilot` CLI (with `codex` as fallback): title, category, linked item,
author, summary, notes, session ID and a few excerpts of the conversation,
bounded to about 28,000 characters. The model answers with strict JSON,
`{"matches": [{"cardId": "...", "reason": "..."}]}`. The board then shows
only those cards, best match first within each column, each with an
`AI Match:` line, and a banner `AI Search: 3 matches for "..."` with a
`Clear` button. The other filters still apply. One AI search runs at a time;
it takes a few seconds and uses your Copilot or Codex quota.

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
- Deleting only a card, or unlinking its session, tells the board not to
  create a new card for that session, however often the AO sync runs. Link
  or import it again whenever you want it back. See
  [Deleting cards and sessions](#deleting-cards-and-sessions).
- Imported sessions (`needsResume`) show their history immediately. The
  agent has to be resumed before it accepts messages. If AO answers
  `SESSION_OPEN_ELSEWHERE`, close the session in the other terminal and
  resume again.
- The conversation is polled every 2.5 seconds while the agent is working
  and every 8 seconds while it is idle, only while the drawer is open.
- This is the same session AO desktop shows: messages sent from either side
  appear in both.

## The card workspace

Clicking a card opens it in a drawer on the right.

- **Width**: `min(780px, 60vw)` by default, never under 520px or over 92% of
  the window. Drag the grip on its left edge to resize, use the arrow keys
  while the grip is focused, or double-click it to restore the default. The
  width is remembered.
- **Details above the chat** stay compact: Summary, GitHub Tracking,
  Maintainer Notes and Timeline each fold to one line with a preview, and
  `Hide Details` gives the whole height to the conversation. GitHub Tracking
  opens by itself when there are unread updates or a sync error.
- **Expand** (the tinted, labelled button next to Close in the header, or
  `F`) switches to a full-width workspace: a context rail of 340px on the
  left (Summary, GitHub status and delta, Notes, Controls, Timeline;
  `Hide Context` folds it away) and the conversation on the right in a
  centered reading column of at most 900px, with the message box at the
  bottom.
- **Collapse** (the same button, now leading the header; or `F`, or `Esc`)
  returns to the side drawer with the same card still open: the draft in the
  message box, open editors and the place you were reading in the
  conversation are kept. Only **Close** (the `X` at the far right) goes back
  to the board.

Keyboard: `/` focuses the search, `N` opens the new task dialog, `F` toggles
the expanded workspace, `Esc` goes back one step (field, workspace, drawer,
board), `Ctrl+Enter` sends a message, saves an editor, or starts AI Search
from a search box.

While the agent works, the conversation ends with a live row: the agent's
mark, animated dots, what it is doing (`Sending your message...`,
`Waiting for Codex to start...`, `Codex is thinking...` with the step it
reports, `Codex is writing a reply...`), the elapsed time and a `Stop`
button. `Resume Session` shows `Waking up agent session...` until AO has
restarted the agent.

## Models

- **New session**: choosing Copilot or Codex in `New Task / Session` (or in
  the Agent Session section of a card) loads that agent's models from AO.
  `Default` lets the agent choose. Models that support reasoning efforts show
  an `Effort` select as well.
- **Running session**: the message box has a `Model` select, and an `Effort`
  select when the chosen model has efforts. A change applies from the next
  turn and is confirmed in a toast and in the card timeline; the session bar
  shows the configured model. `Default` clears the choice.
- **Session that was not resumed**: AO reports no model list for it, so the
  board offers the catalog of its agent. The choice is stored and applies
  once the session is resumed.

AO stores any model name it is given. The board therefore checks the model
and the effort against the list before it passes a change or a new session
on, and answers `400 UNKNOWN_MODEL` otherwise. The approval mode of a session
is not exposed by the board.

## Summaries

- **AUTO**: extracted from the latest substantial assistant message of the
  session, stripped of Markdown, at most about 180 characters. It costs no
  tokens and refreshes when the session sees new activity.
- **AI**: `Summarize with AI` asks the local `copilot` CLI what the card is
  about: the purpose of the pull request, issue or task, not how far the
  work got. The prompt therefore carries the linked GitHub item with its
  description (fetched with `gh`), your notes and the opening of the session
  (the first three requests of the user and the agent's first reply) rather
  than the end of the conversation; the latest turns are added only when
  there is no opening request, or a terse one without a linked item. The
  answer is one or two natural sentences in Simplified Chinese (roughly 30
  to 90 characters); identifiers and `#numbers` are kept as they are. While
  it runs, the button reads `Summarizing...` and the Summary section shows
  `Generating summary with AI...` with the elapsed time.

  The attempts, 45 s each, in order:

  1. `copilot --model gemini-3.8-flash --reasoning-effort low`, which
     answers in about 7 seconds (`--summary-model`, `--summary-effort`);
  2. `copilot --model gemini-3.8-flash` without the effort, for a configured
     model that rejects it (skipped when attempt 1 timed out);
  3. `copilot` with its default model;
  4. `codex exec`.

  The answer is cleaned of Markdown, labels and filler and capped at 180
  characters on a sentence boundary. The CLIs run in an empty scratch
  directory with shell and write tools denied (Copilot) or a read-only
  sandbox (Codex). Line 1 of every prompt starts with
  `[maintainer-board-summary]` (`[maintainer-board-search]` for the two AI
  searches, which use the same attempts), which is how these runs are kept
  out of Import History.
- **MANUAL**: anything you type. Manual and AI summaries are never
  overwritten by extraction; clearing the summary hands control back to AUTO.

## Import History

`Import History` lists the Copilot and Codex sessions AO finds on this
machine and imports the ones you pick. To find an older session whose title
you forgot, the board reads the local session stores itself, read-only:
`~/.copilot/session-store.db` (sessions, turns, checkpoints) and the rollout
files under `~/.codex/sessions/`. A local session is matched to an AO
candidate by AO's candidate ID, `sha256(harness, NUL, config dir, NUL,
native ID)`. A store that is missing or unreadable only means less detail.

- Each row shows the agent, the title, a preview of what you asked (or the
  stored checkpoint summary), the first 8 characters of the **native session
  ID** (hover for the full UUID, click to copy), the **AO session ID** when
  the session is already imported, the project or folder, the message count
  and the last activity. `Import` on a row imports that session and opens
  its card; the checkboxes import several at once.
- **Typing** in the search box ranks the history on the server
  (`GET /api/ao/import-history/search`) and says why each row matched:
  - `Matched ID: 01a1c02a-...`: the beginning of a native UUID (or of one of
    its groups), the AO session ID such as `volcano-2`, or AO's candidate ID;
  - `Matched conversation: "..."` with an excerpt: your prompts, the agent's
    replies and checkpoint summaries;
  - `Matched title`, `Matched summary`, `Matched branch`, `Matched folder`.

  Every word has to match somewhere; a query of three or more words may miss
  one (`Partial match`). A word that is not found literally still matches a
  similar one (`Close match`): one or two typos, an abbreviation such as
  `podgrp`, or a Chinese phrase that is worded a little differently.
- **AI Search** (the button next to the box, or `Ctrl+Enter`) sends your
  description and a compact catalog to the fast summary model: the 60 most
  recent sessions plus the best keyword matches among the older ones, each
  with its ID, title, folder, branch, date, first prompts and summary. The
  model returns up to 8 sessions with a short reason in the language of your
  query; they are listed in that order with `AI match: ...`, and `Reset`
  returns to the list.
- The board's own `Summarize with AI` and `AI Search` runs are never listed,
  whatever their title was shortened or renamed to: the AO title, the stored
  title and the first prompt are all checked, and so is the directory the
  session ran in (the `summarizer` directory next to the database, where the
  AI CLIs are started).

Reading a Codex rollout stops at the first megabyte and the last 256 KB of a
file (the requests of the user sit there; the rest is tool output), files
are parsed in parallel, and a scan is cached for 30 seconds, so a history of
a few hundred megabytes is listed in well under a second.

## Deleting cards and sessions

Three different things, kept apart in the UI:

| Action | The card | The AO session |
| --- | --- | --- |
| **Delete Card Only** | removed | stays in Agent Orchestrator; gets no new card |
| **Delete Card & AO Session** | removed | terminated and permanently deleted from AO |
| **Unlink** (session bar) | stays, without a session | stays in Agent Orchestrator; gets no new card |
| **Delete Session** (session bar) | stays, without a session | terminated and permanently deleted from AO |

- Deleting a card that has a session opens a dialog that names the session
  and offers `Delete Card & AO Session`, `Delete Card Only` and `Cancel`. A
  card without a session asks a plain `Delete Card`.
- Deleting an AO session kills it first (`POST /api/v1/sessions/{id}/kill`;
  already terminated or unknown is fine) and then calls
  `DELETE /api/v1/sessions/{id}?cleanup=true`, which removes the session,
  its AO conversation history and its worktree. Git branches and pull
  requests are kept. While AO is still tearing the session down the delete
  is retried for a few seconds.
- AO never forces a worktree with uncommitted changes or a lock. In that
  case, or when AO is offline, the dialog shows AO's reason and **nothing is
  removed**, neither the card nor the link. Commit or discard the changes
  (or run `ao session cleanup`) and delete again.
- A session that stays in AO after `Delete Card Only` or `Unlink` is
  remembered as dismissed, so the AO sync, which runs every few seconds,
  does not create a second card for it. Linking it to a card or importing it
  again lifts that.

## Architecture

```
 Browser (vanilla ES modules, no build step)
     |  JSON over HTTP + server-sent events
     v
 maintainer-board (single Go binary)
     |-- internal/server   HTTP API, embedded UI, sync loops, SSE hub
     |-- internal/db       SQLite: columns, cards, card_events, settings,
     |                     content_index (conversation text for search)
     |-- internal/ao       typed client for the AO daemon API
     |-- internal/github   gh wrapper, update detection, auto-move rules
     |-- internal/summary  summary extraction, the AI summarizer and searches
     |-- internal/history  read-only index of the local Copilot and Codex stores
     |-- web/              index.html, styles.css, *.js (go:embed)
     |
     |-- HTTP --> AO daemon (http://127.0.0.1:3001)
     |-- exec --> gh (pull requests and issues)
     |-- exec --> copilot / codex (AI summaries and AI search only)
     '-- read --> ~/.copilot/session-store.db, ~/.codex/sessions (Import History)
```

Layout:

```
cmd/maintainer-board/main.go   flags, wiring, graceful shutdown
internal/db/                   schema, migrations, CRUD, ordering
internal/ao/                   AO client: sessions, conversation, spawn, import, events
internal/github/               reference parsing, gh client, decision engine
internal/summary/              extraction, prompts, copilot/codex runner
internal/history/              local Copilot and Codex session stores
internal/server/               handlers, AO sync, GitHub sync, security middleware
web/                           the single-page UI
scripts/                       optional browser tests (headless Chromium)
```

State lives in one SQLite file (`modernc.org/sqlite`, no cgo). The schema
is versioned with `PRAGMA user_version`; a newer binary upgrades an existing
database in place when it starts, and every migration so far only adds
columns and tables, so an older binary can still open the file. The UI keeps
only view preferences (theme, filters, drawer width, folded sections,
notification toggle) in `localStorage`.

AO endpoints used: `GET/POST /api/v1/sessions`, `PATCH /api/v1/sessions/{id}`,
`GET /api/v1/sessions/{id}/conversation`,
`POST .../conversation/messages`, `POST .../conversation/interrupt`,
`POST .../resume-agent`, `POST .../kill`,
`DELETE /api/v1/sessions/{id}?cleanup=true`, `GET /api/v1/projects`,
`GET/POST /api/v1/session-import`, `GET /api/v1/events`,
`GET /api/v1/agents/{agent}/models`,
`GET /api/v1/sessions/{id}/conversation/models` and
`PATCH /api/v1/sessions/{id}/conversation/settings`.

`gh` commands used: `gh api repos/{repo}/issues/{n}` (PR or issue),
`gh pr view {n} --repo {repo} --json ...`, `gh api repos/{repo}/pulls/{n}`
(exact counts), `gh issue view {n} --repo {repo} --json ...`,
`gh api graphql` for unresolved review threads, only for PRs that are
approved with green CI, and `gh api user` once to learn your login. An AI
summary reads the description of the linked item with the first of these.

## HTTP API

All bodies are JSON. Errors have the shape
`{"error": {"code": "...", "message": "..."}}`. Field names are camelCase.

| Method and path | Purpose |
| --- | --- |
| `GET /healthz` | Liveness check. |
| `GET /api/board` | Columns, cards (with their AO session), AO and GitHub status, settings, unread count, and `currentUser` (your GitHub login, or empty). |
| `GET /api/events` | Server-sent events; a `board` event after every change. |
| `PATCH /api/settings` | `{"showOrchestrator": true}` |
| `GET /api/columns`, `POST /api/columns` | List; create `{"title", "color", "role"}`. |
| `PATCH /api/columns/{id}`, `DELETE /api/columns/{id}` | Update title, color, role; delete. |
| `POST /api/columns/reorder` | `{"ids": ["inbox", "..."]}` left to right. |
| `GET /api/cards`, `POST /api/cards` | List; create `{"title", "category", "columnId", "tags", "notes", "summary", "githubRef", "aoSessionId", "agent": {"harness", "prompt", "projectId", "model", "effort"}}`. |
| `GET /api/cards/{id}`, `PATCH /api/cards/{id}` | Read; update `title`, `category`, `tags`, `summary`, `notes`, `autoMove`, `columnId`, `aoSessionId`. |
| `DELETE /api/cards/{id}` | Delete the card; its AO session stays in AO and is dismissed. With `?deleteAoSession=true` (or the body `{"deleteAoSession": true}`) the session is killed and deleted from AO first; if AO refuses, the error carries AO's code (for example `409 WORKSPACE_DIRTY`) and nothing is removed. Answers `{"deleted", "aoSessionId", "aoSessionDeleted"}`. |
| `DELETE /api/cards/{id}/session` | Kill the card's AO session, delete it from AO and detach it; the card stays. `409 NO_SESSION` without one. |
| `POST /api/cards/{id}/unlink` | Detach the AO session from the card only; the session stays in AO and is dismissed. |
| `PATCH /api/cards/{id}/move` | `{"columnId", "position"}` (0-based). |
| `GET /api/cards/{id}/events` | Audit timeline, newest first. |
| `POST /api/cards/{id}/link-github` | `{"ref": "#6247"}` |
| `POST /api/cards/{id}/unlink-github` | Remove the link and tracking state. |
| `POST /api/cards/{id}/sync-github` | Fetch now; returns the card, `newUpdate`, `moved`. |
| `POST /api/cards/{id}/ack` | Mark caught up or reviewed; optional `{"moveToColumnId": "waiting"}`. Also records your review (`REVIEWED` when GitHub has none). |
| `POST /api/cards/{id}/unreview` | Remove a `REVIEWED` mark set on the board. `409 REVIEW_FROM_GITHUB` for a review submitted on GitHub. |
| `POST /api/cards/{id}/summarize` | AI summary of what the card is about; returns the card, `summary`, `engine`. |
| `POST /api/cards/{id}/session` | Start an agent session: `{"harness": "copilot" or "codex", "prompt", "projectId", "model", "effort"}`. |
| `GET /api/cards/{id}/models` | Models the card's session can use and the selected `model` and `reasoningEffort`. `source` is `session`, or `agent` when the session is not running and its agent's catalog is returned. |
| `PATCH /api/cards/{id}/settings` | `{"model", "reasoningEffort"}`, either or both; an empty string resets to the default. Applies from the next turn. |
| `GET /api/ao/agents/{agent}/models` | Models of `copilot` or `codex`: `{"agent", "models": [{"id", "label", "isDefault", "efforts", "defaultEffort"}]}`. |
| `GET /api/search?q=...` | Cards matching every word in their fields, session ID or conversation text: `{"matches": [{"cardId", "matchedBy", "reason", "snippet"}]}`. |
| `POST /api/search/ai` | `{"query"}`; AI search by meaning: `{"engine", "matches": [{"cardId", "reason"}]}`, best match first. `409 AI_SEARCH_BUSY` while another one runs. |
| `POST /api/github/sync` | Sync every linked card; returns a report. |
| `GET /api/sessions` | AO sessions with the ID of their card, if any. |
| `GET /api/sessions/{id}/conversation` | `?limit=60&beforeSequence=N` |
| `POST /api/sessions/{id}/messages` | `{"text", "clientMessageId"}`, answers 202. |
| `POST /api/sessions/{id}/interrupt`, `/resume`, `/kill` | Session control. AO error codes such as `SESSION_OPEN_ELSEWHERE` are passed through. |
| `PATCH /api/sessions/{id}` | Rename in AO: `{"displayName"}`. |
| `GET /api/ao/import-history` | AO's import candidates without the board's own AI runs, each with `nativeId`, `summary`, `preview` and `branch` when the local stores know the session, and `cardId` when it is imported. `GET /api/import/candidates` is the same. |
| `GET /api/ao/import-history/search?q=...` | Ranked matches by ID, metadata and conversation text: `{"query", "total", "matches": [{"candidate", "matchedBy", "score", "reason", "snippet"}]}`. |
| `POST /api/ao/import-history/ai-search` | `{"query"}`; AI search by meaning: `{"query", "provider", "matches": [{"candidate", "reason"}]}`, best match first. `409 AI_SEARCH_BUSY` while another AI search runs. |
| `POST /api/import` | Import `{"ids": [...]}` and create cards. |

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
node --test web/tests/         # Markdown parser and board logic tests (needs Node 18+, optional)
```

The Go tests cover the schema, its migrations from an older database and
ordering, reference parsing, the update detection and auto-move state
machine, the `gh` output parsing including your own review and Prow
commands, the conversation index and both searches, the AI prompts, answer
parsing and engine fallback, the local Copilot and Codex history index
(against a real SQLite store and rollout files in a temporary directory),
the Import History search and AI search, the AO client against a fake
daemon including session deletion, every HTTP handler against fake AO and
GitHub backends, and a guard that keeps emoji out of the repository.

Four optional browser tests drive a headless Chromium through the DevTools
protocol (no npm packages; set `CHROMIUM` if the binary is not
`chromium-browser`):

```sh
node scripts/ui-smoke.mjs  http://127.0.0.1:3012       # read-only walkthrough, screenshots
node scripts/ui-flow.mjs   http://127.0.0.1:3913       # creates and deletes cards and columns
node scripts/ui-models.mjs http://127.0.0.1:3916 http://127.0.0.1:3915   # model selection
node scripts/ui-import-delete.mjs http://127.0.0.1:3926 http://127.0.0.1:3925 data/scratch/import/home   # import search, progress, deletes
```

`ui-smoke.mjs` only reads: it checks typography, the card faces against the
API data, the author filter, search by session ID and by conversation text,
the drawer width and resizing, the expanded workspace, the model pickers,
the Import History dialog with a search by native session ID, and both
themes. It changes no card, no session and no model.

`ui-flow.mjs` modifies the board, so run it against a scratch instance:

```sh
./maintainer-board --addr 127.0.0.1:3913 --db data/scratch/board.db
```

Set `MB_AI=1` to include one real AI Search in it. Neither of these two
scripts starts, messages, resumes or terminates an AO session.

`ui-models.mjs` starts sessions and switches models, so it only runs against
a board that talks to the bundled stand-in for the AO daemon, never against
real sessions:

```sh
node scripts/fake-ao.mjs 3915 &
./maintainer-board --addr 127.0.0.1:3916 --db data/scratch/models.db --ao-url http://127.0.0.1:3915
```

`ui-import-delete.mjs` imports, messages, resumes and deletes sessions, and
runs AI summaries and searches. It needs the fake AO daemon as well, plus
`scripts/fake-ai.mjs` in place of the `copilot` and `codex` CLIs and a
scratch history directory, into which it writes a small Copilot store
(with `python3`) and two Codex rollouts:

```sh
dir=$PWD/data/scratch/import; rm -rf "$dir"; mkdir -p "$dir"
FAKE_AO_DELAY_MS=700 node scripts/fake-ao.mjs 3925 &
FAKE_AI_DELAY_MS=2600 FAKE_AI_LOG=$dir/ai.log ./maintainer-board --addr 127.0.0.1:3926 --db "$dir/board.db" \
  --ao-url http://127.0.0.1:3925 --ao-sync-interval 1s --history-home "$dir/home" \
  --copilot-path "$PWD/scripts/fake-ai.mjs" --codex-path "$PWD/scripts/fake-ai.mjs" &
FAKE_AI_LOG=$dir/ai.log node scripts/ui-import-delete.mjs http://127.0.0.1:3926 http://127.0.0.1:3925 "$dir/home"
```

It checks the Expand button and the way back from the workspace (Collapse,
`F`, `Esc`, Close), the reading position across both layouts, Import History
(IDs on the rows, search by ID, by conversation text and with a typo, AI
Search with its progress and the flags the CLI was called with, import from
a row), the progress indicators of Summarize, Send and Resume, Unlink
against Delete AO Session, and every way to delete a card, including a
delete that AO refuses and a check that a dismissed session gets no new
card from the AO sync.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Header shows `AO Offline` | Is the daemon up? `curl -s http://127.0.0.1:3001/api/v1/projects`. The board reconnects on its own. |
| Header shows `Server Offline` | The board process or the SSH tunnel is down. |
| `403 FORBIDDEN_HOST` | Open the board as `127.0.0.1` or `localhost`, or add the name with `--allowed-hosts`. |
| A card says `Last sync failed` | `gh auth status`, network access to github.com, and API rate limits. |
| CI never turns green | A permanently pending status context; add it to `--ci-ignore-contexts`. |
| `Summarize with AI` or `AI Search` fails | `copilot` or `codex` must be on `PATH` and logged in. The error names the CLI that failed. |
| No `MINE` tag, no `Mine` filter, no `You:` status | The board does not know your login. Check `gh api user`, or start with `--gh-user`. |
| A search does not find text from a chat | The index follows AO in the background; give a new or long session a few seconds. Messages that are still streaming are indexed once complete. |
| A deleted card's session should come back | Create a task and choose `Existing Session`, or link it from a card's Agent Session section. An imported session can also be imported again. |
| `Delete Card & AO Session` says AO refused | The worktree of the session has uncommitted changes or is locked. Commit or discard them, or run `ao session cleanup`, then delete again. Nothing was removed. |
| Import History shows no ID or preview for a session | The local store that holds it was not found or not readable: `~/.copilot/session-store.db`, `~/.codex/sessions/`, or the directory given with `--history-home`. AO must scan the same home directory. |
| Resume answers "open in another terminal" | Exit the agent in that terminal (or in AO desktop), then resume again. |

## Known limitations

- Tool approval prompts of an agent cannot be answered from the board; use
  AO desktop for those. Plain questions can be answered by sending a message.
- Review comment counts come from GitHub's totals. An edited comment is not
  detected as new activity.
- Each `Summarize with AI` and `AI Search` run through Copilot is stored by
  the Copilot CLI as a session of its own. The board hides those runs in its
  Import History list; other tools will still show them.
- Import History searches the first megabyte and the last 256 KB of a Codex
  rollout file. A prompt in the middle of a very long session is found
  through AO once the session is imported (board search), not before.
- Deleting an AO session is AO's `ao session delete --cleanup`: it removes
  the session record and AO's history of it. The board does not delete
  anything from the Copilot or Codex stores itself.
- Unread tracking counts all activity, including your own: after you submit
  a review on GitHub the card shows the new comment as unread until you mark
  it caught up. Your own review status is not affected by that.
- Bots are recognized by name, not by account type, when deciding whether a
  review is stale; an unusual bot name counts as a person.
- A conversation that AO rolls back is re-indexed from scratch, but a single
  edited message keeps its old text in the search index until then.
- One board per database file. There are no user accounts; anyone who can
  reach the port has full access.
