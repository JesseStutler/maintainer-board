// Shared application state and the pure helpers that interpret it.

import { storageGet, storageSet } from './util.js';

export const CATEGORIES = [
  ['PR_REVIEW', 'PR Review'],
  ['ISSUE', 'Issue'],
  ['DEV', 'Dev'],
  ['DESIGN', 'Design'],
  ['TRIAGE', 'Triage'],
];

export const ROLES = [
  ['', 'No automation'],
  ['inbox', 'Inbox'],
  ['in_progress', 'In Progress'],
  ['waiting', 'Waiting'],
  ['updates', 'Updates'],
  ['ready', 'Ready'],
  ['done', 'Done'],
];

export const COLOR_PRESETS = ['#6e7681', '#3b82f6', '#a855f7', '#f59e0b', '#10b981', '#52525b', '#ef4444', '#ec4899', '#14b8a6'];

export const APPROVAL_MODES = [
  ['', 'AO default'],
  ['default', 'Ask before risky tools'],
  ['accept-edits', 'Accept edits'],
  ['auto', 'Auto'],
  ['bypass-permissions', 'Bypass permissions (YOLO)'],
];

export function approvalModeLabel(mode) {
  const hit = APPROVAL_MODES.find(([value]) => value === (mode || ''));
  return hit ? hit[1] : mode;
}

export function approvalModeShort(mode) {
  switch (mode) {
    case 'default':
      return 'Ask';
    case 'accept-edits':
      return 'Accept edits';
    case 'auto':
      return 'Auto';
    case 'bypass-permissions':
      return 'YOLO';
    default:
      return 'Default';
  }
}

const FILTER_KEY = 'mb.filters';
const COLLAPSED_COLS_KEY = 'mb.collapsedCols';
const COMPACT_COLS_KEY = 'mb.compactCols';
const PINNED_CARDS_KEY = 'mb.pinnedCards';
const DEFAULT_FILTERS = { search: '', category: '', agent: '', author: '', updatesOnly: false, showDone: true, compact: false, sort: 'manual' };

/** Value of the author filter that selects the maintainer's own items. */
export const AUTHOR_MINE = '@me';

export const state = {
  board: null,
  filters: { ...DEFAULT_FILTERS, ...storageGet(FILTER_KEY, {}), search: '' },
  collapsedCols: new Set(storageGet(COLLAPSED_COLS_KEY, [])),
  compactCols: new Set(storageGet(COMPACT_COLS_KEY, [])),
  pinnedCards: new Set(storageGet(PINNED_CARDS_KEY, [])),
  selectedCardId: null,
  // Server-side search result for the query identified by `key`: cards
  // found through their session ID or conversation text.
  search: { key: '', matches: new Map() },
  // AI search: status is "idle", "running", "done" or "error".
  ai: { status: 'idle', query: '', engine: '', error: '', matches: new Map() },
  // True until the first successful board load; used to suppress
  // notifications for updates that predate this page view.
  firstLoad: true,
};

const listeners = new Set();

/** Registers a callback invoked after every state change. */
export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function emit() {
  for (const fn of listeners) fn();
}

export function setBoard(board) {
  state.board = board;
  emit();
}

export function setFilter(key, value) {
  state.filters[key] = value;
  const { search, ...persisted } = state.filters;
  storageSet(FILTER_KEY, persisted);
  emit();
}

export function columns() {
  return state.board ? state.board.columns : [];
}

export function cards() {
  return state.board ? state.board.cards : [];
}

export function cardById(id) {
  return cards().find((c) => c.id === id) || null;
}

export function columnById(id) {
  return columns().find((c) => c.id === id) || null;
}

export function columnByRole(role) {
  return columns().find((c) => c.role === role) || null;
}

export function categoryLabel(category) {
  const entry = CATEGORIES.find(([value]) => value === category);
  return entry ? entry[1] : category;
}

export function harnessLabel(harness) {
  if (harness === 'copilot') return 'Copilot';
  if (harness === 'codex') return 'Codex';
  if (!harness) return 'Agent';
  return harness.charAt(0).toUpperCase() + harness.slice(1);
}

/** Label and dot class for the live state of a card's AO session. */
export function activityInfo(card) {
  const session = card.session;
  if (!session) return { label: 'Unavailable', cls: 'is-missing' };
  if (session.isTerminated) return { label: 'Terminated', cls: 'is-terminated' };
  if (session.needsResume) return { label: 'Resume needed', cls: 'is-resume' };
  switch (session.activity && session.activity.state) {
    case 'active':
      return { label: 'Working', cls: 'is-active' };
    case 'waiting_input':
      return { label: 'Needs Input', cls: 'is-waiting_input' };
    case 'blocked':
      return { label: 'Blocked', cls: 'is-blocked' };
    case 'exited':
      return { label: 'Exited', cls: 'is-exited' };
    default:
      return { label: 'Idle', cls: 'is-idle' };
  }
}

export function isSessionActive(card) {
  const s = card && card.session;
  return Boolean(s && !s.isTerminated && !s.needsResume && s.activity && s.activity.state === 'active');
}

/** "#6247" for the default repository, "owner/repo#12" otherwise. */
export function ghRefLabel(card) {
  if (!card.ghNumber) return '';
  const defaultRepo = state.board ? state.board.settings.defaultRepo : '';
  return card.ghRepo && card.ghRepo !== defaultRepo ? `${card.ghRepo}#${card.ghNumber}` : `#${card.ghNumber}`;
}

export function ghUrl(card) {
  if (card.ghUrl) return card.ghUrl;
  if (!card.ghNumber || !card.ghRepo) return '';
  return `https://github.com/${card.ghRepo}/issues/${card.ghNumber}`;
}

export function ghStateInfo(card) {
  switch (card.ghState) {
    case 'OPEN':
      return { label: 'Open', cls: 'pill-success', tone: 'is-success' };
    case 'DRAFT':
      return { label: 'Draft', cls: 'pill-muted', tone: '' };
    case 'MERGED':
      return { label: 'Merged', cls: 'pill-merged', tone: 'is-merged' };
    case 'CLOSED':
      return { label: 'Closed', cls: 'pill-danger', tone: 'is-danger' };
    default:
      return { label: 'Not synced', cls: 'pill-muted', tone: '' };
  }
}

export function ghIconName(card) {
  if (card.ghState === 'MERGED') return 'merge';
  return card.ghKind === 'issue' ? 'issue' : 'pr';
}

export function ciInfo(status) {
  switch (status) {
    case 'SUCCESS':
      return { label: 'CI Passing', cls: 'pill-success', tone: 'is-success', icon: 'check-circle', title: 'Every check passed' };
    case 'FAILURE':
      return { label: 'CI Failing', cls: 'pill-danger', tone: 'is-danger', icon: 'x-circle', title: 'At least one check failed' };
    case 'PENDING':
      return { label: 'CI Pending', cls: 'pill-warning', tone: 'is-warning', icon: 'clock', title: 'Checks are still running' };
    default:
      return null;
  }
}

/** The review decision of the pull request as a whole. */
export function reviewInfo(decision, own = false) {
  switch (decision) {
    case 'APPROVED':
      return { label: 'Approved', short: 'Approved', cls: 'pill-success', tone: 'is-success', icon: 'check', title: 'Review decision: approved' };
    case 'CHANGES_REQUESTED':
      return { label: 'Changes Requested', short: 'Changes Req', cls: 'pill-danger', tone: 'is-danger', icon: 'alert', title: 'Review decision: a reviewer requested changes' };
    case 'REVIEW_REQUIRED':
      if (own) {
        return { label: 'Awaiting review', short: 'Awaiting review', cls: 'pill-muted', tone: '', icon: 'user', title: 'Waiting for another maintainer to approve your pull request' };
      }
      return { label: 'Review Required', short: 'Review Required', cls: 'pill-muted', tone: '', icon: 'user', title: 'Review decision: an approving review is still required' };
    default:
      return null;
  }
}

/** GitHub login of the maintainer running the board, or ''. */
export function currentUser() {
  return (state.board && state.board.currentUser) || '';
}

/** True when the linked pull request or issue was written by the maintainer. */
export function isMine(card) {
  const me = currentUser();
  return Boolean(me && card.ghAuthor && card.ghAuthor.toLowerCase() === me.toLowerCase());
}

const MY_REVIEW_LABELS = {
  APPROVED: 'Approved',
  LGTM: 'LGTM',
  CHANGES_REQUESTED: 'Changes Req',
  COMMENTED: 'Commented',
  REVIEWED: 'Reviewed',
};

const MY_REVIEW_DETAIL = {
  APPROVED: 'You approved this on GitHub',
  LGTM: 'You gave /lgtm on GitHub',
  CHANGES_REQUESTED: 'You requested changes on GitHub',
  COMMENTED: 'You left a review on GitHub',
  REVIEWED: 'You marked this reviewed on the board',
};

/**
 * Where the maintainer personally stands on the linked item, which is not
 * the same as the overall review decision. `kind` is "current" (reviewed and
 * nothing changed since), "stale" (changed after the review), "none" (an
 * open pull request of somebody else that was not reviewed yet), or the
 * function returns null when there is nothing to say.
 */
export function myReviewInfo(card) {
  if (!card.ghNumber || !card.ghLastSyncedAt || isMine(card)) return null;
  const state = card.ghMyReviewState;
  const finished = card.ghState === 'MERGED' || card.ghState === 'CLOSED';
  if (state) {
    const name = MY_REVIEW_LABELS[state] || state;
    const detail = MY_REVIEW_DETAIL[state] || 'You reviewed this';
    if (card.ghIsStaleReview && !finished) {
      return {
        kind: 'stale',
        label: 'Re-review needed',
        cls: 'pill-warning',
        tone: 'is-warning',
        icon: 'refresh',
        title: `${detail}, and it was updated afterwards: new commits or new replies. Your last look: ${name}.`,
      };
    }
    return { kind: 'current', label: `You: ${name}`, cls: 'pill-you', tone: 'is-you', icon: 'check', title: `${detail}. Nothing changed since.` };
  }
  if (card.ghKind === 'pr' && card.ghState === 'OPEN' && currentUser()) {
    return { kind: 'none', label: 'Not reviewed', cls: 'pill-muted', tone: '', icon: 'eye', title: 'You have not reviewed this pull request yet' };
  }
  return null;
}

/**
 * Whether the PR-wide review decision chip adds information beyond the
 * maintainer's personal review badge on the card face.
 */
export function showCardReviewDecision(card, mine) {
  if (!card.ghReviewDecision) return false;
  // On someone else's PR with a personal review badge, or on your own PR
  // (where you are the author waiting on others), default REVIEW_REQUIRED is
  // noisy and reads like a prompt for you to review your own work.
  if (card.ghReviewDecision === 'REVIEW_REQUIRED' && (mine || isMine(card))) return false;
  if (!mine) return true;
  if (mine.kind === 'current' && card.ghReviewDecision === card.ghMyReviewState) return false;
  return true;
}

/** Distinct authors of the linked items on the board, sorted by name. */
export function cardAuthors() {
  const seen = new Map();
  for (const card of cards()) {
    if (!card.ghAuthor || hiddenOrchestrator(card)) continue;
    const key = card.ghAuthor.toLowerCase();
    if (!seen.has(key)) seen.set(key, card.ghAuthor);
  }
  return Array.from(seen.values()).sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()));
}

function matchesAuthor(card, author) {
  if (!author) return true;
  // Without a known login "Mine" cannot be decided, so it does not filter.
  if (author === AUTHOR_MINE) return !currentUser() || isMine(card);
  return Boolean(card.ghAuthor) && card.ghAuthor.toLowerCase() === author.toLowerCase();
}

/** Cards auto-created for orchestrator sessions are hidden unless enabled. */
function hiddenOrchestrator(card) {
  if (!state.board || state.board.settings.showOrchestrator) return false;
  return card.origin === 'ao' && card.session && card.session.kind === 'orchestrator';
}

/** Lower-cased terms of the search box; every one has to match. */
export function searchTokens() {
  return state.filters.search.toLowerCase().split(/\s+/).filter(Boolean);
}

/** Identifies a search by its terms, so results are tied to their query. */
export function searchKey() {
  return searchTokens().join(' ');
}

/**
 * The searchable text of a card itself. The AO session ID and the session
 * branch are matched separately, the ID first: AO puts the ID into the
 * branch name, and a search for it should be reported as a match on the ID.
 */
function cardHaystack(card) {
  return [
    card.title,
    card.summary,
    card.notes,
    card.ghTitle,
    card.ghAuthor ? `${card.ghAuthor} @${card.ghAuthor}` : '',
    card.ghRepo,
    card.ghNumber ? `#${card.ghNumber} ${card.ghNumber}` : '',
    categoryLabel(card.category),
    (card.tags || []).join(' '),
    (card.ghLabels || []).join(' '),
  ]
    .join('\n')
    .toLowerCase();
}

function sessionHaystack(card) {
  return card.session ? `${card.session.harness} ${card.session.branch || ''}`.toLowerCase() : '';
}

/**
 * Matches the card's own fields and its AO session ID. The ID is no longer
 * printed on the card face but stays searchable, in full or in part.
 */
function matchesSearch(card, tokens) {
  if (tokens.length === 0) return true;
  const haystack = `${cardHaystack(card)}\n${sessionHaystack(card)}`;
  const id = (card.aoSessionId || '').toLowerCase();
  return tokens.every((token) => haystack.includes(token) || (id !== '' && id.includes(token)));
}

/** The server's match for this card and the current query, or null. */
function contentMatch(card) {
  const result = state.search;
  if (!result.key || result.key !== searchKey()) return null;
  return result.matches.get(card.id) || null;
}

/** True while the board shows the result of an AI search. */
export function aiSearchActive() {
  return state.ai.status === 'done';
}

/**
 * Why a card is shown for the current search, when the card face alone
 * does not explain it: an AI match with its reason, a match on the hidden
 * session ID, or a match inside the session conversation. Returns a list of
 * { kind, label, text } entries, usually empty.
 */
export function searchMatches(card) {
  if (aiSearchActive()) {
    const match = state.ai.matches.get(card.id);
    return match ? [{ kind: 'ai', label: 'AI Match', text: match.reason, rank: match.rank }] : [];
  }
  const tokens = searchTokens();
  if (tokens.length === 0) return [];
  const out = [];
  if (card.aoSessionId) {
    // Computed here rather than taken from the server so that the badge
    // appears with the first keystroke.
    const id = card.aoSessionId.toLowerCase();
    const haystack = cardHaystack(card);
    if (tokens.some((token) => id.includes(token) && !haystack.includes(token))) {
      out.push({ kind: 'id', label: 'Matched ID', text: card.aoSessionId });
    }
  }
  const server = contentMatch(card);
  if (server && server.matchedBy.includes('conversation') && server.snippet) {
    const who = server.role === 'user' ? 'you' : server.role === 'assistant' ? 'the agent' : server.role;
    const count = server.messages > 1 ? `${server.messages} messages` : '1 message';
    out.push({ kind: 'chat', label: 'In chat', text: server.snippet, title: `Found in the session conversation (${count}); the newest is from ${who}` });
  }
  return out;
}

export function matchesFilters(card) {
  const f = state.filters;
  if (hiddenOrchestrator(card)) return false;
  if (f.category && card.category !== f.category) return false;
  if (f.agent && (!card.session || card.session.harness !== f.agent)) return false;
  if (!matchesAuthor(card, f.author)) return false;
  if (f.updatesOnly && !card.hasUnreadUpdates) return false;
  // An AI search result replaces the keyword search until it is cleared.
  if (aiSearchActive()) return state.ai.matches.has(card.id);
  if (matchesSearch(card, searchTokens())) return true;
  return contentMatch(card) !== null;
}

export function visibleColumns() {
  return columns().filter((col) => state.filters.showDone || col.role !== 'done');
}

/** All cards of a column in board order, ignoring filters. */
export function columnCards(columnId) {
  return cards()
    .filter((c) => c.columnId === columnId)
    .sort((a, b) => a.position - b.position);
}

export function isColumnCollapsed(columnId) {
  return state.collapsedCols.has(columnId);
}

export function toggleColumnCollapsed(columnId) {
  if (state.collapsedCols.has(columnId)) state.collapsedCols.delete(columnId);
  else state.collapsedCols.add(columnId);
  storageSet(COLLAPSED_COLS_KEY, Array.from(state.collapsedCols));
  emit();
}

export function isColumnCompact(columnId) {
  return Boolean(state.filters.compact || state.compactCols.has(columnId));
}

export function toggleColumnCompact(columnId) {
  if (state.filters.compact) {
    // Switching one column back to normal while global compact is active
    // keeps the other columns compact as per-column overrides.
    state.filters.compact = false;
    state.compactCols = new Set(columns().map((c) => c.id).filter((id) => id !== columnId));
    const { search, ...persisted } = state.filters;
    storageSet(FILTER_KEY, persisted);
    storageSet(COMPACT_COLS_KEY, Array.from(state.compactCols));
    emit();
    return;
  }
  if (state.compactCols.has(columnId)) state.compactCols.delete(columnId);
  else state.compactCols.add(columnId);
  storageSet(COMPACT_COLS_KEY, Array.from(state.compactCols));
  emit();
}

export function toggleGlobalCompact() {
  const next = !state.filters.compact;
  state.filters.compact = next;
  if (!next) {
    state.compactCols.clear();
    storageSet(COMPACT_COLS_KEY, []);
  }
  const { search, ...persisted } = state.filters;
  storageSet(FILTER_KEY, persisted);
  emit();
}

export function isCardPinned(cardId) {
  return state.pinnedCards.has(cardId);
}

export function toggleCardPinned(cardId) {
  if (state.pinnedCards.has(cardId)) state.pinnedCards.delete(cardId);
  else state.pinnedCards.add(cardId);
  storageSet(PINNED_CARDS_KEY, Array.from(state.pinnedCards));
  emit();
}

function activityTimestamp(card) {
  const times = [
    card.ghUpdatedAt,
    card.session && card.session.activity && card.session.activity.lastActivityAt,
    card.updatedAt,
  ];
  let best = 0;
  for (const t of times) {
    if (!t) continue;
    const ms = Date.parse(t);
    if (!Number.isNaN(ms) && ms > best) best = ms;
  }
  return best;
}

function needsAttention(card) {
  const open = card.ghState !== 'MERGED' && card.ghState !== 'CLOSED';
  return Boolean(card.hasUnreadUpdates || (open && (card.ghIsStaleReview || card.ghPendingFeedback)));
}

/**
 * Sorts visible cards inside a column. AI search preserves relevance rank;
 * otherwise pinned cards stay on top followed by manual, recent activity,
 * or unread-first ordering.
 */
export function sortColumnCards(list, { sort = state.filters.sort, pinned = state.pinnedCards, aiMatches = null } = {}) {
  const out = [...list];
  if (aiMatches) {
    out.sort((a, b) => aiMatches.get(a.id).rank - aiMatches.get(b.id).rank);
    return out;
  }
  out.sort((a, b) => {
    const pa = pinned.has(a.id) ? 0 : 1;
    const pb = pinned.has(b.id) ? 0 : 1;
    if (pa !== pb) return pa - pb;
    if (sort === 'unread') {
      const ua = needsAttention(a) ? 0 : 1;
      const ub = needsAttention(b) ? 0 : 1;
      if (ua !== ub) return ua - ub;
      const dt = activityTimestamp(b) - activityTimestamp(a);
      if (dt !== 0) return dt;
    } else if (sort === 'recent') {
      const dt = activityTimestamp(b) - activityTimestamp(a);
      if (dt !== 0) return dt;
    }
    return a.position - b.position;
  });
  return out;
}

export function unreadCount() {
  return cards().filter((c) => c.hasUnreadUpdates && !hiddenOrchestrator(c)).length;
}
