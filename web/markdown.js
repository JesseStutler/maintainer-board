// A small, dependency-free Markdown renderer for chat messages and notes.
//
// Safety model: the parser produces a plain-object tree and the renderer
// builds DOM nodes with createElement and textContent only. No HTML string is
// ever assigned to innerHTML, so raw HTML in the input is shown as text, and
// link targets are restricted to http, https and mailto.

import { copyText } from './util.js';
import { icon } from './icons.js';

const MAX_DEPTH = 8;
const MAX_INPUT = 400000;

const LIST_RE = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const FENCE_RE = /^ {0,3}(`{3,}|~{3,})\s*([^\s`]*)[^`]*$/;
const HEADING_RE = /^ {0,3}(#{1,6})\s+(.+?)\s*#*\s*$/;
const HR_RE = /^ {0,3}([-*_])(?:\s*\1){2,}\s*$/;
const QUOTE_RE = /^ {0,3}>/;
const TABLE_RULE_RE = /^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)*\|?\s*$/;
const PUNCT_RE = /[!-/:-@[-`{-~]/;
const WORD_RE = /[A-Za-z0-9]/;
// Bare URLs stop at the first non-ASCII character so that CJK text directly
// following a link is not swallowed into it.
const URL_RE = /https?:\/\/[\x21-\x3b\x3d\x3f-\x7e]+/y;
const ANGLE_LINK_RE = /<((?:https?:\/\/|mailto:)[^\s<>]+)>/y;

/** Returns a normalized URL when raw is an http, https or mailto link, else null. */
export function safeHref(raw) {
  if (typeof raw !== 'string') return null;
  // Browsers ignore control characters and whitespace inside a scheme, so
  // they are removed before the scheme is checked.
  const value = raw.replace(/[\u0000-\u0020\u007f]+/g, '');
  if (!value) return null;
  let url;
  try {
    url = new URL(value);
  } catch (err) {
    return null;
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:' && url.protocol !== 'mailto:') return null;
  return url.href;
}

function indentWidth(line) {
  let width = 0;
  for (const ch of line) {
    if (ch === ' ') width += 1;
    else if (ch === '\t') width += 4;
    else break;
  }
  return width;
}

function stripIndent(line, columns) {
  let removed = 0;
  let i = 0;
  while (i < line.length && removed < columns) {
    if (line[i] === ' ') removed += 1;
    else if (line[i] === '\t') removed += 4;
    else break;
    i += 1;
  }
  return line.slice(i);
}

function isBlank(line) {
  return line.trim() === '';
}

function splitRow(line) {
  let row = line.trim();
  if (row.startsWith('|')) row = row.slice(1);
  if (row.endsWith('|') && !row.endsWith('\\|')) row = row.slice(0, -1);
  return row.split(/(?<!\\)\|/).map((cell) => cell.trim().replace(/\\\|/g, '|'));
}

function isTableStart(lines, i) {
  return (
    lines[i].includes('|') &&
    i + 1 < lines.length &&
    lines[i + 1].includes('-') &&
    TABLE_RULE_RE.test(lines[i + 1]) &&
    splitRow(lines[i]).length === splitRow(lines[i + 1]).length
  );
}

function startsBlock(lines, i) {
  const line = lines[i];
  return (
    FENCE_RE.test(line) ||
    HEADING_RE.test(line) ||
    HR_RE.test(line) ||
    QUOTE_RE.test(line) ||
    LIST_RE.test(line) ||
    isTableStart(lines, i)
  );
}

/** Parses Markdown source into a block tree. */
export function parseBlocks(src) {
  const text = String(src ?? '').replace(/\r\n?/g, '\n');
  return parseLines(text.split('\n'), 0);
}

function parseLines(lines, depth) {
  const blocks = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (isBlank(line)) {
      i += 1;
      continue;
    }

    const fence = FENCE_RE.exec(line);
    if (fence) {
      const marker = fence[1][0];
      const size = fence[1].length;
      const closeRe = new RegExp(`^ {0,3}\\${marker}{${size},}\\s*$`);
      const body = [];
      i += 1;
      while (i < lines.length && !closeRe.test(lines[i])) {
        body.push(lines[i]);
        i += 1;
      }
      i += 1;
      blocks.push({ t: 'code', lang: fence[2] || '', text: body.join('\n') });
      continue;
    }

    const heading = HEADING_RE.exec(line);
    if (heading) {
      blocks.push({ t: 'heading', level: heading[1].length, c: parseInline(heading[2]) });
      i += 1;
      continue;
    }

    if (HR_RE.test(line)) {
      blocks.push({ t: 'hr' });
      i += 1;
      continue;
    }

    if (QUOTE_RE.test(line) && depth < MAX_DEPTH) {
      const inner = [];
      while (i < lines.length && QUOTE_RE.test(lines[i])) {
        inner.push(lines[i].replace(/^ {0,3}> ?/, ''));
        i += 1;
      }
      blocks.push({ t: 'quote', c: parseLines(inner, depth + 1) });
      continue;
    }

    if (LIST_RE.test(line) && depth < MAX_DEPTH) {
      const [list, next] = parseList(lines, i, depth);
      blocks.push(list);
      i = next;
      continue;
    }

    if (isTableStart(lines, i)) {
      const head = splitRow(line).map((cell) => parseInline(cell));
      const align = splitRow(lines[i + 1]).map((cell) => {
        const left = cell.startsWith(':');
        const right = cell.endsWith(':');
        if (left && right) return 'center';
        if (right) return 'right';
        return left ? 'left' : '';
      });
      i += 2;
      const rows = [];
      while (i < lines.length && !isBlank(lines[i]) && lines[i].includes('|')) {
        const cells = splitRow(lines[i]).map((cell) => parseInline(cell));
        while (cells.length < head.length) cells.push([]);
        rows.push(cells.slice(0, head.length));
        i += 1;
      }
      blocks.push({ t: 'table', head, align, rows });
      continue;
    }

    const para = [line.trim()];
    i += 1;
    while (i < lines.length && !isBlank(lines[i]) && !startsBlock(lines, i)) {
      para.push(lines[i].trim());
      i += 1;
    }
    blocks.push({ t: 'p', c: parseInline(para.join('\n')) });
  }
  return blocks;
}

function parseList(lines, start, depth) {
  const first = LIST_RE.exec(lines[start]);
  const baseIndent = indentWidth(first[1]);
  const ordered = /\d/.test(first[2]);
  const list = { t: 'list', ordered, start: ordered ? parseInt(first[2], 10) : 1, items: [] };
  let i = start;

  const isSibling = (line) => {
    const m = LIST_RE.exec(line);
    if (!m) return false;
    const indent = indentWidth(m[1]);
    return indent >= baseIndent && indent <= baseIndent + 1 && /\d/.test(m[2]) === ordered;
  };

  while (i < lines.length && isSibling(lines[i])) {
    const m = LIST_RE.exec(lines[i]);
    const contentIndent = baseIndent + m[2].length + 1;
    const itemLines = [m[3]];
    i += 1;
    while (i < lines.length) {
      const line = lines[i];
      if (isBlank(line)) {
        let j = i + 1;
        while (j < lines.length && isBlank(lines[j])) j += 1;
        if (j < lines.length && indentWidth(lines[j]) >= baseIndent + 2) {
          itemLines.push('');
          i += 1;
          continue;
        }
        break;
      }
      if (indentWidth(line) >= baseIndent + 2) {
        itemLines.push(stripIndent(line, contentIndent));
        i += 1;
        continue;
      }
      if (LIST_RE.test(line) || startsBlock(lines, i)) break;
      itemLines.push(line.trim());
      i += 1;
    }

    let task = null;
    const taskMatch = /^\[([ xX])\]\s+/.exec(itemLines[0]);
    if (taskMatch) {
      task = taskMatch[1] !== ' ';
      itemLines[0] = itemLines[0].slice(taskMatch[0].length);
    }
    list.items.push({ task, c: parseLines(itemLines, depth + 1) });

    let j = i;
    while (j < lines.length && isBlank(lines[j])) j += 1;
    if (j > i) {
      if (j < lines.length && isSibling(lines[j])) i = j;
      else break;
    }
  }
  return [list, i];
}

function findLinkEnd(src, open) {
  // src[open] is "[". Returns the label and destination of an inline link.
  let depth = 0;
  let close = -1;
  for (let i = open; i < src.length; i += 1) {
    const ch = src[i];
    if (ch === '\\') {
      i += 1;
    } else if (ch === '[') {
      depth += 1;
    } else if (ch === ']') {
      depth -= 1;
      if (depth === 0) {
        close = i;
        break;
      }
    } else if (ch === '\n' && src[i + 1] === '\n') {
      return null;
    }
  }
  if (close === -1 || src[close + 1] !== '(') return null;
  let parens = 0;
  let end = -1;
  for (let i = close + 1; i < src.length; i += 1) {
    const ch = src[i];
    if (ch === '\\') {
      i += 1;
    } else if (ch === '(') {
      parens += 1;
    } else if (ch === ')') {
      parens -= 1;
      if (parens === 0) {
        end = i;
        break;
      }
    } else if (ch === '\n') {
      return null;
    }
  }
  if (end === -1) return null;
  let dest = src.slice(close + 2, end).trim();
  const title = /^(\S+)\s+(?:"[^"]*"|'[^']*')$/.exec(dest);
  if (title) dest = title[1];
  if (dest.startsWith('<') && dest.endsWith('>')) dest = dest.slice(1, -1);
  return { label: src.slice(open + 1, close), dest, end: end + 1 };
}

// Returned by findEmphasis when the opener is valid but nothing closes it.
const NO_CLOSER = Symbol('no closer');

function emphasisDelimiter(src, i) {
  const ch = src[i];
  if (ch === '~') return src[i + 1] === '~' ? '~~' : null;
  return src[i + 1] === ch ? ch + ch : ch;
}

function findEmphasis(src, i) {
  const ch = src[i];
  let delim = ch;
  let type = 'em';
  if (ch === '~') {
    if (src[i + 1] !== '~') return null;
    delim = '~~';
    type = 'del';
  } else if (src[i + 1] === ch) {
    delim = ch + ch;
    type = 'strong';
  }
  const start = i + delim.length;
  if (start >= src.length || /\s/.test(src[start])) return null;
  if (ch === '_' && i > 0 && WORD_RE.test(src[i - 1])) return null;

  let j = src.indexOf(delim, start + 1);
  while (j !== -1) {
    const before = src[j - 1];
    const after = src[j + delim.length];
    const closes =
      !/\s/.test(before) &&
      before !== '\\' &&
      (delim.length === 2 || (after !== ch && before !== ch)) &&
      (ch !== '_' || after === undefined || !WORD_RE.test(after));
    if (closes) {
      const inner = src.slice(start, j);
      if (inner.includes('\n\n')) return null;
      return { type, inner, end: j + delim.length };
    }
    j = src.indexOf(delim, j + 1);
  }
  return NO_CLOSER;
}

function trimUrl(url) {
  let out = url;
  for (;;) {
    const last = out[out.length - 1];
    if (/[.,;:!?'"*_~]/.test(last)) {
      out = out.slice(0, -1);
    } else if (last === ')' && (out.match(/\(/g) || []).length < (out.match(/\)/g) || []).length) {
      out = out.slice(0, -1);
    } else {
      return out;
    }
  }
}

/** Parses inline Markdown into a node list. */
export function parseInline(src, depth = 0) {
  const out = [];
  let buf = '';
  const flush = () => {
    if (buf) {
      out.push({ t: 'text', v: buf });
      buf = '';
    }
  };
  const n = src.length;
  // Whether a delimiter can close does not depend on where it was opened, so
  // a delimiter that found no closer once will not find one further right.
  // Remembering that keeps long runs of unmatched openers linear.
  const unclosed = new Set();
  let i = 0;
  while (i < n) {
    const ch = src[i];

    if (ch === '\\' && i + 1 < n && PUNCT_RE.test(src[i + 1])) {
      buf += src[i + 1];
      i += 2;
      continue;
    }

    if (ch === '\n') {
      flush();
      out.push({ t: 'br' });
      i += 1;
      continue;
    }

    if (ch === '`') {
      let run = 1;
      while (src[i + run] === '`') run += 1;
      const fence = '`'.repeat(run);
      let close = src.indexOf(fence, i + run);
      while (close !== -1 && src[close + run] === '`') {
        let k = close;
        while (src[k] === '`') k += 1;
        close = src.indexOf(fence, k);
      }
      if (close !== -1) {
        let code = src.slice(i + run, close).replace(/\n/g, ' ');
        if (code.length > 2 && code.startsWith(' ') && code.endsWith(' ')) code = code.slice(1, -1);
        flush();
        out.push({ t: 'code', v: code });
        i = close + run;
        continue;
      }
      buf += fence;
      i += run;
      continue;
    }

    if (depth < MAX_DEPTH) {
      const isImage = ch === '!' && src[i + 1] === '[';
      if (ch === '[' || isImage) {
        const link = findLinkEnd(src, isImage ? i + 1 : i);
        if (link) {
          const href = safeHref(link.dest);
          flush();
          if (isImage) {
            // Images are not fetched; show the alt text as a link instead.
            const label = [{ t: 'text', v: link.label || 'image' }];
            out.push(href ? { t: 'link', href, c: label } : label[0]);
          } else {
            const label = parseInline(link.label, depth + 1);
            if (href) out.push({ t: 'link', href, c: label });
            else out.push(...label);
          }
          i = link.end;
          continue;
        }
      }

      if (ch === '<') {
        ANGLE_LINK_RE.lastIndex = i;
        const m = ANGLE_LINK_RE.exec(src);
        const href = m ? safeHref(m[1]) : null;
        if (href) {
          flush();
          out.push({ t: 'link', href, c: [{ t: 'text', v: m[1] }] });
          i += m[0].length;
          continue;
        }
      }

      if (ch === 'h' && (i === 0 || !WORD_RE.test(src[i - 1]))) {
        URL_RE.lastIndex = i;
        const m = URL_RE.exec(src);
        if (m) {
          const raw = trimUrl(m[0]);
          const href = safeHref(raw);
          if (href) {
            flush();
            out.push({ t: 'link', href, c: [{ t: 'text', v: raw }] });
            i += raw.length;
            continue;
          }
        }
      }

      if (ch === '*' || ch === '_' || ch === '~') {
        const delim = emphasisDelimiter(src, i);
        if (delim && !unclosed.has(delim)) {
          const em = findEmphasis(src, i);
          if (em === NO_CLOSER) {
            unclosed.add(delim);
          } else if (em) {
            flush();
            out.push({ t: em.type, c: parseInline(em.inner, depth + 1) });
            i = em.end;
            continue;
          }
        }
      }
    }

    buf += ch;
    i += 1;
  }
  flush();
  return out;
}

// Rendering

function el(tag, className) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  return node;
}

function renderInline(nodes, parent) {
  for (const node of nodes) {
    switch (node.t) {
      case 'text':
        parent.appendChild(document.createTextNode(node.v));
        break;
      case 'br':
        parent.appendChild(el('br'));
        break;
      case 'code': {
        const code = el('code');
        code.textContent = node.v;
        parent.appendChild(code);
        break;
      }
      case 'link': {
        const a = el('a');
        a.href = node.href;
        a.target = '_blank';
        a.rel = 'noopener noreferrer nofollow';
        renderInline(node.c, a);
        parent.appendChild(a);
        break;
      }
      case 'strong':
      case 'em':
      case 'del': {
        const wrap = el(node.t);
        renderInline(node.c, wrap);
        parent.appendChild(wrap);
        break;
      }
      default:
        break;
    }
  }
}

function renderCodeBlock(block) {
  const wrap = el('div', 'code-block');
  const head = el('div', 'code-head');
  const lang = el('span');
  lang.textContent = block.lang || 'text';
  const copy = el('button', 'code-copy');
  copy.type = 'button';
  copy.title = 'Copy code';
  const label = el('span');
  label.textContent = 'Copy';
  copy.append(icon('copy', 12), label);
  copy.addEventListener('click', async () => {
    const ok = await copyText(block.text);
    label.textContent = ok ? 'Copied' : 'Copy failed';
    setTimeout(() => {
      label.textContent = 'Copy';
    }, 1500);
  });
  head.append(lang, copy);
  const pre = el('pre');
  const code = el('code');
  code.textContent = block.text;
  pre.appendChild(code);
  wrap.append(head, pre);
  return wrap;
}

function renderBlocks(blocks, parent) {
  for (const block of blocks) {
    switch (block.t) {
      case 'p': {
        const p = el('p');
        renderInline(block.c, p);
        parent.appendChild(p);
        break;
      }
      case 'heading': {
        const hNode = el(`h${block.level}`);
        renderInline(block.c, hNode);
        parent.appendChild(hNode);
        break;
      }
      case 'hr':
        parent.appendChild(el('hr'));
        break;
      case 'code':
        parent.appendChild(renderCodeBlock(block));
        break;
      case 'quote': {
        const quote = el('blockquote');
        renderBlocks(block.c, quote);
        parent.appendChild(quote);
        break;
      }
      case 'list': {
        const list = el(block.ordered ? 'ol' : 'ul');
        if (block.ordered && block.start !== 1) list.start = block.start;
        for (const item of block.items) {
          const li = el('li');
          if (item.task !== null) {
            li.className = 'task';
            const box = el('input');
            box.type = 'checkbox';
            box.checked = item.task;
            box.disabled = true;
            li.appendChild(box);
          }
          const [head, ...rest] = item.c;
          if (head && head.t === 'p') {
            // Tight rendering: the leading paragraph flows inline in the item.
            renderInline(head.c, li);
            renderBlocks(rest, li);
          } else {
            renderBlocks(item.c, li);
          }
          list.appendChild(li);
        }
        parent.appendChild(list);
        break;
      }
      case 'table': {
        const wrap = el('div', 'table-wrap');
        const table = el('table');
        const thead = el('thead');
        const headRow = el('tr');
        block.head.forEach((cell, index) => {
          const th = el('th');
          if (block.align[index]) th.style.textAlign = block.align[index];
          renderInline(cell, th);
          headRow.appendChild(th);
        });
        thead.appendChild(headRow);
        const tbody = el('tbody');
        for (const row of block.rows) {
          const tr = el('tr');
          row.forEach((cell, index) => {
            const td = el('td');
            if (block.align[index]) td.style.textAlign = block.align[index];
            renderInline(cell, td);
            tr.appendChild(td);
          });
          tbody.appendChild(tr);
        }
        table.append(thead, tbody);
        wrap.appendChild(table);
        parent.appendChild(wrap);
        break;
      }
      default:
        break;
    }
  }
}

/** Renders Markdown source into a <div class="md"> element. */
export function renderMarkdown(src) {
  const root = el('div', 'md');
  const text = String(src ?? '');
  if (text.length > MAX_INPUT) {
    // Extremely large input is shown verbatim rather than parsed.
    const pre = el('pre');
    pre.textContent = text;
    root.appendChild(pre);
    return root;
  }
  renderBlocks(parseBlocks(text), root);
  return root;
}
