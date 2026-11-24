// Toasts, modal dialogs and async-button helpers.

import { h } from './util.js';
import { icon, setIcon } from './icons.js';

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

/**
 * Runs an async action while the button is disabled and its icon spins.
 * Errors are shown as a toast unless onError handles them (returns true).
 */
export async function withBusy(button, action, onError) {
  if (button.disabled) return undefined;
  button.disabled = true;
  const span = button.querySelector('.icon');
  const original = span ? span.dataset.icon || null : null;
  const originalNodes = span ? Array.from(span.childNodes) : null;
  if (span) {
    setIcon(span, 'loader', Number(span.dataset.size) || 14);
    span.classList.add('spin');
  }
  try {
    return await action();
  } catch (err) {
    if (!onError || !onError(err)) toastError(err);
    return undefined;
  } finally {
    button.disabled = false;
    if (span) {
      span.classList.remove('spin');
      span.replaceChildren(...originalNodes);
      if (original) span.dataset.icon = original;
      else delete span.dataset.icon;
    }
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
export function openModal({ title, size, body, foot, onClose }) {
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
      if (controller.closed) return;
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
