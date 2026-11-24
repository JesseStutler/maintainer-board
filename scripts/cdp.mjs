// Minimal Chrome DevTools Protocol driver over --remote-debugging-pipe.
// Used by the optional UI smoke test; it needs a local Chromium and nothing else.

import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

export async function launch({ executable = process.env.CHROMIUM || 'chromium-browser', width = 1600, height = 950, userDataDir } = {}) {
  const args = [
    '--headless=new',
    '--no-sandbox',
    '--disable-gpu',
    '--hide-scrollbars',
    '--remote-debugging-pipe',
    '--no-first-run',
    '--no-default-browser-check',
    `--window-size=${width},${height}`,
    `--user-data-dir=${userDataDir}`,
    'about:blank',
  ];
  const child = spawn(executable, args, { stdio: ['ignore', 'ignore', 'pipe', 'pipe', 'pipe'] });
  const writer = child.stdio[3];
  const reader = child.stdio[4];
  let nextId = 1;
  const pending = new Map();
  const listeners = new Set();
  let buffer = Buffer.alloc(0);
  reader.on('data', (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    let index;
    while ((index = buffer.indexOf(0)) !== -1) {
      const raw = buffer.subarray(0, index).toString('utf8');
      buffer = buffer.subarray(index + 1);
      const msg = JSON.parse(raw);
      if (msg.id && pending.has(msg.id)) {
        const { resolve, reject } = pending.get(msg.id);
        pending.delete(msg.id);
        if (msg.error) reject(new Error(`${msg.error.message} ${msg.error.data || ''}`));
        else resolve(msg.result);
      } else {
        for (const fn of listeners) fn(msg);
      }
    }
  });
  const send = (method, params = {}, sessionId) =>
    new Promise((resolve, reject) => {
      const id = nextId++;
      pending.set(id, { resolve, reject });
      writer.write(`${JSON.stringify({ id, method, params, sessionId })}\0`);
    });

  const { targetInfos } = await send('Target.getTargets');
  const page = targetInfos.find((t) => t.type === 'page');
  const { sessionId } = await send('Target.attachToTarget', { targetId: page.targetId, flatten: true });
  const errors = [];
  listeners.add((msg) => {
    if (msg.sessionId !== sessionId) return;
    if (msg.method === 'Runtime.exceptionThrown') {
      const d = msg.params.exceptionDetails;
      errors.push(`exception: ${d.exception && d.exception.description ? d.exception.description : d.text}`);
    } else if (msg.method === 'Runtime.consoleAPICalled' && (msg.params.type === 'error' || msg.params.type === 'warning')) {
      errors.push(`console.${msg.params.type}: ${msg.params.args.map((a) => a.value ?? a.description ?? '').join(' ')}`);
    } else if (msg.method === 'Log.entryAdded' && (msg.params.entry.level === 'error' || msg.params.entry.level === 'warning')) {
      errors.push(`log.${msg.params.entry.level}: ${msg.params.entry.text} ${msg.params.entry.url || ''}`);
    }
  });
  const cmd = (method, params) => send(method, params, sessionId);
  await cmd('Page.enable');
  await cmd('Runtime.enable');
  await cmd('Log.enable');
  await cmd('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });

  const api = {
    errors,
    cmd,
    /** Subscribes to raw protocol events of the page session. */
    onEvent(fn) {
      listeners.add((msg) => {
        if (msg.sessionId === sessionId) fn(msg);
      });
    },
    async goto(url) {
      await cmd('Page.navigate', { url });
    },
    async eval(expression) {
      const res = await cmd('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true, userGesture: true });
      if (res.exceptionDetails) {
        throw new Error(`eval failed: ${res.exceptionDetails.exception ? res.exceptionDetails.exception.description : res.exceptionDetails.text}`);
      }
      return res.result.value;
    },
    async waitFor(expression, timeout = 15000, label = expression) {
      const start = Date.now();
      for (;;) {
        if (await api.eval(`Boolean(${expression})`)) return;
        if (Date.now() - start > timeout) throw new Error(`timed out waiting for ${label}`);
        await new Promise((r) => setTimeout(r, 100));
      }
    },
    sleep: (ms) => new Promise((r) => setTimeout(r, ms)),
    async click(selector) {
      const ok = await api.eval(`(() => { const el = document.querySelector(${JSON.stringify(selector)}); if (!el) return false; el.click(); return true; })()`);
      if (!ok) throw new Error(`click: no element matches ${selector}`);
    },
    async screenshot(path) {
      const { data } = await cmd('Page.captureScreenshot', { format: 'png' });
      mkdirSync(dirname(path), { recursive: true });
      writeFileSync(path, Buffer.from(data, 'base64'));
    },
    async mouse(type, x, y, extra = {}) {
      await cmd('Input.dispatchMouseEvent', { type, x, y, button: 'left', clickCount: 1, ...extra });
    },
    async close() {
      try {
        await send('Browser.close');
      } catch (err) {
        child.kill();
      }
    },
  };
  return api;
}
