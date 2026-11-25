// Optional browser-level smoke test. Usage:
//   node scripts/ui-smoke.mjs http://127.0.0.1:3012 [output-dir]
// Loads the board in headless Chromium, exercises the main views in both
// themes, saves screenshots and fails on any console error or exception.
// It only reads: no card, column or session is created, changed or deleted,
// and no AI model is called.

import { launch } from './cdp.mjs';
import { mkdirSync, rmSync } from 'node:fs';
import { resolve } from 'node:path';

const base = process.argv[2] || 'http://127.0.0.1:3012';
const out = resolve(process.argv[3] || 'data/ui-smoke');
mkdirSync(out, { recursive: true });
// A fresh profile: stored widths and filters of an earlier run would hide
// the defaults this test checks.
rmSync(resolve(out, 'chromium-profile'), { recursive: true, force: true });

const WIDTH = 1600;
const page = await launch({ userDataDir: resolve(out, 'chromium-profile'), width: WIDTH });
let failed = false;
const step = async (name, fn) => {
  try {
    const note = await fn();
    console.log(`ok    ${name}${note ? ` (${note})` : ''}`);
  } catch (err) {
    failed = true;
    console.log(`FAIL  ${name}: ${err.message}`);
    await page.screenshot(`${out}/fail-${name.replace(/[^a-z0-9]+/gi, '-')}.png`).catch(() => {});
  }
};
const skip = (name, why) => console.log(`skip  ${name}: ${why}`);
const q = (s) => JSON.stringify(s);
const apiGet = (path) => page.eval(`fetch(${q(path)}).then((r) => r.json())`);
const key = (k, code = 0) => page.cmd('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code: k.length === 1 ? `Key${k.toUpperCase()}` : k, text: k.length === 1 ? k : undefined, windowsVirtualKeyCode: code });
const fill = (selector, text) =>
  page.eval(`(() => { const el = document.querySelector(${q(selector)}); el.focus(); el.value = ${q(text)}; el.dispatchEvent(new Event('input', { bubbles: true })); return true; })()`);
const px = (selector, prop) => page.eval(`(() => { const el = document.querySelector(${q(selector)}); return el ? parseFloat(getComputedStyle(el)[${q(prop)}]) : null; })()`);
const visibleCards = () => page.eval(`Array.from(document.querySelectorAll('.card')).map((c) => c.dataset.id)`);
const blur = () => page.eval(`document.activeElement && document.activeElement.blur()`);

/** Checks everything the card faces promise against the API data. */
async function checkCardFaces(board) {
  const problems = await page.eval(`(() => {
    const board = ${JSON.stringify({ currentUser: board.currentUser, cards: board.cards })};
    const me = (board.currentUser || '').toLowerCase();
    const problems = [];
    let agents = 0, linked = 0, logos = { copilot: 0, codex: 0, other: 0 };
    for (const card of board.cards) {
      const el = document.querySelector('.card[data-id="' + card.id + '"]');
      if (!el) continue;
      const text = el.textContent;
      const where = card.title.slice(0, 40);
      if (card.aoSessionId) {
        agents++;
        // The AO session ID is not printed on the face, unless the user put
        // it into the title, summary or a tag themselves.
        const own = [card.title, card.summary, (card.tags || []).join(' ')].join(' ');
        if (!own.includes(card.aoSessionId) && text.includes(card.aoSessionId)) problems.push(where + ': session ID is printed on the card face');
        const agent = el.querySelector('.card-foot .agent');
        if (!agent) { problems.push(where + ': no agent badge in the footer'); continue; }
        if (!agent.title.includes(card.aoSessionId)) problems.push(where + ': agent badge tooltip lacks the session ID');
        const harness = card.session ? card.session.harness : '';
        const mark = agent.querySelector('.brand-icon svg');
        if (!mark) problems.push(where + ': no brand mark');
        else if (harness === 'copilot' || harness === 'codex') {
          logos[harness]++;
          if (!agent.querySelector('.brand-' + harness + ' svg[fill="currentColor"] path')) problems.push(where + ': wrong mark for ' + harness);
          const name = agent.querySelector('.agent-name').textContent;
          if (name !== (harness === 'copilot' ? 'Copilot' : 'Codex')) problems.push(where + ': agent name is ' + name);
        } else logos.other++;
        if (!agent.querySelector('.agent-state .state-dot')) problems.push(where + ': no activity dot');
      }
      if (!el.querySelector('.card-foot .card-time') || !el.querySelector('.card-foot .card-time').textContent) problems.push(where + ': no relative time in the footer');
      if (card.ghNumber) {
        linked++;
        const ref = el.querySelector('.card-top .gh-ref');
        if (!ref || !ref.textContent.includes('#' + card.ghNumber)) problems.push(where + ': no reference pill');
        else if (card.ghKind === 'pr' && !ref.textContent.startsWith('PR ')) problems.push(where + ': reference pill is ' + ref.textContent);
        else if (card.ghKind === 'issue' && !ref.textContent.startsWith('Issue ')) problems.push(where + ': reference pill is ' + ref.textContent);
        if (card.ghState) {
          const st = el.querySelector('.card-top .gh-state');
          if (!st || st.textContent !== card.ghState) problems.push(where + ': state badge is ' + (st && st.textContent));
        }
        if (card.ghAuthor) {
          const author = el.querySelector('.card-top .card-author .login');
          if (!author || author.textContent !== '@' + card.ghAuthor) problems.push(where + ': author is ' + (author && author.textContent));
          const mine = Boolean(me) && card.ghAuthor.toLowerCase() === me;
          const tag = el.querySelector('.card-top .badge-mine');
          if (mine !== Boolean(tag)) problems.push(where + ': MINE tag ' + (tag ? 'shown for another author' : 'missing'));
          if (tag && tag.textContent !== 'MINE') problems.push(where + ': MINE tag reads ' + tag.textContent);
        }
        if (!card.ghLastSyncedAt) continue;
        const strip = el.querySelector('.card-status');
        if (!strip) { problems.push(where + ': no status strip'); continue; }
        const chips = Array.from(strip.querySelectorAll('.chip')).map((c) => c.textContent);
        const open = card.ghState !== 'MERGED' && card.ghState !== 'CLOSED';
        if (card.ghKind === 'pr' && open) {
          const ci = { SUCCESS: 'CI Passing', FAILURE: 'CI Failing', PENDING: 'CI Pending' }[card.ghCiStatus];
          if (ci && !chips.includes(ci)) problems.push(where + ': no ' + ci + ' chip in ' + chips.join(' | '));
          const decision = { APPROVED: 'Approved', CHANGES_REQUESTED: 'Changes Req', REVIEW_REQUIRED: 'Review Required' }[card.ghReviewDecision];
          if (decision && !chips.includes(decision)) problems.push(where + ': no ' + decision + ' chip in ' + chips.join(' | '));
        }
        const mine = Boolean(me) && (card.ghAuthor || '').toLowerCase() === me;
        const own = strip.querySelector('.chip[data-review]');
        let want = null;
        if (!mine) {
          const names = { APPROVED: 'Approved', LGTM: 'LGTM', CHANGES_REQUESTED: 'Changes Req', COMMENTED: 'Commented', REVIEWED: 'Reviewed' };
          if (card.ghMyReviewState) want = card.ghIsStaleReview && open ? 'Re-review needed' : 'You: ' + names[card.ghMyReviewState];
          else if (card.ghKind === 'pr' && card.ghState === 'OPEN' && me) want = 'Not reviewed';
        }
        if ((own ? own.textContent : null) !== want) problems.push(where + ': own review chip is ' + (own && own.textContent) + ', want ' + want);
        const counts = strip.querySelector('.card-counts');
        if (!counts || !/^(\\d+ commits? \\/ )?\\d+ comments?$/.test(counts.textContent)) problems.push(where + ': counts read ' + (counts && counts.textContent));
      }
      if (card.hasUnreadUpdates && !el.querySelector('.card-update')) problems.push(where + ': no unread alert bar');
    }
    return { problems, agents, linked, logos };
  })()`);
  if (problems.problems.length) throw new Error(problems.problems.slice(0, 6).join('; '));
  return problems;
}

try {
  await page.goto(base);
  await step('board renders columns', () => page.waitFor(`document.querySelectorAll('.column').length > 0`));
  await step('AO status pill resolves', () => page.waitFor(`!document.querySelector('#ao-status .pill-label').textContent.includes('Connecting')`));
  await page.sleep(400);
  await page.screenshot(`${out}/01-board-dark.png`);

  const board = await apiGet('/api/board');
  const hasCard = await page.eval(`document.querySelectorAll('.card').length > 0`);
  const sessionCard = board.cards.find((c) => c.aoSessionId && c.session && (c.session.kind !== 'orchestrator' || board.settings.showOrchestrator));

  await step('typography is comfortable without browser zoom', async () => {
    const sizes = {
      body: await px('body', 'fontSize'),
      brand: await px('.brand', 'fontSize'),
      columnTitle: await px('.column-title', 'fontSize'),
      columnCount: await px('.column-count', 'fontSize'),
      column: await px('.column', 'width'),
    };
    const want = (name, ok) => {
      if (!ok) throw new Error(`${name} is ${sizes[name]}px (${JSON.stringify(sizes)})`);
    };
    want('body', sizes.body === 14);
    want('brand', sizes.brand >= 16 && sizes.brand <= 18);
    want('columnTitle', sizes.columnTitle === 14);
    want('columnCount', sizes.columnCount === 12);
    want('column', sizes.column >= 328);
    if (hasCard) {
      const card = await page.eval(`(() => {
        const size = (el) => parseFloat(getComputedStyle(el).fontSize);
        const out = { title: size(document.querySelector('.card-title')), small: [] };
        const summary = document.querySelector('.card-summary');
        if (summary) { out.summary = size(summary); out.summaryLines = Math.round(summary.getBoundingClientRect().height / parseFloat(getComputedStyle(summary).lineHeight)); }
        // Every piece of text on a card face is at least 12px.
        for (const el of document.querySelectorAll('.card *')) {
          if (el.children.length === 0 && el.textContent.trim() && size(el) < 12) out.small.push(el.className + ':' + size(el));
        }
        return out;
      })()`);
      if (card.title < 14.5 || card.title > 15) throw new Error(`card title is ${card.title}px`);
      if (card.summary !== undefined && (card.summary !== 13 || card.summaryLines > 3)) throw new Error(`card summary is ${card.summary}px on ${card.summaryLines} lines`);
      if (card.small.length) throw new Error(`text under 12px on a card: ${card.small.slice(0, 5).join(', ')}`);
    }
    return `body ${sizes.body}px, column ${Math.round(sizes.column)}px`;
  });

  if (hasCard) {
    await step('card faces: author, review status, CI, brand marks, no session ID', async () => {
      const res = await checkCardFaces(board);
      return `${res.linked} linked, ${res.agents} with an agent, marks: ${res.logos.copilot} Copilot / ${res.logos.codex} Codex`;
    });
  }

  await step('author filter lists and filters authors', async () => {
    const authors = Array.from(new Set(board.cards.map((c) => c.ghAuthor).filter(Boolean)));
    const options = await page.eval(`Array.from(document.querySelectorAll('#filter-author option')).map((o) => [o.value, o.textContent])`);
    if (options[0][1] !== 'All Authors') throw new Error(`first option is ${options[0][1]}`);
    if (board.currentUser && !options.some(([v, t]) => v === '@me' && t === `Mine (@${board.currentUser})`)) throw new Error(`no Mine option for ${board.currentUser}: ${JSON.stringify(options)}`);
    for (const a of authors) if (!options.some(([v, t]) => v === a && t === `@${a}`)) throw new Error(`no option for @${a}`);
    if (authors.length === 0) return 'no linked cards; only the fixed options were checked';
    const pick = authors[0];
    const before = (await visibleCards()).length;
    await page.eval(`(() => { const s = document.getElementById('filter-author'); s.value = ${q(pick)}; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
    await page.sleep(150);
    const shown = await page.eval(`Array.from(document.querySelectorAll('.card .card-author .login')).map((e) => e.textContent)`);
    const count = (await visibleCards()).length;
    await page.eval(`(() => { const s = document.getElementById('filter-author'); s.value = ''; s.dispatchEvent(new Event('change', { bubbles: true })); })()`);
    await page.sleep(150);
    if (count === 0 || shown.length !== count || shown.some((t) => t.toLowerCase() !== `@${pick}`.toLowerCase())) throw new Error(`filter @${pick} shows ${JSON.stringify(shown)} on ${count} cards`);
    if ((await visibleCards()).length !== before) throw new Error('All Authors did not restore the board');
    return `${authors.length} authors, @${pick} -> ${count} of ${before} cards`;
  });

  if (sessionCard) {
    await step('search by session ID finds the card and says why', async () => {
      const id = sessionCard.aoSessionId;
      await fill('#search', id.slice(0, Math.max(4, id.length - 1)));
      await page.waitFor(`document.querySelector('.card[data-id=${q(sessionCard.id)}]')`);
      await fill('#search', id.toUpperCase());
      await page.sleep(400);
      const info = await page.eval(`(() => { const el = document.querySelector('.card[data-id=${q(sessionCard.id)}]'); if (!el) return null; const m = el.querySelector('.card-match.is-id'); return { badge: m ? m.textContent : null, clear: !document.getElementById('btn-search-clear').hidden }; })()`);
      if (!info) throw new Error(`the card of ${id} is not shown`);
      const visible = [sessionCard.title, sessionCard.summary, sessionCard.notes, (sessionCard.tags || []).join(' ')].join(' ').toLowerCase().includes(id.toLowerCase());
      if (!visible && info.badge !== `Matched ID:${id}`) throw new Error(`badge reads ${info.badge}`);
      if (!info.clear) throw new Error('the Clear button is hidden while a search is active');
      await page.screenshot(`${out}/08-search-session-id.png`);
      await page.click('#btn-search-clear');
      await page.waitFor(`document.getElementById('search').value === '' && document.getElementById('btn-search-clear').hidden`);
      return info.badge || 'the ID is part of the card text, so no badge';
    });

    await step('search finds text inside a session conversation', async () => {
      // Take a distinctive word of a real message that is not on the card.
      const conv = await apiGet(`/api/sessions/${encodeURIComponent(sessionCard.aoSessionId)}/conversation?limit=40`);
      const own = JSON.stringify(sessionCard).toLowerCase();
      let word = null;
      for (const m of conv.messages || []) {
        for (const w of (m.text || '').match(/[A-Za-z][A-Za-z0-9_-]{8,24}/g) || []) {
          if (!own.includes(w.toLowerCase())) { word = w; break; }
        }
        if (word) break;
      }
      if (!word) return 'no distinctive word in the latest messages; skipped';
      const res = await apiGet(`/api/search?q=${encodeURIComponent(word)}`);
      const match = res.matches.find((m) => m.cardId === sessionCard.id);
      if (!match) return `"${word}" is not indexed yet (${res.index.messages} messages indexed); skipped`;
      if (!match.matchedBy.includes('conversation') || !match.snippet) throw new Error(`unexpected match ${JSON.stringify(match)}`);
      await fill('#search', word);
      await page.waitFor(`document.querySelector('.card[data-id=${q(sessionCard.id)}] .card-match.is-chat')`);
      const strip = await page.eval(`document.querySelector('.card[data-id=${q(sessionCard.id)}] .card-match.is-chat').textContent`);
      if (!strip.startsWith('In chat:') || !strip.toLowerCase().includes(word.toLowerCase())) throw new Error(`strip reads ${strip}`);
      await page.screenshot(`${out}/09-search-conversation.png`);
      await page.click('#btn-search-clear');
      return `"${word}" -> ${strip.slice(0, 60)}`;
    });
  } else {
    skip('session ID and conversation search', 'no card with a live AO session');
  }

  await step('AI Search controls are present', async () => {
    const info = await page.eval(`({ label: document.getElementById('btn-ai-search').textContent.trim(), clear: document.getElementById('btn-search-clear').textContent.trim(), banner: document.getElementById('search-banner').hidden })`);
    if (info.label !== 'AI Search' || info.clear !== 'Clear' || !info.banner) throw new Error(JSON.stringify(info));
    // Without a query the button asks for one instead of calling a model.
    await page.click('#btn-ai-search');
    await page.waitFor(`Array.from(document.querySelectorAll('.toast')).some((t) => t.textContent.includes('Type what you are looking for'))`);
    if (await page.eval(`!document.getElementById('search-banner').hidden`)) throw new Error('an empty query started a search');
    await blur();
  });

  if (hasCard) {
    const target = sessionCard ? `.card[data-id=${q(sessionCard.id)}]` : '.card';
    await step('card opens the drawer', async () => {
      await page.click(target);
      await page.waitFor(`document.querySelector('.drawer')`);
      await page.sleep(1500);
    });
    await page.screenshot(`${out}/02-drawer-dark.png`);

    await step('drawer is wide by default and resizable by dragging', async () => {
      const want = Math.min(780, WIDTH * 0.6);
      const width = await px('.drawer', 'width');
      if (Math.abs(width - want) > 1) throw new Error(`default width is ${width}px, want ${want}px`);
      const box = await page.eval(`(() => { const r = document.querySelector('.drawer-resize').getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
      await page.mouse('mousePressed', box.x, box.y);
      await page.mouse('mouseMoved', box.x - 80, box.y, { buttons: 1 });
      await page.mouse('mouseMoved', box.x - 160, box.y, { buttons: 1 });
      await page.sleep(80);
      await page.mouse('mouseReleased', box.x - 160, box.y);
      await page.sleep(120);
      const wider = await px('.drawer', 'width');
      const stored = await page.eval(`JSON.parse(localStorage.getItem('mb.drawerWidth.v2'))`);
      if (Math.abs(wider - (width + 160)) > 3) throw new Error(`after dragging 160px the width is ${wider}px`);
      if (Math.abs(stored - wider) > 1) throw new Error(`stored width is ${stored}`);
      // The limits hold: never under 520px, never over 92% of the window.
      await page.eval(`localStorage.setItem('mb.drawerWidth.v2', '5000')`);
      await page.mouse('mousePressed', box.x - 160, box.y);
      await page.mouse('mouseMoved', 20, box.y, { buttons: 1 });
      await page.mouse('mouseReleased', 20, box.y);
      await page.sleep(120);
      const max = await px('.drawer', 'width');
      await page.mouse('mousePressed', WIDTH - max, box.y);
      await page.mouse('mouseMoved', WIDTH - 100, box.y, { buttons: 1 });
      await page.mouse('mouseReleased', WIDTH - 100, box.y);
      await page.sleep(120);
      const min = await px('.drawer', 'width');
      if (Math.abs(max - Math.floor(WIDTH * 0.92)) > 1 || min !== 520) throw new Error(`limits are ${min}px to ${max}px`);
      // Double-click restores the default.
      await page.eval(`document.querySelector('.drawer-resize').dispatchEvent(new MouseEvent('dblclick', { bubbles: true }))`);
      await page.sleep(120);
      const reset = await px('.drawer', 'width');
      if (Math.abs(reset - want) > 1) throw new Error(`after reset the width is ${reset}px`);
      return `${width}px default, ${min}px to ${max}px`;
    });

    if (sessionCard) {
      await step('drawer header shows the session ID; chat text is comfortable', async () => {
        const info = await page.eval(`(() => {
          const size = (sel) => { const el = document.querySelector(sel); return el ? parseFloat(getComputedStyle(el).fontSize) : null; };
          const chip = document.querySelector('.drawer-top .drawer-session');
          return {
            id: chip ? chip.querySelector('.session-id').textContent : null,
            mark: Boolean(chip && chip.querySelector('.brand-icon svg')),
            body: size('.drawer .msg-body .md'), composer: size('.drawer .composer textarea'), code: size('.drawer .msg-body pre') || size('.drawer .msg-body code'),
            lineHeight: (() => { const el = document.querySelector('.drawer .msg-body .md'); return el ? parseFloat(getComputedStyle(el).lineHeight) / parseFloat(getComputedStyle(el).fontSize) : null; })(),
          };
        })()`);
        if (info.id !== sessionCard.aoSessionId || !info.mark) throw new Error(`header chip: ${JSON.stringify(info)}`);
        if (info.composer !== 14) throw new Error(`composer is ${info.composer}px`);
        if (info.body !== null && (info.body !== 14.5 || Math.abs(info.lineHeight - 1.65) > 0.01)) throw new Error(`chat body is ${info.body}px / ${info.lineHeight}`);
        if (info.code !== null && info.code !== 13) throw new Error(`chat code is ${info.code}px`);
        return info.body === null ? 'no messages loaded; header and composer checked' : `chat ${info.body}px`;
      });
    }

    if (sessionCard) {
      // Read-only: the picker is inspected, never changed, so the model of
      // a real session stays what it is.
      await step('model picker of the session lists its models', async () => {
        const api = await apiGet(`/api/cards/${sessionCard.id}/models`);
        if (api.error) throw new Error(`models API: ${api.error.message}`);
        await page.waitFor(`document.querySelector('.drawer .composer select[aria-label="Model"]').options.length === ${api.models.length + 1} || document.querySelector('.drawer .composer select[aria-label="Model"]').options.length === ${api.models.length + 2}`, 15000, 'model options');
        const info = await page.eval(`(() => {
          const model = document.querySelector('.drawer .composer select[aria-label="Model"]');
          const effort = document.querySelector('.drawer .composer select[aria-label="Effort"]');
          const chip = document.querySelector('.session-bar .session-model');
          return { first: model.options[0].textContent, value: model.value, disabled: model.disabled, effortHidden: effort.hidden, effort: effort.value, chip: chip ? chip.textContent : null };
        })()`);
        if (!info.first.startsWith('Default')) throw new Error(`first option reads ${info.first}`);
        if (info.value !== (api.selected.model || '')) throw new Error(`selected ${info.value}, API says ${api.selected.model}`);
        if (api.models.length > 0 && info.disabled) throw new Error('the picker is disabled although models are known');
        if (api.selected.model && !info.chip) throw new Error('the session bar does not show the configured model');
        return `${api.models.length} models from the ${api.source}, selected: ${api.selected.model || 'default'}${api.needsResume ? ', session not resumed' : ''}`;
      });
    }

    await step('Expand opens the full-width workspace', async () => {
      const label = await page.eval(`document.querySelector('.drawer-expand').textContent.trim()`);
      if (label !== 'Expand') throw new Error(`toggle reads ${label}`);
      await page.click('.drawer-expand');
      await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
      await page.sleep(300);
      const info = await page.eval(`(() => {
        const rect = (sel) => { const el = document.querySelector(sel); return el ? el.getBoundingClientRect() : null; };
        const d = rect('.drawer'), rail = rect('.drawer-details'), main = rect('.drawer-main'), composer = rect('.composer-box'), msg = rect('.chat-scroll .msg');
        return {
          width: d.width, hasChat: Boolean(main), rail: rail && rail.width, railLeft: rail && rail.left, main: main && main.width,
          composer: composer && composer.width, composerCenter: composer && (composer.left + composer.width / 2), mainCenter: main && (main.left + main.width / 2),
          composerBottom: composer && composer.bottom, msg: msg && msg.width,
          toggle: document.querySelector('.drawer-expand').textContent.trim(), back: document.querySelector('.drawer-close').textContent.trim(),
          controls: Boolean(document.querySelector('.drawer-details .section-controls:not([hidden]) select[aria-label="Column"]')),
          controlsInHeader: Boolean(document.querySelector('.drawer-top select[aria-label="Column"]')),
          handle: getComputedStyle(document.querySelector('.drawer-resize')).display,
          sections: Array.from(document.querySelectorAll('.drawer-details .section-title')).map((e) => e.textContent),
        };
      })()`);
      if (info.width < WIDTH * 0.96) throw new Error(`workspace is ${info.width}px wide`);
      if (info.toggle !== 'Split Board' || info.back !== 'Back to Board') throw new Error(`buttons read ${info.toggle} / ${info.back}`);
      if (!info.controls || info.controlsInHeader) throw new Error('the controls did not move into the context rail');
      if (info.handle !== 'none') throw new Error('the resize handle is still shown');
      for (const name of ['Summary', 'GitHub Tracking', 'Maintainer Notes', 'Controls']) if (!info.sections.includes(name)) throw new Error(`rail lacks ${name}: ${info.sections}`);
      if (info.hasChat) {
        if (Math.abs(info.rail - 340) > 1 || info.railLeft !== 0) throw new Error(`rail is ${info.rail}px at ${info.railLeft}`);
        if (Math.abs(info.main - (WIDTH - 340)) > 1) throw new Error(`chat pane is ${info.main}px`);
        if (info.composer > 900 || info.composer < 700 || Math.abs(info.composerCenter - info.mainCenter) > 2) throw new Error(`composer is ${info.composer}px, centered at ${info.composerCenter} in ${info.mainCenter}`);
        if (info.msg !== null && info.msg > 900) throw new Error(`messages are ${info.msg}px wide`);
        if (info.composerBottom > 950 || info.composerBottom < 900) throw new Error(`composer ends at ${info.composerBottom}px, not at the bottom`);
      }
      await page.screenshot(`${out}/10-workspace-dark.png`);
      return info.hasChat ? `rail ${info.rail}px, chat ${info.main}px, reading column ${info.composer}px` : 'card without a conversation';
    });

    if (sessionCard) {
      await step('the context rail collapses and comes back', async () => {
        await page.click('.session-bar .details-toggle');
        await page.waitFor(`getComputedStyle(document.querySelector('.drawer-details')).display === 'none'`);
        const main = await px('.drawer-main', 'width');
        if (Math.abs(main - WIDTH) > 1) throw new Error(`chat pane is ${main}px with the rail hidden`);
        const label = await page.eval(`document.querySelector('.session-bar .details-toggle').textContent.trim()`);
        if (label !== 'Show Context') throw new Error(`toggle reads ${label}`);
        await page.click('.session-bar .details-toggle');
        await page.waitFor(`getComputedStyle(document.querySelector('.drawer-details')).display !== 'none'`);
      });
    }

    await step('F and Esc step between workspace, drawer and board', async () => {
      await blur();
      await key('f', 70);
      await page.waitFor(`document.querySelector('.drawer') && !document.querySelector('.drawer.is-expanded')`);
      if (!(await page.eval(`Boolean(document.querySelector('.drawer-top select[aria-label="Column"]'))`))) throw new Error('the controls did not return to the header');
      await key('f', 70);
      await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
      await key('Escape', 27);
      await page.waitFor(`document.querySelector('.drawer') && !document.querySelector('.drawer.is-expanded')`);
      await page.click('.drawer-expand');
      await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
      await page.click('.drawer-expand');
      await page.waitFor(`!document.querySelector('.drawer.is-expanded') && !document.body.classList.contains('drawer-expanded')`);
    });

    if (sessionCard) {
      await step('details above the chat are compact and collapsible', async () => {
        const info = await page.eval(`(() => {
          const d = document.querySelector('.drawer'), details = document.querySelector('.drawer-details'), chat = document.querySelector('.drawer .chat');
          return { drawer: d.getBoundingClientRect().height, details: details.getBoundingClientRect().height, chat: chat.getBoundingClientRect().height,
            toggles: document.querySelectorAll('.drawer-details .section-toggle').length };
        })()`);
        if (info.details > info.drawer * 0.4 + 1) throw new Error(`details take ${info.details}px of ${info.drawer}px`);
        if (info.toggles < 3) throw new Error(`only ${info.toggles} collapsible sections`);
        // Folding a section leaves one line with a preview.
        const before = await page.eval(`document.querySelector('.drawer-details .section-toggle').getAttribute('aria-expanded')`);
        await page.click('.drawer-details .section-toggle');
        await page.waitFor(`document.querySelector('.drawer-details .section-toggle').getAttribute('aria-expanded') !== ${q(before)}`);
        await page.click('.drawer-details .section-toggle');
        await page.waitFor(`document.querySelector('.drawer-details .section-toggle').getAttribute('aria-expanded') === ${q(before)}`);
        // Hiding all details hands the whole height to the conversation.
        await page.click('.session-bar .details-toggle');
        await page.waitFor(`getComputedStyle(document.querySelector('.drawer-details')).display === 'none'`);
        const tall = await px('.drawer .chat', 'height');
        await page.click('.session-bar .details-toggle');
        await page.waitFor(`getComputedStyle(document.querySelector('.drawer-details')).display !== 'none'`);
        if (tall <= info.chat) throw new Error('hiding the details did not give the chat more room');
        return `details ${Math.round(info.details)}px, chat ${Math.round(info.chat)}px (${Math.round(tall)}px with details hidden)`;
      });
    }

    await step('timeline loads', async () => {
      await page.eval(`document.querySelector('.drawer details.section').open = true`);
      await page.waitFor(`document.querySelectorAll('.drawer .timeline li').length > 0`);
    });
    await step('Escape closes the drawer', async () => {
      await blur();
      await key('Escape', 27);
      await page.waitFor(`!document.querySelector('.drawer')`);
    });
  }

  await step('theme toggles to light', async () => {
    await page.click('#btn-theme');
    await page.waitFor(`document.documentElement.dataset.theme === 'light'`);
  });
  await page.sleep(300);
  await page.screenshot(`${out}/03-board-light.png`);
  if (hasCard) {
    await step('light theme: card faces and workspace', async () => {
      await checkCardFaces(await apiGet('/api/board'));
      const colors = await page.eval(`(() => {
        const c = (sel) => { const el = document.querySelector(sel); return el ? getComputedStyle(el).color : null; };
        return { copilot: c('.card .brand-copilot'), codex: c('.card .brand-codex'), title: c('.card-title'), bg: getComputedStyle(document.body).backgroundColor };
      })()`);
      if (colors.bg !== 'rgb(246, 248, 250)') throw new Error(`light background is ${colors.bg}`);
      if (colors.copilot && colors.codex && colors.copilot === colors.codex) throw new Error('both brand marks have the same tint');
      await page.click(sessionCard ? `.card[data-id=${q(sessionCard.id)}]` : '.card');
      await page.waitFor(`document.querySelector('.drawer')`);
      await page.sleep(1500);
      await page.screenshot(`${out}/04-drawer-light.png`);
      await page.click('.drawer-expand');
      await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
      await page.sleep(400);
      await page.screenshot(`${out}/11-workspace-light.png`);
      // "Back to Board" closes the workspace in one click.
      await page.click('.drawer-top button[aria-label="Close"]');
      await page.waitFor(`!document.querySelector('.drawer') && !document.body.classList.contains('drawer-expanded')`);
      return `Copilot ${colors.copilot || 'n/a'}, Codex ${colors.codex || 'n/a'}`;
    });
  }
  await page.click('#btn-theme');

  await step('columns dialog opens', async () => {
    await page.click('#btn-columns');
    await page.waitFor(`document.querySelectorAll('.col-list .col-row').length > 0`);
  });
  await page.sleep(350);
  await page.screenshot(`${out}/05-columns.png`);
  await page.click('.modal-head button[aria-label="Close"]');

  await step('new task dialog opens', async () => {
    await page.click('#btn-new');
    await page.waitFor(`document.querySelector('.modal .form-grid')`);
    const title = await px('.modal-title', 'fontSize');
    if (title < 16 || title > 18) throw new Error(`modal title is ${title}px`);
  });
  if (board.ao.connected) {
    await step('new task dialog offers the models of each agent', async () => {
      const counts = {};
      for (const [label, agent] of [['Copilot', 'copilot'], ['Codex', 'codex']]) {
        const api = await apiGet(`/api/ao/agents/${agent}/models`);
        if (api.error) throw new Error(`${agent} models: ${api.error.message}`);
        await page.eval(`Array.from(document.querySelectorAll('.modal .segmented button')).find((b) => b.textContent === ${q(label)}).click()`);
        await page.waitFor(`!document.querySelector('.modal select[aria-label="Model"]').disabled && document.querySelector('.modal select[aria-label="Model"]').options.length === ${api.models.length + 1}`, 20000, `${label} models`);
        const info = await page.eval(`({ first: document.querySelector('.modal select[aria-label="Model"]').options[0].textContent, hidden: document.querySelector('.modal select[aria-label="Model"]').closest('.field').hidden, effort: !document.querySelector('.modal select[aria-label="Effort"]').closest('.field').hidden })`);
        if (info.hidden || !info.first.startsWith('Default')) throw new Error(`${label}: ${JSON.stringify(info)}`);
        const fallback = api.models.find((m) => m.isDefault);
        if (info.effort !== Boolean(fallback && fallback.efforts.length)) throw new Error(`${label}: effort field shown = ${info.effort}`);
        counts[label] = api.models.length;
      }
      return `Copilot ${counts.Copilot}, Codex ${counts.Codex}`;
    });
  }
  await page.sleep(350);
  await page.screenshot(`${out}/06-new-task.png`);
  await page.click('.modal-head button[aria-label="Close"]');

  await step('import dialog lists history', async () => {
    await page.click('#btn-import');
    await page.waitFor(`document.querySelector('.import-row, .import-state')`);
    await page.waitFor(`!document.querySelector('.import-state') || !document.querySelector('.import-state').textContent.includes('Scanning')`, 60000, 'import scan');
  });
  await page.sleep(350);
  await page.screenshot(`${out}/07-import.png`);
  await page.click('.modal-head button[aria-label="Close"]');

  await step('no emoji in rendered chrome', async () => {
    const found = await page.eval(`(() => {
      const re = /[\\u{1F000}-\\u{1FAFF}\\u{2600}-\\u{27BF}\\u{2B00}-\\u{2BFF}\\u{FE0F}]/u;
      const bad = [];
      for (const el of document.querySelectorAll('header, .filterbar, .column-header, .card-top, .card-status, .card-foot, .card-update, .modal, .toast')) {
        if (re.test(el.textContent)) bad.push(el.className);
      }
      return bad;
    })()`);
    if (found.length) throw new Error(`emoji found in: ${found.join(', ')}`);
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
