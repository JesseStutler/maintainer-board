// A minimal in-memory stand-in for the AO daemon, for browser tests that
// change session settings or start sessions and must not touch real ones:
//   node scripts/fake-ao.mjs 3915
//   ./maintainer-board --addr 127.0.0.1:3916 --db data/scratch/models/board.db --ao-url http://127.0.0.1:3915
//   node scripts/ui-models.mjs http://127.0.0.1:3916 http://127.0.0.1:3915
// It implements only what the board calls. GET /__state returns everything
// the board sent (spawn requests, settings changes) for assertions.

import { createServer } from 'node:http';

const port = Number(process.argv[2]) || 3915;
const now = () => new Date().toISOString();
const efforts = ['low', 'medium', 'high'];

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
};

const messages = (id) => [
  { kind: 'message', id: `${id}-m1`, sequence: 1, revision: 0, role: 'user', origin: 'human', text: 'Hello from the fake daemon.', streaming: false, createdAt: now() },
  { kind: 'message', id: `${id}-m2`, sequence: 2, revision: 0, role: 'assistant', origin: 'provider', text: 'This conversation exists only for the model picker test.', streaming: false, createdAt: now() },
];

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
  if (req.method === 'GET' && path === '/api/v1/session-import') return send(200, { candidates: [] });
  if (req.method === 'GET' && (m = path.match(/^\/api\/v1\/agents\/([^/]+)\/models$/))) {
    const agent = decodeURIComponent(m[1]);
    if (!catalogs[agent]) return notFound('AGENT_NOT_FOUND', 'Unknown agent adapter');
    return send(200, { agentId: agent, models: catalogs[agent] });
  }
  if (req.method === 'POST' && path === '/api/v1/sessions') {
    const body = await readBody(req);
    state.spawned.push(body);
    const created = session(`demo-${state.sessions.length + 1}`, body.harness, body.displayName || 'New session');
    state.sessions.push(created);
    state.selected[created.id] = { model: body.model || '', reasoningEffort: body.effort || '' };
    return send(201, { session: created });
  }
  if ((m = path.match(/^\/api\/v1\/sessions\/([^/]+)\/(.+)$/))) {
    const id = decodeURIComponent(m[1]);
    const sess = state.sessions.find((s) => s.id === id);
    if (!sess) return notFound('SESSION_NOT_FOUND', 'Unknown session');
    if (req.method === 'GET' && m[2] === 'conversation') {
      return send(200, { sessionId: id, harness: sess.harness, mode: 'chat', controller: 'ready', latestSequence: 2, oldestSequence: 1, hasMoreBefore: false, messages: messages(id), activities: [] });
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
      sess.needsResume = false;
      sess.activity.lastActivityAt = now();
      return send(200, { session: sess });
    }
  }
  return notFound('ROUTE_NOT_FOUND', `${req.method} ${path} has no handler`);
});

server.listen(port, '127.0.0.1', () => console.log(`fake AO daemon on http://127.0.0.1:${port}`));
