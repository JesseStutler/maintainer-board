// A minimal in-memory stand-in for the AO daemon, for browser tests that
// change session settings or start sessions and must not touch real ones:
//   node scripts/fake-ao.mjs 3915
//   ./maintainer-board --addr 127.0.0.1:3916 --db data/scratch/models/board.db --ao-url http://127.0.0.1:3915
//   node scripts/ui-models.mjs http://127.0.0.1:3916 http://127.0.0.1:3915
// It implements only what the board calls. GET /__state returns everything
// the board sent (spawn requests, settings changes, kills, deletes, imports)
// for assertions. Tests shape it through three more endpoints:
//   POST /__candidates {candidates: [...]}  the import history AO reports
//   POST /__dirty {id}                      make DELETE refuse (dirty worktree)
// FAKE_AO_DELAY_MS (default 0) delays resume and delete, so that the
// progress indicators of the board can be observed.

import { createServer } from 'node:http';

const port = Number(process.argv[2]) || 3915;
const now = () => new Date().toISOString();
const efforts = ['low', 'medium', 'high'];
const delay = Number(process.env.FAKE_AO_DELAY_MS) || 0;
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const catalogs = {
  codex: [
    { id: 'gpt-6.2-sol', label: 'GPT-6.2-Sol', isDefault: true, efforts, defaultEffort: 'medium' },
    { id: 'gpt-6.2-astra', label: 'GPT-6.2-Astra', efforts: ['low', 'high'], defaultEffort: 'low' },
  ],
  copilot: [
    { id: 'claude-sonnet-5', label: 'claude-sonnet-5', isDefault: true },
    { id: 'gemini-4-neon', label: 'gemini-4-neon' },
    { id: 'gpt-5.3-codex', label: 'gpt-5.3-codex', efforts, defaultEffort: 'medium' },
  ],
};

const session = (id, harness, displayName, extra = {}) => ({
  id, projectId: 'demo', kind: 'worker', harness, displayName, mode: 'chat', status: 'idle',
  activity: { state: 'idle', lastActivityAt: now() }, isTerminated: false, needsResume: false,
  branch: `ao/${id}`, createdAt: now(), updatedAt: now(), prs: [], ...extra,
});

const state = {
  sessions: [
    session('demo-1', 'codex', 'Running Codex session'),
    session('demo-2', 'copilot', 'Imported Copilot session', { needsResume: true }),
  ],
  // Per-session picker data; a session that is not running has no models.
  selected: { 'demo-1': { model: 'gpt-6.2-sol', reasoningEffort: 'medium' }, 'demo-2': {} },
  spawned: [],
  settings: [],
  // Session lifecycle calls, in order.
  killed: [],
  deleted: [],
  dirty: [],
  // Import history and the candidate IDs the board asked to import.
  candidates: [],
  imported: [],
  // Messages the board sent, per session.
  sent: [],
};
let nextNumber = state.sessions.length + 1;

const conversations = new Map();
const messages = (id) => {
  if (!conversations.has(id)) {
    conversations.set(id, [
      { kind: 'message', id: `${id}-m1`, sequence: 1, revision: 0, role: 'user', origin: 'human', text: 'Hello from the fake daemon.', streaming: false, createdAt: now() },
      { kind: 'message', id: `${id}-m2`, sequence: 2, revision: 0, role: 'assistant', origin: 'provider', text: 'This conversation exists only for the model picker test.', streaming: false, createdAt: now() },
    ]);
  }
  return conversations.get(id);
};

function sessionModels(id) {
  const sess = state.sessions.find((s) => s.id === id);
  const models = sess.needsResume ? [] : catalogs[sess.harness].map((m) => ({ id: m.id, displayName: m.label, description: `${m.label} description`, default: Boolean(m.isDefault), efforts: m.efforts || [], defaultEffort: m.defaultEffort || '' }));
  return { models, selected: state.selected[id] || {} };
}

const readBody = (req) =>
  new Promise((resolve) => {
    let raw = '';
    req.on('data', (chunk) => (raw += chunk));
    req.on('end', () => {
      try {
        resolve(raw ? JSON.parse(raw) : {});
      } catch (err) {
        resolve({});
      }
    });
  });

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${port}`);
  const path = url.pathname;
  const send = (status, body) => {
    res.writeHead(status, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body));
  };
  const notFound = (code, message) => send(404, { error: 'not_found', code, message });
  let m;

  if (req.method === 'GET' && path === '/api/v1/events') {
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' });
    res.write(': connected\n\n');
    const timer = setInterval(() => res.write(': keepalive\n\n'), 15000);
    req.on('close', () => clearInterval(timer));
    return;
  }
  if (req.method === 'GET' && path === '/__state') return send(200, state);
  if (req.method === 'GET' && path === '/api/v1/sessions') return send(200, { sessions: state.sessions });
  if (req.method === 'GET' && path === '/api/v1/projects') return send(200, { projects: [{ id: 'demo', name: 'demo', path: '/demo', kind: 'git', sessionPrefix: 'demo' }] });
  if (req.method === 'GET' && path === '/api/v1/session-import') return send(200, { candidates: state.candidates });
  if (req.method === 'POST' && path === '/api/v1/session-import') {
    // Like AO: every candidate becomes a stopped session named after it.
    const body = await readBody(req);
    const ids = body.ids || [];
    state.imported.push(ids);
    const results = ids.map((id) => {
      const candidate = state.candidates.find((c) => c.id === id);
      if (!candidate) return { id, status: 'failed', error: 'unknown history entry' };
      if (candidate.sessionId) return { id, status: 'exists', sessionId: candidate.sessionId };
      const created = session(`demo-${nextNumber++}`, candidate.harness, candidate.title, { needsResume: true });
      state.sessions.push(created);
      state.selected[created.id] = {};
      candidate.sessionId = created.id;
      return { id, status: 'created', sessionId: created.id };
    });
    return send(200, { results });
  }
  if (req.method === 'POST' && path === '/__candidates') {
    state.candidates = (await readBody(req)).candidates || [];
    return send(200, { ok: true, candidates: state.candidates.length });
  }
  if (req.method === 'POST' && path === '/__dirty') {
    state.dirty.push((await readBody(req)).id);
    return send(200, { ok: true });
  }
  if (req.method === 'GET' && (m = path.match(/^\/api\/v1\/agents\/([^/]+)\/models$/))) {
    const agent = decodeURIComponent(m[1]);
    if (!catalogs[agent]) return notFound('AGENT_NOT_FOUND', 'Unknown agent adapter');
    return send(200, { agentId: agent, models: catalogs[agent] });
  }
  if (req.method === 'POST' && path === '/api/v1/sessions') {
    const body = await readBody(req);
    state.spawned.push(body);
    const created = session(`demo-${nextNumber++}`, body.harness, body.displayName || 'New session');
    state.sessions.push(created);
    state.selected[created.id] = { model: body.model || '', reasoningEffort: body.effort || '' };
    return send(201, { session: created });
  }
  if (req.method === 'DELETE' && (m = path.match(/^\/api\/v1\/sessions\/([^/]+)$/))) {
    // Like AO: only a terminated session is deleted, and a dirty worktree
    // is never forced.
    const id = decodeURIComponent(m[1]);
    const sess = state.sessions.find((s) => s.id === id);
    if (!sess) return notFound('SESSION_NOT_FOUND', 'Unknown session');
    if (!sess.isTerminated) return send(409, { error: 'conflict', code: 'SESSION_NOT_TERMINATED', message: 'session is not terminated' });
    if (delay) await sleep(delay);
    if (state.dirty.includes(id)) return send(409, { error: 'conflict', code: 'WORKSPACE_DIRTY', message: 'worktree has uncommitted changes' });
    state.sessions = state.sessions.filter((s) => s.id !== id);
    for (const candidate of state.candidates) if (candidate.sessionId === id) delete candidate.sessionId;
    state.deleted.push({ id, cleanup: url.searchParams.get('cleanup') });
    return send(200, { ok: true, sessionId: id });
  }
  if ((m = path.match(/^\/api\/v1\/sessions\/([^/]+)\/(.+)$/))) {
    const id = decodeURIComponent(m[1]);
    const sess = state.sessions.find((s) => s.id === id);
    if (!sess) return notFound('SESSION_NOT_FOUND', 'Unknown session');
    if (req.method === 'GET' && m[2] === 'conversation') {
      const all = messages(id);
      const before = Number(url.searchParams.get('beforeSequence')) || Infinity;
      const limit = Number(url.searchParams.get('limit')) || 50;
      const page = all.filter((msg) => msg.sequence < before).slice(-limit);
      return send(200, {
        sessionId: id, harness: sess.harness, mode: 'chat', controller: 'ready', latestSequence: all.length, oldestSequence: page.length ? page[0].sequence : 0,
        hasMoreBefore: page.length > 0 && page[0].sequence > 1, messages: page, activities: [],
      });
    }
    if (req.method === 'POST' && m[2] === 'conversation/messages') {
      // The agent "works" for a moment, then answers.
      const body = await readBody(req);
      const all = messages(id);
      state.sent.push({ id, text: body.text });
      all.push({ kind: 'message', id: `${id}-m${all.length + 1}`, sequence: all.length + 1, revision: 0, role: 'user', origin: 'human', text: body.text, clientMessageId: body.clientMessageId, streaming: false, createdAt: now() });
      sess.activity = { state: 'active', lastActivityAt: now() };
      setTimeout(() => {
        all.push({ kind: 'message', id: `${id}-m${all.length + 1}`, sequence: all.length + 1, revision: 0, role: 'assistant', origin: 'provider', text: `Fake reply to: ${body.text}`, streaming: false, createdAt: now() });
        sess.activity = { state: 'idle', lastActivityAt: now() };
      }, Math.max(delay, 300) * 3);
      return send(202, { turnId: `turn-${all.length}`, state: 'queued', duplicate: false });
    }
    if (req.method === 'POST' && m[2] === 'conversation/interrupt') {
      sess.activity = { state: 'idle', lastActivityAt: now() };
      return send(200, { ok: true });
    }
    if (req.method === 'POST' && m[2] === 'kill') {
      state.killed.push(id);
      if (sess.isTerminated) return send(409, { error: 'conflict', code: 'SESSION_TERMINATED', message: 'session is already terminated' });
      sess.isTerminated = true;
      sess.activity = { state: 'exited', lastActivityAt: now() };
      return send(200, { ok: true, sessionId: id });
    }
    if (req.method === 'GET' && m[2] === 'conversation/models') return send(200, sessionModels(id));
    if (req.method === 'PATCH' && m[2] === 'conversation/settings') {
      // Like the real daemon, this stores any model name without checking it.
      const body = await readBody(req);
      state.settings.push({ id, ...body });
      state.selected[id] = { ...(state.selected[id] || {}), ...body };
      for (const key of Object.keys(state.selected[id])) if (state.selected[id][key] === '') delete state.selected[id][key];
      return send(200, state.selected[id]);
    }
    if (req.method === 'POST' && m[2] === 'resume-agent') {
      if (delay) await sleep(delay);
      sess.needsResume = false;
      sess.activity.lastActivityAt = now();
      return send(200, { session: sess });
    }
  }
  return notFound('ROUTE_NOT_FOUND', `${req.method} ${path} has no handler`);
});

server.listen(port, '127.0.0.1', () => console.log(`fake AO daemon on http://127.0.0.1:${port}`));
