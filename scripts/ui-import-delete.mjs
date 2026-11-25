// Browser test of the workspace toggle, Import History search, the progress
// indicators and the delete dialogs. It imports, messages, resumes and
// DELETES sessions, so it runs only against a board that talks to the fake
// AO daemon of scripts/fake-ao.mjs and the fake AI CLI of scripts/fake-ai.mjs,
// never against real sessions:
//
//   dir=$PWD/data/scratch/import; rm -rf "$dir"; mkdir -p "$dir"
//   FAKE_AO_DELAY_MS=700 node scripts/fake-ao.mjs 3925 &
//   FAKE_AI_DELAY_MS=2600 FAKE_AI_LOG=$dir/ai.log ./maintainer-board --addr 127.0.0.1:3926 --db "$dir/board.db" \
//     --ao-url http://127.0.0.1:3925 --ao-sync-interval 1s --history-home "$dir/home" \
//     --copilot-path "$PWD/scripts/fake-ai.mjs" --codex-path "$PWD/scripts/fake-ai.mjs" &
//   FAKE_AI_LOG=$dir/ai.log node scripts/ui-import-delete.mjs http://127.0.0.1:3926 http://127.0.0.1:3925 "$dir/home"
//
// The script writes a small Copilot and Codex history into <history-home>
// (the Copilot store needs python3; without it those checks are skipped)
// and tells the fake daemon to report it as import candidates.

import { launch } from './cdp.mjs';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';

const [base, fake, homeArg] = process.argv.slice(2);
if (!base || !fake || !homeArg) {
  console.error('usage: node scripts/ui-import-delete.mjs <board-url> <fake-ao-url> <history-home> [output-dir]');
  process.exit(2);
}
const home = resolve(homeArg);
const out = resolve(process.argv[5] || 'data/ui-import-delete');
mkdirSync(out, { recursive: true });
rmSync(resolve(out, 'chromium-profile'), { recursive: true, force: true });

const fakeState = async () => (await fetch(`${fake}/__state`)).json();
const fakePost = (path, body) => fetch(`${fake}${path}`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
try {
  const st = await fakeState();
  if (!Array.isArray(st.deleted) || !Array.isArray(st.candidates)) throw new Error('unexpected state');
} catch (err) {
  console.error(`${fake} is not the fake AO daemon (${err.message}); refusing to run against a real one.`);
  process.exit(2);
}

// History fixture

const BOARD_ID = '8dd57e72-e587-4995-a8d8-15426ebca116';
const HELPER_ID = '3425333e-2b9d-4f05-80a7-6986642cdd8d';
const EVENTS_ID = '01a1c998-e2d3-7c11-a59c-8d52c0f94482';
const CSI_ID = '01a1c02a-7f76-7dc0-97f0-5a417ba188b7';
const candidateId = (harness, nativeId) => createHash('sha256').update(`${harness}\0${resolve(home, `.${harness}`)}\0${nativeId}`).digest('hex');

function writeRollout(id, cwd, branch, user, agent) {
  const dir = resolve(home, '.codex/sessions/2026/11/11');
  mkdirSync(dir, { recursive: true });
  const item = (type, text) => JSON.stringify({ timestamp: 't', type: 'event_msg', payload: { type: 'item_completed', item: { type, content: [{ type: 'text', text }] } } });
  const lines = [JSON.stringify({ timestamp: 't', type: 'session_meta', payload: { id, cwd, git: { branch } } }), item('UserMessage', user), item('AgentMessage', agent)];
  writeFileSync(resolve(dir, `rollout-2026-11-11T07-53-22-${id}.jsonl`), `${lines.join('\n')}\n`);
}

writeRollout(EVENTS_ID, '/root/github/volcano', 'refactor/events', '#6081 #6196 unify the pod and podgroup event/condition refactor, drop the unnecessary retries and rollback', 'The scheduler cache updates PodGroup conditions twice per cycle.');
writeRollout(CSI_ID, '/root/github/volcano', 'master', 'Investigate why preemption regresses for CSI volume limits in PR 6247', 'Reproduced on a kind cluster: the high priority pod stays pending.');

let withCopilot = true;
try {
  mkdirSync(resolve(home, '.copilot'), { recursive: true });
  rmSync(resolve(home, '.copilot/session-store.db'), { force: true });
  execFileSync('python3', [
    '-c',
    `
import sqlite3, sys
db = sqlite3.connect(sys.argv[1])
db.executescript("""
CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, host_type TEXT, branch TEXT, summary TEXT, created_at TEXT, updated_at TEXT);
CREATE TABLE turns (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, turn_index INTEGER NOT NULL, user_message TEXT, assistant_response TEXT);
CREATE TABLE checkpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, checkpoint_number INTEGER NOT NULL, title TEXT, overview TEXT);
""")
db.execute("INSERT INTO sessions (id, cwd, repository, branch, summary) VALUES (?, '/root/tools/maintainer-board', 'volcano-sh/volcano', 'feat/board', 'Tool for PR Review Tracking')", (sys.argv[2],))
db.execute("INSERT INTO sessions (id, cwd, branch, summary) VALUES (?, '/tmp/summarizer', 'main', 'Renamed by hand')", (sys.argv[3],))
db.execute("INSERT INTO turns (session_id, turn_index, user_message, assistant_response) VALUES (?, 0, 'I review many PRs and issues as a maintainer and want a kanban tool for my agent sessions', 'I will build it on top of agent-orchestrator and the gh CLI.')", (sys.argv[2],))
db.execute("INSERT INTO turns (session_id, turn_index, user_message, assistant_response) VALUES (?, 0, '[maintainer-board-summary] Describe what a card is about.', 'A summary.')", (sys.argv[3],))
db.execute("INSERT INTO checkpoints (session_id, checkpoint_number, title, overview) VALUES (?, 1, 'Building Maintainer Board', 'The user wanted a unified zero-emoji Kanban GUI for review sessions.')", (sys.argv[2],))
db.commit()
`,
    resolve(home, '.copilot/session-store.db'),
    BOARD_ID,
    HELPER_ID,
  ]);
} catch (err) {
  withCopilot = false;
  console.log(`skip  Copilot store checks: python3 could not write the fixture (${String(err.message).split('\n')[0]})`);
}

const candidates = [
  { id: candidateId('copilot', BOARD_ID), harness: 'copilot', title: 'Tool for PR Review Tracking', projectId: 'demo', lastActivity: '2026-11-25T03:07:00Z', messageCount: 92 },
  // AO shows the renamed title; only the first prompt in the local store
  // reveals this as one of the board's own AI runs.
  { id: candidateId('copilot', HELPER_ID), harness: 'copilot', title: withCopilot ? 'Renamed by hand' : '[maintainer-board-summary] Describe what a card', lastActivity: '2026-11-25T03:01:00Z', messageCount: 2 },
  { id: candidateId('codex', EVENTS_ID), harness: 'codex', title: '#6081 #6196 pod podgroup event refactor', folder: '/root/github/volcano', lastActivity: '2026-11-11T07:53:22Z', messageCount: 240, sessionId: 'demo-1' },
  { id: candidateId('codex', CSI_ID), harness: 'codex', title: 'Investigate why preemption regresses', folder: '/root/github/volcano', lastActivity: '2026-10-21T10:16:38Z', messageCount: 61 },
  { id: 'aoonly00000000000000000000000000000000000000000000000000000000ff', harness: 'copilot', title: 'Kubeflow Trainer integration design', folder: '/root/kubeflow', lastActivity: '2026-09-01T00:00:00Z', messageCount: 12 },
  { id: 'legacy0000000000000000000000000000000000000000000000000000000001', harness: 'copilot', title: 'Reply in 1 concise Simplified Chinese sentence describing what this PR', lastActivity: '2026-11-25T03:00:00Z', messageCount: 2 },
];
await fakePost('/__candidates', { candidates });
const REAL = 4;

// Browser

const page = await launch({ userDataDir: resolve(out, 'chromium-profile') });
let failed = false;
const step = async (name, fn) => {
  try {
    const note = await fn();
    console.log(`ok    ${name}${note ? ` (${note})` : ''}`);
  } catch (err) {
    failed = true;
    console.log(`FAIL  ${name}: ${err.message}`);
    await page.screenshot(`${out}/fail-${name.replace(/[^a-z0-9]+/gi, '-')}.png`).catch(() => {});
    // Leave no dialog behind for the next step.
    await page.eval(`(() => { for (let i = 0; i < 4; i += 1) { const b = document.querySelector('.modal-head button[aria-label="Close"]'); if (b) b.click(); } })()`).catch(() => {});
  }
};
const q = (s) => JSON.stringify(s);
const apiGet = (path) => page.eval(`fetch(${q(path)}).then((r) => r.json())`);
const key = (k, code = 0) => page.cmd('Input.dispatchKeyEvent', { type: 'keyDown', key: k, code: k.length === 1 ? `Key${k.toUpperCase()}` : k, text: k.length === 1 ? k : undefined, windowsVirtualKeyCode: code });
const fill = (selector, text) =>
  page.eval(`(() => { const el = document.querySelector(${q(selector)}); if (!el) throw new Error('no element ' + ${q(selector)}); el.focus(); el.value = ${q(text)}; el.dispatchEvent(new Event('input', { bubbles: true })); return true; })()`);
const text = (selector) => page.eval(`(() => { const el = document.querySelector(${q(selector)}); return el ? el.textContent.trim() : null; })()`);
const clickText = (selector, label) =>
  page.eval(`(() => { const el = Array.from(document.querySelectorAll(${q(selector)})).find((e) => e.textContent.trim() === ${q(label)}); if (!el) throw new Error('no ' + ${q(selector)} + ' reading ' + ${q(label)}); el.click(); return true; })()`);
const blur = () => page.eval(`document.activeElement && document.activeElement.blur()`);
const cardBySession = async (id) => (await apiGet('/api/cards')).cards.find((c) => c.aoSessionId === id);
const openCard = async (card) => {
  await page.click(`.card[data-id="${card.id}"]`);
  await page.waitFor(`document.querySelector('.drawer .title-input') && document.querySelector('.drawer .title-input').value === ${q(card.title)}`, 15000, `drawer of ${card.title}`);
};
const closeDrawer = async () => {
  if (await page.eval(`Boolean(document.querySelector('.drawer'))`)) {
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer')`);
  }
};
const rows = () =>
  page.eval(`Array.from(document.querySelectorAll('.import-row:not(.is-head)')).map((r) => ({
    id: r.dataset.id, title: r.querySelector('.import-title').textContent, imported: r.classList.contains('is-imported'),
    reason: r.querySelector('.import-reason') ? r.querySelector('.import-reason').textContent : '',
    ai: Boolean(r.querySelector('.import-reason.is-ai')), preview: r.querySelector('.import-preview') ? r.querySelector('.import-preview').textContent : '',
    chips: Array.from(r.querySelectorAll('.id-chip')).map((c) => ({ text: c.textContent, title: c.title, ao: c.classList.contains('is-ao') })),
    agent: r.querySelector('.import-agent').textContent, logo: Boolean(r.querySelector('.import-agent .brand-icon svg')), action: r.querySelector('.import-action').textContent.trim(),
  }))`);
const SEARCH = '.import-search input';
const searchFor = async (query, count) => {
  await fill(SEARCH, query);
  // The count says "(searching...)" until the ranked server answer replaced
  // the quick match in the browser.
  await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === ${count} && !document.querySelector('.import-count').textContent.includes('searching')`, 8000, `${count} rows for "${query}"`);
  return rows();
};
const dialogButtons = () => page.eval(`Array.from(document.querySelectorAll('.modal-foot button')).map((b) => b.textContent.trim())`);
const aiLog = () =>
  process.env.FAKE_AI_LOG && existsSync(process.env.FAKE_AI_LOG)
    ? readFileSync(process.env.FAKE_AI_LOG, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l))
    : null;

try {
  await page.goto(base);
  await page.waitFor(`document.querySelectorAll('.card').length >= 2`, 20000, 'cards of the fake sessions');
  const running = await cardBySession('demo-1');
  const sleeping = await cardBySession('demo-2');

  await step('Expand is a prominent, labelled button next to Close', async () => {
    await openCard(running);
    const info = await page.eval(`(() => {
      const b = document.querySelector('.drawer-expand'), close = document.querySelector('.drawer-close'), ghost = document.querySelector('.drawer-top button[aria-label="Delete card"]');
      const cs = getComputedStyle(b), gs = getComputedStyle(ghost), r = b.getBoundingClientRect(), cr = close.getBoundingClientRect();
      return { label: b.textContent.trim(), title: b.title, svg: Boolean(b.querySelector('svg')), cls: b.className, next: b.nextElementSibling === close,
        border: cs.borderTopColor, bg: cs.backgroundColor, color: cs.color, ghostBorder: gs.borderTopColor, ghostBg: gs.backgroundColor,
        width: r.width, gap: cr.left - r.right, closeLabel: close.textContent.trim(), closeTitle: close.title, closeRight: window.innerWidth - cr.right };
    })()`);
    if (info.label !== 'Expand' || !info.svg) throw new Error(`button reads "${info.label}", icon ${info.svg}`);
    if (info.title !== 'Expand to full workspace (F)') throw new Error(`tooltip reads ${info.title}`);
    if (!info.cls.includes('btn-expand')) throw new Error(`class is ${info.cls}`);
    if (info.border === info.ghostBorder || info.bg === info.ghostBg || info.bg === 'rgba(0, 0, 0, 0)') throw new Error(`not accented: border ${info.border}, background ${info.bg}`);
    if (!info.next || info.gap > 12) throw new Error(`not next to Close (gap ${info.gap}px)`);
    if (info.width < 80) throw new Error(`only ${info.width}px wide, the label is hidden`);
    if (info.closeLabel !== '' || info.closeTitle !== 'Close (Esc)' || info.closeRight > 20) throw new Error(`Close reads "${info.closeLabel}" / ${info.closeTitle}, ${info.closeRight}px from the edge`);
    await page.screenshot(`${out}/01-drawer-expand-button.png`);
    return `${Math.round(info.width)}px, ${info.color} on ${info.bg}`;
  });

  await step('Collapse returns to the drawer with the same card; Close goes to the board', async () => {
    await page.waitFor(`document.querySelectorAll('.chat-scroll .msg').length >= 2`, 10000, 'the conversation');
    await fill('.composer textarea', 'a draft that must survive');
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await page.sleep(250);
    const info = await page.eval(`(() => {
      const b = document.querySelector('.drawer-expand'), close = document.querySelector('.drawer-close');
      const r = b.getBoundingClientRect(), cr = close.getBoundingClientRect();
      return { label: b.textContent.trim(), title: b.title, left: r.left, cls: b.className, svg: Boolean(b.querySelector('svg')),
        closeLabel: close.textContent.trim(), closeTitle: close.title, closeRight: window.innerWidth - cr.right, closeLeft: cr.left,
        back: Array.from(document.querySelectorAll('.drawer-top button')).map((x) => x.textContent.trim()).filter((t) => /board/i.test(t)) };
    })()`);
    if (info.label !== 'Collapse' || !info.svg || !info.cls.includes('btn-collapse')) throw new Error(`toggle reads "${info.label}" (${info.cls})`);
    if (info.left > 40) throw new Error(`Collapse is ${info.left}px from the left edge, not leading the header`);
    if (!/side drawer/.test(info.title) || !/stays open/.test(info.title)) throw new Error(`tooltip reads ${info.title}`);
    if (info.closeLabel !== '' || info.closeRight > 24 || info.closeLeft < info.left + 400) throw new Error(`Close is not a separate X at the far right: "${info.closeLabel}", ${info.closeRight}px`);
    if (info.back.length) throw new Error(`a button still reads ${info.back}`);
    await page.screenshot(`${out}/02-workspace-collapse-button.png`);

    const same = `document.querySelector('.drawer') && !document.querySelector('.drawer.is-expanded') && !document.body.classList.contains('drawer-expanded')
      && document.querySelector('.drawer .title-input').value === ${q(running.title)} && document.querySelector('.composer textarea').value === 'a draft that must survive'
      && document.querySelectorAll('.chat-scroll .msg').length >= 2`;
    await page.click('.drawer-expand');
    await page.waitFor(same, 5000, 'the same card in the side drawer after Collapse');
    if ((await text('.drawer-expand')) !== 'Expand') throw new Error('the toggle did not turn back into Expand');

    // F toggles, Esc steps back one level at a time.
    await blur();
    await key('f', 70);
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await key('f', 70);
    await page.waitFor(same, 5000, 'the drawer after F');
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await blur();
    await key('Escape', 27);
    await page.waitFor(same, 5000, 'the drawer after Esc in the workspace');
    await key('Escape', 27);
    await page.waitFor(`!document.querySelector('.drawer')`, 5000, 'the board after a second Esc');

    // Close leaves the workspace in one click.
    await openCard(running);
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer') && !document.body.classList.contains('drawer-expanded')`);
  });

  await step('the reading position survives Expand and Collapse', async () => {
    // A long conversation, read somewhere in the middle.
    for (let i = 0; i < 4; i += 1) {
      await page.eval(`fetch('/api/sessions/demo-1/messages', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ text: 'MARK ${i} ' + 'filler line of the conversation. '.repeat(60) }) })`);
      await page.sleep(150);
    }
    await openCard(running);
    await page.waitFor(`document.querySelectorAll('.chat-scroll .msg-user').length >= 5 && !document.querySelector('.msg-thinking')`, 30000, 'the long conversation');
    const visible = `(() => { const s = document.querySelector('.chat-scroll'), top = s.getBoundingClientRect().top; const el = Array.from(s.children).find((c) => c.getBoundingClientRect().bottom > top); return (el.textContent.match(/MARK \\d/) || [el.className])[0]; })()`;
    await page.eval(`(() => { const s = document.querySelector('.chat-scroll'); const el = Array.from(s.querySelectorAll('.msg-user')).find((m) => m.textContent.includes('MARK 1')); s.scrollTop += el.getBoundingClientRect().top - s.getBoundingClientRect().top; })()`);
    const before = await page.eval(visible);
    if (before !== 'MARK 1') throw new Error(`the test did not scroll to MARK 1 (${before})`);
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await page.sleep(200);
    const expanded = await page.eval(visible);
    await page.click('.drawer-expand');
    await page.waitFor(`!document.querySelector('.drawer.is-expanded')`);
    await page.sleep(200);
    const collapsed = await page.eval(visible);
    if (expanded !== 'MARK 1' || collapsed !== 'MARK 1') throw new Error(`reading ${before}, then ${expanded} expanded and ${collapsed} collapsed`);
    await closeDrawer();
  });

  await step('Import History lists sessions with native and AO IDs', async () => {
    await page.click('#btn-import');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length > 0`, 30000, 'the history');
    const ui = await page.eval(`(() => {
      const input = document.querySelector(${q(SEARCH)}), ai = document.querySelector('.import-ai');
      return { placeholder: input.placeholder, focused: document.activeElement === input, ai: ai.textContent.trim(), aiNext: ai.previousElementSibling.contains(input),
        pills: Array.from(document.querySelectorAll('.import-harness button')).map((b) => b.textContent), clearHidden: document.querySelector('.import-clear').hidden,
        hint: document.querySelector('.import-hint').textContent, top: input.getBoundingClientRect().top < document.querySelector('.import-list').getBoundingClientRect().top };
    })()`);
    if (ui.placeholder !== 'Search by session ID, title, topic, or past prompt...') throw new Error(`placeholder reads ${ui.placeholder}`);
    if (!ui.focused || !ui.top || !ui.clearHidden) throw new Error(`search box: focused ${ui.focused}, above the list ${ui.top}, clear hidden ${ui.clearHidden}`);
    if (ui.ai !== 'AI Search' || !ui.aiNext) throw new Error(`AI button reads "${ui.ai}", next to the input ${ui.aiNext}`);
    if (ui.pills.join(',') !== 'All,Copilot,Codex') throw new Error(`harness pills: ${ui.pills}`);
    if (!ui.hint.includes('Press AI Search for fuzzy semantic lookup')) throw new Error(`hint reads ${ui.hint}`);

    // "Hide Imported" is on: the session imported as demo-1 is not listed,
    // nor are the two AI helper runs.
    let list = await rows();
    if (list.length !== REAL - 1) throw new Error(`${list.length} rows, want ${REAL - 1}: ${list.map((r) => r.title)}`);
    if (list.some((r) => /Renamed by hand|maintainer-board-summary|Reply in 1 concise/.test(r.title))) throw new Error(`an AI helper run is listed: ${list.map((r) => r.title)}`);
    await clickText('.import-toolbar .toggle', 'Hide Imported');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === ${REAL}`);
    list = await rows();
    const csi = list.find((r) => r.title === 'Investigate why preemption regresses');
    if (!csi || !csi.logo || csi.agent !== 'Codex' || csi.action !== 'Import') throw new Error(`codex row: ${JSON.stringify(csi)}`);
    if (csi.chips.length !== 1 || csi.chips[0].text !== '01a1c02a' || !csi.chips[0].title.includes(CSI_ID)) throw new Error(`native ID chip: ${JSON.stringify(csi.chips)}`);
    if (!csi.preview.startsWith('Investigate why preemption regresses for CSI volume limits')) throw new Error(`preview reads ${csi.preview}`);
    const events = list.find((r) => r.title.includes('podgroup event refactor'));
    if (!events.imported || events.action !== 'Open Card') throw new Error(`imported row: ${JSON.stringify(events)}`);
    if (events.chips.map((c) => c.text).join(',') !== '01a1c998,demo-1' || !events.chips[1].ao || !events.chips[1].title.includes('Imported into Agent Orchestrator as demo-1')) throw new Error(`chips of the imported row: ${JSON.stringify(events.chips)}`);
    if (withCopilot) {
      const board = list.find((r) => r.title === 'Tool for PR Review Tracking');
      if (!board || board.agent !== 'Copilot' || board.chips[0].text !== '8dd57e72' || !board.preview.includes('zero-emoji Kanban GUI')) throw new Error(`copilot row: ${JSON.stringify(board)}`);
    }
    const design = list.find((r) => r.title === 'Kubeflow Trainer integration design');
    if (!design || design.chips.length !== 0) throw new Error(`AO-only row: ${JSON.stringify(design)}`);
    await page.screenshot(`${out}/03-import-history.png`);
    return `${list.length} sessions`;
  });

  await step('searching by session ID, content and with typos explains each match', async () => {
    let list = await searchFor('01a1c02a', 1);
    if (list[0].title !== 'Investigate why preemption regresses' || list[0].reason !== `Matched ID: ${CSI_ID}`) throw new Error(`native ID prefix: ${JSON.stringify(list[0])}`);
    if (await page.eval(`document.querySelector('.import-clear').hidden`)) throw new Error('no Clear button while a query is typed');
    await page.screenshot(`${out}/04-import-search-id.png`);
    // The AO session ID of an imported session; search ignores Hide Imported.
    list = await searchFor('demo-1', 1);
    if (!list[0].imported || list[0].reason !== 'Matched ID: demo-1') throw new Error(`AO session ID: ${JSON.stringify(list[0])}`);
    // Words that are only in the conversation, not in any title.
    list = await searchFor('retries rollback', 1);
    if (!list[0].title.includes('podgroup event refactor') || !list[0].reason.startsWith('Matched conversation: "') || !list[0].reason.includes('retries and rollback')) throw new Error(`content: ${JSON.stringify(list[0])}`);
    list = await searchFor('kind cluster pending', 1);
    if (list[0].title !== 'Investigate why preemption regresses' || !list[0].reason.includes('Reproduced on a kind cluster')) throw new Error(`agent reply: ${JSON.stringify(list[0])}`);
    await page.screenshot(`${out}/05-import-search-content.png`);
    // A typo still finds the session and says the match is approximate.
    list = await searchFor('premption', 1);
    if (list[0].title !== 'Investigate why preemption regresses' || !list[0].reason.startsWith('Close match. ')) throw new Error(`typo: ${JSON.stringify(list[0])}`);
    if (withCopilot) {
      list = await searchFor('agent-orchestrator kanban', 1);
      if (list[0].title !== 'Tool for PR Review Tracking') throw new Error(`copilot content: ${JSON.stringify(list[0])}`);
      if ((await searchFor('Describe what a card is about', 0)).length !== 0) throw new Error('the AI helper run was found');
    }
    // The harness pills narrow search results too.
    list = await searchFor('root/github', 2);
    if (!list.every((r) => r.reason === 'Matched folder: /root/github/volcano')) throw new Error(`folder: ${JSON.stringify(list.map((r) => r.reason))}`);
    await clickText('.import-harness button', 'Copilot');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === 0`);
    await clickText('.import-harness button', 'Codex');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === 2`);
    await clickText('.import-harness button', 'All');
    // Nothing found points to AI Search; Clear brings the list back.
    await searchFor('zzzzqqqq', 0);
    if (!(await text('.import-state')).includes('Press AI Search for fuzzy semantic lookup')) throw new Error(`empty state reads ${await text('.import-state')}`);
    await page.click('.import-clear');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === ${REAL} && document.querySelector(${q(SEARCH)}).value === ''`);
  });

  await step('AI Search shows progress, then ranked matches with reasons and Reset', async () => {
    const before = (aiLog() || []).length;
    await fill(SEARCH, 'the session about the podgroup event refactoring');
    await page.click('.import-ai');
    await page.waitFor(`document.querySelector('.import-banner.is-running .status-strip')`, 4000, 'the progress strip');
    const busy = await page.eval(`(() => { const b = document.querySelector('.import-ai'); return {
      label: b.textContent.trim(), disabled: b.disabled, spinner: Boolean(b.querySelector('.spinner')), busy: b.getAttribute('aria-busy'),
      strip: document.querySelector('.import-banner .status-label').textContent, bar: Boolean(document.querySelector('.import-banner .progress-strip')),
      spin: getComputedStyle(document.querySelector('.import-banner .spinner')).animationName, stale: document.querySelector('.import-list').classList.contains('is-stale') }; })()`);
    if (busy.label !== 'Searching with AI...' || !busy.disabled || !busy.spinner || busy.busy !== 'true') throw new Error(`AI button while searching: ${JSON.stringify(busy)}`);
    if (busy.strip !== 'Searching history with AI...' || !busy.bar || busy.spin !== 'spin' || !busy.stale) throw new Error(`progress strip: ${JSON.stringify(busy)}`);
    await page.screenshot(`${out}/06-import-ai-searching.png`);
    await page.waitFor(`document.querySelector('.import-banner.is-done, .import-banner.is-error')`, 60000, 'the AI answer');
    const banner = await page.eval(`({ cls: document.querySelector('.import-banner').className, text: document.querySelector('.import-banner .banner-text').textContent, reset: document.querySelector('.import-banner [data-action="reset-ai"]').textContent })`);
    if (!banner.cls.includes('is-done')) throw new Error(banner.text);
    if (banner.text !== 'AI Search: 1 match for "the session about the podgroup event refactoring"' || banner.reset !== 'Reset') throw new Error(`banner reads ${banner.text} / ${banner.reset}`);
    const list = await rows();
    if (list.length !== 1 || !list[0].title.includes('podgroup event refactor') || !list[0].ai || !list[0].reason.startsWith('AI match: ')) throw new Error(`AI rows: ${JSON.stringify(list)}`);
    if ((await text('.import-ai')) !== 'AI Search' || (await page.eval(`document.querySelector('.import-ai').disabled`))) throw new Error('the AI button did not return to normal');
    await page.screenshot(`${out}/07-import-ai-results.png`);
    const log = aiLog();
    let note = 'AI log not checked (FAKE_AI_LOG is not set)';
    if (log) {
      const call = log[log.length - 1];
      if (log.length !== before + 1) throw new Error(`${log.length - before} CLI calls for one search`);
      if (call.flags.join(' ') !== '--model gemini-3.8-flash --reasoning-effort low') throw new Error(`CLI flags: ${call.flags.join(' ')}`);
      if (!call.firstLine.startsWith('[maintainer-board-search]')) throw new Error(`prompt starts with ${call.firstLine}`);
      note = call.flags.join(' ');
    }
    await page.click('.import-banner [data-action="reset-ai"]');
    await page.waitFor(`document.querySelector('.import-banner').hidden && !document.querySelector('.import-reason.is-ai')`);
    await page.click('.import-clear');
    await page.waitFor(`document.querySelectorAll('.import-row:not(.is-head)').length === ${REAL}`);
    return note;
  });

  let importedCard = null;
  await step('Import on a row imports that session and opens its card', async () => {
    await searchFor('01a1c02a', 1);
    await page.click('.import-row:not(.is-head) .import-one');
    await page.waitFor(`!document.querySelector('.modal') && document.querySelector('.drawer .title-input') && document.querySelector('.drawer .title-input').value === 'Investigate why preemption regresses'`, 20000, 'the card of the imported session');
    const st = await fakeState();
    if (JSON.stringify(st.imported) !== JSON.stringify([[candidateId('codex', CSI_ID)]])) throw new Error(`AO was asked to import ${JSON.stringify(st.imported)}`);
    importedCard = (await apiGet('/api/cards')).cards.find((c) => c.title === 'Investigate why preemption regresses');
    if (!importedCard || !importedCard.aoSessionId) throw new Error('the imported session has no card');
    await closeDrawer();
    return importedCard.aoSessionId;
  });

  await step('Summarize with AI shows a spinner, a status row and the elapsed time', async () => {
    const before = (aiLog() || []).length;
    await openCard(running);
    await page.click('.drawer [data-action="summarize"]');
    await page.waitFor(`document.querySelector('.summary-progress .status-strip')`, 4000, 'the status row');
    const busy = await page.eval(`(() => { const b = document.querySelector('.drawer [data-action="summarize"]'); return {
      label: b.querySelector('.busy-label').textContent, disabled: b.disabled, spinner: Boolean(b.querySelector('.spinner')),
      strip: document.querySelector('.summary-progress .status-label').textContent, ai: document.querySelector('.summary-progress .status-strip').classList.contains('is-ai'),
      shimmer: getComputedStyle(document.querySelector('.summary-progress .status-strip')).animationName }; })()`);
    if (busy.label !== 'Summarizing...' || !busy.disabled || !busy.spinner) throw new Error(`button while summarizing: ${JSON.stringify(busy)}`);
    if (busy.strip !== 'Generating summary with AI...' || !busy.ai || busy.shimmer !== 'shimmer') throw new Error(`status row: ${JSON.stringify(busy)}`);
    // The counter appears once the run takes longer than two seconds.
    let elapsed = '';
    try {
      await page.waitFor(`/^\\d+s$/.test(document.querySelector('.summary-progress .elapsed') ? document.querySelector('.summary-progress .elapsed').textContent : '')`, 4000, 'the elapsed counter');
      elapsed = await text('.summary-progress .elapsed');
      await page.screenshot(`${out}/08-summarizing.png`);
    } catch (err) {
      if (await page.eval(`Boolean(document.querySelector('.summary-progress'))`)) throw err;
    }
    await page.waitFor(`!document.querySelector('.summary-progress') && document.querySelector('.drawer .summary-text') && document.querySelector('.drawer .summary-text').textContent.includes(${q(running.title)})`, 60000, 'the new summary');
    if ((await text('.drawer [data-action="summarize"] .btn-label')) !== 'Summarize with AI') throw new Error('the button did not return to normal');
    const log = aiLog();
    if (log) {
      const call = log[log.length - 1];
      if (log.length !== before + 1 || call.flags.join(' ') !== '--model gemini-3.8-flash --reasoning-effort low' || !call.firstLine.startsWith('[maintainer-board-summary]')) throw new Error(`CLI call: ${JSON.stringify(log.slice(before))}`);
    }
    return elapsed ? `counter reached ${elapsed}` : 'finished before the counter showed';
  });

  await step('Send shows the agent at work until its reply arrives', async () => {
    await fill('.composer textarea', 'please look at the scheduler cache');
    await page.click('.composer-actions .btn-primary');
    await page.waitFor(`document.querySelector('.chat-scroll .msg-thinking')`, 3000, 'the thinking row');
    // First the message is on its way, then the agent works on it.
    await page.waitFor(`document.querySelector('.msg-thinking .thinking-label').textContent === 'Codex is thinking...'`, 8000, 'the working state');
    const info = await page.eval(`(() => { const row = document.querySelector('.msg-thinking'), s = document.querySelector('.chat-scroll'); return {
      dots: row.querySelectorAll('.thinking-dots i').length, wave: getComputedStyle(row.querySelector('.thinking-dots i')).animationName, role: row.querySelector('.msg-role').textContent,
      logo: Boolean(row.querySelector('.brand-codex svg')), stop: row.querySelector('.thinking-stop') ? row.querySelector('.thinking-stop').textContent.trim() : null,
      last: s.lastElementChild === row, visible: row.getBoundingClientRect().bottom <= s.getBoundingClientRect().bottom + 2,
      composerStop: !document.querySelector('.composer-actions .btn-danger').hidden, hint: document.querySelector('.composer-hint').textContent }; })()`);
    if (info.dots !== 3 || info.wave !== 'dot-wave' || info.role !== 'Codex' || !info.logo) throw new Error(`thinking row: ${JSON.stringify(info)}`);
    if (info.stop !== 'Stop' || !info.composerStop) throw new Error(`Stop is not offered next to the indicator: ${JSON.stringify(info)}`);
    if (!info.last || !info.visible) throw new Error(`the row is not at the visible end of the conversation: ${JSON.stringify(info)}`);
    await page.screenshot(`${out}/09-agent-thinking.png`);
    await page.waitFor(`!document.querySelector('.msg-thinking') && Array.from(document.querySelectorAll('.chat-scroll .msg-assistant .msg-body')).some((m) => m.textContent.includes('Fake reply to: please look at the scheduler cache'))`, 30000, 'the reply');
    if ((await text('.composer-actions .btn-primary')) !== 'Send') throw new Error('the Send button did not return to normal');
    const sent = (await fakeState()).sent;
    if (sent[sent.length - 1].text !== 'please look at the scheduler cache') throw new Error(`AO received ${JSON.stringify(sent)}`);
    await closeDrawer();
  });

  await step('Resume shows the session waking up', async () => {
    await openCard(sleeping);
    await page.waitFor(`document.querySelector('.drawer [data-action="resume"]')`);
    await page.click('.drawer [data-action="resume"]');
    await page.waitFor(`document.querySelector('.session-alerts .alert.is-working')`, 3000, 'the waking-up banner');
    const info = await page.eval(`(() => { const box = document.querySelector('.alert.is-working'), b = box.querySelector('[data-action="resume"]'); return {
      title: box.querySelector('.alert-title').textContent, spinner: Boolean(box.querySelector(':scope > .spinner')), bar: Boolean(box.querySelector('.progress-strip')),
      button: b.textContent.trim(), disabled: b.disabled, buttonSpinner: Boolean(b.querySelector('.spinner')), elapsed: box.querySelector('.elapsed').textContent }; })()`);
    if (info.title !== 'Waking up agent session...' || !info.spinner || !info.bar) throw new Error(`banner: ${JSON.stringify(info)}`);
    if (info.button !== 'Resuming session...' || !info.disabled || !info.buttonSpinner || !/^\d+s$/.test(info.elapsed)) throw new Error(`button: ${JSON.stringify(info)}`);
    await page.screenshot(`${out}/10-resuming.png`);
    await page.waitFor(`!document.querySelector('.alert.is-working') && !document.querySelector('.drawer [data-action="resume"]') && !document.querySelector('.composer textarea').disabled`, 20000, 'the resumed session');
  });

  await step('Unlink and Delete AO Session are told apart', async () => {
    const info = await page.eval(`(() => { const u = document.querySelector('.session-unlink'), d = document.querySelector('.session-delete'); return { unlink: u.title, unlinkLabel: u.getAttribute('aria-label'), del: d.title, delLabel: d.getAttribute('aria-label') }; })()`);
    if (info.unlinkLabel !== 'Unlink from card' || !info.unlink.includes('detaches the session from this card only; keeps the session in Agent Orchestrator')) throw new Error(`Unlink reads ${info.unlinkLabel}: ${info.unlink}`);
    if (info.delLabel !== 'Delete AO Session' || !info.del.includes('permanently removes the session from Agent Orchestrator')) throw new Error(`Delete reads ${info.delLabel}: ${info.del}`);
    // In the workspace both carry their labels.
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    await page.sleep(200);
    const labels = await page.eval(`['.session-unlink', '.session-delete'].map((s) => { const l = document.querySelector(s + ' .btn-label'); return getComputedStyle(l).display === 'none' ? '' : l.textContent; })`);
    if (labels.join(',') !== 'Unlink,Delete Session') throw new Error(`labels in the workspace: ${labels}`);
    await page.screenshot(`${out}/11-session-controls.png`);
    await page.click('.drawer-expand');
    await page.waitFor(`!document.querySelector('.drawer.is-expanded')`);

    // Unlink: confirmation says the session stays; AO is not touched.
    await page.click('.session-unlink');
    await page.waitFor(`document.querySelector('.modal')`);
    const dialog = await page.eval(`({ title: document.querySelector('.modal-title').textContent, text: document.querySelector('.modal-body').textContent })`);
    if (dialog.title !== 'Unlink from card' || !dialog.text.includes('keeps running in Agent Orchestrator') || !dialog.text.includes('use Delete AO Session instead')) throw new Error(`unlink dialog: ${JSON.stringify(dialog)}`);
    await clickText('.modal-foot button', 'Unlink from Card');
    await page.waitFor(`!document.querySelector('.modal') && document.querySelector('.drawer .start-session')`, 10000, 'the card without a session');
    let st = await fakeState();
    if (st.killed.includes('demo-2') || st.deleted.some((d) => d.id === 'demo-2') || !st.sessions.some((s) => s.id === 'demo-2')) throw new Error('unlinking touched the AO session');
    // Link it again for the next step.
    await page.eval(`fetch('/api/cards/${sleeping.id}', { method: 'PATCH', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ aoSessionId: 'demo-2' }) })`);
    await page.waitFor(`document.querySelector('.session-delete')`, 10000, 'the session linked again');

    // Delete AO Session: the session leaves AO, the card stays.
    await page.click('.session-delete');
    await page.waitFor(`document.querySelector('.modal')`);
    const del = await page.eval(`({ title: document.querySelector('.modal-title').textContent, text: document.querySelector('.modal-body').textContent })`);
    if (del.title !== 'Delete AO session' || !del.text.includes('Permanently delete AO session demo-2 from Agent Orchestrator') || !del.text.includes('The card stays on the board')) throw new Error(`delete dialog: ${JSON.stringify(del)}`);
    await page.sleep(300);
    await page.screenshot(`${out}/12-delete-session-dialog.png`);
    await clickText('.modal-foot button', 'Delete AO Session');
    await page.waitFor(`document.querySelector('.modal-foot button.is-busy')`, 3000, 'the busy button');
    const busy = await page.eval(`(() => { const b = document.querySelector('.modal-foot button.is-busy'); return { label: b.textContent.trim(), spinner: Boolean(b.querySelector('.spinner')), others: Array.from(document.querySelectorAll('.modal-foot button')).filter((x) => x !== b).every((x) => x.disabled) }; })()`);
    if (busy.label !== 'Deleting...' || !busy.spinner || !busy.others) throw new Error(`while deleting: ${JSON.stringify(busy)}`);
    await page.waitFor(`!document.querySelector('.modal') && document.querySelector('.drawer .start-session')`, 20000, 'the card without its session');
    st = await fakeState();
    if (!st.killed.includes('demo-2') || JSON.stringify(st.deleted) !== JSON.stringify([{ id: 'demo-2', cleanup: 'true' }]) || st.sessions.some((s) => s.id === 'demo-2')) throw new Error(`AO state: killed ${st.killed}, deleted ${JSON.stringify(st.deleted)}`);
    const card = (await apiGet('/api/cards')).cards.find((c) => c.id === sleeping.id);
    if (!card || card.aoSessionId !== '' || card.title !== sleeping.title) throw new Error(`card after the delete: ${JSON.stringify(card)}`);
    const events = (await apiGet(`/api/cards/${sleeping.id}/events?limit=1`)).events.map((e) => e.message);
    if (events[0] !== 'Deleted AO session demo-2 from Agent Orchestrator') throw new Error(`timeline: ${events}`);
  });

  await step('a card without a session asks a plain Delete Card', async () => {
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal')`);
    const buttons = await dialogButtons();
    if (buttons.join(',') !== 'Cancel,Delete Card') throw new Error(`buttons: ${buttons}`);
    await clickText('.modal-foot button', 'Cancel');
    await page.waitFor(`!document.querySelector('.modal') && document.querySelector('.drawer')`);
    await closeDrawer();
  });

  await step('Delete Card Only keeps the AO session and the card does not come back', async () => {
    await openCard(running);
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal')`);
    const dialog = await page.eval(`({ title: document.querySelector('.modal-title').textContent, text: document.querySelector('.modal-body').textContent, terms: Array.from(document.querySelectorAll('.choice-list dt')).map((d) => d.textContent),
      danger: document.querySelector('.modal-foot .btn-danger-solid').textContent.trim(), focused: document.activeElement.textContent.trim() })`);
    const buttons = await dialogButtons();
    if (buttons.join(',') !== 'Cancel,Delete Card Only,Delete Card & AO Session') throw new Error(`buttons: ${buttons}`);
    if (dialog.danger !== 'Delete Card & AO Session' || dialog.focused !== 'Delete Card & AO Session') throw new Error(`primary action: ${dialog.danger}, focused ${dialog.focused}`);
    if (!dialog.text.includes('linked to the Codex session demo-1 in Agent Orchestrator') || dialog.terms.join(',') !== 'Delete Card & AO Session,Delete Card Only') throw new Error(`dialog text: ${dialog.text}`);
    if (!dialog.text.includes('permanently deletes it from Agent Orchestrator') || !dialog.text.includes('The session stays in Agent Orchestrator')) throw new Error(`dialog text: ${dialog.text}`);
    await page.sleep(300);
    await page.screenshot(`${out}/13-delete-card-dialog.png`);
    // Cancel changes nothing.
    await clickText('.modal-foot button', 'Cancel');
    await page.waitFor(`!document.querySelector('.modal') && document.querySelector('.drawer')`);
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal')`);
    await clickText('.modal-foot button', 'Delete Card Only');
    await page.waitFor(`!document.querySelector('.modal') && !document.querySelector('.drawer') && !document.querySelector('.card[data-id="${running.id}"]')`, 10000, 'the card to disappear');
    const st = await fakeState();
    if (st.killed.includes('demo-1') || st.deleted.some((d) => d.id === 'demo-1') || !st.sessions.some((s) => s.id === 'demo-1')) throw new Error('Delete Card Only touched the AO session');
    // Several AO sync cycles later (the board syncs every second here)
    // the session still has no card.
    await page.sleep(3500);
    if (await cardBySession('demo-1')) throw new Error('AO sync created a new card for the session that was kept only in AO');
    const sessions = (await apiGet('/api/sessions')).sessions;
    if (!sessions.some((s) => s.id === 'demo-1' && s.cardId === '')) throw new Error('the session is no longer offered for linking');
  });

  if (importedCard) {
    await step('a refused AO delete is reported and nothing is removed', async () => {
      await fakePost('/__dirty', { id: importedCard.aoSessionId });
      await openCard(importedCard);
      await page.click('.drawer-top button[aria-label="Delete card"]');
      await page.waitFor(`document.querySelector('.modal')`);
      await clickText('.modal-foot button', 'Delete Card & AO Session');
      await page.waitFor(`document.querySelector('.dialog-error') && !document.querySelector('.dialog-error').hidden`, 20000, 'the error in the dialog');
      const message = await text('.dialog-error');
      if (!message.includes(`AO did not delete session ${importedCard.aoSessionId}`) || !message.includes('uncommitted changes') || !message.includes('Nothing was removed from the board.')) throw new Error(`error reads ${message}`);
      if (await page.eval(`Array.from(document.querySelectorAll('.modal-foot button')).some((b) => b.disabled)`)) throw new Error('the dialog stayed locked after the failure');
      await page.screenshot(`${out}/14-delete-refused.png`);
      await clickText('.modal-foot button', 'Cancel');
      await page.waitFor(`!document.querySelector('.modal')`);
      const card = await cardBySession(importedCard.aoSessionId);
      if (!card || card.id !== importedCard.id) throw new Error('the card was removed although AO kept the session');
      await closeDrawer();
    });
  }

  await step('Delete Card & AO Session removes both', async () => {
    // A fresh session: spawn one through the board, then delete it.
    const created = await page.eval(`fetch('/api/cards', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: 'Throwaway task', agent: { harness: 'copilot' } }) }).then((r) => r.json())`);
    const card = created.card;
    if (!card.aoSessionId) throw new Error(`no session was started: ${JSON.stringify(created.warnings)}`);
    await page.waitFor(`document.querySelector('.card[data-id="${card.id}"]')`);
    await openCard(card);
    await page.click('.drawer-top button[aria-label="Delete card"]');
    await page.waitFor(`document.querySelector('.modal')`);
    await clickText('.modal-foot button', 'Delete Card & AO Session');
    await page.waitFor(`!document.querySelector('.modal') && !document.querySelector('.drawer') && !document.querySelector('.card[data-id="${card.id}"]')`, 20000, 'the card to disappear');
    const toast = await page.eval(`Array.from(document.querySelectorAll('.toast-success .toast-body')).map((t) => t.textContent).pop()`);
    if (toast !== `Card and AO session ${card.aoSessionId} deleted.`) throw new Error(`toast reads ${toast}`);
    const st = await fakeState();
    if (!st.killed.includes(card.aoSessionId) || !st.deleted.some((d) => d.id === card.aoSessionId && d.cleanup === 'true') || st.sessions.some((s) => s.id === card.aoSessionId)) throw new Error(`AO state: killed ${st.killed}, deleted ${JSON.stringify(st.deleted)}`);
    await page.sleep(2500);
    if (await cardBySession(card.aoSessionId)) throw new Error('the card of a deleted session came back');
    return card.aoSessionId;
  });
} finally {
  await page.close();
}

// The refused delete above is answered with HTTP 409 on purpose, and the
// browser logs every failed request; anything else is a real problem.
const problems = page.errors.filter((e) => !(e.includes('status of 409') && e.includes('deleteAoSession')));
if (problems.length) {
  failed = true;
  console.log('Browser console problems:');
  for (const e of problems) console.log(`  ${e}`);
} else {
  console.log('ok    no console errors or exceptions');
}
console.log(`Screenshots: ${out}`);
process.exit(failed ? 1 : 0);
