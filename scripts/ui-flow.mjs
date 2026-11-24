// Optional end-to-end UI flow test. It CREATES AND DELETES cards and columns,
// so point it at a scratch instance (separate --db), never at the real board:
//   node scripts/ui-flow.mjs http://127.0.0.1:3913 [output-dir]
// It does not start, message, resume or terminate any AO session.

import { launch } from './cdp.mjs';
import { mkdirSync } from 'node:fs';
import { resolve } from 'node:path';

const base = process.argv[2];
if (!base) {
  console.error('usage: node scripts/ui-flow.mjs <base-url-of-a-scratch-instance> [output-dir]');
  process.exit(2);
}
const out = resolve(process.argv[3] || 'data/ui-flow');
mkdirSync(out, { recursive: true });

const page = await launch({ userDataDir: resolve(out, 'chromium-profile') });
let failed = false;
const step = async (name, fn) => {
  try {
    await fn();
    console.log(`ok    ${name}`);
  } catch (err) {
    failed = true;
    console.log(`FAIL  ${name}: ${err.message}`);
    await page.screenshot(`${out}/fail-${name.replace(/[^a-z0-9]+/gi, '-')}.png`).catch(() => {});
  }
};
const q = (s) => JSON.stringify(s);
const fill = (selector, text) =>
  page.eval(`(() => { const el = document.querySelector(${q(selector)}); if (!el) throw new Error('no element ' + ${q(selector)}); el.focus(); el.value = ${q(text)}; el.dispatchEvent(new Event('input', { bubbles: true })); return true; })()`);
const key = (k, modifiers = 0) => page.cmd('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code: k, modifiers, windowsVirtualKeyCode: k === 'Enter' ? 13 : k === 'Escape' ? 27 : 0 });
const clickText = (selector, text) =>
  page.eval(`(() => { const el = Array.from(document.querySelectorAll(${q(selector)})).find((e) => e.textContent.trim().includes(${q(text)})); if (!el) throw new Error('no ' + ${q(selector)} + ' with text ' + ${q(text)}); el.click(); return true; })()`);
const apiGet = (path) => page.eval(`fetch(${q(path)}).then((r) => r.json())`);
const cardByTitle = async (title) => (await apiGet('/api/cards')).cards.find((c) => c.title === title);
const center = (selector) =>
  page.eval(`(() => { const el = document.querySelector(${q(selector)}); if (!el) throw new Error('no element ' + ${q(selector)}); const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);

const TITLE = 'UI flow test card';

try {
  await page.goto(base);
  await page.waitFor(`document.querySelectorAll('.column').length > 0`);

  await step('create a task from the dialog', async () => {
    await page.click('#btn-new');
    await page.waitFor(`document.querySelector('.modal .form-grid')`);
    await fill('.modal input[placeholder="What needs to be done"]', TITLE);
    await fill('.modal input[placeholder="scheduler, sig-scheduling"]', 'alpha, beta');
    await clickText('.modal-foot button', 'Create Task');
    await page.waitFor(`document.querySelector('.drawer .title-input') && document.querySelector('.drawer .title-input').value === ${q(TITLE)}`);
    const card = await cardByTitle(TITLE);
    if (!card || card.tags.join(',') !== 'alpha,beta' || card.category !== 'DEV') throw new Error(`unexpected card ${JSON.stringify(card)}`);
  });

  await step('add a tag in the drawer', async () => {
    await fill('.drawer .tag-input', 'gamma');
    await page.eval(`document.querySelector('.drawer .tag-input').focus()`);
    await key('Enter');
    await page.waitFor(`document.querySelectorAll('.drawer .tag-row .tag').length === 3`);
  });

  await step('edit notes with Markdown', async () => {
    await page.eval(`Array.from(document.querySelectorAll('.drawer .section')).find((s) => s.textContent.includes('Maintainer Notes')).querySelector('button').click()`);
    await page.waitFor(`document.querySelector('.drawer .section textarea.textarea')`);
    await fill('.drawer .section textarea.textarea', '**bold** note\n\n- item one\n- item two\n\n```go\nfmt.Println("hi")\n```');
    await clickText('.drawer .edit-actions button', 'Save');
    await page.waitFor(`document.querySelector('.drawer .notes-view strong') && document.querySelectorAll('.drawer .notes-view li').length === 2 && document.querySelector('.drawer .notes-view .code-copy')`);
  });

  await step('edit the summary manually', async () => {
    await page.eval(`Array.from(document.querySelectorAll('.drawer .section')).find((s) => s.querySelector('.section-title') && s.querySelector('.section-title').textContent === 'Summary').querySelectorAll('button')[1].click()`);
    await page.waitFor(`document.querySelector('.drawer .section textarea.textarea')`);
    await fill('.drawer .section textarea.textarea', 'Manual summary for the flow test.');
    await clickText('.drawer .edit-actions button', 'Save');
    await page.waitFor(`document.querySelector('.drawer .badge-source-manual')`);
  });

  await step('move with the column dropdown', async () => {
    await page.eval(`(() => { const sel = document.querySelector('.drawer select[aria-label="Column"]'); sel.value = 'in_progress'; sel.dispatchEvent(new Event('change', { bubbles: true })); })()`);
    await page.waitFor(`Array.from(document.querySelectorAll('.column[data-column-id="in_progress"] .card-title')).some((t) => t.textContent === ${q(TITLE)})`);
  });
  await page.screenshot(`${out}/01-manual-card-drawer.png`);

  await step('close the drawer', async () => {
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer')`);
  });

  await step('drag the card to another column', async () => {
    const card = await cardByTitle(TITLE);
    const from = await center(`.card[data-id="${card.id}"]`);
    const to = await center('.column[data-column-id="waiting"] .column-body');
    await page.cmd('Input.setInterceptDrags', { enabled: true });
    let dragData = null;
    const waitDrag = new Promise((resolveDrag) => {
      const poll = setInterval(() => {
        if (dragData) {
          clearInterval(poll);
          resolveDrag();
        }
      }, 50);
      setTimeout(() => {
        clearInterval(poll);
        resolveDrag();
      }, 4000);
    });
    page.onEvent((msg) => {
      if (msg.method === 'Input.dragIntercepted') dragData = msg.params.data;
    });
    await page.mouse('mousePressed', from.x, from.y);
    await page.mouse('mouseMoved', from.x + 12, from.y + 6, { buttons: 1 });
    await page.mouse('mouseMoved', from.x + 40, from.y + 10, { buttons: 1 });
    await waitDrag;
    if (!dragData) throw new Error('the browser did not start a drag');
    await page.cmd('Input.dispatchDragEvent', { type: 'dragEnter', x: to.x, y: to.y, data: dragData });
    await page.cmd('Input.dispatchDragEvent', { type: 'dragOver', x: to.x, y: to.y, data: dragData });
    await page.sleep(150);
    const highlighted = await page.eval(`document.querySelector('.column[data-column-id="waiting"]').classList.contains('is-drop-target') && Boolean(document.querySelector('.column[data-column-id="waiting"] .drop-indicator'))`);
    await page.screenshot(`${out}/02-drag-over.png`);
    await page.cmd('Input.dispatchDragEvent', { type: 'drop', x: to.x, y: to.y, data: dragData });
    await page.mouse('mouseReleased', to.x, to.y);
    await page.cmd('Input.setInterceptDrags', { enabled: false });
    if (!highlighted) throw new Error('drop target was not highlighted while dragging');
    await page.waitFor(`Array.from(document.querySelectorAll('.column[data-column-id="waiting"] .card-title')).some((t) => t.textContent === ${q(TITLE)})`);
    const after = await cardByTitle(TITLE);
    if (after.columnId !== 'waiting') throw new Error(`server has column ${after.columnId}`);
  });

  await step('search filters the board', async () => {
    await fill('#search', 'flow test gamma');
    await page.waitFor(`document.querySelectorAll('.card').length === 1 && document.querySelector('#card-count').textContent.startsWith('1 of')`);
    await fill('#search', '');
    await page.waitFor(`document.querySelectorAll('.card').length > 1 || !document.querySelector('#card-count').textContent.includes(' of ')`);
  });

  const unread = (await apiGet('/api/cards')).cards.find((c) => c.hasUnreadUpdates);
  if (unread) {
    await step('Updates Only shows unread cards', async () => {
      await page.click('#toggle-updates');
      await page.waitFor(`document.querySelectorAll('.card').length > 0 && Array.from(document.querySelectorAll('.card')).every((c) => c.classList.contains('has-updates'))`);
      const title = await page.eval('document.title');
      if (!/^\(\d+\) Maintainer Board$/.test(title)) throw new Error(`tab title is ${title}`);
    });
    await step('unread banner and Mark Caught Up', async () => {
      await page.click(`.card[data-id="${unread.id}"]`);
      await page.waitFor(`document.querySelector('.drawer .alert-warning')`);
      await page.sleep(1200);
      await page.screenshot(`${out}/03-unread-drawer.png`);
      await clickText('.drawer .alert-warning button', 'Mark Caught Up');
      await page.waitFor(`!document.querySelector('.drawer .alert-warning')`);
      const card = (await apiGet(`/api/cards/${unread.id}`));
      if (card.hasUnreadUpdates || card.updateCycles !== 0 || card.unreadUpdates !== 0) throw new Error('ack did not reset the unread state');
      await page.click('.drawer-top button[aria-label="Close"]');
      await page.click('#toggle-updates');
    });
  } else {
    console.log('skip  no card with unread updates on this instance');
  }

  await step('add, rename and delete a column', async () => {
    await page.click('#btn-columns');
    await page.waitFor(`document.querySelector('.col-row.is-new')`);
    await fill('.col-row.is-new input[type="text"]', 'QA Flow');
    await clickText('.col-row.is-new button', 'Add Column');
    await page.waitFor(`Array.from(document.querySelectorAll('.column-title')).some((t) => t.textContent === 'QA Flow')`);
    await page.waitFor(`Array.from(document.querySelectorAll('.col-list input[type="text"]')).some((i) => i.value === 'QA Flow')`);
    await page.eval(`(() => { const input = Array.from(document.querySelectorAll('.col-list input[type="text"]')).find((i) => i.value === 'QA Flow'); input.focus(); input.value = 'QA Renamed'; input.blur(); })()`);
    await page.waitFor(`Array.from(document.querySelectorAll('.column-title')).some((t) => t.textContent === 'QA Renamed')`);
    await page.sleep(300);
    await page.screenshot(`${out}/04-columns.png`);
    await page.eval(`document.querySelector('.col-list button[aria-label="Delete QA Renamed"]').click()`);
    await page.waitFor(`document.querySelector('.modal-narrow')`);
    await clickText('.modal-narrow .modal-foot button', 'Delete Column');
    await page.waitFor(`!Array.from(document.querySelectorAll('.column-title')).some((t) => t.textContent === 'QA Renamed')`);
    await page.click('.modal-head button[aria-label="Close"]');
  });

  await step('hostile content is rendered as text', async () => {
    const payload = '<img src=x onerror="window.__xss=1"><script>window.__xss=2</script>';
    await page.eval(`fetch('/api/cards', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: ${q(payload)}, summary: ${q(payload)}, tags: [${q(payload.slice(0, 30))}], notes: ${q(`${payload}\n\n[click](javascript:window.__xss=3) [ok](https://example.com/a)\n\n![img](javascript:window.__xss=4)`)} }) }).then((r) => r.json())`);
    await page.waitFor(`Array.from(document.querySelectorAll('.card-title')).some((t) => t.textContent.includes('onerror'))`);
    await page.eval(`Array.from(document.querySelectorAll('.card')).find((c) => c.textContent.includes('onerror')).click()`);
    await page.waitFor(`document.querySelector('.drawer .notes-view .md')`);
    await page.sleep(300);
    const result = await page.eval(`({ xss: window.__xss === undefined ? null : window.__xss, images: document.querySelectorAll('.drawer img, .board img').length, scripts: document.querySelectorAll('.drawer script, .board script').length, hrefs: Array.from(document.querySelectorAll('.drawer .notes-view a')).map((a) => a.getAttribute('href')) })`);
    if (result.xss !== null || result.images !== 0 || result.scripts !== 0) throw new Error(`markup was interpreted: ${JSON.stringify(result)}`);
    if (result.hrefs.length !== 1 || result.hrefs[0] !== 'https://example.com/a') throw new Error(`unexpected links: ${JSON.stringify(result.hrefs)}`);
    await page.screenshot(`${out}/05-hostile-content.png`);
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal-narrow')`);
    await clickText('.modal-narrow .modal-foot button', 'Delete Card');
    await page.waitFor(`!document.querySelector('.drawer')`);
  });

  await step('delete the test card', async () => {
    const card = await cardByTitle(TITLE);
    await page.click(`.card[data-id="${card.id}"]`);
    await page.waitFor(`document.querySelector('.drawer')`);
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal-narrow')`);
    await clickText('.modal-narrow .modal-foot button', 'Delete Card');
    await page.waitFor(`!document.querySelector('.drawer')`);
    if (await cardByTitle(TITLE)) throw new Error('card still exists');
  });
} finally {
  await page.close();
}

if (page.errors.length) {
  failed = true;
  console.log('Browser console problems:');
  for (const e of page.errors) console.log(`  ${e}`);
} else {
  console.log('ok    no console errors or exceptions');
}
console.log(`Screenshots: ${out}`);
process.exit(failed ? 1 : 0);
