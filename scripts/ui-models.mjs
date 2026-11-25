// Browser test of model selection. It starts sessions and changes session
// settings, so it runs only against a board that talks to the fake AO
// daemon of scripts/fake-ao.mjs, never against real sessions:
//   node scripts/ui-models.mjs <board-url> <fake-ao-url> [output-dir]

import { launch } from './cdp.mjs';
import { mkdirSync, rmSync } from 'node:fs';
import { resolve } from 'node:path';

const [base, fake] = process.argv.slice(2);
if (!base || !fake) {
  console.error('usage: node scripts/ui-models.mjs <board-url> <fake-ao-url> [output-dir]');
  process.exit(2);
}
const out = resolve(process.argv[4] || 'data/ui-models');
mkdirSync(out, { recursive: true });
rmSync(resolve(out, 'chromium-profile'), { recursive: true, force: true });

const fakeState = async () => (await fetch(`${fake}/__state`)).json();
try {
  const st = await fakeState();
  if (!Array.isArray(st.spawned)) throw new Error('unexpected state');
} catch (err) {
  console.error(`${fake} is not the fake AO daemon (${err.message}); refusing to run against a real one.`);
  process.exit(2);
}

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
  }
};
const q = (s) => JSON.stringify(s);
const apiGet = (path) => page.eval(`fetch(${q(path)}).then((r) => r.json())`);
const options = (selector) => page.eval(`Array.from(document.querySelector(${q(selector)}).options).map((o) => [o.value, o.textContent])`);
const choose = (selector, value) =>
  page.eval(`(() => { const el = document.querySelector(${q(selector)}); if (!Array.from(el.options).some((o) => o.value === ${q(value)})) throw new Error('no option ' + ${q(value)} + ' in ' + ${q(selector)}); el.value = ${q(value)}; el.dispatchEvent(new Event('change', { bubbles: true })); return true; })()`);
const clickText = (selector, text) =>
  page.eval(`(() => { const el = Array.from(document.querySelectorAll(${q(selector)})).find((e) => e.textContent.trim() === ${q(text)}); if (!el) throw new Error('no ' + ${q(selector)} + ' reading ' + ${q(text)}); el.click(); return true; })()`);
const lastToast = () => page.eval(`(() => { const t = document.querySelectorAll('.toast .toast-body'); return t.length ? t[t.length - 1].textContent : ''; })()`);
const cardBySession = async (id) => (await apiGet('/api/cards')).cards.find((c) => c.aoSessionId === id);
const MODEL = '.drawer .composer select[aria-label="Model"]';
const EFFORT = '.drawer .composer select[aria-label="Effort"]';

try {
  await page.goto(base);
  await page.waitFor(`document.querySelectorAll('.card').length >= 2`, 20000, 'cards of the fake sessions');

  await step('new task dialog lists the models of the chosen agent', async () => {
    await page.click('#btn-new');
    await page.waitFor(`document.querySelector('.modal .form-grid')`);
    const hidden = await page.eval(`document.querySelector('.modal select[aria-label="Model"]').closest('.field').hidden`);
    if (!hidden) throw new Error('the model field shows without an agent');
    await clickText('.modal .segmented button', 'Copilot');
    await page.waitFor(`document.querySelector('.modal select[aria-label="Model"]').options.length === 4`);
    const copilot = await options('.modal select[aria-label="Model"]');
    if (copilot[0][0] !== '' || copilot[0][1] !== 'Default (claude-sonnet-5)' || copilot.map((o) => o[0]).join(',') !== ',claude-sonnet-5,gemini-4-neon,gpt-5.3-codex') throw new Error(`Copilot models: ${JSON.stringify(copilot)}`);
    // The default Copilot model has no efforts; one that has them shows the field.
    if (!(await page.eval(`document.querySelector('.modal select[aria-label="Effort"]').closest('.field').hidden`))) throw new Error('effort shows for a model without efforts');
    await choose('.modal select[aria-label="Model"]', 'gpt-5.3-codex');
    await page.waitFor(`!document.querySelector('.modal select[aria-label="Effort"]').closest('.field').hidden`);
    await clickText('.modal .segmented button', 'Codex');
    await page.waitFor(`Array.from(document.querySelector('.modal select[aria-label="Model"]').options).some((o) => o.value === 'gpt-6.2-astra')`);
    const codex = await options('.modal select[aria-label="Model"]');
    if (codex[0][1] !== 'Default (GPT-6.2-Sol)' || codex.length !== 3) throw new Error(`Codex models: ${JSON.stringify(codex)}`);
    // The Copilot choice does not carry over to Codex.
    if ((await page.eval(`document.querySelector('.modal select[aria-label="Model"]').value`)) !== '') throw new Error('a Copilot model stayed selected for Codex');
    const defaults = await options('.modal select[aria-label="Effort"]');
    if (defaults.map((o) => o[1]).join(',') !== 'Default (medium),low,medium,high') throw new Error(`efforts of the default model: ${JSON.stringify(defaults)}`);
    await choose('.modal select[aria-label="Model"]', 'gpt-6.2-astra');
    const astra = await options('.modal select[aria-label="Effort"]');
    if (astra.map((o) => o[1]).join(',') !== 'Default (low),low,high') throw new Error(`efforts of astra: ${JSON.stringify(astra)}`);
    await page.sleep(200);
    await page.screenshot(`${out}/01-new-task-model.png`);
  });

  await step('the chosen model and effort start the session', async () => {
    await choose('.modal select[aria-label="Effort"]', 'high');
    await page.eval(`(() => { const el = document.querySelector('.modal input[placeholder="What needs to be done"]'); el.value = 'Model test task'; el.dispatchEvent(new Event('input', { bubbles: true })); })()`);
    await clickText('.modal-foot button', 'Create Task');
    await page.waitFor(`document.querySelector('.drawer .title-input') && document.querySelector('.drawer .title-input').value === 'Model test task'`);
    const spawned = (await fakeState()).spawned;
    const req = spawned[spawned.length - 1];
    if (req.harness !== 'codex' || req.model !== 'gpt-6.2-astra' || req.effort !== 'high') throw new Error(`spawn request: ${JSON.stringify(req)}`);
    // The new session opens with that model selected and shown in its bar.
    await page.waitFor(`document.querySelector(${q(MODEL)}) && document.querySelector(${q(MODEL)}).value === 'gpt-6.2-astra'`);
    await page.waitFor(`document.querySelector('.session-bar .session-model') && document.querySelector('.session-bar .session-model').textContent === 'GPT-6.2-Astrahigh'`);
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer')`);
    return `${req.model} / ${req.effort}`;
  });

  await step('a running session switches model and effort from the composer', async () => {
    const card = await cardBySession('demo-1');
    await page.click(`.card[data-id="${card.id}"]`);
    await page.waitFor(`document.querySelector(${q(MODEL)}) && !document.querySelector(${q(MODEL)}).disabled && document.querySelector(${q(MODEL)}).value === 'gpt-6.2-sol'`);
    if ((await page.eval(`document.querySelector(${q(EFFORT)}).value`)) !== 'medium') throw new Error('the selected effort is not shown');
    if ((await page.eval(`document.querySelector('.session-bar .session-model').textContent`)) !== 'GPT-6.2-Solmedium') throw new Error('the session bar does not show the model');
    const tip = await page.eval(`document.querySelector(${q(MODEL)} + ' option[value="gpt-6.2-astra"]').title`);
    if (tip !== 'GPT-6.2-Astra description') throw new Error(`option tooltip reads ${tip}`);
    await page.sleep(300);
    await page.screenshot(`${out}/02-session-model.png`);

    await choose(MODEL, 'gpt-6.2-astra');
    await page.waitFor(`document.querySelector('.session-bar .session-model').textContent === 'GPT-6.2-Astra'`);
    if ((await lastToast()) !== 'Model set to GPT-6.2-Astra for the next turn.') throw new Error(`toast reads ${await lastToast()}`);
    // The effort of the old model does not apply to the new one.
    const efforts = await options(EFFORT);
    if (efforts.map((o) => o[1]).join(',') !== 'Default (low),low,high' || (await page.eval(`document.querySelector(${q(EFFORT)}).value`)) !== '') throw new Error(`efforts after the switch: ${JSON.stringify(efforts)}`);
    await choose(EFFORT, 'high');
    await page.waitFor(`document.querySelector('.session-bar .session-model').textContent === 'GPT-6.2-Astrahigh'`);
    if ((await lastToast()) !== 'Model set to GPT-6.2-Astra, effort high for the next turn.') throw new Error(`toast reads ${await lastToast()}`);
    const sent = (await fakeState()).settings.filter((s) => s.id === 'demo-1');
    if (JSON.stringify(sent) !== JSON.stringify([{ id: 'demo-1', model: 'gpt-6.2-astra', reasoningEffort: '' }, { id: 'demo-1', reasoningEffort: 'high' }])) throw new Error(`AO received ${JSON.stringify(sent)}`);
    if ((await fakeState()).settings.some((s) => 'approvalMode' in s)) throw new Error('the approval mode was sent');
    const events = (await apiGet(`/api/cards/${card.id}/events?limit=3`)).events.map((e) => e.message);
    if (events[0] !== 'Agent model set to gpt-6.2-astra (effort high)') throw new Error(`timeline: ${JSON.stringify(events)}`);
  });

  await step('the picker works in the expanded workspace and resets to the default', async () => {
    await page.click('.drawer-expand');
    await page.waitFor(`document.querySelector('.drawer.is-expanded')`);
    if (!(await page.eval(`Boolean(document.querySelector('.drawer.is-expanded .composer select[aria-label="Model"]'))`))) throw new Error('no model picker in the workspace');
    await choose(MODEL, '');
    await page.waitFor(`!document.querySelector('.session-bar .session-model')`);
    if ((await lastToast()) !== 'Model set to the default model for the next turn.') throw new Error(`toast reads ${await lastToast()}`);
    const efforts = await options(EFFORT);
    if (efforts[0][1] !== 'Default (medium)') throw new Error(`efforts of the default model: ${JSON.stringify(efforts)}`);
    await page.sleep(300);
    await page.screenshot(`${out}/03-workspace-model.png`);
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer')`);
  });

  await step('a session that was not resumed still offers its agent models', async () => {
    const card = await cardBySession('demo-2');
    const api = await apiGet(`/api/cards/${card.id}/models`);
    if (api.source !== 'agent' || !api.needsResume || api.models.length !== 3) throw new Error(`models API: ${JSON.stringify(api)}`);
    await page.click(`.card[data-id="${card.id}"]`);
    await page.waitFor(`document.querySelector(${q(MODEL)}) && !document.querySelector(${q(MODEL)}).disabled && document.querySelector(${q(MODEL)}).options.length === 4`);
    if (!(await page.eval(`document.querySelector(${q(EFFORT)}).hidden`))) throw new Error('effort shows for a model without efforts');
    await choose(MODEL, 'gemini-4-neon');
    await page.waitFor(`document.querySelector('.session-bar .session-model') && document.querySelector('.session-bar .session-model').textContent === 'gemini-4-neon'`);
    if ((await lastToast()) !== 'Model set to gemini-4-neon. It applies once you resume the session.') throw new Error(`toast reads ${await lastToast()}`);
    await page.sleep(300);
    await page.screenshot(`${out}/04-sleeping-session-model.png`);
    if ((await fakeState()).selected['demo-2'].model !== 'gemini-4-neon') throw new Error('AO did not store the model');
  });

  await step('a refused change is reported and rolled back', async () => {
    // Offer a model the server does not know, as a stale page would.
    await page.eval(`(() => { const el = document.querySelector(${q(MODEL)}); el.add(new Option('Retired model', 'retired-model')); })()`);
    const before = (await fakeState()).settings.length;
    await choose(MODEL, 'retired-model');
    await page.waitFor(`Array.from(document.querySelectorAll('.toast-error .toast-body')).some((t) => t.textContent.startsWith('The model was not changed: Copilot has no model named'))`);
    await page.waitFor(`document.querySelector(${q(MODEL)}).value === 'gemini-4-neon' && !document.querySelector(${q(MODEL)}).disabled`);
    if ((await fakeState()).settings.length !== before) throw new Error('the unknown model reached AO');
  });

  await step('the start form of a card offers models too', async () => {
    await page.click('.drawer-top button[aria-label="Close"]');
    await page.waitFor(`!document.querySelector('.drawer')`);
    await page.eval(`fetch('/api/cards', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: 'Start later' }) })`);
    await page.waitFor(`Array.from(document.querySelectorAll('.card-title')).some((t) => t.textContent === 'Start later')`);
    await page.eval(`Array.from(document.querySelectorAll('.card')).find((c) => c.querySelector('.card-title').textContent === 'Start later').click()`);
    await page.waitFor(`document.querySelector('.start-session select[aria-label="Model"]') && document.querySelector('.start-session select[aria-label="Model"]').options.length === 4`);
    await clickText('.start-session .segmented button', 'Codex');
    await page.waitFor(`Array.from(document.querySelector('.start-session select[aria-label="Model"]').options).some((o) => o.value === 'gpt-6.2-sol')`);
    await choose('.start-session select[aria-label="Model"]', 'gpt-6.2-sol');
    await choose('.start-session select[aria-label="Effort"]', 'low');
    await clickText('.start-session button', 'Start Agent Session');
    await page.waitFor(`document.querySelector(${q(MODEL)}) && document.querySelector(${q(MODEL)}).value === 'gpt-6.2-sol'`, 20000, 'the new session with its model');
    const spawned = (await fakeState()).spawned;
    const req = spawned[spawned.length - 1];
    if (req.harness !== 'codex' || req.model !== 'gpt-6.2-sol' || req.effort !== 'low') throw new Error(`spawn request: ${JSON.stringify(req)}`);
  });
} finally {
  await page.close();
}

// The refused change above is answered with HTTP 400 on purpose, and the
// browser logs every failed request; anything else is a real problem.
const problems = page.errors.filter((e) => !(e.includes('status of 400') && e.includes('/settings')));
if (problems.length) {
  failed = true;
  console.log('Browser console problems:');
  for (const e of problems) console.log(`  ${e}`);
} else {
  console.log('ok    no console errors or exceptions');
}
console.log(`Screenshots: ${out}`);
process.exit(failed ? 1 : 0);
