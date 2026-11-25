// Card workspace: metadata, summary, GitHub tracking, notes, audit timeline
// and the embedded AO chat. It opens as a resizable side drawer next to the
// board and expands to a full-width, two-pane workspace on demand.

import { h, clear, relTime, fmtDateTime, fmtClock, uuid, autoGrow, plural, storageGet, storageSet } from './util.js';
import { icon, brandIcon } from './icons.js';
import { api, cardPath, sessionPath } from './api.js';
import { renderMarkdown } from './markdown.js';
import { createModelPicker, fillModelSelect, fillEffortSelect, effectiveModel } from './models.js';
import { toast, toastError, withBusy, confirmDialog, actionDialog, setButtonBusy, spinner, thinkingDots, statusStrip, elapsed, segmented, select } from './ui.js';
import {
  state,
  emit,
  CATEGORIES,
  columns,
  cardById,
  columnByRole,
  harnessLabel,
  activityInfo,
  isSessionActive,
  ghRefLabel,
  ghUrl,
  ghStateInfo,
  ghIconName,
  ciInfo,
  reviewInfo,
  myReviewInfo,
  isMine,
  currentUser,
} from './state.js';

// The width key is versioned: widths saved by the first release were
// clamped to its narrower drawer and would defeat the wider default.
const WIDTH_KEY = 'mb.drawerWidth.v2';
const COLLAPSE_KEY = 'mb.detailsCollapsed';
const RAIL_KEY = 'mb.railCollapsed';
const SECTIONS_KEY = 'mb.sectionsCollapsed';
const MIN_WIDTH = 520;
const MAX_WIDTH_RATIO = 0.92;
const RESIZE_STEP = 32;
const CHAT_PAGE = 60;
const POLL_ACTIVE_MS = 2500;
const POLL_IDLE_MS = 8000;
const POLL_HIDDEN_MS = 20000;
const LONG_MESSAGE = 6000;

const ACTIVITY_ICONS = {
  command: 'terminal',
  file_change: 'file',
  plan: 'columns',
  reasoning: 'sparkles',
  approval: 'alert',
  error: 'x-circle',
  mcp_tool: 'zap',
  user_input: 'user',
  ao_action: 'info',
  auto_review: 'pr',
  usage: 'info',
  system: 'info',
};

let ctx = null;
let onRefresh = () => {};

export function initDrawer(options) {
  onRefresh = options.onRefresh || onRefresh;
  applyWidth(storageGet(WIDTH_KEY, null));
}

export function isDrawerOpen() {
  return ctx !== null;
}

export function isDrawerExpanded() {
  return Boolean(ctx && ctx.expanded);
}

/**
 * Switches between the side drawer and the full-width workspace. The same
 * elements are kept, so drafts, scroll position and open editors survive.
 */
export function setDrawerExpanded(expanded) {
  if (!ctx || ctx.expanded === expanded) return;
  const atEnd = ctx.chat ? nearBottom() : false;
  const anchor = ctx.chat && !atEnd ? chatAnchor() : null;
  const focused = document.activeElement;
  ctx.expanded = expanded;
  ctx.el.classList.toggle('is-expanded', expanded);
  document.body.classList.toggle('drawer-expanded', expanded);
  placeControls();
  delete ctx.sigs.top;
  delete ctx.sigs.sessionBar;
  updateDrawer();
  autoGrow(ctx.els.title);
  // The conversation reflows to another width: keep the reader where they
  // were, at the end or at the same message.
  if (atEnd) scrollChatToEnd(false);
  else if (anchor) restoreChatAnchor(anchor);
  if (focused && focused !== document.activeElement && ctx.el.contains(focused)) focused.focus({ preventScroll: true });
}

/** The first conversation item in view and how far it is from the top. */
function chatAnchor() {
  const scroller = ctx.els.chatScroll;
  const top = scroller.getBoundingClientRect().top;
  for (const el of scroller.children) {
    const rect = el.getBoundingClientRect();
    if (rect.bottom > top) return { el, offset: rect.top - top };
  }
  return null;
}

function restoreChatAnchor(anchor) {
  const scroller = ctx.els.chatScroll;
  if (!anchor.el.isConnected) return;
  scroller.scrollTop += anchor.el.getBoundingClientRect().top - scroller.getBoundingClientRect().top - anchor.offset;
}

export function toggleDrawerExpanded() {
  if (ctx) setDrawerExpanded(!ctx.expanded);
}

function widthBounds() {
  const min = Math.min(MIN_WIDTH, window.innerWidth);
  return { min, max: Math.max(min, Math.floor(window.innerWidth * MAX_WIDTH_RATIO)) };
}

/**
 * Sets the drawer width in pixels, clamped to the allowed range. A falsy
 * value removes the override and restores the default from the stylesheet.
 */
function applyWidth(px) {
  const root = document.documentElement;
  const value = Number(px);
  if (!value) {
    root.style.removeProperty('--drawer-w');
    return null;
  }
  const { min, max } = widthBounds();
  const width = Math.round(Math.min(Math.max(value, min), max));
  root.style.setProperty('--drawer-w', `${width}px`);
  return width;
}

/** Replaces a card in the shared state with a fresher copy from the API. */
function applyCard(card) {
  if (!state.board || !card) return;
  const list = state.board.cards;
  const index = list.findIndex((c) => c.id === card.id);
  if (index >= 0) list[index] = card;
  else list.push(card);
  emit();
}

async function patchCard(body) {
  const card = await api.patch(cardPath(ctx.cardId), body);
  applyCard(card);
  return card;
}

export function openDrawer(cardId) {
  if (ctx && ctx.cardId === cardId) return;
  const card = cardById(cardId);
  if (!card) return;
  closeDrawer(true);
  state.selectedCardId = cardId;
  ctx = build(card);
  document.getElementById('drawer-root').appendChild(ctx.el);
  document.body.classList.add('drawer-open');
  autoGrow(ctx.els.title);
  if (card.aoSessionId) startChat(card.aoSessionId);
  emit();
}

export function closeDrawer(silent = false) {
  if (!ctx) return;
  stopChat();
  ctx.el.remove();
  ctx = null;
  state.selectedCardId = null;
  document.body.classList.remove('drawer-open', 'drawer-expanded');
  if (!silent) emit();
}

/** Renders a section only when the data it depends on changed. */
function section(key, signature, render) {
  const sig = JSON.stringify(signature);
  if (ctx.sigs[key] === sig) return;
  ctx.sigs[key] = sig;
  render();
}

function build(card) {
  const els = {};

  els.category = select(CATEGORIES, card.category, { title: 'Category', 'aria-label': 'Category' });
  els.category.addEventListener('change', () => patchCard({ category: els.category.value }).catch(failAndResync));

  els.column = h('select', { class: 'select', title: 'Column', 'aria-label': 'Column' });
  els.column.addEventListener('change', () => patchCard({ columnId: els.column.value }).catch(failAndResync));

  els.autoMove = h('input', { type: 'checkbox', 'aria-label': 'Auto-move' });
  els.autoMove.addEventListener('change', () => patchCard({ autoMove: els.autoMove.checked }).catch(failAndResync));

  els.openGitHub = h(
    'a',
    { class: 'btn btn-sm btn-collapsible', target: '_blank', rel: 'noopener noreferrer', title: 'Open the linked pull request or issue on GitHub' },
    icon('external', 14),
    h('span', { class: 'btn-label' }, 'Open GitHub'),
  );

  // Category, column and auto-move sit in the header of the side drawer
  // and move into the Controls section of the context rail when expanded.
  els.controls = h(
    'div',
    { class: 'drawer-controls' },
    h('label', { class: 'control' }, h('span', { class: 'control-label' }, 'Category'), els.category),
    h('label', { class: 'control' }, h('span', { class: 'control-label' }, 'Column'), els.column),
    h(
      'label',
      { class: 'switch', title: 'Allow automatic column moves when the linked pull request or issue changes' },
      els.autoMove,
      h('span', { class: 'track' }),
      h('span', { class: 'switch-label' }, 'Auto-move'),
    ),
  );
  els.controlsSlot = h('div', { class: 'panel controls-panel' });
  els.controlsSection = h('section', { class: 'section section-controls', hidden: true }, h('div', { class: 'section-head' }, h('h3', { class: 'section-title' }, 'Controls')), els.controlsSlot);

  els.sessionChip = h('span', { class: 'drawer-session', hidden: true });
  // One button switches between the two layouts: "Expand" next to Close in
  // the side drawer, "Collapse" leading the header of the workspace.
  els.expand = h('button', { class: 'btn btn-sm drawer-expand btn-expand', type: 'button', onClick: toggleDrawerExpanded });
  els.close = h('button', { class: 'btn btn-ghost btn-icon btn-sm drawer-close', type: 'button', 'aria-label': 'Close', onClick: () => closeDrawer() }, icon('x', 15));
  els.topSpacer = h('span', { class: 'spacer' });

  const top = h(
    'div',
    { class: 'drawer-top' },
    els.sessionChip,
    els.controls,
    els.topSpacer,
    els.openGitHub,
    h('button', { class: 'btn btn-ghost btn-icon btn-sm btn-danger', type: 'button', title: 'Delete card', 'aria-label': 'Delete card', onClick: deleteCard }, icon('trash', 15)),
    els.expand,
    els.close,
  );
  els.top = top;

  els.title = h('textarea', { class: 'title-input', rows: '1', spellcheck: 'false', 'aria-label': 'Card title' });
  els.title.value = card.title;
  els.title.addEventListener('input', () => autoGrow(els.title));
  els.title.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      els.title.blur();
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      els.title.value = cardById(ctx.cardId).title;
      autoGrow(els.title);
      els.title.blur();
    }
  });
  els.title.addEventListener('blur', () => {
    const current = cardById(ctx.cardId);
    const next = els.title.value.replace(/\s+/g, ' ').trim();
    if (!current) return;
    if (!next) {
      els.title.value = current.title;
      autoGrow(els.title);
    } else if (next !== current.title) {
      patchCard({ title: next }).catch(failAndResync);
    }
  });

  els.optimizeTitle = h(
    'button',
    {
      class: 'btn btn-ghost btn-sm title-ai-btn',
      type: 'button',
      title: 'Optimize title with AI from the linked PR/issue and conversation',
      'aria-label': 'Optimize title with AI',
      onClick: (e) => withBusy(e.currentTarget, optimizeTitle, null, 'Optimizing...'),
    },
    icon('sparkles', 14),
    h('span', { class: 'btn-label' }, 'AI Title'),
  );
  const titleRow = h('div', { class: 'title-row' }, els.title, els.optimizeTitle);

  els.tags = h('div', { class: 'tag-row' });
  els.tagInput = h('input', { class: 'tag-input', type: 'text', placeholder: 'Add tag', maxlength: '40', 'aria-label': 'Add tag' });
  els.tagInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      addTag();
    } else if (e.key === 'Escape' && els.tagInput.value) {
      e.stopPropagation();
      els.tagInput.value = '';
    }
  });
  els.tagInput.addEventListener('blur', addTag);

  els.summary = h('section', { class: 'section' });
  els.github = h('section', { class: 'section' });
  els.notes = h('section', { class: 'section' });
  els.agentStart = h('section', { class: 'section' });
  els.timelineList = h('ul', { class: 'timeline' });
  els.timeline = h(
    'details',
    { class: 'section' },
    h('summary', null, icon('chevron-right', 13), 'Timeline'),
    h('div', { class: 'panel' }, els.timelineList),
  );
  els.timeline.addEventListener('toggle', () => {
    if (els.timeline.open) loadTimeline(true);
  });

  const details = h('div', { class: 'drawer-details' }, titleRow, els.tags, els.summary, els.github, els.notes);
  if (!card.aoSessionId) details.appendChild(els.agentStart);
  details.append(els.controlsSection, els.timeline);

  const body = h('div', { class: 'drawer-body' }, details);
  const el = h('aside', { class: 'drawer', 'aria-label': 'Card details' }, resizeHandle(), top, body);

  if (card.aoSessionId) {
    els.sessionBar = h('div', { class: 'session-bar' });
    els.sessionAlerts = h('div', { class: 'session-alerts' });
    els.chatScroll = h('div', { class: 'chat-scroll', tabindex: '0', 'aria-label': 'Conversation' });
    els.jump = h(
      'button',
      { class: 'btn btn-sm jump-latest', type: 'button', hidden: true, onClick: () => scrollChatToEnd(true) },
      icon('arrow-down', 13),
      h('span', null, 'Latest'),
    );
    els.loadMore = h(
      'div',
      { class: 'chat-more' },
      h('button', { class: 'btn btn-sm', type: 'button', onClick: (e) => withBusy(e.currentTarget, loadEarlier) }, icon('history', 13), h('span', null, 'Load earlier messages')),
    );
    els.chatScroll.addEventListener('scroll', () => {
      if (nearBottom()) els.jump.hidden = true;
    });
    els.composerInput = h('textarea', { rows: '2', spellcheck: 'false', 'aria-label': 'Message' });
    els.composerInput.value = sessionStorageGet(card.aoSessionId);
    els.composerInput.addEventListener('input', () => {
      autoGrow(els.composerInput);
      sessionStorageSet(ctx.sessionId, els.composerInput.value);
      updateComposer();
    });
    els.composerInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        sendMessage();
      }
    });
    els.send = h('button', { class: 'btn btn-primary btn-sm', type: 'button', title: 'Send (Ctrl+Enter)', onClick: sendMessage }, icon('send', 13), h('span', null, 'Send'));
    els.stop = h(
      'button',
      { class: 'btn btn-sm btn-danger', type: 'button', hidden: true, title: 'Interrupt the running turn', onClick: (e) => withBusy(e.currentTarget, interrupt, null, 'Stopping...') },
      icon('stop', 13),
      h('span', null, 'Stop'),
    );
    els.composerHint = h('span', { class: 'hint composer-hint' }, 'Ctrl+Enter to send');
    // Model and effort of the next turn; filled by loadSessionModels().
    els.model = h('select', { class: 'select select-sm', 'aria-label': 'Model', title: 'Model for the next turn of this session', disabled: true }, h('option', { value: '' }, 'Model'));
    els.effort = h('select', { class: 'select select-sm', 'aria-label': 'Effort', title: 'Reasoning effort for the next turn', hidden: true });
    els.model.addEventListener('change', () => changeSessionSettings({ model: els.model.value, reasoningEffort: '' }));
    els.effort.addEventListener('change', () => changeSessionSettings({ reasoningEffort: els.effort.value }));
    els.modelPicker = h('span', { class: 'model-picker' }, els.model, els.effort);
    body.appendChild(
      h(
        'div',
        { class: 'drawer-main' },
        els.sessionBar,
        els.sessionAlerts,
        h('div', { class: 'chat' }, els.chatScroll, els.jump),
        h(
          'div',
          { class: 'composer' },
          h('div', { class: 'composer-box' }, els.composerInput, h('div', { class: 'composer-actions' }, els.modelPicker, els.composerHint, h('span', { class: 'spacer' }), els.stop, els.send)),
        ),
      ),
    );
    el.classList.add('has-chat');
    if (storageGet(COLLAPSE_KEY, false)) el.classList.add('details-collapsed');
    if (storageGet(RAIL_KEY, false)) el.classList.add('rail-collapsed');
  }

  return {
    cardId: card.id,
    sessionId: card.aoSessionId,
    el,
    els,
    sigs: {},
    expanded: false,
    editing: { summary: false, notes: false },
    sessionError: null,
    // Running operations, each { since }: an AI summary, a session resume.
    summarizing: null,
    resuming: null,
    sending: false,
    timelineStamp: '',
    chat: null,
    sessionsForLink: null,
    harness: 'copilot',
    // Model picker data of the session: null until loaded.
    models: null,
    modelsKey: '',
  };
}

/**
 * The grip on the left edge of the side drawer. Drag it to resize, use the
 * arrow keys while it is focused, or double-click to restore the default
 * width. The chosen width is remembered in localStorage.
 */
function resizeHandle() {
  const handle = h('div', {
    class: 'drawer-resize',
    role: 'separator',
    tabindex: '0',
    'aria-orientation': 'vertical',
    'aria-label': 'Resize the card drawer',
    title: 'Drag to resize. Double-click to reset the width.',
  });
  const commit = (px) => {
    storageSet(WIDTH_KEY, applyWidth(px));
    if (ctx) autoGrow(ctx.els.title);
  };
  handle.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    handle.classList.add('is-active');
    document.body.classList.add('is-resizing');
    let x = e.clientX;
    let frame = 0;
    const move = (ev) => {
      x = ev.clientX;
      // One layout per frame keeps the drag smooth with a long conversation.
      if (!frame) {
        frame = requestAnimationFrame(() => {
          frame = 0;
          applyWidth(window.innerWidth - x);
        });
      }
    };
    const end = () => {
      cancelAnimationFrame(frame);
      handle.classList.remove('is-active');
      document.body.classList.remove('is-resizing');
      handle.removeEventListener('pointermove', move);
      handle.removeEventListener('pointerup', end);
      handle.removeEventListener('pointercancel', end);
      commit(window.innerWidth - x);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', end);
    handle.addEventListener('pointercancel', end);
  });
  handle.addEventListener('dblclick', () => commit(null));
  handle.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    const current = ctx ? ctx.el.getBoundingClientRect().width : MIN_WIDTH;
    commit(current + (e.key === 'ArrowLeft' ? RESIZE_STEP : -RESIZE_STEP));
  });
  return handle;
}

/** Puts the card controls where the current layout shows them. */
function placeControls() {
  const { els } = ctx;
  if (ctx.expanded) els.controlsSlot.appendChild(els.controls);
  else els.top.insertBefore(els.controls, els.topSpacer);
  els.controlsSection.hidden = !ctx.expanded;
}

/**
 * The detail sections fold differently per layout, and each layout keeps
 * its own choices: "split" is the side drawer with a conversation below,
 * "rail" the context rail of the expanded workspace, "full" a card without
 * a conversation.
 */
function sectionLayout(card) {
  if (!card.aoSessionId) return 'full';
  return ctx.expanded ? 'rail' : 'split';
}

/** Whether a detail section is folded: the stored choice, or a default. */
function sectionCollapsed(key, card) {
  if (ctx.editing[key]) return false;
  const layout = sectionLayout(card);
  const prefs = storageGet(SECTIONS_KEY, {});
  const stored = prefs && prefs[`${layout}.${key}`];
  if (typeof stored === 'boolean') return stored;
  // Above a conversation the details stay compact so the chat keeps the
  // height: GitHub tracking opens by itself only when there is something
  // to act on, and empty notes stay folded.
  if (layout !== 'split') return false;
  if (key === 'github') return !card.hasUnreadUpdates && !card.ghError;
  if (key === 'notes') return !card.notes.trim();
  return false;
}

function toggleSection(key) {
  const card = cardById(ctx.cardId);
  if (!card) return;
  const prefs = storageGet(SECTIONS_KEY, {}) || {};
  prefs[`${sectionLayout(card)}.${key}`] = !sectionCollapsed(key, card);
  storageSet(SECTIONS_KEY, prefs);
  rerender(key);
}

/**
 * Header of a collapsible section. Folded, it shows a one-line preview of
 * the content instead of the action buttons.
 */
function sectionHead(key, title, card, { badge = null, peek = '', peekTone = '', actions = [] } = {}) {
  const collapsed = sectionCollapsed(key, card);
  const toggle = () => toggleSection(key);
  const head = h(
    'div',
    { class: ['section-head', collapsed && 'is-collapsed'] },
    h(
      'h3',
      { class: 'section-heading' },
      h(
        'button',
        { class: 'section-toggle', type: 'button', 'aria-expanded': String(!collapsed), title: collapsed ? `Show ${title}` : `Hide ${title}`, onClick: toggle },
        icon(collapsed ? 'chevron-right' : 'chevron-down', 13),
        h('span', { class: 'section-title' }, title),
      ),
    ),
    badge,
  );
  if (collapsed) head.appendChild(h('span', { class: ['section-peek', peekTone], title: peek, onClick: toggle }, peek));
  else head.append(h('span', { class: 'spacer' }), ...actions.filter(Boolean));
  return { head, collapsed };
}

/** First non-empty line of a text, without Markdown marks, for a preview. */
function firstLine(text) {
  const line = String(text || '')
    .split('\n')
    .map((l) =>
      l
        .replace(/^\s*(?:#{1,6}|>|[-*+]|\d+\.)\s+/, '')
        .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
        .replace(/[*_`~]+/g, '')
        .trim(),
    )
    .find(Boolean);
  return line ? line.slice(0, 200) : '';
}

function sessionStorageGet(sessionId) {
  try {
    return sessionStorage.getItem(`mb.draft.${sessionId}`) || '';
  } catch (err) {
    return '';
  }
}

function sessionStorageSet(sessionId, value) {
  try {
    if (value) sessionStorage.setItem(`mb.draft.${sessionId}`, value);
    else sessionStorage.removeItem(`mb.draft.${sessionId}`);
  } catch (err) {
    // Drafts are a convenience; ignore storage failures.
  }
}

function failAndResync(err) {
  toastError(err);
  if (ctx) {
    ctx.sigs = {};
    updateDrawer();
  }
}

/**
 * Asks how to delete the open card. A card with an AO session offers to
 * delete the session from Agent Orchestrator as well, or to keep it there;
 * the request runs while the dialog shows its progress.
 */
async function deleteCard() {
  const card = cardById(ctx.cardId);
  if (!card) return;
  const sessionId = card.aoSessionId;
  const remove = async (withSession) => {
    await api.del(cardPath(card.id, withSession ? '?deleteAoSession=true' : ''));
  };
  const intro = h('p', { class: 'modal-text' }, 'Delete ', h('strong', null, card.title), '? Notes, summary and the timeline of this card are removed.');
  let message = intro;
  let actions = [{ label: 'Delete Card', value: 'card', variant: 'danger', busyLabel: 'Deleting...', autofocus: true, run: () => remove(false) }];
  // A session AO no longer reports is already gone; there is nothing to choose.
  if (sessionId && !(card.sessionMissing && state.board.ao.connected)) {
    const agent = card.session ? harnessLabel(card.session.harness) : 'agent';
    const offline = !state.board.ao.connected;
    message = h(
      'div',
      { class: 'delete-choice' },
      intro,
      h('p', { class: 'modal-text' }, 'This card is linked to the ', agent, ' session ', h('strong', { class: 'mono' }, sessionId), ' in Agent Orchestrator.'),
      h(
        'dl',
        { class: 'choice-list' },
        h('dt', null, 'Delete Card & AO Session'),
        h('dd', null, 'Also terminates the session and permanently deletes it from Agent Orchestrator, together with its AO conversation history. Git branches and pull requests are kept.'),
        h('dt', null, 'Delete Card Only'),
        h('dd', null, 'Removes the card from this board. The session stays in Agent Orchestrator and does not get a new card automatically; you can link or import it again later.'),
      ),
    );
    actions = [
      { label: 'Delete Card Only', value: 'card', busyLabel: 'Deleting...', run: () => remove(false) },
      {
        label: 'Delete Card & AO Session',
        value: 'both',
        variant: 'danger',
        busyLabel: 'Deleting...',
        autofocus: !offline,
        disabled: offline,
        title: offline ? 'AO is offline, so the session cannot be deleted right now' : 'Delete from Maintainer Board and from Agent Orchestrator',
        run: () => remove(true),
      },
    ];
  } else if (sessionId) {
    message = h('div', null, intro, h('p', { class: 'modal-text' }, 'Agent Orchestrator no longer reports its session ', h('strong', { class: 'mono' }, sessionId), ', so there is nothing to delete there.'));
  }
  const choice = await actionDialog({ title: 'Delete card', message, actions, size: sessionId ? 'medium' : 'narrow' });
  if (!choice) return;
  if (ctx && ctx.cardId === card.id) closeDrawer();
  toast(choice === 'both' ? `Card and AO session ${sessionId} deleted.` : sessionId ? `Card deleted. Session ${sessionId} stays in Agent Orchestrator.` : 'Card deleted.', 'success');
  onRefresh();
}

function addTag() {
  const value = ctx.els.tagInput.value.replace(/,/g, ' ').trim();
  ctx.els.tagInput.value = '';
  if (!value) return;
  const card = cardById(ctx.cardId);
  if (!card || card.tags.some((t) => t.toLowerCase() === value.toLowerCase())) return;
  patchCard({ tags: [...card.tags, value] }).catch(failAndResync);
}

// Detail sections

function renderTop(card) {
  const { els } = ctx;
  if (document.activeElement !== els.category) els.category.value = card.category;
  if (document.activeElement !== els.column) {
    clear(els.column);
    for (const col of columns()) els.column.appendChild(h('option', { value: col.id }, col.title));
    els.column.value = card.columnId;
  }
  els.autoMove.checked = card.autoMove;
  const url = ghUrl(card);
  els.openGitHub.hidden = !url;
  if (url) els.openGitHub.href = url;

  els.sessionChip.hidden = !card.aoSessionId;
  if (card.aoSessionId) {
    const harness = card.session ? card.session.harness : '';
    els.sessionChip.title = `AO session ${card.aoSessionId}`;
    els.sessionChip.replaceChildren(
      brandIcon(harness, 16),
      h('span', { class: 'session-harness' }, card.session ? harnessLabel(harness) : 'Agent'),
      h('span', { class: 'mono session-id' }, card.aoSessionId),
    );
  }

  if (ctx.expanded) {
    // Leaving the workspace returns to the drawer with this card still
    // open; only Close goes all the way back to the board.
    els.expand.title = 'Collapse to the side drawer; this card stays open (F or Esc)';
    els.expand.setAttribute('aria-label', 'Collapse to the side drawer');
    els.expand.replaceChildren(icon('panel-right', 14), h('span', { class: 'btn-label' }, 'Collapse'));
    els.close.title = 'Close this card and return to the board';
  } else {
    els.expand.title = 'Expand to full workspace (F)';
    els.expand.setAttribute('aria-label', 'Expand to full workspace');
    els.expand.replaceChildren(icon('maximize', 14), h('span', { class: 'btn-label' }, 'Expand'));
    els.close.title = 'Close (Esc)';
  }
  els.expand.classList.toggle('btn-expand', !ctx.expanded);
  els.expand.classList.toggle('btn-collapse', ctx.expanded);
  els.expand.setAttribute('aria-pressed', String(ctx.expanded));

  if (document.activeElement !== els.title && els.title.value !== card.title) {
    els.title.value = card.title;
    autoGrow(els.title);
  }
}

function renderTags(card) {
  const { els } = ctx;
  const chips = card.tags.map((tag) =>
    h(
      'span',
      { class: 'tag' },
      h('span', null, tag),
      h(
        'button',
        {
          type: 'button',
          title: `Remove tag ${tag}`,
          'aria-label': `Remove tag ${tag}`,
          onClick: () => patchCard({ tags: cardById(ctx.cardId).tags.filter((t) => t !== tag) }).catch(failAndResync),
        },
        icon('x', 10),
      ),
    ),
  );
  els.tags.replaceChildren(...chips, els.tagInput);
}

function editor({ value, placeholder, minHeight, onSave, onCancel }) {
  const area = h('textarea', { class: 'textarea', placeholder, spellcheck: 'false' });
  area.value = value;
  if (minHeight) area.style.minHeight = `${minHeight}px`;
  const save = h('button', { class: 'btn btn-primary btn-sm', type: 'button' }, 'Save');
  const cancel = h('button', { class: 'btn btn-sm', type: 'button', onClick: onCancel }, 'Cancel');
  const submit = () => withBusy(save, () => onSave(area.value));
  save.addEventListener('click', submit);
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      submit();
    } else if (e.key === 'Escape') {
      e.stopPropagation();
      onCancel();
    }
  });
  const wrap = h('div', null, area, h('div', { class: 'edit-actions' }, h('span', { class: 'hint' }, 'Ctrl+Enter to save, Esc to cancel'), h('span', { class: 'spacer' }), cancel, save));
  requestAnimationFrame(() => {
    area.focus();
    area.setSelectionRange(area.value.length, area.value.length);
  });
  return wrap;
}

function rerender(key) {
  delete ctx.sigs[key];
  updateDrawer();
}

function renderSummary(card) {
  const { els } = ctx;
  const source = card.summarySource || 'auto';
  const { head, collapsed } = sectionHead('summary', 'Summary', card, {
    badge: h('span', { class: `badge badge-source-${source}`, title: sourceTitle(source) }, source),
    peek: firstLine(card.summary) || 'No summary yet',
    actions: [
      h(
        'button',
        {
          class: 'btn btn-sm btn-collapsible',
          type: 'button',
          title: 'Summarize with AI: ask the local Copilot CLI (Codex as fallback) what this card is about, in one or two sentences',
          'aria-label': 'Summarize with AI',
          dataset: { action: 'summarize' },
          onClick: summarize,
        },
        icon('sparkles', 14),
        h('span', { class: 'btn-label' }, 'Summarize with AI'),
      ),
      h(
        'button',
        {
          class: 'btn btn-sm',
          type: 'button',
          title: 'Edit the summary',
          onClick: () => {
            ctx.editing.summary = true;
            rerender('summary');
          },
        },
        icon('pencil', 14),
        h('span', null, 'Edit'),
      ),
    ],
  });
  // A running AI summary: the button is busy, and the place where the
  // summary will appear says so, with the time it has taken so far.
  const progress = ctx.summarizing ? h('div', { class: 'summary-progress' }, statusStrip('Generating summary with AI...', { since: ctx.summarizing.since })) : null;
  if (progress) {
    progress.firstChild.classList.add('is-ai');
    // A folded section has no buttons in its head.
    const button = head.querySelector('[data-action="summarize"]');
    if (button) setButtonBusy(button, 'Summarizing...');
  }
  if (collapsed) {
    els.summary.replaceChildren(...[head, progress].filter(Boolean));
    return;
  }
  let body;
  if (ctx.editing.summary) {
    body = editor({
      value: card.summary,
      placeholder: 'One or two sentences: what this task is about and where it stands.',
      onSave: async (value) => {
        await patchCard({ summary: value });
        ctx.editing.summary = false;
        rerender('summary');
      },
      onCancel: () => {
        ctx.editing.summary = false;
        rerender('summary');
      },
    });
  } else if (card.summary) {
    body = h('div', { class: 'panel' }, h('p', { class: 'summary-text' }, card.summary));
  } else {
    body = h(
      'div',
      { class: 'panel' },
      h(
        'p',
        { class: 'summary-text is-empty' },
        card.aoSessionId
          ? 'No summary yet. It is extracted automatically from the agent conversation, or use Summarize with AI.'
          : 'No summary yet. Write one with Edit, or link a session or pull request and use Summarize with AI.',
      ),
    );
  }
  if (progress && !ctx.editing.summary) body.classList.add('is-updating');
  els.summary.replaceChildren(...[head, progress, body].filter(Boolean));
}

function sourceTitle(source) {
  if (source === 'ai') return 'Written by the AI summarizer';
  if (source === 'manual') return 'Edited by you; automatic extraction is paused';
  return 'Extracted automatically from the latest agent messages';
}

async function summarize() {
  if (!ctx || ctx.summarizing) return;
  const active = ctx;
  active.summarizing = { since: Date.now() };
  rerender('summary');
  try {
    const res = await api.post(cardPath(active.cardId, '/summarize'));
    active.summarizing = null;
    if (ctx === active) ctx.editing.summary = false;
    applyCard(res.card);
    // The engine is "copilot", "codex", or the model the Copilot CLI used.
    const engine = res.engine === 'copilot' || res.engine === 'codex' ? harnessLabel(res.engine) : `${res.engine} (Copilot CLI)`;
    toast(`Summary updated with ${engine}.`, 'success');
  } catch (err) {
    toastError(err);
  } finally {
    active.summarizing = null;
    if (ctx === active) rerender('summary');
  }
}

async function optimizeTitle() {
  if (!ctx) return;
  const cardId = ctx.cardId;
  const res = await api.post(cardPath(cardId, '/optimize-title'));
  applyCard(res.card);
  if (ctx && ctx.cardId === cardId) {
    ctx.els.title.value = res.card.title;
    autoGrow(ctx.els.title);
  }
  const engine = res.engine === 'copilot' || res.engine === 'codex' ? harnessLabel(res.engine) : `${res.engine} (Copilot CLI)`;
  toast(`Title optimized with ${engine}.`, 'success');
}

function renderNotes(card) {
  const { els } = ctx;
  const startEdit = () => {
    ctx.editing.notes = true;
    rerender('notes');
  };
  const { head, collapsed } = sectionHead('notes', 'Maintainer Notes', card, {
    peek: firstLine(card.notes) || 'No notes yet',
    actions: [ctx.editing.notes ? null : h('button', { class: 'btn btn-sm', type: 'button', title: 'Edit the notes', onClick: startEdit }, icon('pencil', 14), h('span', null, 'Edit'))],
  });
  if (collapsed) {
    els.notes.replaceChildren(head);
    return;
  }
  let body;
  if (ctx.editing.notes) {
    body = editor({
      value: card.notes,
      placeholder: 'Review notes, follow-ups, links. Markdown is supported.',
      minHeight: 120,
      onSave: async (value) => {
        await patchCard({ notes: value });
        ctx.editing.notes = false;
        rerender('notes');
      },
      onCancel: () => {
        ctx.editing.notes = false;
        rerender('notes');
      },
    });
  } else if (card.notes.trim()) {
    body = h('div', { class: 'panel notes-view', onDblclick: startEdit, title: 'Double-click to edit' }, renderMarkdown(card.notes));
  } else {
    body = h('div', { class: 'panel notes-view is-empty', onDblclick: startEdit }, 'No notes yet. Use Edit to add review notes or follow-ups.');
  }
  els.notes.replaceChildren(head, body);
}

function pill(cls, label, iconName, title) {
  return h('span', { class: `pill ${cls}`, title: title || label }, iconName ? icon(iconName, 12) : h('span', { class: 'dot' }), h('span', null, label));
}

function stat(iconName, label, value, title) {
  return h('span', { class: 'meta-item', title: title || label }, icon(iconName, 12), h('span', { class: 'mono' }, String(value)), h('span', null, label));
}

function renderGitHub(card) {
  const { els } = ctx;
  const nodes = [];

  if (!card.ghNumber) {
    const input = h('input', {
      class: 'input mono',
      type: 'text',
      placeholder: '#6247, owner/repo#123 or a GitHub pull request or issue URL',
      spellcheck: 'false',
      'aria-label': 'GitHub reference',
    });
    const button = h('button', { class: 'btn', type: 'button' }, icon('link', 13), h('span', null, 'Link PR / Issue'));
    const submit = () => {
      const ref = input.value.trim();
      if (!ref) {
        input.focus();
        return;
      }
      withBusy(button, async () => {
        const updated = await api.post(cardPath(ctx.cardId, '/link-github'), { ref });
        input.value = '';
        applyCard(updated);
        toast(`Linked ${ghRefLabel(updated)}.`, 'success');
      }, null, 'Linking...');
    };
    button.addEventListener('click', submit);
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        submit();
      }
    });
    const { head, collapsed } = sectionHead('github', 'GitHub Tracking', card, { peek: 'Not linked to a pull request or issue' });
    nodes.push(head);
    if (!collapsed) {
      nodes.push(
        h('div', { class: 'gh-link-form' }, input, button),
        h('p', { class: 'hint' }, 'Linking records the current state as the reviewed baseline. Later commits, comments, CI results and review decisions show up as updates.'),
      );
    }
    els.github.replaceChildren(...nodes);
    return;
  }

  const syncButton = h(
    'button',
    {
      class: 'btn btn-sm btn-collapsible',
      type: 'button',
      title: 'Sync Now: fetch the latest state from GitHub',
      'aria-label': 'Sync Now',
      onClick: (e) =>
        withBusy(e.currentTarget, async () => {
          const res = await api.post(cardPath(ctx.cardId, '/sync-github'));
          applyCard(res.card);
          if (res.moved) toast('New activity found; the card was moved.', 'info');
          else if (res.newUpdate) toast('New activity found.', 'info');
          else toast('Synced. No new activity.', 'success');
        }, () => {
          // A failed sync may still have recorded an error on the card.
          onRefresh();
          return false;
        }, 'Syncing...'),
    },
    icon('refresh', 14),
    h('span', { class: 'btn-label' }, 'Sync Now'),
  );
  const unlinkButton = h(
    'button',
    {
      class: 'btn btn-sm btn-collapsible',
      type: 'button',
      title: 'Unlink: stop tracking this pull request or issue on the card',
      'aria-label': 'Unlink',
      onClick: async (e) => {
        const button = e.currentTarget;
        const ok = await confirmDialog({
          title: 'Unlink from GitHub',
          message: `Stop tracking ${ghRefLabel(card)} on this card? The update history baseline is discarded.`,
          confirmLabel: 'Unlink',
        });
        if (!ok) return;
        withBusy(button, async () => {
          applyCard(await api.post(cardPath(ctx.cardId, '/unlink-github')));
        });
      },
    },
    icon('unlink', 14),
    h('span', { class: 'btn-label' }, 'Unlink'),
  );
  const mine = myReviewInfo(card);
  const stateInfo = ghStateInfo(card);
  const peekParts = [`${card.ghKind === 'issue' ? 'Issue' : 'PR'} ${ghRefLabel(card)}`, card.ghLastSyncedAt ? stateInfo.label : 'Not synced'];
  if (card.hasUnreadUpdates) peekParts.push(`Updated: ${card.updateSummary || 'new activity'}`);
  else if (mine) peekParts.push(mine.label);
  const { head, collapsed } = sectionHead('github', 'GitHub Tracking', card, {
    peek: peekParts.join(' / '),
    peekTone: card.hasUnreadUpdates || (mine && mine.kind === 'stale') ? 'is-warning' : '',
    actions: [syncButton, unlinkButton],
  });
  nodes.push(head);
  if (collapsed) {
    els.github.replaceChildren(...nodes);
    return;
  }

  const stack = h('div', { class: 'stack' });

  if (card.hasUnreadUpdates) {
    const isPR = card.ghKind === 'pr';
    const actions = !isPR
      ? h(
          'div',
          { class: 'alert-actions' },
          h(
            'button',
            {
              class: 'btn btn-sm btn-warning',
              type: 'button',
              title: 'Clear the update banner for this issue',
              onClick: (e) => withBusy(e.currentTarget, () => acknowledge('')),
            },
            icon('check', 13),
            h('span', null, 'Mark Caught Up'),
          ),
        )
      : null;
    stack.appendChild(
      h(
        'div',
        { class: 'alert alert-warning', role: 'status' },
        icon('alert', 15),
        h(
          'div',
          { class: 'alert-body' },
          h('div', { class: 'alert-title' }, 'Updated since your last review'),
          h('div', { class: 'alert-detail' }, card.updateSummary || 'New activity'),
          actions,
        ),
      ),
    );
  }

  if (card.ghError) {
    stack.appendChild(
      h(
        'div',
        { class: 'alert alert-danger' },
        icon('x-circle', 15),
        h('div', { class: 'alert-body' }, h('div', { class: 'alert-title' }, 'Last sync failed'), h('div', { class: 'alert-detail' }, card.ghError)),
      ),
    );
  }

  const refLink = h('a', { class: 'mono', href: ghUrl(card), target: '_blank', rel: 'noopener noreferrer' }, `${card.ghRepo}#${card.ghNumber}`);
  if (!card.ghLastSyncedAt) {
    stack.appendChild(
      h('div', { class: 'panel' }, h('div', { class: 'gh-strip' }, pill('pill-muted', 'Not synced'), refLink, h('span', { class: 'hint' }, 'Waiting for the first sync with GitHub.'))),
    );
    nodes.push(stack);
    els.github.replaceChildren(...nodes);
    return;
  }

  const strip = h(
    'div',
    { class: 'gh-strip' },
    h('span', { class: 'badge' }, card.ghKind === 'issue' ? 'Issue' : 'PR'),
    refLink,
    pill(stateInfo.cls, stateInfo.label, ghIconName(card)),
  );
  if (card.ghKind === 'pr') {
    const ci = ciInfo(card.ghCiStatus);
    if (ci) strip.appendChild(pill(ci.cls, ci.label, ci.icon, ci.title));
    const review = reviewInfo(card.ghReviewDecision);
    if (review) strip.appendChild(pill(review.cls, review.label, review.icon, review.title));
  }
  strip.appendChild(h('p', { class: 'gh-title' }, card.ghTitle));
  if (card.ghLabels && card.ghLabels.length) {
    strip.appendChild(h('div', { class: 'tag-row', style: { margin: '0' } }, card.ghLabels.map((l) => h('span', { class: 'tag' }, h('span', null, l)))));
  }

  const stats = h('div', { class: 'gh-stats' });
  if (card.ghAuthor) {
    stats.appendChild(
      h('span', { class: 'meta-item', title: 'Author' }, icon('user', 13), h('span', { class: 'mono' }, `@${card.ghAuthor}`), isMine(card) ? h('span', { class: 'badge badge-mine' }, 'MINE') : null),
    );
  }
  if (card.ghKind === 'pr') {
    stats.appendChild(stat('commit', card.ghCommitCount === 1 ? 'commit' : 'commits', card.ghCommitCount, `Head ${card.ghHeadSha.slice(0, 9)}`));
  }
  stats.appendChild(stat('message', card.ghCommentCount === 1 ? 'comment' : 'comments', card.ghCommentCount, 'Conversation comments'));
  if (card.ghKind === 'pr') {
    stats.appendChild(stat('pr', card.ghReviewCommentCount === 1 ? 'review comment' : 'review comments', card.ghReviewCommentCount, 'Inline review comments plus review summaries'));
  }
  stats.appendChild(h('span', { class: 'meta-item', title: `Updated on GitHub ${fmtDateTime(card.ghUpdatedAt)}` }, icon('clock', 12), `updated ${relTime(card.ghUpdatedAt)}`));
  stats.appendChild(h('span', { class: 'spacer' }));
  stats.appendChild(h('span', { class: 'hint', title: `Baseline acknowledged ${fmtDateTime(card.ackAt)}` }, `synced ${relTime(card.ghLastSyncedAt)}`));

  stack.appendChild(h('div', { class: 'panel' }, strip, stats, reviewRow(card, mine)));
  nodes.push(stack);
  els.github.replaceChildren(...nodes);
}

/**
 * "Your review" line of the GitHub panel: where the maintainer personally
 * stands, when they last looked, and the buttons to record or clear a
 * review that was done outside GitHub.
 */
function reviewRow(card, mine) {
  const row = h('div', { class: 'gh-review' }, h('span', { class: 'gh-review-label' }, 'Your review'));
  const kind = card.ghKind === 'issue' ? 'issue' : 'pull request';
  if (isMine(card)) {
    row.appendChild(h('span', { class: 'hint' }, `You opened this ${kind}.`));
    return row;
  }
  const open = card.ghState !== 'MERGED' && card.ghState !== 'CLOSED';
  if (mine) {
    const node = pill(mine.cls, mine.label, mine.icon, mine.title);
    node.dataset.review = mine.kind;
    row.appendChild(node);
  } else {
    row.appendChild(h('span', { class: 'hint' }, currentUser() ? 'No review recorded.' : 'Your GitHub login is unknown, so reviews on GitHub are not detected.'));
  }
  if (card.ghMyReviewState && card.ghMyReviewedAt) {
    row.appendChild(h('span', { class: 'hint', title: new Date(card.ghMyReviewedAt).toLocaleString() }, `last look ${relTime(card.ghMyReviewedAt)}`));
  }
  row.appendChild(h('span', { class: 'spacer' }));
  if (card.ghMyReviewState === 'REVIEWED') {
    row.appendChild(
      h(
        'button',
        {
          class: 'btn btn-ghost btn-sm',
          type: 'button',
          title: 'Remove the review mark you set on the board',
          onClick: (e) =>
            withBusy(e.currentTarget, async () => {
              applyCard(await api.post(cardPath(ctx.cardId, '/unreview')));
              toast('Review mark cleared.', 'success');
            }),
        },
        h('span', null, 'Clear'),
      ),
    );
  }
  // Issues have no GitHub review API, so allow marking them reviewed locally.
  // Pull requests reflect actual GitHub reviews only (display-only here).
  if (card.ghKind !== 'pr' && open && !card.hasUnreadUpdates && (!mine || mine.kind !== 'current')) {
    row.appendChild(
      h(
        'button',
        {
          class: 'btn btn-sm',
          type: 'button',
          title: `Record that you reviewed the current state of this ${kind}`,
          onClick: (e) => withBusy(e.currentTarget, () => acknowledge('')),
        },
        icon('check', 14),
        h('span', null, 'Mark Reviewed'),
      ),
    );
  }
  return row;
}

async function acknowledge(moveToColumnId) {
  const body = moveToColumnId ? { moveToColumnId } : {};
  const before = cardById(ctx.cardId);
  const hadUpdates = Boolean(before && before.hasUnreadUpdates);
  applyCard(await api.post(cardPath(ctx.cardId, '/ack'), body));
  if (moveToColumnId) toast('Marked caught up and moved.', 'success');
  else toast(hadUpdates ? 'Marked caught up.' : 'Marked reviewed.', 'success');
}

async function loadTimeline(force) {
  const card = cardById(ctx.cardId);
  if (!card || !ctx.els.timeline.open) return;
  if (!force && ctx.timelineStamp === card.updatedAt) return;
  ctx.timelineStamp = card.updatedAt;
  const cardId = ctx.cardId;
  try {
    const res = await api.get(cardPath(cardId, '/events?limit=100'));
    if (!ctx || ctx.cardId !== cardId) return;
    const rows = res.events.map((ev) =>
      h('li', null, h('time', { title: new Date(ev.createdAt).toLocaleString() }, fmtDateTime(ev.createdAt)), h('span', { class: 'kind' }, ev.kind), h('span', { class: 'text' }, ev.message)),
    );
    ctx.els.timelineList.replaceChildren(...(rows.length ? rows : [h('li', null, h('span', { class: 'hint' }, 'No events yet.'))]));
  } catch (err) {
    ctx.els.timelineList.replaceChildren(h('li', null, h('span', { class: 'hint' }, `Could not load the timeline: ${err.message}`)));
  }
}

// Agent session: start or link

function renderAgentStart(card) {
  const { els } = ctx;
  const ao = state.board.ao;
  const head = h('div', { class: 'section-head' }, h('h3', { class: 'section-title' }, 'Agent Session'));
  if (!ao.connected) {
    els.agentStart.replaceChildren(
      head,
      h('div', { class: 'alert alert-danger' }, icon('x-circle', 15), h('div', { class: 'alert-body' }, h('div', { class: 'alert-title' }, 'AO is offline'), h('div', { class: 'alert-detail' }, ao.error || 'Agent sessions can be started once the AO daemon is reachable.'))),
    );
    return;
  }

  const effortWrap = h('label', { class: 'inline-field', hidden: true }, h('span', { class: 'hint' }, 'Effort'));
  const picker = createModelPicker({
    initial: ctx.startModel || {},
    onChange: () => {
      effortWrap.hidden = !picker.hasEfforts;
      // While the list loads the selects are empty; keep the earlier choice.
      if (!picker.model.disabled) ctx.startModel = picker.value();
    },
  });
  effortWrap.appendChild(picker.effort);
  const harness = segmented(
    [
      ['copilot', 'Copilot'],
      ['codex', 'Codex'],
    ],
    ctx.harness,
    (value) => {
      ctx.harness = value;
      picker.load(value);
    },
  );
  picker.load(ctx.harness);
  const prompt = h('textarea', { class: 'textarea', placeholder: 'Initial prompt for the agent (optional). The session is named after the card title.', spellcheck: 'false' });
  prompt.value = ctx.startPrompt || '';
  prompt.addEventListener('input', () => {
    ctx.startPrompt = prompt.value;
  });
  const projects = ao.projects || [];
  const project = projects.length > 1 ? select(projects.map((p) => [p.id, p.name || p.id]), ao.defaultProject, { title: 'AO project', 'aria-label': 'AO project' }) : null;
  const start = h(
    'button',
    {
      class: 'btn btn-primary',
      type: 'button',
      onClick: (e) =>
        withBusy(e.currentTarget, async () => {
          const updated = await api.post(cardPath(ctx.cardId, '/session'), {
            harness: harness.value,
            prompt: prompt.value,
            projectId: project ? project.value : '',
            ...picker.value(),
          });
          toast(`Started ${harnessLabel(harness.value)} session ${updated.aoSessionId}.`, 'success');
          applyCard(updated);
        }, null, 'Starting session...'),
    },
    icon('play', 13),
    h('span', null, 'Start Agent Session'),
  );

  const linkRow = h('div', { class: 'row' });
  const free = (ctx.sessionsForLink || []).filter((s) => !s.cardId && !s.isTerminated);
  if (ctx.sessionsForLink === null) {
    ctx.sessionsForLink = [];
    api
      .get('/api/sessions')
      .then((res) => {
        if (!ctx || ctx.cardId !== card.id) return;
        ctx.sessionsForLink = res.sessions;
        rerender('agentStart');
      })
      .catch(() => {});
  } else if (free.length > 0) {
    const pick = select(
      free.map((s) => [s.id, `${s.id}  ${harnessLabel(s.harness)}  ${s.displayName || (s.kind === 'orchestrator' ? 'Orchestrator' : '')}`.trim()]),
      free[0].id,
      { 'aria-label': 'Existing AO session' },
    );
    pick.style.flex = '1';
    linkRow.append(
      h('span', { class: 'hint' }, 'Or link an existing session'),
      pick,
      h(
        'button',
        {
          class: 'btn btn-sm',
          type: 'button',
          onClick: (e) => withBusy(e.currentTarget, async () => patchCard({ aoSessionId: pick.value })),
        },
        icon('link', 13),
        h('span', null, 'Link'),
      ),
    );
  }

  els.agentStart.replaceChildren(
    head,
    h(
      'div',
      { class: 'panel start-session' },
      h('div', { class: 'row' }, harness, project, h('span', { class: 'hint' }, 'Starts a new AO worker session in chat mode.')),
      h('div', { class: 'row' }, h('label', { class: 'inline-field' }, h('span', { class: 'hint' }, 'Model'), picker.model), effortWrap),
      prompt,
      h('div', { class: 'row' }, start),
      free.length > 0 ? linkRow : null,
    ),
  );
}

// Agent session: live pane

function renderSessionBar(card) {
  const { els } = ctx;
  const info = activityInfo(card);
  const session = card.session;
  // The details are a panel above the chat in the side drawer and a rail
  // beside it when expanded; each layout remembers its own choice.
  const expanded = ctx.expanded;
  const flag = expanded ? 'rail-collapsed' : 'details-collapsed';
  const collapsed = ctx.el.classList.contains(flag);
  const what = expanded ? 'Context' : 'Details';
  const nodes = [
    brandIcon(session ? session.harness : '', 16),
    h('span', { class: 'session-harness' }, session ? harnessLabel(session.harness) : 'Agent'),
    h('span', { class: 'session-activity', title: session && session.activity ? `Last activity ${fmtDateTime(session.activity.lastActivityAt)}` : '' }, h('span', { class: `state-dot ${info.cls}` }), info.label),
  ];
  if (session && session.branch) {
    nodes.push(h('span', { class: 'session-branch mono', title: `Branch ${session.branch}` }, icon('branch', 13), h('span', null, session.branch)));
  }
  const chosen = ctx.models && ctx.models.selected;
  if (chosen && chosen.model) {
    const known = ctx.models.models.find((m) => m.id === chosen.model);
    nodes.push(
      h(
        'span',
        { class: 'session-model', title: `Model for the next turn${chosen.reasoningEffort ? `, reasoning effort ${chosen.reasoningEffort}` : ''}. Change it next to the message box.` },
        h('span', { class: 'name' }, known ? known.label : chosen.model),
        chosen.reasoningEffort ? h('span', { class: 'effort' }, chosen.reasoningEffort) : null,
      ),
    );
  }
  if (session) {
    nodes.push(
      h(
        'span',
        { class: 'session-info', tabindex: '0', title: 'This is the same live session shown in AO desktop. Messages sent here appear there, and the other way around.' },
        icon('info', 14),
      ),
    );
  }
  nodes.push(
    h('span', { class: 'spacer' }),
    h(
      'button',
      {
        class: 'btn btn-ghost btn-sm details-toggle',
        type: 'button',
        'aria-pressed': String(!collapsed),
        title: collapsed ? 'Show summary, GitHub tracking and notes' : 'Hide summary, GitHub tracking and notes to give the conversation more room',
        onClick: () => {
          const next = !ctx.el.classList.contains(flag);
          ctx.el.classList.toggle(flag, next);
          storageSet(expanded ? RAIL_KEY : COLLAPSE_KEY, next);
          rerender('sessionBar');
        },
      },
      icon(expanded ? 'panel-left' : collapsed ? 'chevron-down' : 'chevron-up', 14),
      h('span', null, `${collapsed ? 'Show' : 'Hide'} ${what}`),
    ),
  );
  if (session && !session.isTerminated) {
    nodes.push(
      h(
        'button',
        { class: 'btn btn-ghost btn-icon btn-sm btn-danger', type: 'button', title: 'Terminate: stop the agent process; the session and its history stay in Agent Orchestrator', 'aria-label': 'Terminate session', onClick: terminateSession },
        icon('power', 14),
      ),
    );
  }
  // Two different things, named apart: Unlink only detaches, Delete removes
  // the session from Agent Orchestrator.
  nodes.push(
    h(
      'button',
      {
        class: 'btn btn-ghost btn-sm btn-collapsible session-unlink',
        type: 'button',
        title: 'Unlink from card: detaches the session from this card only; keeps the session in Agent Orchestrator',
        'aria-label': 'Unlink from card',
        onClick: unlinkSession,
      },
      icon('unlink', 14),
      h('span', { class: 'btn-label' }, 'Unlink'),
    ),
    h(
      'button',
      {
        class: 'btn btn-ghost btn-sm btn-danger btn-collapsible session-delete',
        type: 'button',
        title: 'Delete AO Session: permanently removes the session from Agent Orchestrator and detaches it from this card; the card stays',
        'aria-label': 'Delete AO Session',
        onClick: deleteSession,
      },
      icon('trash', 14),
      h('span', { class: 'btn-label' }, 'Delete Session'),
    ),
  );
  els.sessionBar.replaceChildren(...nodes);
}

function alertBox(kind, iconName, title, detail, actions) {
  return h(
    'div',
    { class: `alert alert-${kind}` },
    icon(iconName, 15),
    h('div', { class: 'alert-body' }, h('div', { class: 'alert-title' }, title), detail ? h('div', { class: 'alert-detail' }, detail) : null, actions && actions.length ? h('div', { class: 'alert-actions' }, actions) : null),
  );
}

function renderSessionAlerts(card) {
  const { els } = ctx;
  const session = card.session;
  const nodes = [];
  const unlink = h(
    'button',
    { class: 'btn btn-sm', type: 'button', title: 'Detaches the session from this card only; keeps the session in Agent Orchestrator', onClick: unlinkSession },
    icon('unlink', 13),
    h('span', null, 'Unlink from Card'),
  );
  const remove = h(
    'button',
    { class: 'btn btn-sm btn-danger', type: 'button', title: 'Permanently removes the session from Agent Orchestrator and detaches it from this card', onClick: deleteSession },
    icon('trash', 13),
    h('span', null, 'Delete AO Session'),
  );

  if (!session) {
    const offline = !state.board.ao.connected;
    nodes.push(
      alertBox(
        'danger',
        'x-circle',
        offline ? 'AO is offline' : `AO does not report session ${card.aoSessionId}`,
        offline ? 'The conversation is unavailable until the AO daemon is reachable again.' : 'The session may have been deleted in AO. Unlink it to start or link another session.',
        offline ? [] : [unlink],
      ),
    );
  } else if (session.isTerminated) {
    nodes.push(
      alertBox('info', 'info', 'This session was terminated', 'The history stays readable. Unlink it to start a new agent session from this card, or delete it from Agent Orchestrator as well.', [unlink, remove]),
    );
  } else if (session.needsResume) {
    const resume = h('button', { class: 'btn btn-sm btn-primary', type: 'button', dataset: { action: 'resume' }, onClick: resumeSession }, icon('play', 13), h('span', null, 'Resume Session'));
    if (ctx.resuming) {
      // AO starts the agent and replays the session; that takes a while.
      setButtonBusy(resume, 'Resuming session...');
      const box = alertBox('info', 'info', 'Waking up agent session...', 'AO is starting the agent and restoring the conversation. This can take a little while.', [resume, elapsed(ctx.resuming.since, 0)]);
      box.classList.add('is-working');
      box.replaceChild(spinner(15), box.firstChild);
      box.appendChild(h('span', { class: 'progress-strip', 'aria-hidden': 'true' }));
      nodes.push(box);
    } else {
      nodes.push(
        alertBox('info', 'info', 'The agent is not running', 'This imported session shows its history, but the agent has to be resumed before it can take new messages.', [resume]),
      );
    }
  } else if (session.activity && (session.activity.state === 'waiting_input' || session.activity.state === 'blocked')) {
    nodes.push(
      alertBox('warning', 'alert', 'The agent is waiting for input', 'Answer a question by sending a message below. Tool approval prompts have to be answered in AO desktop.'),
    );
  }

  if (ctx.sessionError) {
    const elsewhere = ctx.sessionError.code === 'SESSION_OPEN_ELSEWHERE';
    nodes.push(
      alertBox('danger', 'x-circle', elsewhere ? 'Session is open in another terminal' : 'Could not resume the session', ctx.sessionError.message, [
        h(
          'button',
          {
            class: 'btn btn-sm',
            type: 'button',
            onClick: () => {
              ctx.sessionError = null;
              rerender('sessionAlerts');
            },
          },
          'Dismiss',
        ),
      ]),
    );
  }
  els.sessionAlerts.replaceChildren(...nodes);
}

function onResumeError(err) {
  ctx.sessionError = { code: err.code, message: err.message };
  rerender('sessionAlerts');
  return true;
}

async function resumeSession() {
  if (!ctx || ctx.resuming) return;
  const active = ctx;
  active.sessionError = null;
  active.resuming = { since: Date.now() };
  rerender('sessionAlerts');
  renderChat(true);
  try {
    await api.post(sessionPath(active.sessionId, '/resume'));
    toast(`Session ${active.sessionId} resumed.`, 'success');
    onRefresh();
    if (ctx === active) pollChat();
  } catch (err) {
    if (ctx === active) onResumeError(err);
    else toastError(err);
  } finally {
    active.resuming = null;
    if (ctx === active) {
      rerender('sessionAlerts');
      renderChat(true);
    }
  }
}

async function terminateSession() {
  const sessionId = ctx.sessionId;
  const done = await actionDialog({
    title: 'Terminate session',
    message: `Terminate AO session ${sessionId}? The agent process is stopped. The session and its conversation history stay in Agent Orchestrator and remain readable here.`,
    actions: [{ label: 'Terminate', value: 'terminate', variant: 'danger', busyLabel: 'Terminating...', autofocus: true, run: () => api.post(sessionPath(sessionId, '/kill')) }],
  });
  if (!done) return;
  toast(`Session ${sessionId} terminated.`, 'success');
  onRefresh();
}

/** Detaches the session from the card. The session itself is not touched. */
async function unlinkSession() {
  const sessionId = ctx.sessionId;
  const cardId = ctx.cardId;
  const message = h(
    'div',
    null,
    h('p', { class: 'modal-text' }, 'Detach AO session ', h('strong', { class: 'mono' }, sessionId), ' from this card?'),
    h(
      'p',
      { class: 'modal-text' },
      'This only removes the link. The session keeps running in Agent Orchestrator with its full history, and it does not get a new card automatically; link or import it again whenever you want it back on the board.',
    ),
    h('p', { class: 'modal-text hint' }, 'To remove the session itself, use Delete AO Session instead.'),
  );
  let card = null;
  const done = await actionDialog({
    title: 'Unlink from card',
    message,
    actions: [
      {
        label: 'Unlink from Card',
        value: 'unlink',
        variant: 'primary',
        busyLabel: 'Unlinking...',
        autofocus: true,
        run: async () => {
          card = (await api.post(cardPath(cardId, '/unlink'))).card;
        },
      },
    ],
  });
  if (!done) return;
  toast(`Session ${sessionId} unlinked. It stays in Agent Orchestrator.`, 'success');
  applyCard(card);
}

/**
 * Deletes the session from Agent Orchestrator for good and detaches it from
 * the card. The card stays on the board.
 */
async function deleteSession() {
  const sessionId = ctx.sessionId;
  const cardId = ctx.cardId;
  const current = cardById(cardId);
  const running = current && current.session && !current.session.isTerminated && !current.session.needsResume;
  const message = h(
    'div',
    null,
    h('p', { class: 'modal-text' }, 'Permanently delete AO session ', h('strong', { class: 'mono' }, sessionId), ' from Agent Orchestrator?'),
    h(
      'p',
      { class: 'modal-text' },
      running ? 'The running agent is terminated first. ' : '',
      'The session disappears from Agent Orchestrator and its desktop app, together with its AO conversation history. This cannot be undone. Git branches and pull requests are kept.',
    ),
    h('p', { class: 'modal-text hint' }, 'The card stays on the board with its notes, summary and GitHub link. To keep the session and only detach it, use Unlink instead.'),
  );
  let card = null;
  const done = await actionDialog({
    title: 'Delete AO session',
    message,
    actions: [
      {
        label: 'Delete AO Session',
        value: 'delete',
        variant: 'danger',
        busyLabel: 'Deleting...',
        run: async () => {
          card = (await api.del(cardPath(cardId, '/session'))).card;
        },
      },
    ],
  });
  if (!done) return;
  toast(`AO session ${sessionId} deleted from Agent Orchestrator.`, 'success');
  applyCard(card);
  onRefresh();
}

/**
 * Loads the models the session can use and what is selected, and fills the
 * selects next to the message box. A session that is not running reports
 * the catalog of its agent instead; its choice applies after Resume.
 */
async function loadSessionModels() {
  if (!ctx || !ctx.els.model) return;
  const active = ctx;
  try {
    const data = await api.get(cardPath(active.cardId, '/models'));
    if (ctx !== active) return;
    active.models = data;
  } catch (err) {
    if (ctx !== active) return;
    active.models = null;
    active.els.model.replaceChildren(new Option('Model unavailable', ''));
    active.els.model.disabled = true;
    active.els.model.title = `The model list could not be loaded: ${err.message}`;
    active.els.effort.hidden = true;
    rerender('sessionBar');
    return;
  }
  renderSessionModels();
}

function renderSessionModels() {
  const { els, models } = ctx;
  if (!models) return;
  const card = cardById(ctx.cardId);
  const usable = Boolean(card && card.session && !card.session.isTerminated);
  fillModelSelect(els.model, models.models, models.selected.model || '');
  els.model.disabled = !usable || models.models.length === 0;
  els.model.title = models.needsResume
    ? 'Model for this session. It is not running, so the choice applies once you resume it.'
    : 'Model for the next turn of this session';
  const hasEfforts = fillEffortSelect(els.effort, effectiveModel(models.models, els.model.value), models.selected.reasoningEffort || '');
  els.effort.hidden = !hasEfforts;
  els.effort.disabled = !usable;
  rerender('sessionBar');
}

/** Sends a model or effort change to AO and confirms it, or rolls it back. */
async function changeSessionSettings(change) {
  if (!ctx || !ctx.models) return;
  const active = ctx;
  const { els } = active;
  els.model.disabled = true;
  els.effort.disabled = true;
  try {
    const res = await api.patch(cardPath(active.cardId, '/settings'), change);
    if (ctx !== active) return;
    active.models = { ...active.models, selected: res.selected, needsResume: res.needsResume };
    const known = active.models.models.find((m) => m.id === res.selected.model);
    const name = res.selected.model ? (known ? known.label : res.selected.model) : 'the default model';
    const effort = res.selected.reasoningEffort ? `, effort ${res.selected.reasoningEffort}` : '';
    toast(res.needsResume ? `Model set to ${name}${effort}. It applies once you resume the session.` : `Model set to ${name}${effort} for the next turn.`, 'success');
  } catch (err) {
    if (ctx !== active) return;
    toast(err.code === 'SESSION_NOT_FOUND' ? 'AO no longer has this session, so its model cannot be changed.' : `The model was not changed: ${err.message}`, 'error');
  }
  if (ctx === active) renderSessionModels();
}

function canSend(card) {
  const s = card.session;
  return Boolean(s && !s.isTerminated && !s.needsResume && state.board.ao.connected);
}

function updateComposer() {
  if (!ctx || !ctx.els.composerInput) return;
  const card = cardById(ctx.cardId);
  if (!card) return;
  const { els } = ctx;
  const sendable = canSend(card);
  const harness = card.session ? harnessLabel(card.session.harness) : 'the agent';
  els.composerInput.disabled = !sendable;
  if (sendable) {
    els.composerInput.placeholder = `Message ${harness}`;
  } else if (card.session && card.session.needsResume) {
    els.composerInput.placeholder = 'Resume the session to send messages';
  } else if (card.session && card.session.isTerminated) {
    els.composerInput.placeholder = 'This session was terminated';
  } else {
    els.composerInput.placeholder = 'The session is unavailable';
  }
  els.send.disabled = !sendable || ctx.sending || els.composerInput.value.trim() === '';
  els.stop.hidden = !isSessionActive(card);
  els.composerHint.textContent = isSessionActive(card) ? `${harness} is working. New messages are queued by AO.` : 'Ctrl+Enter to send';
}

// Chat

function startChat(sessionId) {
  ctx.chat = {
    sessionId,
    items: new Map(),
    nodes: new Map(),
    expanded: new Set(),
    pending: [],
    hasMoreBefore: false,
    olderLoaded: false,
    loadedOnce: false,
    loading: false,
    streaming: false,
    error: null,
    timer: null,
    stopped: false,
    lastCount: 0,
    // awaiting: a message was sent and the agent has not answered yet.
    // busySince: when the current stretch of agent work began.
    awaiting: null,
    busySince: 0,
  };
  renderChat();
  pollChat();
}

function stopChat() {
  if (!ctx || !ctx.chat) return;
  ctx.chat.stopped = true;
  clearTimeout(ctx.chat.timer);
}

function schedulePoll() {
  const chat = ctx && ctx.chat;
  if (!chat || chat.stopped) return;
  clearTimeout(chat.timer);
  const card = cardById(ctx.cardId);
  const fast = isSessionActive(card) || chat.streaming || chat.pending.length > 0 || Boolean(chat.awaiting);
  let delay = fast ? POLL_ACTIVE_MS : POLL_IDLE_MS;
  if (document.hidden) delay = POLL_HIDDEN_MS;
  chat.timer = setTimeout(pollChat, delay);
}

async function pollChat() {
  const chat = ctx && ctx.chat;
  if (!chat || chat.stopped || chat.loading) return;
  chat.loading = true;
  try {
    const conv = await api.get(sessionPath(chat.sessionId, `/conversation?limit=${CHAT_PAGE}`));
    if (!ctx || ctx.chat !== chat) return;
    mergeConversation(chat, conv, false);
    chat.error = null;
  } catch (err) {
    if (!ctx || ctx.chat !== chat) return;
    chat.error = err;
  } finally {
    chat.loading = false;
  }
  chat.loadedOnce = true;
  renderChat();
  schedulePoll();
}

function mergeConversation(chat, conv, older) {
  const fetched = [...(conv.messages || []), ...(conv.activities || [])];
  if (older) {
    chat.hasMoreBefore = Boolean(conv.hasMoreBefore);
    chat.olderLoaded = true;
  } else {
    if (fetched.length > 0) {
      // Within the freshly fetched window the server is authoritative:
      // drop local items it no longer reports (edits, rollbacks).
      const floor = Math.min(...fetched.map((item) => item.sequence));
      const ids = new Set(fetched.map((item) => item.id));
      for (const [id, item] of chat.items) {
        if (item.sequence >= floor && !ids.has(id)) chat.items.delete(id);
      }
    }
    if (!chat.olderLoaded) chat.hasMoreBefore = Boolean(conv.hasMoreBefore);
    chat.streaming = (conv.messages || []).some((m) => m.streaming);
    const now = Date.now();
    chat.pending = chat.pending.filter((p) => {
      if (now - p.at > 90000) return false;
      return !(conv.messages || []).some(
        (m) => m.role === 'user' && (m.clientMessageId === p.id || ((m.text || '').trim() === p.text && Date.parse(m.createdAt) >= p.at - 10000)),
      );
    });
  }
  for (const item of fetched) chat.items.set(item.id, item);
}

async function loadEarlier() {
  const chat = ctx.chat;
  let oldest = Infinity;
  for (const item of chat.items.values()) oldest = Math.min(oldest, item.sequence);
  if (!Number.isFinite(oldest)) return;
  const conv = await api.get(sessionPath(chat.sessionId, `/conversation?limit=${CHAT_PAGE}&beforeSequence=${oldest}`));
  if (!ctx || ctx.chat !== chat) return;
  const scroller = ctx.els.chatScroll;
  const before = scroller.scrollHeight - scroller.scrollTop;
  mergeConversation(chat, conv, true);
  renderChat(true);
  scroller.scrollTop = scroller.scrollHeight - before;
}

function nearBottom() {
  const scroller = ctx.els.chatScroll;
  return scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 90;
}

function scrollChatToEnd(smooth) {
  const scroller = ctx.els.chatScroll;
  scroller.scrollTo({ top: scroller.scrollHeight, behavior: smooth ? 'smooth' : 'auto' });
  ctx.els.jump.hidden = true;
}

function roleLabel(message, card) {
  if (message.role === 'assistant') return card && card.session ? harnessLabel(card.session.harness) : 'Agent';
  if (message.origin === 'automation' || message.origin === 'daemon') return 'Automation';
  return 'You';
}

function renderMessage(message, card, chat) {
  const full = message.text || '';
  const long = full.length > LONG_MESSAGE && !chat.expanded.has(message.id);
  const text = long ? `${full.slice(0, LONG_MESSAGE)}\n\n...` : full;
  const body = h('div', { class: 'msg-body' }, renderMarkdown(text));
  if (long) {
    body.appendChild(
      h(
        'div',
        { class: 'edit-actions' },
        h(
          'button',
          {
            class: 'btn btn-sm',
            type: 'button',
            onClick: () => {
              chat.expanded.add(message.id);
              chat.nodes.delete(message.id);
              renderChat(true);
            },
          },
          `Show full message (${full.length.toLocaleString()} characters)`,
        ),
      ),
    );
  }
  return h(
    'div',
    { class: ['msg', `msg-${message.role}`, message.streaming && 'msg-streaming'] },
    h(
      'div',
      { class: 'msg-head' },
      message.role === 'assistant' ? brandIcon(card && card.session ? card.session.harness : '', 14) : null,
      h('span', { class: 'msg-role' }, roleLabel(message, card)),
      h('time', { title: new Date(message.createdAt).toLocaleString() }, fmtClock(message.createdAt)),
    ),
    body,
  );
}

function renderActivity(activity) {
  const done = activity.status === 'completed' || activity.status === 'resolved' || activity.status === 'recovered';
  return h(
    'div',
    { class: `activity is-${activity.status}`, title: `${activity.activityKind.replace(/_/g, ' ')}: ${activity.summary}` },
    icon(ACTIVITY_ICONS[activity.activityKind] || 'info', 12),
    h('span', { class: 'label' }, activity.summary || activity.activityKind.replace(/_/g, ' ')),
    done ? null : h('span', { class: 'status' }, activity.status),
  );
}

function itemKey(item) {
  if (item.kind === 'activity') return `a|${item.revision}|${item.status}|${item.summary}`;
  return `m|${item.revision}|${(item.text || '').length}|${item.streaming}`;
}

function renderChat(keepScroll = false) {
  if (!ctx || !ctx.chat) return;
  const chat = ctx.chat;
  const { els } = ctx;
  const scroller = els.chatScroll;
  const card = cardById(ctx.cardId);
  const wasNearBottom = nearBottom();
  const first = !chat.rendered;

  const items = Array.from(chat.items.values()).sort((a, b) => a.sequence - b.sequence || String(a.createdAt).localeCompare(String(b.createdAt)));
  const desired = [];
  if (chat.hasMoreBefore) desired.push(els.loadMore);

  if (!chat.loadedOnce) {
    desired.push(stateNode(chat, 'state:head', 'loading', h('div', { class: 'chat-empty' }, 'Loading conversation')));
  } else if (items.length === 0 && chat.pending.length === 0) {
    const text = chat.error ? chat.error.message : 'No messages yet. Send the first message below.';
    desired.push(stateNode(chat, 'state:head', `empty:${text}`, h('div', { class: 'chat-empty' }, text)));
  }

  const live = new Set();
  for (const item of items) {
    live.add(item.id);
    const key = itemKey(item);
    let entry = chat.nodes.get(item.id);
    if (!entry || entry.key !== key) {
      entry = { key, el: item.kind === 'activity' ? renderActivity(item) : renderMessage(item, card, chat) };
      chat.nodes.set(item.id, entry);
    }
    desired.push(entry.el);
  }
  for (const id of Array.from(chat.nodes.keys())) {
    if (!live.has(id) && !id.startsWith('state:') && !id.startsWith('pending:')) chat.nodes.delete(id);
  }

  for (const p of chat.pending) {
    const id = `pending:${p.id}`;
    let entry = chat.nodes.get(id);
    if (!entry) {
      entry = {
        key: id,
        el: h('div', { class: 'msg msg-user msg-pending' }, h('div', { class: 'msg-head' }, h('span', { class: 'msg-role' }, 'You'), h('span', null, 'Sending')), h('div', { class: 'msg-body' }, renderMarkdown(p.text))),
      };
      chat.nodes.set(id, entry);
    }
    desired.push(entry.el);
  }
  for (const id of Array.from(chat.nodes.keys())) {
    if (id.startsWith('pending:') && !chat.pending.some((p) => `pending:${p.id}` === id)) chat.nodes.delete(id);
  }

  if (chat.loadedOnce && chat.error && items.length > 0) {
    desired.push(stateNode(chat, 'state:tail', `error:${chat.error.message}`, h('div', { class: 'activity is-failed' }, icon('x-circle', 12), h('span', { class: 'label' }, `Conversation refresh failed: ${chat.error.message}`))));
  } else {
    const busy = chatBusy(card, chat, items);
    if (busy) desired.push(stateNode(chat, 'state:tail', `busy:${busy.label}|${busy.detail}|${busy.stoppable}`, thinkingBubble(card, busy, chat.busySince)));
  }

  let cursor = scroller.firstChild;
  for (const node of desired) {
    if (node === cursor) cursor = cursor.nextSibling;
    else scroller.insertBefore(node, cursor);
  }
  while (cursor) {
    const next = cursor.nextSibling;
    cursor.remove();
    cursor = next;
  }

  const grew = desired.length > chat.lastCount;
  chat.lastCount = desired.length;
  if (chat.loadedOnce) chat.rendered = true;
  if (keepScroll) return;
  if (first || wasNearBottom) {
    scroller.scrollTop = scroller.scrollHeight;
    els.jump.hidden = true;
  } else if (grew) {
    els.jump.hidden = false;
  }
}

const AWAIT_REPLY_MS = 90000;

function latestSequence(items) {
  let latest = 0;
  for (const item of items) latest = Math.max(latest, item.sequence || 0);
  return latest;
}

/**
 * Says what the agent is doing right now, or null when it is idle: a
 * message is on its way, the agent has not picked it up yet, it is
 * thinking (with the step it reports), or it is writing its reply.
 */
function chatBusy(card, chat, items) {
  const active = isSessionActive(card);
  const harness = card && card.session ? harnessLabel(card.session.harness) : 'Agent';
  if (chat.awaiting) {
    const wait = chat.awaiting;
    if (active || chat.streaming) wait.started = true;
    const answered = items.some((item) => item.kind !== 'activity' && item.role === 'assistant' && !item.streaming && item.sequence > wait.sequence);
    // The wait ends when the turn it started is over, or when nothing
    // happened for too long to still call it waiting.
    if ((wait.started && !active && !chat.streaming) || (answered && !active) || Date.now() - wait.at > AWAIT_REPLY_MS) chat.awaiting = null;
  }
  let busy = null;
  if (ctx.sending) {
    busy = { label: 'Sending your message...', detail: '', stoppable: false };
  } else if (active || chat.streaming) {
    let detail = '';
    if (!chat.streaming) {
      // The newest step the agent reports as still running.
      for (let i = items.length - 1; i >= 0 && i >= items.length - 12; i -= 1) {
        const item = items[i];
        if (item.kind === 'activity' && (item.status === 'running' || item.status === 'pending' || item.status === 'in_progress')) {
          detail = item.summary || item.activityKind.replace(/_/g, ' ');
          break;
        }
      }
    }
    busy = { label: chat.streaming ? `${harness} is writing a reply...` : `${harness} is thinking...`, detail, stoppable: active };
  } else if (chat.awaiting || chat.pending.length > 0) {
    busy = { label: `Waiting for ${harness} to start...`, detail: '', stoppable: false };
  }
  if (!busy) chat.busySince = 0;
  else if (!chat.busySince) chat.busySince = Date.now();
  return busy;
}

/**
 * The live row at the end of the conversation while the agent works: its
 * mark, animated dots, what it is doing, for how long, and Stop.
 */
function thinkingBubble(card, busy, since) {
  const harness = card && card.session ? card.session.harness : '';
  return h(
    'div',
    { class: 'msg msg-assistant msg-thinking', role: 'status' },
    h('div', { class: 'msg-head' }, brandIcon(harness, 14), h('span', { class: 'msg-role' }, harness ? harnessLabel(harness) : 'Agent'), elapsed(since, 2000)),
    h(
      'div',
      { class: 'thinking-body' },
      thinkingDots(),
      h('span', { class: 'thinking-label' }, busy.label),
      busy.detail ? h('span', { class: 'thinking-detail', title: busy.detail }, busy.detail) : null,
      busy.stoppable
        ? h(
            'button',
            { class: 'btn btn-sm btn-danger thinking-stop', type: 'button', title: 'Interrupt the running turn', onClick: (e) => withBusy(e.currentTarget, interrupt, null, 'Stopping...') },
            icon('stop', 12),
            h('span', null, 'Stop'),
          )
        : null,
    ),
  );
}

/** Caches placeholder rows (loading, empty, working) so they are not rebuilt on every poll. */
function stateNode(chat, id, key, el) {
  const entry = chat.nodes.get(id);
  if (entry && entry.key === key) return entry.el;
  chat.nodes.set(id, { key, el });
  return el;
}

async function sendMessage() {
  if (!ctx || !ctx.chat || ctx.sending) return;
  const { els, chat } = ctx;
  const text = els.composerInput.value.trim();
  const card = cardById(ctx.cardId);
  if (!text || !card || !canSend(card)) return;
  const pending = { id: uuid(), text, at: Date.now() };
  chat.pending.push(pending);
  els.composerInput.value = '';
  sessionStorageSet(ctx.sessionId, '');
  autoGrow(els.composerInput);
  ctx.sending = true;
  const restoreSend = setButtonBusy(els.send, 'Sending...');
  // The reply is awaited from here on: the conversation shows the agent
  // at work until its turn is over.
  chat.awaiting = { at: Date.now(), sequence: latestSequence(chat.items.values()), started: false };
  updateComposer();
  renderChat(true);
  scrollChatToEnd(false);
  const active = ctx;
  try {
    await api.post(sessionPath(chat.sessionId, '/messages'), { text, clientMessageId: pending.id });
  } catch (err) {
    chat.awaiting = null;
    chat.pending = chat.pending.filter((p) => p !== pending);
    if (ctx === active && !els.composerInput.value) {
      els.composerInput.value = text;
      sessionStorageSet(ctx.sessionId, text);
      autoGrow(els.composerInput);
    }
    toastError(err);
  } finally {
    restoreSend();
    if (ctx === active) {
      ctx.sending = false;
      updateComposer();
      const follow = nearBottom();
      renderChat(true);
      if (follow) scrollChatToEnd(false);
      clearTimeout(chat.timer);
      chat.timer = setTimeout(pollChat, 400);
      els.composerInput.focus();
    }
  }
}

async function interrupt() {
  await api.post(sessionPath(ctx.sessionId, '/interrupt'));
  toast('Interrupt sent.', 'info');
  onRefresh();
}

/** Syncs the open drawer with the shared state. Called on every state change. */
export function updateDrawer() {
  if (!ctx) return;
  const card = cardById(ctx.cardId);
  if (!card) {
    closeDrawer();
    toast('The open card no longer exists.', 'info');
    return;
  }
  if (card.aoSessionId !== ctx.sessionId) {
    // The session was linked, started or unlinked: rebuild the whole drawer.
    const id = ctx.cardId;
    const wasExpanded = ctx.expanded;
    closeDrawer(true);
    openDrawer(id);
    if (wasExpanded) setDrawerExpanded(true);
    return;
  }

  const session = card.session;
  section(
    'top',
    [card.category, card.columnId, card.autoMove, card.title, ghUrl(card), columns().map((c) => [c.id, c.title]), card.aoSessionId, session && session.harness, ctx.expanded],
    () => renderTop(card),
  );
  section('tags', card.tags, () => renderTags(card));
  if (!ctx.editing.summary || !ctx.sigs.summary) {
    section('summary', [card.summary, card.summarySource, ctx.editing.summary, Boolean(card.aoSessionId), sectionCollapsed('summary', card), Boolean(ctx.summarizing)], () => renderSummary(card));
  }
  if (!ctx.editing.notes || !ctx.sigs.notes) {
    section('notes', [card.notes, ctx.editing.notes, sectionCollapsed('notes', card)], () => renderNotes(card));
  }
  const linkInput = ctx.els.github.querySelector('.gh-link-form input');
  if (!(linkInput && (document.activeElement === linkInput || linkInput.value))) {
    section(
      'github',
      [
        card.ghRepo, card.ghNumber, card.ghKind, card.ghState, card.ghCiStatus, card.ghReviewDecision, card.ghTitle, card.ghAuthor,
        card.ghHeadSha, card.ghCommitCount, card.ghCommentCount, card.ghReviewCommentCount, card.ghLabels, card.ghUpdatedAt,
        card.ghLastSyncedAt, card.ghError, card.hasUnreadUpdates, card.updateCycles, card.updateSummary, card.ackAt,
        card.ghMyReviewState, card.ghMyReviewedAt, card.ghIsStaleReview, currentUser(), sectionCollapsed('github', card),
        (columnByRole('waiting') || {}).title, Math.floor(Date.now() / 60000),
      ],
      () => renderGitHub(card),
    );
  }

  if (!card.aoSessionId) {
    const ao = state.board.ao;
    const startActive = ctx.els.agentStart.contains(document.activeElement) && document.activeElement.tagName !== 'BUTTON';
    if (!startActive) {
      section('agentStart', [ao.connected, ao.error, ao.defaultProject, (ao.projects || []).map((p) => p.id), (ctx.sessionsForLink || []).map((s) => [s.id, s.cardId])], () => renderAgentStart(card));
    }
  } else {
    section(
      'sessionBar',
      [
        session && [session.harness, session.branch, session.isTerminated, session.needsResume, session.activity],
        card.sessionMissing,
        ctx.expanded,
        ctx.el.classList.contains('details-collapsed'),
        ctx.el.classList.contains('rail-collapsed'),
        ctx.models && ctx.models.selected,
      ],
      () => renderSessionBar(card),
    );
    section(
      'sessionAlerts',
      [session && [session.isTerminated, session.needsResume, session.activity && session.activity.state], card.sessionMissing, state.board.ao.connected, ctx.sessionError, Boolean(ctx.resuming)],
      () => renderSessionAlerts(card),
    );
    updateComposer();
    // The model list depends on whether the agent runs: load it when the
    // drawer opens and again after a resume or a termination.
    const modelsKey = JSON.stringify([Boolean(session), session && session.needsResume, session && session.isTerminated, state.board.ao.connected]);
    if (modelsKey !== ctx.modelsKey) {
      ctx.modelsKey = modelsKey;
      if (session && state.board.ao.connected) loadSessionModels();
    }
    const active = isSessionActive(card);
    if (ctx.chat && active !== ctx.chat.wasActive) {
      // The agent started or finished a turn: refresh now instead of waiting
      // for the next scheduled poll.
      ctx.chat.wasActive = active;
      const follow = nearBottom();
      renderChat(true);
      if (follow) scrollChatToEnd(false);
      clearTimeout(ctx.chat.timer);
      ctx.chat.timer = setTimeout(pollChat, 150);
    }
  }
  loadTimeline(false);
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && ctx && ctx.chat && !ctx.chat.stopped) {
    clearTimeout(ctx.chat.timer);
    pollChat();
  }
});
