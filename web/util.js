// Small DOM and formatting helpers shared by every module.

const BOOLEAN_PROPS = new Set(['hidden', 'disabled', 'checked', 'selected', 'draggable', 'open', 'readOnly', 'multiple']);

/**
 * Creates an element. Attributes: `class`, `dataset`, `style` (custom
 * properties and style fields), `on<Event>` listeners, boolean properties,
 * `value`, and plain attributes. Children may be nodes, strings, arrays or
 * falsy values (skipped). Strings always become text nodes.
 */
export function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  if (attrs) {
    for (const [key, value] of Object.entries(attrs)) {
      if (value === null || value === undefined) continue;
      if (key === 'class') {
        el.className = Array.isArray(value) ? value.filter(Boolean).join(' ') : value;
      } else if (key === 'dataset') {
        for (const [k, v] of Object.entries(value)) {
          if (v !== null && v !== undefined) el.dataset[k] = v;
        }
      } else if (key === 'style') {
        for (const [k, v] of Object.entries(value)) {
          if (k.startsWith('--')) el.style.setProperty(k, v);
          else el.style[k] = v;
        }
      } else if (key.startsWith('on') && typeof value === 'function') {
        el.addEventListener(key.slice(2).toLowerCase(), value);
      } else if (BOOLEAN_PROPS.has(key)) {
        el[key] = Boolean(value);
      } else if (key === 'value') {
        el.value = value;
      } else if (value === true) {
        el.setAttribute(key, '');
      } else if (value !== false) {
        el.setAttribute(key, String(value));
      }
    }
  }
  append(el, children);
  return el;
}

export function append(parent, children) {
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (Array.isArray(child)) append(parent, child);
    else if (child instanceof Node) parent.appendChild(child);
    else parent.appendChild(document.createTextNode(String(child)));
  }
}

export function clear(el) {
  el.replaceChildren();
  return el;
}

const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

function parseTime(value) {
  if (!value) return null;
  const t = new Date(value);
  return Number.isNaN(t.getTime()) ? null : t;
}

/** Compact relative time: "just now", "5m ago", "3h ago", "2d ago", "Nov 12". */
export function relTime(value) {
  const t = parseTime(value);
  if (!t) return '';
  const seconds = Math.round((Date.now() - t.getTime()) / 1000);
  if (seconds < 45) return 'just now';
  if (seconds < 3600) return `${Math.max(1, Math.round(seconds / 60))}m ago`;
  if (seconds < 86400) return `${Math.round(seconds / 3600)}h ago`;
  if (seconds < 86400 * 30) return `${Math.round(seconds / 86400)}d ago`;
  const sameYear = t.getFullYear() === new Date().getFullYear();
  return `${MONTHS[t.getMonth()]} ${t.getDate()}${sameYear ? '' : `, ${t.getFullYear()}`}`;
}

function pad(n) {
  return String(n).padStart(2, '0');
}

/** Absolute local time: "Nov 24, 13:04". */
export function fmtDateTime(value) {
  const t = parseTime(value);
  if (!t) return '';
  return `${MONTHS[t.getMonth()]} ${t.getDate()}, ${pad(t.getHours())}:${pad(t.getMinutes())}`;
}

export function fmtClock(value) {
  const t = parseTime(value);
  if (!t) return '';
  const today = new Date();
  const sameDay = t.toDateString() === today.toDateString();
  const clock = `${pad(t.getHours())}:${pad(t.getMinutes())}`;
  return sameDay ? clock : `${MONTHS[t.getMonth()]} ${t.getDate()}, ${clock}`;
}

export function uuid() {
  if (globalThis.crypto && typeof globalThis.crypto.randomUUID === 'function') {
    return globalThis.crypto.randomUUID();
  }
  const bytes = new Uint8Array(16);
  globalThis.crypto.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export function debounce(fn, wait) {
  let timer = null;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
}

export function plural(n, noun) {
  return `${n} ${noun}${n === 1 ? '' : 's'}`;
}

export function storageGet(key, fallback) {
  try {
    const raw = localStorage.getItem(key);
    return raw === null ? fallback : JSON.parse(raw);
  } catch (err) {
    return fallback;
  }
}

export function storageSet(key, value) {
  try {
    localStorage.setItem(key, JSON.stringify(value));
  } catch (err) {
    // Storage may be unavailable (private mode); preferences then last for the page only.
  }
}

/** Grows a textarea to fit its content, up to its CSS max-height. */
export function autoGrow(textarea) {
  textarea.style.height = 'auto';
  textarea.style.height = `${textarea.scrollHeight + 2}px`;
}

export async function copyText(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (err) {
    const area = h('textarea', { value: text, style: { position: 'fixed', opacity: '0' } });
    document.body.appendChild(area);
    area.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch (err2) {
      ok = false;
    }
    area.remove();
    return ok;
  }
}
