// Application entry point: wiring of the header, filters, live updates,
// theme and notifications.

import { debounce, plural, relTime, storageGet, storageSet } from './util.js';
import { hydrateIcons, setIcon } from './icons.js';
import { api } from './api.js';
import { toast, toastError, withBusy, hasModal, closeTopModal } from './ui.js';
import { state, subscribe, emit, setBoard, setFilter, cards, unreadCount, ghRefLabel } from './state.js';
import { initBoard, renderBoard } from './board.js';
import { initDrawer, openDrawer, closeDrawer, isDrawerOpen, updateDrawer } from './drawer.js';
import { openNewTask, openColumns, openImport } from './modals.js';

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

// Header

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
    aoPill.className = 'pill pill-success';
    label.textContent = 'AO Connected';
    aoPill.title = `AO daemon at ${board.ao.url}. Last sync ${relTime(board.ao.lastSyncAt) || 'pending'}.`;
  } else {
    aoPill.className = 'pill pill-danger';
    label.textContent = 'AO Offline';
    aoPill.title = board.ao.error || `The AO daemon at ${board.ao.url} is not reachable.`;
  }
  if (!board) return;

  $('repo-context').textContent = board.settings.defaultRepo;

  const unread = unreadCount();
  const updates = $('updates-pill');
  updates.hidden = unread === 0;
  updates.querySelector('.pill-label').textContent = plural(unread, 'update');
  document.title = unread > 0 ? `(${unread}) Maintainer Board` : 'Maintainer Board';

  const sync = $('btn-sync');
  const span = sync.querySelector('.icon');
  if (!sync.dataset.busy) {
    span.classList.toggle('spin', board.github.syncing);
  }
  const every = Math.round(board.github.intervalSeconds / 60) >= 1 ? `${Math.round(board.github.intervalSeconds / 60)} min` : `${board.github.intervalSeconds}s`;
  sync.title = board.github.error
    ? `Last GitHub sync had errors: ${board.github.error}`
    : `Sync every linked pull request and issue now. Automatic sync runs every ${every}; last sync ${relTime(board.github.lastSyncAt) || 'pending'}.`;

  const f = state.filters;
  for (const button of $('filter-category').querySelectorAll('button')) button.classList.toggle('active', button.dataset.value === f.category);
  for (const button of $('filter-agent').querySelectorAll('button')) button.classList.toggle('active', button.dataset.value === f.agent);
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

  $('btn-new').addEventListener('click', () => openNewTask({ onCreated: (card) => openDrawer(card.id) }));
  $('btn-import').addEventListener('click', () => openImport({ onImported: scheduleRefresh }));
  $('btn-columns').addEventListener('click', () => openColumns({ onChanged: scheduleRefresh }));
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
    const button = e.currentTarget;
    button.dataset.busy = '1';
    withBusy(button, async () => {
      const report = await api.post('/api/github/sync');
      const parts = [`Synced ${plural(report.synced, 'item')}`];
      if (report.updated > 0) parts.push(`${plural(report.updated, 'new update')}`);
      if (report.moved > 0) parts.push(`${plural(report.moved, 'card')} moved`);
      if (report.failed > 0) parts.push(`${report.failed} failed`);
      toast(`${parts.join(', ')}.`, report.failed > 0 ? 'warning' : 'success');
      if (report.failed > 0 && report.errors.length > 0) toast(report.errors[0], 'error');
    }).finally(() => {
      delete button.dataset.busy;
      scheduleRefresh();
    });
  });

  const search = $('search');
  search.addEventListener(
    'input',
    debounce(() => setFilter('search', search.value), 80),
  );
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      if (search.value) {
        search.value = '';
        setFilter('search', '');
      } else {
        search.blur();
      }
    }
  });
  $('filter-category').addEventListener('click', (e) => {
    const button = e.target.closest('button');
    if (button) setFilter('category', button.dataset.value);
  });
  $('filter-agent').addEventListener('click', (e) => {
    const button = e.target.closest('button');
    if (button) setFilter('agent', button.dataset.value);
  });
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
      if (closeTopModal()) return;
      if (isDrawerOpen()) {
        if (isTyping(document.activeElement)) document.activeElement.blur();
        else closeDrawer();
      }
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey || isTyping(e.target) || hasModal()) return;
    if (e.key === '/') {
      e.preventDefault();
      search.focus();
      search.select();
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
