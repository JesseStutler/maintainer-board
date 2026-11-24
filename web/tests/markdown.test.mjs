// Parser tests for the Markdown renderer. Run with: node --test web/tests/
// The renderer only ever builds DOM nodes from this tree with textContent,
// so these tests pin down the two places where input could become active
// content: link targets and node types.

import test from 'node:test';
import assert from 'node:assert/strict';
import { parseBlocks, parseInline, safeHref } from '../markdown.js';

const text = (v) => ({ t: 'text', v });

function collect(nodes, type, out = []) {
  for (const node of nodes || []) {
    if (node.t === type) out.push(node);
    if (Array.isArray(node.c)) collect(node.c, type, out);
    if (node.items) for (const item of node.items) collect(item.c, type, out);
    if (node.head) for (const cell of node.head) collect(cell, type, out);
    if (node.rows) for (const row of node.rows) for (const cell of row) collect(cell, type, out);
  }
  return out;
}

test('safeHref allows only http, https and mailto', () => {
  assert.equal(safeHref('https://github.com/volcano-sh/volcano/pull/6247'), 'https://github.com/volcano-sh/volcano/pull/6247');
  assert.equal(safeHref('http://127.0.0.1:3001/x'), 'http://127.0.0.1:3001/x');
  assert.equal(safeHref('mailto:dev@example.com'), 'mailto:dev@example.com');
  for (const bad of [
    'javascript:alert(1)',
    'JaVaScRiPt:alert(1)',
    ' javascript:alert(1)',
    'java\nscript:alert(1)',
    'java\tscript:alert(1)',
    '\u0001javascript:alert(1)',
    'data:text/html,<script>alert(1)</script>',
    'vbscript:msgbox(1)',
    'file:///etc/passwd',
    'ao://sessions/volcano/volcano-2',
    '//evil.example/x',
    '/relative/path',
    'relative',
    '',
    null,
    undefined,
    42,
  ]) {
    assert.equal(safeHref(bad), null, `accepted ${JSON.stringify(bad)}`);
  }
});

test('unsafe link targets never become link nodes', () => {
  const src = [
    '[a](javascript:alert(1))',
    '![b](javascript:alert(2))',
    '<javascript:alert(3)>',
    '[c](data:text/html;base64,PHNjcmlwdD4=)',
    '[d]( java\tscript:alert(4) )',
    '[ok](https://example.com/x "title")',
  ].join(' ');
  const links = collect(parseBlocks(src), 'link');
  assert.deepEqual(links.map((l) => l.href), ['https://example.com/x']);
});

test('raw HTML stays plain text', () => {
  const blocks = parseBlocks('<img src=x onerror="alert(1)"> and <script>alert(2)</script>');
  assert.deepEqual(blocks, [{ t: 'p', c: [text('<img src=x onerror="alert(1)"> and <script>alert(2)</script>')] }]);
  const types = new Set();
  (function walk(nodes) {
    for (const n of nodes) {
      types.add(n.t);
      if (Array.isArray(n.c)) walk(n.c);
    }
  })(blocks);
  assert.deepEqual([...types].sort(), ['p', 'text']);
});

test('inline formatting', () => {
  assert.deepEqual(parseInline('a **bold** and *em* and ~~gone~~ `code`'), [
    text('a '),
    { t: 'strong', c: [text('bold')] },
    text(' and '),
    { t: 'em', c: [text('em')] },
    text(' and '),
    { t: 'del', c: [text('gone')] },
    text(' '),
    { t: 'code', v: 'code' },
  ]);
  assert.deepEqual(parseInline('*a **b** c*'), [{ t: 'em', c: [text('a '), { t: 'strong', c: [text('b')] }, text(' c')] }]);
  assert.deepEqual(parseInline('``a ` b``'), [{ t: 'code', v: 'a ` b' }]);
  assert.deepEqual(parseInline('line one\nline two'), [text('line one'), { t: 'br' }, text('line two')]);
});

test('identifiers and arithmetic are not emphasis', () => {
  for (const plain of ['node_volume_limits and pod_group_info', '2 * 3 * 4 = 24', 'a * b', 'unclosed **bold', 'unclosed `code', '\\*escaped\\* \\_text\\_']) {
    const nodes = parseInline(plain);
    assert.ok(nodes.every((n) => n.t === 'text'), `${plain} produced ${JSON.stringify(nodes)}`);
  }
  assert.deepEqual(parseInline('\\*escaped\\*'), [text('*escaped*')]);
  // An asterisk that cannot open emphasis does not disable later emphasis.
  assert.deepEqual(parseInline('2 * 3 and *em* here'), [text('2 * 3 and '), { t: 'em', c: [text('em')] }, text(' here')]);
  assert.deepEqual(parseInline('a_b and _em_'), [text('a_b and '), { t: 'em', c: [text('em')] }]);
  // An opener nothing closes stays literal, and so do the ones after it.
  assert.deepEqual(parseInline('**a **b'), [text('**a **b')]);
  assert.deepEqual(parseInline('`**not bold**`'), [{ t: 'code', v: '**not bold**' }]);
});

test('links and bare URLs', () => {
  assert.deepEqual(parseInline('see [the **PR**](https://github.com/a/b/pull/1).'), [
    text('see '),
    { t: 'link', href: 'https://github.com/a/b/pull/1', c: [text('the '), { t: 'strong', c: [text('PR')] }] },
    text('.'),
  ]);
  assert.deepEqual(parseInline('at https://example.com/a_b(c)/d, then'), [
    text('at '),
    { t: 'link', href: 'https://example.com/a_b(c)/d', c: [text('https://example.com/a_b(c)/d')] },
    text(', then'),
  ]);
  assert.deepEqual(parseInline('(https://example.com/x).'), [
    text('('),
    { t: 'link', href: 'https://example.com/x', c: [text('https://example.com/x')] },
    text(').'),
  ]);
  // CJK text right after a URL is not part of the link.
  const cjk = parseInline('\u89c1 https://github.com/a/b/pull/1\uff0c\u7136\u540e\u5408\u5e76');
  assert.equal(cjk[1].href, 'https://github.com/a/b/pull/1');
  assert.deepEqual(cjk[2], text('\uff0c\u7136\u540e\u5408\u5e76'));
  assert.deepEqual(parseInline('<https://example.com/y>'), [{ t: 'link', href: 'https://example.com/y', c: [text('https://example.com/y')] }]);
  assert.deepEqual(parseInline('xhttps://example.com'), [text('xhttps://example.com')]);
  assert.deepEqual(parseInline('![diagram](https://example.com/d.png)'), [{ t: 'link', href: 'https://example.com/d.png', c: [text('diagram')] }]);
  assert.deepEqual(parseInline('[label] without target'), [text('[label] without target')]);
});

test('headings, rules, quotes and paragraphs', () => {
  assert.deepEqual(parseBlocks('# Title\n\n### Sub ###\n\npara one\nstill one\n\n---\n\n> quoted\n> more'), [
    { t: 'heading', level: 1, c: [text('Title')] },
    { t: 'heading', level: 3, c: [text('Sub')] },
    { t: 'p', c: [text('para one'), { t: 'br' }, text('still one')] },
    { t: 'hr' },
    { t: 'quote', c: [{ t: 'p', c: [text('quoted'), { t: 'br' }, text('more')] }] },
  ]);
  assert.deepEqual(parseBlocks('#hashtag is not a heading'), [{ t: 'p', c: [text('#hashtag is not a heading')] }]);
});

test('fenced code keeps its content verbatim', () => {
  const src = 'before\n```go\nfunc main() {\n\tfmt.Println("<b>**hi**</b>")\n}\n```\nafter';
  assert.deepEqual(parseBlocks(src), [
    { t: 'p', c: [text('before')] },
    { t: 'code', lang: 'go', text: 'func main() {\n\tfmt.Println("<b>**hi**</b>")\n}' },
    { t: 'p', c: [text('after')] },
  ]);
  assert.deepEqual(parseBlocks('~~~\na\n```\nb\n~~~'), [{ t: 'code', lang: '', text: 'a\n```\nb' }]);
  assert.deepEqual(parseBlocks('```sh\nunterminated'), [{ t: 'code', lang: 'sh', text: 'unterminated' }]);
});

test('lists: ordered, nested and task items', () => {
  const blocks = parseBlocks('- one\n- two\n  - nested a\n  - nested b\n- three\n\n3. third\n4. fourth');
  assert.equal(blocks.length, 2);
  const [bullets, numbers] = blocks;
  assert.equal(bullets.ordered, false);
  assert.equal(bullets.items.length, 3);
  assert.deepEqual(bullets.items[0].c, [{ t: 'p', c: [text('one')] }]);
  const nested = bullets.items[1].c[1];
  assert.equal(nested.t, 'list');
  assert.deepEqual(nested.items.map((i) => i.c[0].c[0].v), ['nested a', 'nested b']);
  assert.equal(numbers.ordered, true);
  assert.equal(numbers.start, 3);
  assert.equal(numbers.items.length, 2);

  const tasks = parseBlocks('- [x] done\n- [ ] open\n- plain')[0];
  assert.deepEqual(tasks.items.map((i) => i.task), [true, false, null]);
  assert.deepEqual(tasks.items[0].c, [{ t: 'p', c: [text('done')] }]);

  // A paragraph ends where a list begins, as chat messages often omit the blank line.
  const mixed = parseBlocks('Summary:\n- first\n- second');
  assert.deepEqual(mixed.map((b) => b.t), ['p', 'list']);
  // Emphasis at the start of a line is not a bullet; "* * *" is a rule.
  assert.deepEqual(parseBlocks('**bold** start').map((b) => b.t), ['p']);
  assert.deepEqual(parseBlocks('* * *').map((b) => b.t), ['hr']);
});

test('tables', () => {
  const [table] = parseBlocks('| Case | Result | Note |\n|:-----|:------:|-----:|\n| preempt | **ok** | a \\| b |\n| short |');
  assert.equal(table.t, 'table');
  assert.deepEqual(table.align, ['left', 'center', 'right']);
  assert.deepEqual(table.head, [[text('Case')], [text('Result')], [text('Note')]]);
  assert.deepEqual(table.rows[0], [[text('preempt')], [{ t: 'strong', c: [text('ok')] }], [text('a | b')]]);
  assert.deepEqual(table.rows[1], [[text('short')], [], []]);
  // A pipe in prose is not a table.
  assert.deepEqual(parseBlocks('a | b\nc | d').map((b) => b.t), ['p']);
});

test('pathological input terminates quickly', () => {
  const started = Date.now();
  parseBlocks(`${'> '.repeat(500)}deep`);
  parseBlocks(`${'  '.repeat(300)}- deep`);
  parseInline(`${'['.repeat(3000)}x${']'.repeat(3000)}(https://example.com)`);
  parseInline('*'.repeat(20000));
  parseInline('`'.repeat(20001));
  parseInline(`${'**a '.repeat(5000)}`);
  parseBlocks(`${'|a'.repeat(2000)}\n${'|-'.repeat(2000)}\n`);
  assert.ok(Date.now() - started < 5000, `took ${Date.now() - started} ms`);
});
