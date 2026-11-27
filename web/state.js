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
const DEFAULT_FILTERS = { repo: '', search: '', category: '', agent: '', author: '', updatesOnly: false, showDone: true, compact: false, sort: 'manual' };

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

/** All linked GitHub items on a card, primary link first. */
export function cardLinks(card) {
  if (!card) return [];
  if (Array.isArray(card.ghLinks) && card.ghLinks.length > 0) return card.ghLinks;
  if (!card.ghNumber) return [];
  return [
    {
      id: '',
      repo: card.ghRepo || '',
      number: card.ghNumber,
      kind: card.ghKind || 'pr',
      url: card.ghUrl || '',
      title: card.ghTitle || '',
      author: card.ghAuthor || '',
      state: card.ghState || '',
      ciStatus: card.ghCiStatus || '',
      reviewDecision: card.ghReviewDecision || '',
      headSha: card.ghHeadSha || '',
      commitCount: card.ghCommitCount || 0,
      commentCount: card.ghCommentCount || 0,
      reviewCommentCount: card.ghReviewCommentCount || 0,
      labels: card.ghLabels || [],
      updatedAt: card.ghUpdatedAt || '',
      lastSyncedAt: card.ghLastSyncedAt || '',
      error: card.ghError || '',
      myReviewState: card.ghMyReviewState || '',
      myReviewedAt: card.ghMyReviewedAt || '',
      isStaleReview: Boolean(card.ghIsStaleReview),
      pendingFeedback: card.ghPendingFeedback || '',
      hasUnreadUpdates: Boolean(card.hasUnreadUpdates),
      unreadUpdates: card.unreadUpdates || 0,
      updateCycles: card.updateCycles || 0,
      updateSummary: card.updateSummary || '',
      ackAt: card.ackAt || '',
    },
  ];
}

/** Configured repositories from board settings (always includes defaultRepo). */
export function configuredRepos() {
  const settings = state.board && state.board.settings;
  if (!settings) return [];
  const list = Array.isArray(settings.repos) ? settings.repos.filter((r) => r && r.repo) : [];
  if (list.length > 0) return list;
  return settings.defaultRepo ? [{ repo: settings.defaultRepo, workspacePath: '' }] : [];
}

/** Active repository context: the selected repo board, or defaultRepo in All view. */
export function activeRepo() {
  if (state.filters.repo) return state.filters.repo;
  return (state.board && state.board.settings && state.board.settings.defaultRepo) || '';
}

function normalizeDirPath(p) {
  return String(p || '').replace(/\/+$/, '');
}

function repoForCWD(cwd) {
  const clean = normalizeDirPath(cwd);
  if (!clean) return '';
  let bestRepo = '';
  let bestLen = 0;
  for (const item of configuredRepos()) {
    const root = normalizeDirPath(item.workspacePath);
    if (!root) continue;
    if ((clean === root || clean.startsWith(`${root}/`)) && root.length > bestLen) {
      bestLen = root.length;
      bestRepo = item.repo;
    }
  }
  return bestRepo;
}

/**
 * Repositories a card belongs to: every linked GitHub repo plus its explicit
 * board repo. Legacy cards with neither fall back to matching their session
 * workspace path or the board's defaultRepo.
 */
export function cardRepos(card) {
  if (!card) return [];
  const seen = new Map();
  const add = (repo) => {
    const clean = String(repo || '').trim();
    if (!clean) return;
    const key = clean.toLowerCase();
    if (!seen.has(key)) seen.set(key, clean);
  };
  for (const link of cardLinks(card)) add(link.repo);
  if (card.repo) add(card.repo);
  if (seen.size === 0) {
    const fromCwd = card.session && card.session.cwd ? repoForCWD(card.session.cwd) : '';
    if (fromCwd) add(fromCwd);
    else if (state.board && state.board.settings && state.board.settings.defaultRepo) {
      add(state.board.settings.defaultRepo);
    }
  }
  return Array.from(seen.values());
}

/** Primary repository of a card (first linked repo, explicit card.repo, or fallback). */
export function cardPrimaryRepo(card) {
  return cardRepos(card)[0] || '';
}

export function cardBelongsToRepo(card, repo) {
  if (!repo) return true;
  const want = repo.toLowerCase();
  return cardRepos(card).some((r) => r.toLowerCase() === want);
}

/**
 * Preferred project/workspace ID for spawning an agent in `repo`. Returns a
 * configured workspacePath or matching AO project ID, or ''.
 */
export function workspaceForRepo(repo) {
  if (!repo || !state.board) return '';
  const want = repo.toLowerCase();
  const entry = configuredRepos().find((r) => r.repo.toLowerCase() === want);
  const projects = (state.board.ao && state.board.ao.projects) || [];
  if (entry && entry.workspacePath) {
    const clean = normalizeDirPath(entry.workspacePath);
    const match = projects.find((p) => normalizeDirPath(p.path) === clean || p.id === entry.workspacePath);
    return match ? match.id : entry.workspacePath;
  }
  const shortName = repo.includes('/') ? repo.split('/')[1].toLowerCase() : want;
  const byName = projects.find((p) => (p.name || '').toLowerCase() === shortName || (p.id || '').toLowerCase() === shortName);
  return byName ? byName.id : '';
}

/** Summary of all repositories for the top-left switcher dropdown. */
export function boardRepoSummary() {
  const visible = cards().filter((c) => !hiddenOrchestrator(c));
  const map = new Map();
  const ensure = (repo, meta = {}) => {
    const key = repo.toLowerCase();
    let item = map.get(key);
    if (!item) {
      item = { repo, workspacePath: meta.workspacePath || '', configured: Boolean(meta.configured), count: 0, unread: 0 };
      map.set(key, item);
    } else if (meta.configured) {
      item.configured = true;
      item.repo = repo;
      if (meta.workspacePath) item.workspacePath = meta.workspacePath;
    }
    return item;
  };
  for (const r of configuredRepos()) {
    ensure(r.repo, { workspacePath: r.workspacePath || '', configured: true });
  }
  let totalUnread = 0;
  for (const card of visible) {
    if (card.hasUnreadUpdates) totalUnread += 1;
    for (const r of cardRepos(card)) {
      const item = ensure(r);
      item.count += 1;
      if (card.hasUnreadUpdates) item.unread += 1;
    }
  }
  return {
    totalCount: visible.length,
    totalUnread,
    repos: Array.from(map.values()),
  };
}

/** "#6247" for the active/default repository, "owner/repo#12" otherwise. */
export function linkRefLabel(link) {
  if (!link || !link.number) return '';
  const refRepo = activeRepo();
  return link.repo && refRepo && link.repo.toLowerCase() !== refRepo.toLowerCase() ? `${link.repo}#${link.number}` : `#${link.number}`;
}

export function ghRefLabel(card) {
  const first = cardLinks(card)[0];
  return first ? linkRefLabel(first) : '';
}

export function linkUrl(link) {
  if (!link) return '';
  if (link.url) return link.url;
  if (!link.number || !link.repo) return '';
  return `https://github.com/${link.repo}/issues/${link.number}`;
}

export function ghUrl(card) {
  const first = cardLinks(card)[0];
  return first ? linkUrl(first) : '';
}

export function linkStateInfo(link) {
  switch (link && link.state) {
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

export function ghStateInfo(card) {
  return linkStateInfo(cardLinks(card)[0] || { state: card.ghState });
}

export function linkIconName(link) {
  if (link && link.state === 'MERGED') return 'merge';
  return link && link.kind === 'issue' ? 'issue' : 'pr';
}

export function ghIconName(card) {
  return linkIconName(cardLinks(card)[0] || { state: card.ghState, kind: card.ghKind });
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

/** True when the given GitHub link was opened by the maintainer. */
export function isLinkMine(link) {
  const me = currentUser();
  return Boolean(me && link && link.author && link.author.toLowerCase() === me.toLowerCase());
}

/** True when the primary linked pull request or issue was written by the maintainer. */
export function isMine(card) {
  const first = cardLinks(card)[0];
  if (first) return isLinkMine(first);
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
 * Where the maintainer personally stands on a single linked item.
 * `kind` is "current" (reviewed/replied and nothing changed since), "stale"
 * (changed after the review/reply), "none" (an open PR of somebody else not
 * yet reviewed), or null when there is nothing to show.
 */
export function linkReviewInfo(link) {
  if (!link || !link.number || !link.lastSyncedAt || isLinkMine(link)) return null;
  const reviewState = link.myReviewState;
  const finished = link.state === 'MERGED' || link.state === 'CLOSED';
  const isIssue = link.kind === 'issue';
  if (reviewState) {
    const name = isIssue && reviewState === 'COMMENTED' ? 'Replied' : (MY_REVIEW_LABELS[reviewState] || reviewState);
    const detail =
      isIssue && reviewState === 'COMMENTED'
        ? 'You commented on this issue on GitHub'
        : (MY_REVIEW_DETAIL[reviewState] || 'You reviewed this');
    if (link.isStaleReview && !finished) {
      const label = isIssue ? (link.pendingFeedback || 'New replies') : 'Re-review needed';
      const title = isIssue
        ? `${detail}, and new replies arrived afterwards.`
        : `${detail}, and it was updated afterwards: new commits or new replies. Your last look: ${name}.`;
      return {
        kind: 'stale',
        label,
        cls: 'pill-warning',
        tone: 'is-warning',
        icon: isIssue ? 'message' : 'refresh',
        title,
      };
    }
    return { kind: 'current', label: `You: ${name}`, cls: 'pill-you', tone: 'is-you', icon: 'check', title: `${detail}. Nothing changed since.` };
  }
  if (link.kind === 'pr' && link.state === 'OPEN' && currentUser()) {
    return { kind: 'none', label: 'Not reviewed', cls: 'pill-muted', tone: '', icon: 'eye', title: 'You have not reviewed this pull request yet' };
  }
  return null;
}

/** Where the maintainer personally stands on the card's primary linked item. */
export function myReviewInfo(card) {
  return linkReviewInfo(cardLinks(card)[0]);
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
  const repo = state.filters.repo;
  for (const card of cards()) {
    if (hiddenOrchestrator(card)) continue;
    if (repo && !cardBelongsToRepo(card, repo)) continue;
    for (const link of cardLinks(card)) {
      if (!link.author) continue;
      const key = link.author.toLowerCase();
      if (!seen.has(key)) seen.set(key, link.author);
    }
  }
  return Array.from(seen.values()).sort((a, b) => a.toLowerCase().localeCompare(b.toLowerCase()));
}

function matchesAuthor(card, author) {
  if (!author) return true;
  const links = cardLinks(card);
  // Without a known login "Mine" cannot be decided, so it does not filter.
  if (author === AUTHOR_MINE) return !currentUser() || links.some(isLinkMine);
  const want = author.toLowerCase();
  return links.some((link) => Boolean(link.author) && link.author.toLowerCase() === want);
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
  const parts = [
    card.title,
    card.summary,
    card.notes,
    categoryLabel(card.category),
    (card.tags || []).join(' '),
  ];
  for (const link of cardLinks(card)) {
    parts.push(
      link.title,
      link.author ? `${link.author} @${link.author}` : '',
      link.repo,
      link.number ? `#${link.number} ${link.number}` : '',
      (link.labels || []).join(' '),
    );
  }
  return parts.join('\n').toLowerCase();
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
  if (f.repo && !cardBelongsToRepo(card, f.repo)) return false;
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
  if (card.hasUnreadUpdates) return true;
  for (const link of cardLinks(card)) {
    const open = link.state !== 'MERGED' && link.state !== 'CLOSED';
    if (link.hasUnreadUpdates || (open && (link.isStaleReview || link.pendingFeedback))) return true;
  }
  const open = card.ghState !== 'MERGED' && card.ghState !== 'CLOSED';
  return Boolean(open && (card.ghIsStaleReview || card.ghPendingFeedback));
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
  const repo = state.filters.repo;
  return cards().filter((c) => c.hasUnreadUpdates && !hiddenOrchestrator(c) && (!repo || cardBelongsToRepo(c, repo))).length;
}

export function otherReposUnreadCount() {
  const repo = state.filters.repo;
  if (!repo) return 0;
  return cards().filter((c) => c.hasUnreadUpdates && !hiddenOrchestrator(c) && !cardBelongsToRepo(c, repo)).length;
}
