// Card detail drawer: metadata, summary, GitHub tracking, notes, audit
// timeline and the embedded AO chat.

import { h, clear, relTime, fmtDateTime, fmtClock, uuid, autoGrow, plural, storageGet, storageSet } from './util.js';
import { icon } from './icons.js';
import { api, cardPath, sessionPath } from './api.js';
import { renderMarkdown } from './markdown.js';
import { toast, toastError, withBusy, confirmDialog, segmented, select } from './ui.js';
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
} from './state.js';

const WIDTH_KEY = 'mb.drawerWidth';
const COLLAPSE_KEY = 'mb.detailsCollapsed';
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
  applyWidth(storageGet(WIDTH_KEY, 680));
}

export function isDrawerOpen() {
  return ctx !== null;
}

function applyWidth(px) {
  const max = Math.max(420, window.innerWidth - 120);
  const width = Math.min(Math.max(Number(px) || 680, 460), max);
  document.documentElement.style.setProperty('--drawer-w', `${width}px`);
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
  document.body.classList.remove('drawer-open');
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
    { class: 'btn btn-sm', target: '_blank', rel: 'noopener noreferrer', title: 'Open the linked pull request or issue on GitHub' },
    icon('external', 13),
    h('span', null, 'Open GitHub'),
  );

  const top = h(
    'div',
    { class: 'drawer-top' },
    els.category,
    els.column,
    h(
      'label',
      { class: 'switch', title: 'Allow automatic column moves when the linked pull request or issue changes' },
      els.autoMove,
      h('span', { class: 'track' }),
      h('span', null, 'Auto-move'),
    ),
    h('span', { class: 'spacer' }),
    els.openGitHub,
    h('button', { class: 'btn btn-ghost btn-icon btn-sm btn-danger', type: 'button', title: 'Delete card', 'aria-label': 'Delete card', onClick: deleteCard }, icon('trash', 14)),
    h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: 'Close (Esc)', 'aria-label': 'Close', onClick: () => closeDrawer() }, icon('x', 15)),
  );

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
    h('summary', null, icon('chevron-right', 12), 'Timeline'),
    h('div', { class: 'panel' }, els.timelineList),
  );
  els.timeline.addEventListener('toggle', () => {
    if (els.timeline.open) loadTimeline(true);
  });

  const details = h('div', { class: 'drawer-details' }, els.title, els.tags, els.summary, els.github, els.notes);
  if (!card.aoSessionId) details.appendChild(els.agentStart);
  details.appendChild(els.timeline);

  const el = h('aside', { class: 'drawer', 'aria-label': 'Card details' }, resizeHandle(), top, details);

  if (card.aoSessionId) {
    els.sessionBar = h('div', { class: 'session-bar' });
    els.sessionNote = h('div', { class: 'session-note' });
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
      { class: 'btn btn-sm btn-danger', type: 'button', hidden: true, title: 'Interrupt the running turn', onClick: (e) => withBusy(e.currentTarget, interrupt) },
      icon('stop', 13),
      h('span', null, 'Stop'),
    );
    els.composerHint = h('span', { class: 'hint' }, 'Ctrl+Enter to send');
    el.append(
      els.sessionBar,
      els.sessionNote,
      els.sessionAlerts,
      h('div', { class: 'chat' }, els.chatScroll, els.jump),
      h(
        'div',
        { class: 'composer' },
        h('div', { class: 'composer-box' }, els.composerInput, h('div', { class: 'composer-actions' }, els.composerHint, h('span', { class: 'spacer' }), els.stop, els.send)),
      ),
    );
    el.classList.add('has-chat');
    if (storageGet(COLLAPSE_KEY, false)) el.classList.add('details-collapsed');
  }

  return {
    cardId: card.id,
    sessionId: card.aoSessionId,
    el,
    els,
    sigs: {},
    editing: { summary: false, notes: false },
    sessionError: null,
    sending: false,
    timelineStamp: '',
    chat: null,
    sessionsForLink: null,
    harness: 'copilot',
  };
}

function resizeHandle() {
  const handle = h('div', { class: 'drawer-resize', title: 'Drag to resize' });
  handle.addEventListener('mousedown', (e) => {
    e.preventDefault();
    handle.classList.add('is-active');
    const move = (ev) => {
      storageSet(WIDTH_KEY, applyWidth(window.innerWidth - ev.clientX));
    };
    const up = () => {
      handle.classList.remove('is-active');
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);
    };
    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  });
  return handle;
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

async function deleteCard() {
  const card = cardById(ctx.cardId);
  if (!card) return;
  const message = h(
    'div',
    null,
    h('p', { class: 'modal-text' }, 'Delete ', h('strong', null, card.title), '? Notes, summary and the timeline of this card are removed.'),
    card.aoSessionId
      ? h('p', { class: 'modal-text' }, 'The AO session ', h('strong', null, card.aoSessionId), ' keeps running in AO and will not get a new card automatically.')
      : null,
  );
  if (!(await confirmDialog({ title: 'Delete card', message, confirmLabel: 'Delete Card', danger: true }))) return;
  try {
    await api.del(cardPath(card.id));
    closeDrawer();
    toast('Card deleted.', 'success');
    onRefresh();
  } catch (err) {
    toastError(err);
  }
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
  const head = h(
    'div',
    { class: 'section-head' },
    h('h3', { class: 'section-title' }, 'Summary'),
    h('span', { class: `badge badge-source-${source}`, title: sourceTitle(source) }, source),
    h('span', { class: 'spacer' }),
    h(
      'button',
      {
        class: 'btn btn-sm',
        type: 'button',
        title: 'Ask the local Copilot CLI (Codex as fallback) for a two-line objective and status',
        onClick: (e) => withBusy(e.currentTarget, summarize),
      },
      icon('sparkles', 13),
      h('span', null, 'Summarize with AI'),
    ),
    h(
      'button',
      {
        class: 'btn btn-sm',
        type: 'button',
        onClick: () => {
          ctx.editing.summary = true;
          rerender('summary');
        },
      },
      icon('pencil', 13),
      h('span', null, 'Edit'),
    ),
  );
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
  els.summary.replaceChildren(head, body);
}

function sourceTitle(source) {
  if (source === 'ai') return 'Written by the AI summarizer';
  if (source === 'manual') return 'Edited by you; automatic extraction is paused';
  return 'Extracted automatically from the latest agent messages';
}

async function summarize() {
  const res = await api.post(cardPath(ctx.cardId, '/summarize'));
  ctx.editing.summary = false;
  applyCard(res.card);
  toast(`Summary updated with ${harnessLabel(res.engine)}.`, 'success');
}

function renderNotes(card) {
  const { els } = ctx;
  const startEdit = () => {
    ctx.editing.notes = true;
    rerender('notes');
  };
  const head = h(
    'div',
    { class: 'section-head' },
    h('h3', { class: 'section-title' }, 'Maintainer Notes'),
    h('span', { class: 'spacer' }),
    ctx.editing.notes ? null : h('button', { class: 'btn btn-sm', type: 'button', onClick: startEdit }, icon('pencil', 13), h('span', null, 'Edit')),
  );
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
      });
    };
    button.addEventListener('click', submit);
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        submit();
      }
    });
    nodes.push(
      h('div', { class: 'section-head' }, h('h3', { class: 'section-title' }, 'GitHub Tracking')),
      h('div', { class: 'gh-link-form' }, input, button),
      h('p', { class: 'hint' }, 'Linking records the current state as the reviewed baseline. Later commits, comments, CI results and review decisions show up as updates.'),
    );
    els.github.replaceChildren(...nodes);
    return;
  }

  const syncButton = h(
    'button',
    {
      class: 'btn btn-sm',
      type: 'button',
      title: 'Fetch the latest state from GitHub',
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
        }),
    },
    icon('refresh', 13),
    h('span', null, 'Sync Now'),
  );
  const unlinkButton = h(
    'button',
    {
      class: 'btn btn-sm',
      type: 'button',
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
    icon('unlink', 13),
    h('span', null, 'Unlink'),
  );
  nodes.push(h('div', { class: 'section-head' }, h('h3', { class: 'section-title' }, 'GitHub Tracking'), h('span', { class: 'spacer' }), syncButton, unlinkButton));

  const stack = h('div', { class: 'stack' });

  if (card.hasUnreadUpdates) {
    const waiting = columnByRole('waiting');
    const actions = h(
      'div',
      { class: 'alert-actions' },
      h(
        'button',
        {
          class: 'btn btn-sm btn-warning',
          type: 'button',
          title: 'Record the current state as reviewed and clear the update markers',
          onClick: (e) => withBusy(e.currentTarget, () => acknowledge('')),
        },
        icon('check', 13),
        h('span', null, 'Mark Caught Up'),
      ),
    );
    if (waiting) {
      actions.appendChild(
        h(
          'button',
          {
            class: 'btn btn-sm btn-warning',
            type: 'button',
            title: `Mark caught up and move the card to ${waiting.title}`,
            onClick: (e) => withBusy(e.currentTarget, () => acknowledge(waiting.id)),
          },
          h('span', null, 'Reviewed'),
          icon('arrow-right', 13),
          h('span', null, waiting.title),
        ),
      );
    }
    stack.appendChild(
      h(
        'div',
        { class: 'alert alert-warning', role: 'status' },
        icon('alert', 15),
        h(
          'div',
          { class: 'alert-body' },
          h(
            'div',
            { class: 'alert-title' },
            card.updateCycles > 1 ? `Updated ${card.updateCycles}x since your last review` : 'Updated since your last review',
          ),
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

  const stateInfo = ghStateInfo(card);
  const strip = h(
    'div',
    { class: 'gh-strip' },
    h('span', { class: 'badge' }, card.ghKind === 'issue' ? 'Issue' : 'PR'),
    refLink,
    pill(stateInfo.cls, stateInfo.label, ghIconName(card)),
  );
  if (card.ghKind === 'pr') {
    const ci = ciInfo(card.ghCiStatus);
    if (ci) strip.appendChild(pill(ci.cls, ci.label, ci.icon));
    const review = reviewInfo(card.ghReviewDecision);
    if (review) strip.appendChild(pill(review.cls, review.label, review.icon));
  }
  strip.appendChild(h('p', { class: 'gh-title' }, card.ghTitle));
  if (card.ghLabels && card.ghLabels.length) {
    strip.appendChild(h('div', { class: 'tag-row', style: { margin: '0' } }, card.ghLabels.map((l) => h('span', { class: 'tag' }, h('span', null, l)))));
  }

  const stats = h('div', { class: 'gh-stats' });
  if (card.ghAuthor) stats.appendChild(h('span', { class: 'meta-item', title: 'Author' }, icon('user', 12), h('span', { class: 'mono' }, card.ghAuthor)));
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

  stack.appendChild(h('div', { class: 'panel' }, strip, stats));
  nodes.push(stack);
  els.github.replaceChildren(...nodes);
}

async function acknowledge(moveToColumnId) {
  const body = moveToColumnId ? { moveToColumnId } : {};
  applyCard(await api.post(cardPath(ctx.cardId, '/ack'), body));
  toast(moveToColumnId ? 'Marked caught up and moved.' : 'Marked caught up.', 'success');
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

  const harness = segmented(
    [
      ['copilot', 'Copilot'],
      ['codex', 'Codex'],
    ],
    ctx.harness,
    (value) => {
      ctx.harness = value;
    },
  );
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
          });
          toast(`Started ${harnessLabel(harness.value)} session ${updated.aoSessionId}.`, 'success');
          applyCard(updated);
        }),
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
  const collapsed = ctx.el.classList.contains('details-collapsed');
  const nodes = [
    icon('bot', 15),
    h('span', { class: 'mono session-id', title: 'AO session ID' }, card.aoSessionId),
    h('span', { class: 'badge badge-harness' }, session ? harnessLabel(session.harness) : 'Agent'),
    h('span', { class: 'session-activity', title: session && session.activity ? `Last activity ${fmtDateTime(session.activity.lastActivityAt)}` : '' }, h('span', { class: `state-dot ${info.cls}` }), info.label),
  ];
  if (session && session.branch) {
    nodes.push(h('span', { class: 'session-branch mono', title: `Branch ${session.branch}` }, icon('branch', 12), h('span', null, session.branch)));
  }
  nodes.push(
    h('span', { class: 'spacer' }),
    h(
      'button',
      {
        class: 'btn btn-ghost btn-sm',
        type: 'button',
        title: collapsed ? 'Show summary, GitHub tracking and notes' : 'Hide the details above to give the conversation more room',
        onClick: () => {
          const next = !ctx.el.classList.contains('details-collapsed');
          ctx.el.classList.toggle('details-collapsed', next);
          storageSet(COLLAPSE_KEY, next);
          rerender('sessionBar');
        },
      },
      icon(collapsed ? 'chevron-down' : 'chevron-up', 13),
      h('span', null, collapsed ? 'Show Details' : 'Hide Details'),
    ),
  );
  if (session && !session.isTerminated) {
    nodes.push(
      h('button', { class: 'btn btn-ghost btn-icon btn-sm btn-danger', type: 'button', title: 'Terminate this AO session', 'aria-label': 'Terminate session', onClick: terminateSession }, icon('power', 14)),
    );
  }
  nodes.push(h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: 'Unlink this session from the card', 'aria-label': 'Unlink session', onClick: unlinkSession }, icon('unlink', 14)));
  els.sessionBar.replaceChildren(...nodes);

  els.sessionNote.hidden = !session;
  els.sessionNote.textContent = 'This is the same live session shown in AO desktop. Messages sent here appear there, and the other way around.';
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
  const unlink = h('button', { class: 'btn btn-sm', type: 'button', onClick: unlinkSession }, icon('unlink', 13), h('span', null, 'Unlink Session'));

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
    nodes.push(alertBox('info', 'info', 'This session was terminated', 'The history stays readable. Unlink it to start a new agent session from this card.', [unlink]));
  } else if (session.needsResume) {
    const resume = h('button', { class: 'btn btn-sm btn-primary', type: 'button', onClick: (e) => withBusy(e.currentTarget, resumeSession, onResumeError) }, icon('play', 13), h('span', null, 'Resume Session'));
    nodes.push(
      alertBox('info', 'info', 'The agent is not running', 'This imported session shows its history, but the agent has to be resumed before it can take new messages.', [resume]),
    );
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
  ctx.sessionError = null;
  await api.post(sessionPath(ctx.sessionId, '/resume'));
  toast(`Session ${ctx.sessionId} resumed.`, 'success');
  onRefresh();
  pollChat();
}

async function terminateSession() {
  const ok = await confirmDialog({
    title: 'Terminate session',
    message: `Terminate AO session ${ctx.sessionId}? The agent process is stopped; the conversation history stays readable.`,
    confirmLabel: 'Terminate',
    danger: true,
  });
  if (!ok || !ctx) return;
  try {
    await api.post(sessionPath(ctx.sessionId, '/kill'));
    toast(`Session ${ctx.sessionId} terminated.`, 'success');
    onRefresh();
  } catch (err) {
    toastError(err);
  }
}

async function unlinkSession() {
  const ok = await confirmDialog({
    title: 'Unlink session',
    message: `Detach AO session ${ctx.sessionId} from this card? The session keeps running in AO and will not get a new card automatically.`,
    confirmLabel: 'Unlink',
  });
  if (!ok || !ctx) return;
  try {
    await patchCard({ aoSessionId: '' });
  } catch (err) {
    toastError(err);
  }
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
  const fast = isSessionActive(card) || chat.streaming || chat.pending.length > 0;
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
    h('div', { class: 'msg-head' }, h('span', { class: 'msg-role' }, roleLabel(message, card)), h('time', { title: new Date(message.createdAt).toLocaleString() }, fmtClock(message.createdAt))),
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
  } else if (isSessionActive(card) && !chat.streaming) {
    const harness = card.session ? harnessLabel(card.session.harness) : 'Agent';
    desired.push(stateNode(chat, 'state:tail', 'working', h('div', { class: 'working' }, icon('loader', 13, 'spin'), `${harness} is working`)));
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
  updateComposer();
  renderChat(true);
  scrollChatToEnd(false);
  const active = ctx;
  try {
    await api.post(sessionPath(chat.sessionId, '/messages'), { text, clientMessageId: pending.id });
  } catch (err) {
    chat.pending = chat.pending.filter((p) => p !== pending);
    if (ctx === active && !els.composerInput.value) {
      els.composerInput.value = text;
      sessionStorageSet(ctx.sessionId, text);
      autoGrow(els.composerInput);
    }
    toastError(err);
  } finally {
    if (ctx === active) {
      ctx.sending = false;
      updateComposer();
      renderChat(true);
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
    closeDrawer(true);
    openDrawer(id);
    return;
  }

  const session = card.session;
  section('top', [card.category, card.columnId, card.autoMove, card.title, ghUrl(card), columns().map((c) => [c.id, c.title])], () => renderTop(card));
  section('tags', card.tags, () => renderTags(card));
  if (!ctx.editing.summary || !ctx.sigs.summary) {
    section('summary', [card.summary, card.summarySource, ctx.editing.summary, Boolean(card.aoSessionId)], () => renderSummary(card));
  }
  if (!ctx.editing.notes || !ctx.sigs.notes) {
    section('notes', [card.notes, ctx.editing.notes], () => renderNotes(card));
  }
  const linkInput = ctx.els.github.querySelector('.gh-link-form input');
  if (!(linkInput && (document.activeElement === linkInput || linkInput.value))) {
    section(
      'github',
      [
        card.ghRepo, card.ghNumber, card.ghKind, card.ghState, card.ghCiStatus, card.ghReviewDecision, card.ghTitle, card.ghAuthor,
        card.ghHeadSha, card.ghCommitCount, card.ghCommentCount, card.ghReviewCommentCount, card.ghLabels, card.ghUpdatedAt,
        card.ghLastSyncedAt, card.ghError, card.hasUnreadUpdates, card.updateCycles, card.updateSummary, card.ackAt,
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
      [session && [session.harness, session.branch, session.isTerminated, session.needsResume, session.activity], card.sessionMissing, ctx.el.classList.contains('details-collapsed')],
      () => renderSessionBar(card),
    );
    section(
      'sessionAlerts',
      [session && [session.isTerminated, session.needsResume, session.activity && session.activity.state], card.sessionMissing, state.board.ao.connected, ctx.sessionError],
      () => renderSessionAlerts(card),
    );
    updateComposer();
    const active = isSessionActive(card);
    if (ctx.chat && active !== ctx.chat.wasActive) {
      // The agent started or finished a turn: refresh now instead of waiting
      // for the next scheduled poll.
      ctx.chat.wasActive = active;
      renderChat(true);
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
