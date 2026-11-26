// Kanban board rendering and drag-and-drop.

import { h, relTime, plural } from './util.js';
import { icon, brandIcon } from './icons.js';
import { api, cardPath } from './api.js';
import { toast, toastError } from './ui.js';
import {
  state,
  emit,
  cards,
  cardById,
  columns,
  visibleColumns,
  columnCards,
  sortColumnCards,
  matchesFilters,
  isColumnCollapsed,
  toggleColumnCollapsed,
  isColumnCompact,
  toggleColumnCompact,
  isCardPinned,
  toggleCardPinned,
  categoryLabel,
  harnessLabel,
  activityInfo,
  ghRefLabel,
  ghStateInfo,
  ciInfo,
  reviewInfo,
  myReviewInfo,
  showCardReviewDecision,
  isMine,
  searchMatches,
  aiSearchActive,
} from './state.js';

let handlers = { onOpenCard: () => {}, onAddCard: () => {}, onRefresh: () => {} };
let drag = null;
let pendingRender = false;
let activeMenu = null;

export function initBoard(options) {
  handlers = { ...handlers, ...options };
}

function closeCardMenu() {
  if (!activeMenu) return;
  activeMenu.cleanup();
  activeMenu.el.remove();
  activeMenu = null;
}

/**
 * Floating quick-action menu for a card: pin to top or move directly to any
 * column without dragging across a crowded board.
 */
function openCardMenu(card, pos) {
  closeCardMenu();
  const pinned = isCardPinned(card.id);
  const pinBtn = h(
    'button',
    {
      class: 'card-menu-item',
      type: 'button',
      onClick: () => {
        closeCardMenu();
        toggleCardPinned(card.id);
        toast(pinned ? 'Card unpinned.' : 'Card pinned to top.', 'info');
      },
    },
    icon('pin', 13),
    h('span', null, pinned ? 'Unpin from top' : 'Pin to top'),
  );

  const targets = columns().map((col) => {
    const current = col.id === card.columnId;
    return h(
      'button',
      {
        class: ['card-menu-item', current && 'is-current'],
        type: 'button',
        disabled: current,
        onClick: () => {
          closeCardMenu();
          const first = columnCards(col.id).find((c) => c.id !== card.id);
          moveCard(card.id, col.id, first ? first.id : null, { toTop: true, label: col.title });
        },
      },
      h('span', { class: 'card-menu-dot', style: { background: col.color } }),
      h('span', { class: 'card-menu-label' }, col.title),
      current ? icon('check', 13) : null,
    );
  });

  const menu = h(
    'div',
    { class: 'card-menu', role: 'menu', 'aria-label': `Actions for ${card.title}` },
    pinBtn,
    h('div', { class: 'card-menu-sep' }),
    h('div', { class: 'card-menu-head' }, 'Move to'),
    ...targets,
  );
  menu.addEventListener('click', (e) => e.stopPropagation());
  document.body.appendChild(menu);

  const rect = menu.getBoundingClientRect();
  const x = Math.max(8, Math.min(pos.x, window.innerWidth - rect.width - 8));
  const y = Math.max(8, Math.min(pos.y, window.innerHeight - rect.height - 8));
  menu.style.left = `${x}px`;
  menu.style.top = `${y}px`;

  const onPointerDown = (e) => {
    if (!menu.contains(e.target)) closeCardMenu();
  };
  const onKeyDown = (e) => {
    if (e.key === 'Escape') {
      e.stopPropagation();
      closeCardMenu();
    }
  };
  const onDismiss = () => closeCardMenu();
  document.addEventListener('pointerdown', onPointerDown, true);
  document.addEventListener('keydown', onKeyDown, true);
  window.addEventListener('resize', onDismiss);
  window.addEventListener('scroll', onDismiss, true);
  activeMenu = {
    el: menu,
    cleanup: () => {
      document.removeEventListener('pointerdown', onPointerDown, true);
      document.removeEventListener('keydown', onKeyDown, true);
      window.removeEventListener('resize', onDismiss);
      window.removeEventListener('scroll', onDismiss, true);
    },
  };
}

/** A compact status chip for the card face. */
function chip(tone, label, iconName, title) {
  return h('span', { class: ['chip', tone && `chip-${tone}`], title: title || label }, iconName ? icon(iconName, 12) : null, h('span', null, label));
}

const CHIP_TONES = { 'is-success': 'success', 'is-danger': 'danger', 'is-warning': 'warning', 'is-merged': 'merged', 'is-you': 'you' };

/**
 * Agent badge of the card footer: brand mark, agent name and live state.
 * The AO session ID is deliberately kept off the card face; it is in the
 * tooltip here and in the header of the card workspace.
 */
function agentBadge(card) {
  if (!card.aoSessionId) return null;
  const info = activityInfo(card);
  const harness = card.session ? card.session.harness : '';
  const name = card.session ? harnessLabel(harness) : 'Agent';
  return h(
    'span',
    { class: ['agent', `agent-${harness || 'other'}`], title: `${name} session ${card.aoSessionId}: ${info.label}`, dataset: { harness: harness || 'other' } },
    brandIcon(harness, 15),
    h('span', { class: 'agent-name' }, name),
    h('span', { class: 'agent-state' }, h('span', { class: `state-dot ${info.cls}` }), h('span', null, info.label)),
  );
}

function cardTime(card) {
  if (card.session && card.session.activity && card.session.activity.lastActivityAt) {
    return { at: card.session.activity.lastActivityAt, what: 'Last agent activity' };
  }
  if (card.ghUpdatedAt) return { at: card.ghUpdatedAt, what: 'Last GitHub update' };
  return { at: card.updatedAt, what: 'Card updated' };
}

/** Top row: category, linked pull request or issue with its state, author. */
function cardTop(card) {
  const pinned = isCardPinned(card.id);
  const top = h('div', { class: 'card-top' }, h('span', { class: `badge badge-cat-${card.category}` }, categoryLabel(card.category)));
  if (pinned) {
    top.appendChild(h('span', { class: 'card-pin', title: 'Pinned to top of column' }, icon('pin', 11)));
  }
  if (card.ghNumber) {
    const info = ghStateInfo(card);
    const kind = card.ghKind === 'issue' ? 'Issue ' : card.ghKind === 'pr' ? 'PR ' : '';
    // One pill: what it is and where it stands, for example "PR #6247 | OPEN".
    top.appendChild(
      h(
        'span',
        { class: `gh-pill state-${card.ghState || 'NONE'}`, title: `${card.ghRepo}#${card.ghNumber}: ${card.ghTitle || 'not synced yet'} (${info.label})` },
        h('span', { class: 'gh-ref' }, `${kind}${ghRefLabel(card)}`),
        card.ghState ? h('span', { class: 'gh-state' }, card.ghState) : null,
      ),
    );
  }
  if (card.ghAuthor) {
    const mine = isMine(card);
    top.appendChild(
      h(
        'span',
        { class: ['card-author', mine && 'is-mine'], title: mine ? `Opened by you (@${card.ghAuthor})` : `Opened by @${card.ghAuthor}` },
        mine ? h('span', { class: 'badge badge-mine' }, 'MINE') : h('span', { class: 'login' }, `@${card.ghAuthor}`),
      ),
    );
  }
  return top;
}

/**
 * Status strip of a linked card: review decision, the maintainer's own
 * review, CI, and activity counts. Returns null for cards without a link.
 */
function cardStatus(card) {
  if (!card.ghNumber) return null;
  const strip = h('div', { class: 'card-status' });
  if (!card.ghLastSyncedAt) {
    strip.appendChild(chip(card.ghError ? 'danger' : '', card.ghError ? 'Sync failed' : 'Not synced', card.ghError ? 'x-circle' : 'clock', card.ghError || 'Waiting for the first sync with GitHub'));
    return strip;
  }
  const open = card.ghState !== 'MERGED' && card.ghState !== 'CLOSED';
  const isPR = card.ghKind === 'pr';
  const mine = myReviewInfo(card);
  if (isPR && open && showCardReviewDecision(card, mine)) {
    const review = reviewInfo(card.ghReviewDecision, isMine(card));
    if (review) strip.appendChild(chip(CHIP_TONES[review.tone], review.short, review.icon, review.title));
  }
  if (mine) {
    const node = chip(CHIP_TONES[mine.tone], mine.label, mine.icon, mine.title);
    node.classList.add('chip-mine');
    node.dataset.review = mine.kind;
    strip.appendChild(node);
  }
  if (isMine(card) && open && card.ghPendingFeedback && !card.hasUnreadUpdates) {
    const node = chip(
      'warning',
      card.ghPendingFeedback,
      'message',
      'Commented after your last commit or reply on GitHub. Clears automatically when you push or reply.',
    );
    node.classList.add('chip-mine');
    strip.appendChild(node);
  }
  if (isPR && open) {
    const ci = ciInfo(card.ghCiStatus);
    if (ci) strip.appendChild(chip(CHIP_TONES[ci.tone], ci.label, ci.icon, ci.title));
  }
  if (card.ghError) strip.appendChild(chip('danger', 'Sync failed', 'x-circle', card.ghError));

  const comments = card.ghCommentCount + card.ghReviewCommentCount;
  const counts = [];
  if (isPR) counts.push(plural(card.ghCommitCount, 'commit'));
  counts.push(plural(comments, 'comment'));
  const detail = isPR
    ? `${plural(card.ghCommentCount, 'conversation comment')} and ${plural(card.ghReviewCommentCount, 'review comment')}`
    : plural(card.ghCommentCount, 'comment');
  strip.appendChild(h('span', { class: 'card-counts', title: detail }, counts.join(' / ')));
  return strip;
}

function renderCard(card) {
  const el = h(
    'article',
    {
      class: ['card', card.hasUnreadUpdates && 'has-updates', isCardPinned(card.id) && 'is-pinned', state.selectedCardId === card.id && 'is-selected'],
      draggable: true,
      tabindex: '0',
      role: 'button',
      'aria-label': card.title,
      dataset: { id: card.id },
    },
    h(
      'button',
      {
        class: 'btn btn-ghost btn-icon btn-sm card-menu-btn',
        type: 'button',
        title: 'Quick move or pin (or right-click card)',
        'aria-label': 'Quick move or pin card',
        onClick: (e) => {
          e.stopPropagation();
          const rect = e.currentTarget.getBoundingClientRect();
          openCardMenu(card, { x: rect.left, y: rect.bottom + 4 });
        },
      },
      icon('more', 14),
    ),
    cardTop(card),
    h('h3', { class: 'card-title' }, card.title),
  );

  if (card.summary) {
    el.appendChild(h('p', { class: 'card-summary' }, card.summary.replace(/\s*\n\s*/g, ' ')));
  }

  // Why the card matches the current search, when the face does not show it.
  for (const match of searchMatches(card)) {
    el.appendChild(
      h(
        'div',
        { class: `card-match is-${match.kind}`, title: match.title || `${match.label}: ${match.text}`, dataset: { match: match.kind } },
        h('span', { class: 'label' }, `${match.label}:`),
        h('span', { class: 'text' }, match.text),
      ),
    );
  }

  const status = cardStatus(card);
  if (status) el.appendChild(status);

  if (card.hasUnreadUpdates) {
    el.appendChild(
      h(
        'div',
        { class: 'card-update', title: card.updateSummary || 'New activity since your last review' },
        icon('alert', 13),
        h('span', { class: 'text' }, card.updateSummary || 'New activity'),
      ),
    );
  }

  const tags = card.tags || [];
  if (tags.length > 0) {
    const row = h('div', { class: 'card-tags' });
    for (const tag of tags.slice(0, 4)) row.appendChild(h('span', { class: 'tag' }, h('span', null, tag)));
    if (tags.length > 4) row.appendChild(h('span', { class: 'tag', title: tags.slice(4).join(', ') }, `+${tags.length - 4}`));
    el.appendChild(row);
  }

  const time = cardTime(card);
  el.appendChild(
    h(
      'div',
      { class: 'card-foot' },
      agentBadge(card),
      h('span', { class: 'spacer' }),
      h('time', { class: 'card-time', title: `${time.what}: ${new Date(time.at).toLocaleString()}` }, relTime(time.at)),
    ),
  );

  el.addEventListener('click', () => handlers.onOpenCard(card.id));
  el.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    openCardMenu(card, { x: e.clientX, y: e.clientY });
  });
  el.addEventListener('keydown', (e) => {
    if (e.target === el && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      handlers.onOpenCard(card.id);
    }
  });
  el.addEventListener('dragstart', (e) => {
    drag = { cardId: card.id, indicator: h('div', { class: 'drop-indicator' }) };
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', card.id);
    requestAnimationFrame(() => el.classList.add('is-dragging'));
  });
  el.addEventListener('dragend', endDrag);
  return el;
}

function endDrag() {
  if (!drag) return;
  drag.indicator.remove();
  drag = null;
  for (const el of document.querySelectorAll('.is-drop-target')) el.classList.remove('is-drop-target');
  for (const el of document.querySelectorAll('.card.is-dragging')) el.classList.remove('is-dragging');
  if (pendingRender) {
    pendingRender = false;
    renderBoard();
  }
}

/** The card element the dragged card would be inserted before, or null. */
function dropBefore(body, clientY) {
  const candidates = Array.from(body.querySelectorAll('.card:not(.is-dragging)'));
  for (const el of candidates) {
    const rect = el.getBoundingClientRect();
    if (clientY < rect.top + rect.height / 2) return el;
  }
  return null;
}

async function moveCard(cardId, columnId, beforeId, opts = {}) {
  const card = cardById(cardId);
  if (!card) return;
  const others = columnCards(columnId).filter((c) => c.id !== cardId);
  let index;
  if (opts.toTop) {
    index = 0;
  } else if (beforeId) {
    index = others.findIndex((c) => c.id === beforeId);
    if (index < 0) index = others.length;
  } else {
    // Dropped below the last visible card: place it right after that card,
    // which may not be the end of the column while a filter is active.
    const visible = others.filter(matchesFilters);
    index = visible.length > 0 ? others.indexOf(visible[visible.length - 1]) + 1 : 0;
  }
  if (card.columnId === columnId && columnCards(columnId).findIndex((c) => c.id === cardId) === index) return;

  // Optimistic update; the server response and the next board event confirm it.
  const from = card.columnId;
  card.columnId = columnId;
  others.splice(index, 0, card);
  others.forEach((c, i) => {
    c.position = i;
  });
  if (from !== columnId) {
    columnCards(from).forEach((c, i) => {
      c.position = i;
    });
  }
  emit();
  try {
    await api.patch(cardPath(cardId, '/move'), { columnId, position: index });
    if (opts.label) toast(`Moved to ${opts.label}.`, 'success');
  } catch (err) {
    toastError(err);
  }
  handlers.onRefresh();
}

function renderColumn(column) {
  const all = columnCards(column.id);
  const visible = sortColumnCards(all.filter(matchesFilters), {
    aiMatches: aiSearchActive() ? state.ai.matches : null,
  });
  const count = visible.length === all.length ? String(all.length) : `${visible.length}/${all.length}`;
  const unreadInCol = visible.filter((c) => c.hasUnreadUpdates).length;

  if (isColumnCollapsed(column.id)) {
    const rail = h(
      'section',
      {
        class: ['column', 'is-collapsed', unreadInCol > 0 && 'has-unread'],
        'aria-label': `${column.title} (collapsed, ${count})`,
        title: `Expand ${column.title} (${plural(all.length, 'card')})`,
        dataset: { columnId: column.id, role: column.role },
        style: { '--col-accent': column.color },
      },
      h(
        'button',
        {
          class: 'column-rail-btn',
          type: 'button',
          onClick: () => toggleColumnCollapsed(column.id),
        },
        icon('chevron-right', 14),
        h('span', { class: 'column-accent' }),
        h('span', { class: ['column-count', unreadInCol > 0 && 'is-warning'] }, count),
        h('span', { class: 'column-rail-title' }, column.title),
      ),
    );
    rail.addEventListener('dragover', (e) => {
      if (!drag) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      for (const other of document.querySelectorAll('.column.is-drop-target')) {
        if (other !== rail) other.classList.remove('is-drop-target');
      }
      rail.classList.add('is-drop-target');
    });
    rail.addEventListener('dragleave', (e) => {
      if (!drag || rail.contains(e.relatedTarget)) return;
      rail.classList.remove('is-drop-target');
    });
    rail.addEventListener('drop', (e) => {
      if (!drag) return;
      e.preventDefault();
      const cardId = drag.cardId;
      endDrag();
      moveCard(cardId, column.id, null, { toTop: true, label: column.title });
    });
    return rail;
  }

  const body = h('div', { class: 'column-body', dataset: { columnId: column.id } });
  if (visible.length === 0) {
    body.appendChild(h('div', { class: 'column-empty' }, all.length > 0 ? 'No cards match the filters' : 'No cards'));
  } else {
    for (const card of visible) body.appendChild(renderCard(card));
  }

  const compact = isColumnCompact(column.id);
  const el = h(
    'section',
    {
      class: ['column', compact && 'is-compact'],
      'aria-label': column.title,
      dataset: { columnId: column.id, role: column.role },
      style: { '--col-accent': column.color },
    },
    h(
      'header',
      { class: 'column-header' },
      h('span', { class: 'column-accent' }),
      h('h2', { class: 'column-title', title: column.title }, column.title),
      h('span', { class: 'column-count', title: `${plural(all.length, 'card')} in this column` }, count),
      h('span', { class: 'spacer' }),
      h(
        'button',
        {
          class: ['btn', 'btn-ghost', 'btn-icon', 'btn-sm', 'column-compact-btn', compact && 'is-active'],
          type: 'button',
          title: compact ? `Normal card view in ${column.title}` : `Compact cards in ${column.title}`,
          'aria-label': compact ? `Normal card view in ${column.title}` : `Compact cards in ${column.title}`,
          'aria-pressed': String(compact),
          onClick: () => toggleColumnCompact(column.id),
        },
        icon('compact', 14),
      ),
      h(
        'button',
        { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: `Collapse ${column.title}`, 'aria-label': `Collapse ${column.title}`, onClick: () => toggleColumnCollapsed(column.id) },
        icon('chevron-left', 14),
      ),
      h(
        'button',
        { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', title: `New task in ${column.title}`, 'aria-label': `New task in ${column.title}`, onClick: () => handlers.onAddCard(column.id) },
        icon('plus', 14),
      ),
    ),
    body,
  );

  el.addEventListener('dragover', (e) => {
    if (!drag) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    for (const other of document.querySelectorAll('.column.is-drop-target')) {
      if (other !== el) other.classList.remove('is-drop-target');
    }
    el.classList.add('is-drop-target');
    const before = dropBefore(body, e.clientY);
    if (before) {
      if (drag.indicator.nextSibling !== before || drag.indicator.parentNode !== body) body.insertBefore(drag.indicator, before);
    } else if (drag.indicator.parentNode !== body || drag.indicator.nextSibling !== null) {
      body.appendChild(drag.indicator);
    }
  });
  el.addEventListener('dragleave', (e) => {
    if (!drag || el.contains(e.relatedTarget)) return;
    el.classList.remove('is-drop-target');
    if (drag.indicator.parentNode === body) drag.indicator.remove();
  });
  el.addEventListener('drop', (e) => {
    if (!drag) return;
    e.preventDefault();
    const cardId = drag.cardId;
    let next = drag.indicator.parentNode === body ? drag.indicator.nextElementSibling : dropBefore(body, e.clientY);
    while (next && (!next.classList.contains('card') || next.dataset.id === cardId)) next = next.nextElementSibling;
    const beforeId = next ? next.dataset.id : null;
    endDrag();
    moveCard(cardId, column.id, beforeId);
  });
  return el;
}

export function renderBoard() {
  const boardEl = document.getElementById('board');
  if (!state.board) return;
  if (drag) {
    pendingRender = true;
    return;
  }
  const scroll = new Map();
  for (const body of boardEl.querySelectorAll('.column-body')) scroll.set(body.dataset.columnId, body.scrollTop);
  const active = document.activeElement;
  const focusedId = active && active.classList && active.classList.contains('card') ? active.dataset.id : null;

  const cols = visibleColumns();
  if (cols.length === 0) {
    boardEl.replaceChildren(h('div', { class: 'board-empty' }, 'No columns to show. Enable Show Done or add a column.'));
  } else {
    boardEl.replaceChildren(...cols.map(renderColumn));
  }
  for (const body of boardEl.querySelectorAll('.column-body')) {
    const top = scroll.get(body.dataset.columnId);
    if (top) body.scrollTop = top;
  }
  if (focusedId) {
    const again = boardEl.querySelector(`.card[data-id="${CSS.escape(focusedId)}"]`);
    if (again) again.focus({ preventScroll: true });
  }

  const visibleIds = new Set(cols.map((c) => c.id));
  const inView = cards().filter((c) => visibleIds.has(c.columnId));
  const shown = inView.filter(matchesFilters).length;
  const countEl = document.getElementById('card-count');
  countEl.textContent = shown === inView.length ? plural(inView.length, 'card') : `${shown} of ${plural(inView.length, 'card')}`;
}
