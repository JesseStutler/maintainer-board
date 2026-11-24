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

const FILTER_KEY = 'mb.filters';
const DEFAULT_FILTERS = { search: '', category: '', agent: '', updatesOnly: false, showDone: true };

export const state = {
  board: null,
  filters: { ...DEFAULT_FILTERS, ...storageGet(FILTER_KEY, {}), search: '' },
  selectedCardId: null,
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
  if (session.needsResume) return { label: 'Not Resumed', cls: 'is-resume' };
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
      return { label: 'CI passing', short: 'CI', cls: 'pill-success', tone: 'is-success', icon: 'check-circle' };
    case 'FAILURE':
      return { label: 'CI failing', short: 'CI', cls: 'pill-danger', tone: 'is-danger', icon: 'x-circle' };
    case 'PENDING':
      return { label: 'CI running', short: 'CI', cls: 'pill-warning', tone: 'is-warning', icon: 'clock' };
    default:
      return null;
  }
}

export function reviewInfo(decision) {
  switch (decision) {
    case 'APPROVED':
      return { label: 'Approved', cls: 'pill-success', tone: 'is-success', icon: 'check' };
    case 'CHANGES_REQUESTED':
      return { label: 'Changes requested', cls: 'pill-danger', tone: 'is-danger', icon: 'alert' };
    case 'REVIEW_REQUIRED':
      return { label: 'Review required', cls: 'pill-muted', tone: '', icon: 'user' };
    default:
      return null;
  }
}

/** Cards auto-created for orchestrator sessions are hidden unless enabled. */
function hiddenOrchestrator(card) {
  if (!state.board || state.board.settings.showOrchestrator) return false;
  return card.origin === 'ao' && card.session && card.session.kind === 'orchestrator';
}

function matchesSearch(card, tokens) {
  if (tokens.length === 0) return true;
  const haystack = [
    card.title,
    card.summary,
    card.notes,
    card.ghTitle,
    card.ghAuthor,
    card.ghRepo,
    card.ghNumber ? `#${card.ghNumber} ${card.ghNumber}` : '',
    card.aoSessionId,
    categoryLabel(card.category),
    (card.tags || []).join(' '),
    (card.ghLabels || []).join(' '),
    card.session ? `${card.session.harness} ${card.session.branch || ''}` : '',
  ]
    .join('\n')
    .toLowerCase();
  return tokens.every((token) => haystack.includes(token));
}

export function matchesFilters(card) {
  const f = state.filters;
  if (hiddenOrchestrator(card)) return false;
  if (f.category && card.category !== f.category) return false;
  if (f.agent && (!card.session || card.session.harness !== f.agent)) return false;
  if (f.updatesOnly && !card.hasUnreadUpdates) return false;
  const tokens = f.search.toLowerCase().split(/\s+/).filter(Boolean);
  return matchesSearch(card, tokens);
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

export function unreadCount() {
  return cards().filter((c) => c.hasUnreadUpdates && !hiddenOrchestrator(c)).length;
}
