// Application entry point: wiring of the header, filters, live updates,
// theme and notifications.

import { h, debounce, plural, relTime, storageGet, storageSet } from './util.js';
import { hydrateIcons, setIcon, icon } from './icons.js';
import { api } from './api.js';
import { toast, toastError, withBusy, hasModal, closeTopModal, spinner, elapsed } from './ui.js';
import {
  state,
  subscribe,
  emit,
  setBoard,
  setFilter,
  toggleGlobalCompact,
  cards,
  unreadCount,
  otherReposUnreadCount,
  boardRepoSummary,
  ghRefLabel,
  currentUser,
  cardAuthors,
  AUTHOR_MINE,
  searchKey,
  harnessLabel,
  matchesFilters,
} from './state.js';
import { initBoard, renderBoard } from './board.js';
import { initDrawer, openDrawer, closeDrawer, isDrawerOpen, updateDrawer, isDrawerExpanded, setDrawerExpanded, toggleDrawerExpanded } from './drawer.js';
import { openNewTask, openColumns, openImport, openSettings, openAddRepo } from './modals.js';

const THEME_KEY = 'mb.theme';
const NOTIFY_KEY = 'mb.notify';
const SAFETY_POLL_MS = 15000;

const $ = (id) => document.getElementById(id);

let serverReachable = true;
let notifyEnabled = false;
let notifyPrimed = false;
const seenCycles = new Map();

// Board loading

let refreshing = null;
let refreshQueued = false;

async function refreshBoard() {
  if (refreshing) {
    refreshQueued = true;
    return refreshing;
  }
  refreshing = (async () => {
    try {
      const board = await api.get('/api/board');
      serverReachable = true;
      setBoard(board);
      state.firstLoad = false;
    } catch (err) {
      const wasReachable = serverReachable;
      serverReachable = false;
      if (wasReachable) {
        if (state.board) toast(err.message, 'error');
        else $('board').replaceChildren(Object.assign(document.createElement('div'), { className: 'board-empty', textContent: err.message }));
      }
      renderHeader();
    } finally {
      refreshing = null;
      if (refreshQueued) {
        refreshQueued = false;
        refreshBoard();
      }
    }
  })();
  return refreshing;
}

const scheduleRefresh = debounce(refreshBoard, 100);

function connectEvents() {
  const source = new EventSource('/api/events');
  source.addEventListener('board', scheduleRefresh);
  source.addEventListener('open', scheduleRefresh);
  // EventSource reconnects on its own; the safety poll covers the gap.
}

// Search

let contentSeq = 0;
let contentVersion = -1;

/**
 * Asks the server which cards match the search box through their session
 * ID or the text of their conversation. The card's own fields are matched
 * in the browser, so this only adds results; it never delays typing.
 */
async function runContentSearch() {
  const key = searchKey();
  const seq = ++contentSeq;
  if (!key) {
    if (state.search.key) {
      state.search = { key: '', matches: new Map() };
      emit();
    }
    return;
  }
  const version = state.board ? state.board.version : -1;
  try {
    const res = await api.get(`/api/search?q=${encodeURIComponent(state.filters.search)}`);
    if (seq !== contentSeq) return;
    contentVersion = version;
    state.search = { key, matches: new Map(res.matches.map((m) => [m.cardId, m])) };
    emit();
  } catch (err) {
    // The local match on card fields still works; try again on the next change.
  }
}

const scheduleContentSearch = debounce(runContentSearch, 200);

function clearAISearch() {
  if (state.ai.status === 'idle') return;
  state.ai = { status: 'idle', query: '', engine: '', error: '', matches: new Map() };
}

function clearSearch() {
  const input = $('search');
  input.value = '';
  clearAISearch();
  setFilter('search', '');
  runContentSearch();
}

async function runAISearch() {
  const input = $('search');
  const query = input.value.trim();
  if (!query) {
    input.focus();
    toast('Type what you are looking for, then start AI Search.', 'info');
    return;
  }
  if (state.ai.status === 'running') return;
  const button = $('btn-ai-search');
  state.ai = { status: 'running', query, engine: '', error: '', matches: new Map() };
  emit();
  await withBusy(
    button,
    async () => {
      const res = await api.post('/api/search/ai', { query });
      // A search that was cleared while it ran is not shown.
      if (state.ai.status !== 'running' || state.ai.query !== query) return;
      state.ai = {
        status: 'done',
        query: res.query,
        engine: res.engine,
        error: '',
        matches: new Map(res.matches.map((m, i) => [m.cardId, { reason: m.reason, rank: i + 1 }])),
      };
    },
    (err) => {
      if (state.ai.status === 'running' && state.ai.query === query) {
        state.ai = { status: 'error', query, engine: '', error: err.message, matches: new Map() };
      }
      return true;
    },
    'Searching with AI...',
  );
  emit();
}

/** Banner under the filter bar: progress, result or failure of AI Search. */
function renderSearchBanner() {
  const banner = $('search-banner');
  const ai = state.ai;
  const key = JSON.stringify([ai.status, ai.query, ai.engine, ai.error, ai.matches.size, ai.status === 'done' ? cards().filter((c) => ai.matches.has(c.id) && matchesFilters(c)).length : 0]);
  if (banner.dataset.key === key) return;
  banner.dataset.key = key;
  banner.hidden = ai.status === 'idle';
  banner.className = `search-banner is-${ai.status}`;
  if (ai.status === 'idle') {
    banner.replaceChildren();
    return;
  }
  const clear = (label) =>
    h(
      'button',
      {
        class: 'btn btn-sm',
        type: 'button',
        dataset: { action: 'clear-ai' },
        onClick: () => {
          clearAISearch();
          emit();
        },
      },
      icon('x', 13),
      h('span', null, label),
    );
  const quoted = h('span', { class: 'query' }, `"${ai.query}"`);
  if (ai.status === 'running') {
    banner.replaceChildren(
      spinner(15),
      h('span', { class: 'banner-text' }, h('strong', null, 'Searching with AI:'), ' reading the board for ', quoted),
      elapsed(),
      h('span', { class: 'spacer' }),
      clear('Cancel'),
      h('span', { class: 'progress-strip', 'aria-hidden': 'true' }),
    );
  } else if (ai.status === 'error') {
    banner.replaceChildren(icon('x-circle', 15), h('span', { class: 'banner-text' }, h('strong', null, 'AI Search failed:'), ` ${ai.error}`), h('span', { class: 'spacer' }), clear('Dismiss'));
  } else {
    const total = ai.matches.size;
    const shown = cards().filter((c) => ai.matches.has(c.id) && matchesFilters(c)).length;
    const hidden = total - shown;
    banner.replaceChildren(
      icon('sparkles', 15),
      h('span', { class: 'banner-text' }, h('strong', null, 'AI Search:'), ` ${total} ${total === 1 ? 'match' : 'matches'} for `, quoted),
      h('span', { class: 'hint' }, `answered by ${harnessLabel(ai.engine)}${hidden > 0 ? `; ${hidden} hidden by the other filters` : ''}`),
      h('span', { class: 'spacer' }),
      clear('Clear'),
    );
  }
}

// Header

let authorOptionsKey = '';

/**
 * Fills the author filter: everyone, the maintainer, then each author that
 * has a card on the board. A selected author who left the board stays
 * listed, so an empty board still explains itself.
 */
function renderAuthorFilter() {
  const select = $('filter-author');
  const me = currentUser();
  const selected = state.filters.author || '';
  const authors = cardAuthors();
  if (selected && selected !== AUTHOR_MINE && !authors.some((a) => a.toLowerCase() === selected.toLowerCase())) authors.push(selected);
  const key = JSON.stringify([me, authors]);
  if (key !== authorOptionsKey) {
    authorOptionsKey = key;
    const options = [new Option('All Authors', '')];
    if (me) options.push(new Option(`Mine (@${me})`, AUTHOR_MINE));
    for (const author of authors) options.push(new Option(`@${author}`, author));
    select.replaceChildren(...options);
  }
  // "Mine" needs a known login; fall back to everyone until it is detected.
  const value = Array.from(select.options).some((o) => o.value === selected) ? selected : '';
  if (select.value !== value) select.value = value;
  select.classList.toggle('is-active', value !== '');
}

let repoMenuOpen = false;

function closeRepoMenu() {
  if (!repoMenuOpen) return;
  repoMenuOpen = false;
  const menu = $('repo-menu');
  const btn = $('btn-repo-switcher');
  if (menu) menu.hidden = true;
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

function positionRepoMenu() {
  const menu = $('repo-menu');
  const btn = $('btn-repo-switcher');
  if (!menu || !btn) return;
  const r = btn.getBoundingClientRect();
  const maxLeft = Math.max(8, window.innerWidth - 340);
  menu.style.left = `${Math.max(8, Math.min(r.left, maxLeft))}px`;
  menu.style.top = `${r.bottom + 6}px`;
}

function renderRepoMenu() {
  const menu = $('repo-menu');
  if (!menu || !state.board) return;
  const summary = boardRepoSummary();
  const selected = (state.filters.repo || '').toLowerCase();

  const makeItem = ({ label, sub, count, unread, active, onClick, actionBtn }) => {
    const meta = [];
    if (unread > 0) meta.push(h('span', { class: 'repo-menu-badge is-unread', title: `${plural(unread, 'unread update')}` }, String(unread)));
    meta.push(h('span', { class: 'repo-menu-count mono' }, String(count)));
    if (actionBtn) meta.push(actionBtn);
    return h(
      'div',
      {
        class: ['repo-menu-item', active && 'is-active'],
        role: 'menuitemradio',
        'aria-checked': String(active),
        tabIndex: 0,
        onClick,
        onKeydown: (e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            onClick();
          }
        },
      },
      h('span', { class: 'repo-menu-check' }, active ? icon('check', 13) : null),
      h('span', { class: 'repo-menu-main' }, h('span', { class: 'repo-menu-label mono' }, label), sub ? h('span', { class: 'repo-menu-sub mono', title: sub }, sub) : null),
      h('span', { class: 'repo-menu-meta' }, ...meta),
    );
  };

  const nodes = [
    makeItem({
      label: 'All Repos',
      sub: `Default: ${state.board.settings.defaultRepo}`,
      count: summary.totalCount,
      unread: summary.totalUnread,
      active: selected === '',
      onClick: () => {
        setFilter('repo', '');
        closeRepoMenu();
      },
    }),
  ];

  if (summary.repos.length > 0) {
    nodes.push(h('div', { class: 'repo-menu-sep', role: 'separator' }));
    for (const item of summary.repos) {
      const editBtn = h(
        'button',
        {
          class: 'repo-menu-edit',
          type: 'button',
          title: item.configured ? `Configure ${item.repo}` : `Save ${item.repo} and bind a local workspace`,
          'aria-label': `Configure ${item.repo}`,
          onClick: (e) => {
            e.stopPropagation();
            closeRepoMenu();
            openAddRepo({ initialRepo: item.repo, initialPath: item.workspacePath || '', onAdded: scheduleRefresh });
          },
        },
        icon('pencil', 11),
      );
      nodes.push(
        makeItem({
          label: item.repo,
          sub: item.workspacePath || (item.configured ? 'No local workspace bound' : 'Discovered from cards'),
          count: item.count,
          unread: item.unread,
          active: selected === item.repo.toLowerCase(),
          actionBtn: editBtn,
          onClick: () => {
            setFilter('repo', item.repo);
            closeRepoMenu();
          },
        }),
      );
    }
  }

  nodes.push(
    h('div', { class: 'repo-menu-sep', role: 'separator' }),
    h(
      'button',
      {
        class: 'repo-menu-action',
        type: 'button',
        role: 'menuitem',
        onClick: () => {
          closeRepoMenu();
          openAddRepo({ onAdded: scheduleRefresh });
        },
      },
      icon('plus', 13),
      h('span', null, 'Add Repository...'),
    ),
    h(
      'button',
      {
        class: 'repo-menu-action is-muted',
        type: 'button',
        role: 'menuitem',
        onClick: () => {
          closeRepoMenu();
          openSettings({ onSaved: scheduleRefresh });
        },
      },
      icon('settings', 13),
      h('span', null, 'Manage in Settings...'),
    ),
  );

  menu.replaceChildren(...nodes);
}

function renderHeader() {
  const board = state.board;
  const aoPill = $('ao-status');
  const label = aoPill.querySelector('.pill-label');
  if (!serverReachable) {
    aoPill.className = 'pill pill-danger';
    label.textContent = 'Server Offline';
    aoPill.title = 'The Maintainer Board server is not reachable.';
  } else if (!board) {
    aoPill.className = 'pill pill-muted';
    label.textContent = 'AO Connecting';
  } else if (board.ao.connected) {
    const isNative = board.ao.url === 'native' || String(board.ao.url || '').startsWith('native://');
    aoPill.className = 'pill pill-success';
    label.textContent = isNative ? 'Native Ready' : 'AO Connected';
    aoPill.title = isNative
      ? `Native Codex & Copilot runtime. Last sync ${relTime(board.ao.lastSyncAt) || 'pending'}.`
      : `AO daemon at ${board.ao.url}. Last sync ${relTime(board.ao.lastSyncAt) || 'pending'}.`;
  } else {
    const isNative = board.ao.url === 'native' || String(board.ao.url || '').startsWith('native://');
    aoPill.className = 'pill pill-danger';
    label.textContent = isNative ? 'Runtime Offline' : 'AO Offline';
    aoPill.title = board.ao.error || (isNative ? 'The local native runtime is not reachable.' : `The AO daemon at ${board.ao.url} is not reachable.`);
  }
  if (!board) return;

  const selectedRepo = state.filters.repo || '';
  const repoBtn = $('btn-repo-switcher');
  const repoLabel = $('repo-context');
  const otherDot = $('repo-other-dot');
  const summary = boardRepoSummary();
  if (selectedRepo) {
    repoLabel.textContent = selectedRepo;
    repoBtn.classList.add('is-scoped');
    repoBtn.title = `Showing ${selectedRepo}. Click to switch repository or add a new one.`;
  } else if (summary.repos.length > 1) {
    repoLabel.textContent = `All Repos (${summary.repos.length})`;
    repoBtn.classList.remove('is-scoped');
    repoBtn.title = `Showing all repositories. Bare #123 references resolve to ${board.settings.defaultRepo}.`;
  } else {
    repoLabel.textContent = board.settings.defaultRepo;
    repoBtn.classList.remove('is-scoped');
    repoBtn.title = `Showing ${board.settings.defaultRepo}. Click to add another repository board.`;
  }
  const otherUnread = otherReposUnreadCount();
  otherDot.hidden = otherUnread === 0;
  if (otherUnread > 0) {
    otherDot.title = `${plural(otherUnread, 'unread update')} in other repositories`;
  }
  if (repoMenuOpen) renderRepoMenu();

  const unread = unreadCount();
  const updates = $('updates-pill');
  updates.hidden = unread === 0;
  updates.querySelector('.pill-label').textContent = plural(unread, 'update');
  document.title = unread > 0 ? `(${unread}) Maintainer Board` : 'Maintainer Board';

  const sync = $('btn-sync');
  const span = sync.querySelector('.icon');
  // While the button runs its own sync it shows a spinner instead.
  if (span && sync.getAttribute('aria-busy') !== 'true') {
    span.classList.toggle('spin', board.github.syncing);
  }
  const every = Math.round(board.github.intervalSeconds / 60) >= 1 ? `${Math.round(board.github.intervalSeconds / 60)} min` : `${board.github.intervalSeconds}s`;
  sync.title = board.github.error
    ? `Last GitHub sync had errors: ${board.github.error}`
    : `Sync every linked pull request and issue now. Automatic sync runs every ${every}; last sync ${relTime(board.github.lastSyncAt) || 'pending'}.`;

  const f = state.filters;
  for (const button of $('filter-category').querySelectorAll('button')) button.classList.toggle('active', button.dataset.value === f.category);
  for (const button of $('filter-agent').querySelectorAll('button')) button.classList.toggle('active', button.dataset.value === f.agent);
  renderAuthorFilter();
  $('btn-search-clear').hidden = !f.search && state.ai.status === 'idle';
  renderSearchBanner();
  // Cards change while a search is active: refresh the server-side matches.
  if (searchKey() && board.version !== contentVersion) {
    contentVersion = board.version;
    scheduleContentSearch();
  }
  $('toggle-compact').setAttribute('aria-pressed', String(Boolean(f.compact)));
  document.body.classList.toggle('is-compact', Boolean(f.compact));
  const sortSelect = $('filter-sort');
  const sortVal = f.sort || 'manual';
  if (sortSelect.value !== sortVal) sortSelect.value = sortVal;
  sortSelect.classList.toggle('is-active', sortVal !== 'manual');
  $('toggle-updates').setAttribute('aria-pressed', String(f.updatesOnly));
  $('toggle-done').setAttribute('aria-pressed', String(f.showDone));
  $('toggle-orchestrator').setAttribute('aria-pressed', String(board.settings.showOrchestrator));
}

// Theme

function applyTheme(theme) {
  document.documentElement.setAttribute('data-theme', theme);
  $('theme-label').textContent = theme === 'dark' ? 'Dark' : 'Light';
  setIcon($('btn-theme').querySelector('.icon'), theme === 'dark' ? 'moon' : 'sun');
  $('btn-theme').title = theme === 'dark' ? 'Dark theme is on. Click for light.' : 'Light theme is on. Click for dark.';
}

function currentTheme() {
  return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
}

// Notifications

function renderNotifyButton() {
  const button = $('btn-notify');
  button.setAttribute('aria-pressed', String(notifyEnabled));
  setIcon(button.querySelector('.icon'), notifyEnabled ? 'bell' : 'bell-off');
  button.title = notifyEnabled
    ? 'Browser notifications for new updates are on. Click to turn off.'
    : 'Browser notifications for new updates are off. Click to enable.';
}

async function toggleNotifications() {
  if (notifyEnabled) {
    notifyEnabled = false;
    storageSet(NOTIFY_KEY, false);
    renderNotifyButton();
    return;
  }
  if (!('Notification' in window)) {
    toast('This browser does not support notifications.', 'warning');
    return;
  }
  let permission = Notification.permission;
  if (permission === 'default') permission = await Notification.requestPermission();
  if (permission !== 'granted') {
    toast('Notifications are blocked for this site in the browser settings.', 'warning');
    return;
  }
  notifyEnabled = true;
  storageSet(NOTIFY_KEY, true);
  renderNotifyButton();
  toast('Notifications enabled for new pull request and issue updates.', 'success');
}

function checkNotifications() {
  if (!state.board) return;
  const current = new Set();
  for (const card of cards()) {
    if (!card.hasUnreadUpdates) continue;
    current.add(card.id);
    const previous = seenCycles.get(card.id) || 0;
    if (card.updateCycles > previous && notifyPrimed && notifyEnabled) {
      try {
        const ref = ghRefLabel(card);
        const note = new Notification(ref ? `Update on ${ref}` : 'Update', {
          body: `${card.title}\n${card.updateSummary}`,
          tag: `mb-${card.id}`,
          silent: true,
        });
        note.onclick = () => {
          window.focus();
          openDrawer(card.id);
          note.close();
        };
      } catch (err) {
        // Notification construction can fail when permission was revoked.
      }
    }
    seenCycles.set(card.id, card.updateCycles);
  }
  for (const id of Array.from(seenCycles.keys())) {
    if (!current.has(id)) seenCycles.delete(id);
  }
  notifyPrimed = true;
}

// Rendering pipeline

function render() {
  renderHeader();
  renderBoard();
  updateDrawer();
  checkNotifications();
}

/** Opens the card of a session that was just imported, once the board has it. */
async function openImportedCard(cardId) {
  await refreshBoard();
  openDrawer(cardId);
}

function isTyping(target) {
  if (!target) return false;
  const tag = target.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || target.isContentEditable;
}

function init() {
  hydrateIcons();
  applyTheme(currentTheme());
  notifyEnabled = storageGet(NOTIFY_KEY, false) && 'Notification' in window && Notification.permission === 'granted';
  renderNotifyButton();

  initBoard({
    onOpenCard: (id) => openDrawer(id),
    onAddCard: (columnId) => openNewTask({ columnId, onCreated: (card) => openDrawer(card.id) }),
    onRefresh: scheduleRefresh,
  });
  initDrawer({ onRefresh: scheduleRefresh });
  subscribe(render);

  $('btn-repo-switcher').addEventListener('click', (e) => {
    e.stopPropagation();
    if (!state.board) return;
    if (repoMenuOpen) {
      closeRepoMenu();
      return;
    }
    repoMenuOpen = true;
    renderRepoMenu();
    positionRepoMenu();
    $('repo-menu').hidden = false;
    $('btn-repo-switcher').setAttribute('aria-expanded', 'true');
  });
  document.addEventListener('click', (e) => {
    if (repoMenuOpen && !$('repo-switcher').contains(e.target)) closeRepoMenu();
  });
  window.addEventListener('resize', () => {
    if (repoMenuOpen) positionRepoMenu();
  });

  $('btn-new').addEventListener('click', () => openNewTask({ onCreated: (card) => openDrawer(card.id) }));
  $('btn-import').addEventListener('click', () => openImport({ onImported: scheduleRefresh, onOpenCard: openImportedCard }));
  $('btn-columns').addEventListener('click', () => openColumns({ onChanged: scheduleRefresh }));
  $('btn-settings').addEventListener('click', () => openSettings({ onSaved: scheduleRefresh }));
  $('btn-theme').addEventListener('click', () => {
    const next = currentTheme() === 'dark' ? 'light' : 'dark';
    try {
      localStorage.setItem(THEME_KEY, next);
    } catch (err) {
      // The theme then applies to this page view only.
    }
    applyTheme(next);
  });
  $('btn-notify').addEventListener('click', toggleNotifications);
  $('btn-sync').addEventListener('click', (e) => {
    withBusy(
      e.currentTarget,
      async () => {
        const report = await api.post('/api/github/sync');
        const parts = [`Synced ${plural(report.synced, 'item')}`];
        if (report.updated > 0) parts.push(`${plural(report.updated, 'new update')}`);
        if (report.moved > 0) parts.push(`${plural(report.moved, 'card')} moved`);
        if (report.failed > 0) parts.push(`${report.failed} failed`);
        toast(`${parts.join(', ')}.`, report.failed > 0 ? 'warning' : 'success');
        if (report.failed > 0 && report.errors.length > 0) toast(report.errors[0], 'error');
      },
      null,
      'Syncing...',
    ).finally(scheduleRefresh);
  });

  const search = $('search');
  search.addEventListener(
    'input',
    debounce(() => {
      // Typing a new query leaves the AI result behind.
      clearAISearch();
      setFilter('search', search.value);
      scheduleContentSearch();
    }, 80),
  );
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      if (search.value || state.ai.status !== 'idle') clearSearch();
      else search.blur();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (e.ctrlKey || e.metaKey) {
        runAISearch();
      } else {
        setFilter('search', search.value);
        runContentSearch();
      }
    }
  });
  $('btn-ai-search').addEventListener('click', runAISearch);
  $('btn-search-clear').addEventListener('click', clearSearch);
  $('filter-category').addEventListener('click', (e) => {
    const button = e.target.closest('button');
    if (button) setFilter('category', button.dataset.value);
  });
  $('filter-agent').addEventListener('click', (e) => {
    const button = e.target.closest('button');
    if (button) setFilter('agent', button.dataset.value);
  });
  $('filter-author').addEventListener('change', (e) => setFilter('author', e.target.value));
  $('filter-sort').addEventListener('change', (e) => setFilter('sort', e.target.value));
  $('toggle-compact').addEventListener('click', () => toggleGlobalCompact());
  $('toggle-updates').addEventListener('click', () => setFilter('updatesOnly', !state.filters.updatesOnly));
  $('updates-pill').addEventListener('click', () => setFilter('updatesOnly', !state.filters.updatesOnly));
  $('toggle-done').addEventListener('click', () => setFilter('showDone', !state.filters.showDone));
  $('toggle-orchestrator').addEventListener('click', async () => {
    if (!state.board) return;
    const next = !state.board.settings.showOrchestrator;
    try {
      const settings = await api.patch('/api/settings', { showOrchestrator: next });
      state.board.settings = { ...state.board.settings, ...settings };
      emit();
      scheduleRefresh();
    } catch (err) {
      toastError(err);
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      if (repoMenuOpen) {
        closeRepoMenu();
        return;
      }
      if (closeTopModal()) return;
      if (isDrawerOpen()) {
        // One step back at a time: leave the field, then the expanded
        // workspace, then the card.
        if (isTyping(document.activeElement)) document.activeElement.blur();
        else if (isDrawerExpanded()) setDrawerExpanded(false);
        else closeDrawer();
      }
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey || isTyping(e.target) || hasModal()) return;
    if (e.key === '/') {
      if (isDrawerExpanded()) return;
      e.preventDefault();
      search.focus();
      search.select();
    } else if (e.key === 'f' && isDrawerOpen()) {
      e.preventDefault();
      toggleDrawerExpanded();
    } else if (e.key === 'n') {
      e.preventDefault();
      openNewTask({ onCreated: (card) => openDrawer(card.id) });
    }
  });

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) scheduleRefresh();
  });

  refreshBoard();
  connectEvents();
  setInterval(refreshBoard, SAFETY_POLL_MS);
  // Relative timestamps age even when nothing else changes.
  setInterval(() => {
    if (state.board && !document.hidden) emit();
  }, 60000);
}

init();
