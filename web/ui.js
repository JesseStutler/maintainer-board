// Toasts, modal dialogs and async-button helpers.

import { h } from './util.js';
import { icon } from './icons.js';

const TOAST_ICONS = { success: 'check-circle', error: 'x-circle', warning: 'alert', info: 'info' };

/** Shows a transient message. Errors stay longer than confirmations. */
export function toast(message, type = 'info', timeout) {
  const root = document.getElementById('toasts');
  const el = h(
    'div',
    { class: `toast toast-${type}`, role: type === 'error' ? 'alert' : 'status' },
    icon(TOAST_ICONS[type] || 'info', 15),
    h('div', { class: 'toast-body' }, message),
    h('button', { class: 'toast-close', type: 'button', 'aria-label': 'Dismiss', onClick: () => el.remove() }, icon('x', 13)),
  );
  root.appendChild(el);
  while (root.children.length > 5) root.firstChild.remove();
  const ms = timeout ?? (type === 'error' ? 9000 : type === 'warning' ? 7000 : 4000);
  setTimeout(() => el.remove(), ms);
  return el;
}

export function toastError(err) {
  const message = err && err.message ? err.message : 'Something went wrong.';
  toast(message, 'error');
}

/** A small rotating ring: something is in progress. */
export function spinner(size = 14) {
  return h('span', { class: 'spinner', role: 'presentation', 'aria-hidden': 'true', style: { width: `${size}px`, height: `${size}px` } });
}

/** Three dots in a slow wave: the agent or a model is thinking. */
export function thinkingDots() {
  return h('span', { class: 'thinking-dots', 'aria-hidden': 'true' }, h('i'), h('i'), h('i'));
}

function elapsedText(since) {
  const seconds = Math.max(0, Math.floor((Date.now() - since) / 1000));
  if (seconds < 60) return `${seconds}s`;
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`;
}

/**
 * A counter of the seconds since `since` ("3s", "1m 04s"). It stays empty
 * for the first `after` milliseconds, so quick operations show no number,
 * and stops by itself once the element left the page.
 */
export function elapsed(since = Date.now(), after = 2000) {
  const el = h('span', { class: 'elapsed mono', 'aria-hidden': 'true' });
  const tick = () => {
    el.textContent = Date.now() - since >= after ? elapsedText(since) : '';
  };
  tick();
  const timer = setInterval(() => {
    if (!el.isConnected && Date.now() - since > 1500) clearInterval(timer);
    else tick();
  }, 500);
  return el;
}

/**
 * A row that reports a running operation: spinner, label, optional detail
 * and the elapsed time. `since` keeps the counter going across re-renders.
 */
export function statusStrip(label, { since = Date.now(), detail = '', dots = false, actions = [] } = {}) {
  return h(
    'div',
    { class: 'status-strip', role: 'status' },
    dots ? thinkingDots() : spinner(14),
    h('span', { class: 'status-label' }, label),
    detail ? h('span', { class: 'status-detail' }, detail) : null,
    elapsed(since),
    actions.length ? h('span', { class: 'status-actions' }, actions) : null,
    h('span', { class: 'progress-strip', 'aria-hidden': 'true' }),
  );
}

/**
 * Puts a button into its busy state: disabled, with a spinner in place of
 * its icon and, when given, `busyLabel` ("Resuming...") in place of its
 * label. An icon-only button shows just the spinner. Returns a function
 * that restores the button exactly as it was.
 */
export function setButtonBusy(button, busyLabel) {
  if (button.getAttribute('aria-busy') === 'true') return () => {};
  const saved = Array.from(button.childNodes);
  const hadLabel = button.textContent.trim() !== '';
  const wasDisabled = button.disabled;
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  button.classList.add('is-busy');
  const nodes = [spinner(13)];
  // "btn-label" lets a collapsible header button hide the text as before.
  if (hadLabel) nodes.push(h('span', { class: 'btn-label busy-label' }, busyLabel || button.textContent.trim()));
  button.replaceChildren(...nodes);
  return () => {
    button.replaceChildren(...saved);
    button.removeAttribute('aria-busy');
    button.classList.remove('is-busy');
    button.disabled = wasDisabled;
  };
}

/**
 * Runs an async action while the button shows its busy state (see
 * setButtonBusy). Errors are shown as a toast unless onError handles them
 * (returns true).
 */
export async function withBusy(button, action, onError, busyLabel) {
  if (button.disabled) return undefined;
  const restore = setButtonBusy(button, busyLabel);
  try {
    return await action();
  } catch (err) {
    if (!onError || !onError(err)) toastError(err);
    return undefined;
  } finally {
    restore();
  }
}

const modalStack = [];

function focusable(root) {
  return Array.from(
    root.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'),
  ).filter((el) => !el.disabled && el.offsetParent !== null);
}

/**
 * Opens a modal dialog. `body` and `foot` are nodes (or arrays of nodes).
 * Returns a controller with `close()` and the body and foot containers.
 */
export function openModal({ title, size, body, foot, onClose, canClose }) {
  const previous = document.activeElement;
  const bodyEl = h('div', { class: 'modal-body' }, body);
  const footEl = foot ? h('div', { class: 'modal-foot' }, foot) : null;
  const modal = h(
    'div',
    { class: ['modal', size ? `modal-${size}` : ''], role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
    h(
      'div',
      { class: 'modal-head' },
      h('h2', { class: 'modal-title' }, title),
      h('span', { class: 'spacer' }),
      h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', 'aria-label': 'Close', title: 'Close (Esc)', onClick: () => controller.close() }, icon('x', 15)),
    ),
    bodyEl,
    footEl,
  );
  const backdrop = h('div', { class: 'modal-backdrop' }, modal);
  let pressedOnBackdrop = false;
  backdrop.addEventListener('mousedown', (e) => {
    pressedOnBackdrop = e.target === backdrop;
  });
  backdrop.addEventListener('click', (e) => {
    if (e.target === backdrop && pressedOnBackdrop) controller.close();
  });
  modal.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab') return;
    const items = focusable(modal);
    if (items.length === 0) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });

  const controller = {
    el: modal,
    body: bodyEl,
    foot: footEl,
    closed: false,
    close() {
      if (controller.closed || (canClose && !canClose())) return;
      controller.closed = true;
      backdrop.remove();
      const index = modalStack.indexOf(controller);
      if (index >= 0) modalStack.splice(index, 1);
      if (onClose) onClose();
      if (previous && typeof previous.focus === 'function' && document.contains(previous)) previous.focus();
    },
  };
  modalStack.push(controller);
  document.getElementById('modal-root').appendChild(backdrop);
  const target = modal.querySelector('[data-autofocus]') || focusable(bodyEl)[0] || focusable(modal)[0];
  if (target) target.focus();
  return controller;
}

export function hasModal() {
  return modalStack.length > 0;
}

export function closeTopModal() {
  const top = modalStack[modalStack.length - 1];
  if (!top) return false;
  top.close();
  return true;
}

/** Asks for confirmation. Resolves to true when the user confirms. */
export function confirmDialog({ title, message, confirmLabel = 'Confirm', danger = false }) {
  return new Promise((resolve) => {
    let result = false;
    const confirm = h(
      'button',
      { class: ['btn', danger ? 'btn-danger-solid' : 'btn-primary'], type: 'button', 'data-autofocus': true },
      confirmLabel,
    );
    const cancel = h('button', { class: 'btn', type: 'button' }, 'Cancel');
    const modal = openModal({
      title,
      size: 'narrow',
      body: typeof message === 'string' ? h('p', { class: 'modal-text' }, message) : message,
      foot: [h('span', { class: 'spacer' }), cancel, confirm],
      onClose: () => resolve(result),
    });
    confirm.addEventListener('click', () => {
      result = true;
      modal.close();
    });
    cancel.addEventListener('click', () => modal.close());
  });
}

/**
 * Asks the user to choose one of several actions, and runs the chosen one
 * while the dialog stays open: its button shows `busyLabel` with a spinner,
 * the others are disabled. A failure is shown inside the dialog, which then
 * offers the choice again. Resolves to the `value` of the action that
 * succeeded, or null when the dialog was cancelled.
 *
 * actions: [{ label, value, run, busyLabel, variant ('primary' | 'danger'
 * | ''), icon, title, disabled, autofocus }]
 */
export function actionDialog({ title, message, actions, cancelLabel = 'Cancel', size = 'narrow' }) {
  return new Promise((resolve) => {
    let result = null;
    let running = false;
    const error = h('div', { class: 'field-error dialog-error', role: 'alert', hidden: true });
    const cancel = h('button', { class: 'btn', type: 'button' }, cancelLabel);
    const buttons = actions.map((action) => {
      const variant = action.variant === 'danger' ? 'btn-danger-solid' : action.variant === 'primary' ? 'btn-primary' : '';
      const button = h(
        'button',
        { class: ['btn', variant], type: 'button', title: action.title, disabled: Boolean(action.disabled), 'data-autofocus': action.autofocus ? true : null, dataset: { action: action.value } },
        action.icon ? icon(action.icon, 14) : null,
        h('span', null, action.label),
      );
      button.addEventListener('click', async () => {
        if (running) return;
        running = true;
        error.hidden = true;
        const others = [cancel, ...buttons].filter((b) => b !== button && !b.disabled);
        for (const b of others) b.disabled = true;
        const restore = setButtonBusy(button, action.busyLabel);
        let failure = null;
        try {
          await action.run();
        } catch (err) {
          failure = err || new Error('Something went wrong.');
        }
        restore();
        for (const b of others) b.disabled = false;
        running = false;
        if (failure) {
          error.textContent = failure.message || 'Something went wrong.';
          error.hidden = false;
          return;
        }
        result = action.value;
        modal.close();
      });
      return button;
    });
    const body = h('div', null, typeof message === 'string' ? h('p', { class: 'modal-text' }, message) : message, error);
    const modal = openModal({
      title,
      size,
      body,
      foot: [h('span', { class: 'spacer' }), cancel, ...buttons],
      // Closing in the middle of a request would hide its outcome.
      canClose: () => !running,
      onClose: () => resolve(result),
    });
    cancel.addEventListener('click', () => modal.close());
  });
}

/** Builds a segmented control; returns the element with a `value` accessor. */
export function segmented(options, initial, onChange) {
  let current = initial;
  const el = h('div', { class: 'segmented', role: 'group' });
  const buttons = options.map(([value, label]) => {
    const button = h('button', { type: 'button', class: value === current ? 'active' : '', dataset: { value } }, label);
    button.addEventListener('click', () => {
      if (current === value) return;
      current = value;
      for (const b of buttons) b.classList.toggle('active', b === button);
      if (onChange) onChange(value);
    });
    return button;
  });
  el.append(...buttons);
  Object.defineProperty(el, 'value', { get: () => current });
  return el;
}

/** Builds a <select> from [value, label] pairs. */
export function select(options, value, attrs = {}) {
  const el = h('select', { class: 'select', ...attrs });
  for (const [optValue, label] of options) {
    el.appendChild(h('option', { value: optValue }, label));
  }
  el.value = value;
  return el;
}
