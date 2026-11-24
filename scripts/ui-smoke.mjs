// Optional browser-level smoke test. Usage:
//   node scripts/ui-smoke.mjs http://127.0.0.1:3012 [output-dir]
// Loads the board in headless Chromium, exercises the main views in both
// themes, saves screenshots and fails on any console error or exception.

import { launch } from './cdp.mjs';
import { mkdirSync } from 'node:fs';
import { resolve } from 'node:path';

const base = process.argv[2] || 'http://127.0.0.1:3012';
const out = resolve(process.argv[3] || 'data/ui-smoke');
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
  }
};

try {
  await page.goto(base);
  await step('board renders columns', () => page.waitFor(`document.querySelectorAll('.column').length > 0`));
  await step('AO status pill resolves', () => page.waitFor(`!document.querySelector('#ao-status .pill-label').textContent.includes('Connecting')`));
  await page.sleep(400);
  await page.screenshot(`${out}/01-board-dark.png`);

  const hasCard = await page.eval(`document.querySelectorAll('.card').length > 0`);
  if (hasCard) {
    await step('card opens the drawer', async () => {
      await page.click('.card');
      await page.waitFor(`document.querySelector('.drawer')`);
      await page.sleep(1500);
    });
    await page.screenshot(`${out}/02-drawer-dark.png`);
    await step('timeline loads', async () => {
      await page.eval(`document.querySelector('.drawer details.section').open = true`);
      await page.waitFor(`document.querySelectorAll('.drawer .timeline li').length > 0`);
    });
    await step('Escape closes the drawer', async () => {
      await page.eval(`document.activeElement && document.activeElement.blur()`);
      await page.cmd('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
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
    await page.click('.card');
    await page.waitFor(`document.querySelector('.drawer')`);
    await page.sleep(1500);
    await page.screenshot(`${out}/04-drawer-light.png`);
    await page.click('.drawer-top button[aria-label="Close"]');
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
  });
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
      for (const el of document.querySelectorAll('header, .filterbar, .column-header, .modal, .toast')) {
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
