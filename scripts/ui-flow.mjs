// Optional end-to-end UI flow test. It CREATES AND DELETES cards and columns,
// so point it at a scratch instance (separate --db), never at the real board:
//   node scripts/ui-flow.mjs http://127.0.0.1:3913 [output-dir]
// It does not start, message, resume or terminate any AO session.
// Two parts are optional:
//   MB_FLOW_ISSUE=#6394   an issue of the default repository that someone
//                         else opened, for the Mark Reviewed steps (needs gh);
//   MB_AI=1               also run one real AI Search through the local CLI.

import { launch } from './cdp.mjs';
import { mkdirSync, rmSync } from 'node:fs';
import { resolve } from 'node:path';

const base = process.argv[2];
if (!base) {
  console.error('usage: node scripts/ui-flow.mjs <base-url-of-a-scratch-instance> [output-dir]');
  process.exit(2);
}
const out = resolve(process.argv[3] || 'data/ui-flow');
mkdirSync(out, { recursive: true });
rmSync(resolve(out, 'chromium-profile'), { recursive: true, force: true });
const ISSUE = process.env.MB_FLOW_ISSUE || '#6394';
const WITH_AI = process.env.MB_AI === '1';

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
const apiSend = (method, path, body) =>
  page.eval(`fetch(${q(path)}, { method: ${q(method)}, headers: { 'Content-Type': 'application/json' }, body: ${q(JSON.stringify(body || {}))} }).then(async (r) => ({ status: r.status, body: await r.json() }))`);
/** Clicks the button with this exact label inside the drawer section titled `title`. */
const clickInSection = (title, label) =>
  page.eval(`(() => {
    const section = Array.from(document.querySelectorAll('.drawer .section')).find((s) => s.querySelector('.section-title') && s.querySelector('.section-title').textContent === ${q(title)});
    if (!section) throw new Error('no section ' + ${q(title)});
    const button = Array.from(section.querySelectorAll('button')).find((b) => b.textContent.trim() === ${q(label)});
    if (!button) throw new Error('no button ' + ${q(label)} + ' in ' + ${q(title)});
    button.click();
    return true;
  })()`);
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
    await clickInSection('Maintainer Notes', 'Edit');
    await page.waitFor(`document.querySelector('.drawer .section textarea.textarea')`);
    await fill('.drawer .section textarea.textarea', '**bold** note\n\n- item one\n- item two\n\n```go\nfmt.Println("hi")\n```');
    await clickText('.drawer .edit-actions button', 'Save');
    await page.waitFor(`document.querySelector('.drawer .notes-view strong') && document.querySelectorAll('.drawer .notes-view li').length === 2 && document.querySelector('.drawer .notes-view .code-copy')`);
  });

  await step('edit the summary manually', async () => {
    await clickInSection('Summary', 'Edit');
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

  await step('sections fold to one line and remember it', async () => {
    const toggle = `Array.from(document.querySelectorAll('.drawer .section')).find((s) => s.querySelector('.section-title') && s.querySelector('.section-title').textContent === 'Maintainer Notes').querySelector('.section-toggle')`;
    await page.eval(`${toggle}.click()`);
    await page.waitFor(`${toggle}.getAttribute('aria-expanded') === 'false' && !document.querySelector('.drawer .notes-view')`);
    const peek = await page.eval(`${toggle}.closest('.section-head').querySelector('.section-peek').textContent`);
    if (!peek.includes('bold note')) throw new Error(`folded notes preview reads ${peek}`);
    const stored = await page.eval(`JSON.parse(localStorage.getItem('mb.sectionsCollapsed'))`);
    if (stored['full.notes'] !== true) throw new Error(`stored state is ${JSON.stringify(stored)}`);
    await page.eval(`${toggle}.click()`);
    await page.waitFor(`document.querySelector('.drawer .notes-view strong')`);
  });

  await step('expanded workspace for a card without a conversation', async () => {
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await page.sleep(250);
    const info = await page.eval(`(() => {
      const d = document.querySelector('.drawer').getBoundingClientRect();
      const title = document.querySelector('.drawer .title-input').getBoundingClientRect();
      return { width: d.width, viewport: window.innerWidth, title: title.width, titleCenter: title.left + title.width / 2,
        controls: Boolean(document.querySelector('.drawer-details .section-controls:not([hidden]) select[aria-label="Column"]')),
        labels: Array.from(document.querySelectorAll('.controls-panel .control-label')).map((e) => e.textContent) };
    })()`);
    if (info.width !== info.viewport) throw new Error(`workspace is ${info.width}px of ${info.viewport}px`);
    if (info.title > 920 || Math.abs(info.titleCenter - info.viewport / 2) > 12) throw new Error(`details are not a centered reading column: ${JSON.stringify(info)}`);
    if (!info.controls || info.labels.join(',') !== 'Category,Column') throw new Error(`controls section: ${JSON.stringify(info)}`);
    await page.screenshot(`${out}/06-workspace-no-chat.png`);
    // The controls keep working from the rail.
    await page.eval(`(() => { const sel = document.querySelector('.controls-panel select[aria-label="Category"]'); sel.value = 'DESIGN'; sel.dispatchEvent(new Event('change', { bubbles: true })); })()`);
    await page.waitFor(`Array.from(document.querySelectorAll('.card')).some((c) => c.querySelector('.card-title').textContent === ${q(TITLE)} && c.querySelector('.badge-cat-DESIGN'))`, 15000, 'category change from the rail');
    await page.eval(`document.activeElement && document.activeElement.blur()`);
    await key('Escape');
    await page.waitFor(`document.querySelector('.drawer') && !document.querySelector('.drawer.is-expanded')`);
    if (!(await page.eval(`Boolean(document.querySelector('.drawer-top select[aria-label="Column"]'))`))) throw new Error('controls did not return to the header');
  });

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

  await step('search by AO session ID through the API and the search box', async () => {
    const session = (await apiGet('/api/cards')).cards.find((c) => c.aoSessionId);
    if (!session) {
      console.log('skip  no card with an AO session on this instance');
      return;
    }
    const res = await apiGet(`/api/search?q=${encodeURIComponent(session.aoSessionId)}`);
    if (!res.matches.some((m) => m.cardId === session.id)) throw new Error(`GET /api/search does not find ${session.aoSessionId}`);
    await fill('#search', session.aoSessionId);
    await page.waitFor(`document.querySelector('.card[data-id=${q(session.id)}]')`);
    await page.click('#btn-search-clear');
    await page.waitFor(`document.getElementById('search').value === ''`);
  });

  const created = await apiSend('POST', '/api/cards', { githubRef: ISSUE, columnId: 'inbox' });
  const issue = created.status === 201 ? created.body.card : null;
  if (issue && issue.ghKind === 'issue' && issue.ghAuthor && issue.ghAuthor.toLowerCase() !== ((await apiGet('/api/board')).currentUser || '').toLowerCase()) {
    await step('author shows on the card and in the author filter', async () => {
      await page.waitFor(`document.querySelector('.card[data-id=${q(issue.id)}] .card-author .login')`);
      const face = await page.eval(`(() => { const el = document.querySelector('.card[data-id=${q(issue.id)}]'); return { author: el.querySelector('.card-author .login').textContent, ref: el.querySelector('.gh-ref').textContent, state: el.querySelector('.gh-state').textContent, mine: Boolean(el.querySelector('.badge-mine')) }; })()`);
      if (face.author !== `@${issue.ghAuthor}` || !face.ref.startsWith('Issue #') || face.state !== issue.ghState || face.mine) throw new Error(`card face: ${JSON.stringify(face)}`);
      await page.waitFor(`Array.from(document.querySelectorAll('#filter-author option')).some((o) => o.value === ${q(issue.ghAuthor)})`);
      await page.eval(`(() => { const s = document.getElementById('filter-author'); s.value = ${q(issue.ghAuthor)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
      await page.waitFor(`document.querySelectorAll('.card').length > 0 && Array.from(document.querySelectorAll('.card')).every((c) => c.querySelector('.card-author .login') && c.querySelector('.card-author .login').textContent === ${q(`@${issue.ghAuthor}`)})`);
      // The author filter combines with the search box.
      await fill('#search', 'no-such-text-anywhere');
      await page.waitFor(`document.querySelectorAll('.card').length === 0`);
      await fill('#search', '');
      await page.waitFor(`document.querySelectorAll('.card').length > 0`);
      const me = (await apiGet('/api/board')).currentUser;
      if (me) {
        await page.eval(`(() => { const s = document.getElementById('filter-author'); s.value = '@me'; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
        await page.waitFor(`!document.querySelector('.card[data-id=${q(issue.id)}]')`);
      }
      await page.eval(`(() => { const s = document.getElementById('filter-author'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
      await page.waitFor(`document.querySelector('.card[data-id=${q(issue.id)}]')`);
    });

    await step('Mark Reviewed records a review by hand, Clear removes it', async () => {
      await page.click(`.card[data-id="${issue.id}"]`);
      await page.waitFor(`document.querySelector('.drawer .gh-review')`);
      if (await page.eval(`Boolean(document.querySelector('.card[data-id=${q(issue.id)}] .chip[data-review]'))`)) throw new Error('an issue nobody reviewed already has a review chip');
      await clickInSection('GitHub Tracking', 'Mark Reviewed');
      await page.waitFor(`document.querySelector('.card[data-id=${q(issue.id)}] .chip[data-review="current"]')`);
      const chip = await page.eval(`document.querySelector('.card[data-id=${q(issue.id)}] .chip[data-review]').textContent`);
      const card = await apiGet(`/api/cards/${issue.id}`);
      if (chip !== 'You: Reviewed' || card.ghMyReviewState !== 'REVIEWED' || !card.ghMyReviewedAt || card.ghIsStaleReview) throw new Error(`chip ${chip}, card ${card.ghMyReviewState} ${card.ghMyReviewedAt}`);
      await page.waitFor(`document.querySelector('.drawer .gh-review .pill-you')`);
      await page.sleep(300);
      await page.screenshot(`${out}/07-marked-reviewed.png`);
      await clickInSection('GitHub Tracking', 'Clear');
      await page.waitFor(`!document.querySelector('.card[data-id=${q(issue.id)}] .chip[data-review]')`);
      if ((await apiGet(`/api/cards/${issue.id}`)).ghMyReviewState !== '') throw new Error('the review mark was not cleared');
      await page.click('.drawer-top button[aria-label="Close"]');
      await page.waitFor(`!document.querySelector('.drawer')`);
    });
  } else {
    console.log(`skip  author and Mark Reviewed steps: ${ISSUE} is not an issue by someone else, or GitHub is unreachable (${created.status})`);
  }
  if (issue) {
    await apiSend('DELETE', `/api/cards/${issue.id}`);
    await page.waitFor(`!document.querySelector('.card[data-id=${q(issue.id)}]')`);
  }

  if (WITH_AI) {
    await step('AI Search filters the board and explains each match', async () => {
      const all = await page.eval(`document.querySelectorAll('.card').length`);
      await fill('#search', 'the card created by the UI flow test');
      await page.sleep(200);
      await page.click('#btn-ai-search');
      await page.waitFor(`document.querySelector('.search-banner.is-running')`, 5000, 'running banner');
      if (!(await page.eval(`document.getElementById('btn-ai-search').disabled && Boolean(document.querySelector('#btn-ai-search .spin'))`))) throw new Error('no spinner on the AI Search button while it runs');
      await page.waitFor(`document.querySelector('.search-banner.is-done, .search-banner.is-error')`, 240000, 'AI search to finish');
      const banner = await page.eval(`({ cls: document.getElementById('search-banner').className, text: document.querySelector('#search-banner .banner-text').textContent })`);
      if (!banner.cls.includes('is-done')) throw new Error(banner.text);
      if (!/^AI Search: \d+ match(es)? for "the card created by the UI flow test"$/.test(banner.text)) throw new Error(`banner reads ${banner.text}`);
      const shown = await page.eval(`Array.from(document.querySelectorAll('.card')).map((c) => ({ title: c.querySelector('.card-title').textContent, match: c.querySelector('.card-match.is-ai') ? c.querySelector('.card-match.is-ai').textContent : null }))`);
      if (shown.length === 0 || shown.some((c) => !c.match || !c.match.startsWith('AI Match:'))) throw new Error(`cards shown: ${JSON.stringify(shown)}`);
      if (!shown.some((c) => c.title === TITLE)) throw new Error(`the test card is not among the matches: ${JSON.stringify(shown)}`);
      await page.screenshot(`${out}/08-ai-search.png`);
      await page.eval(`document.querySelector('#search-banner [data-action="clear-ai"]').click()`);
      await page.waitFor(`document.getElementById('search-banner').hidden && !document.querySelector('.card-match.is-ai')`);
      await page.click('#btn-search-clear');
      await page.waitFor(`document.querySelectorAll('.card').length === ${all}`);
    });
  } else {
    console.log('skip  AI Search against the real CLI (set MB_AI=1 to include it)');
  }

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
