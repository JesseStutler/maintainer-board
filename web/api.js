// Thin JSON client for the Maintainer Board API.

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

async function request(method, path, body) {
  const options = { method, headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    options.headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }
  let response;
  try {
    response = await fetch(path, options);
  } catch (err) {
    throw new ApiError(0, 'NETWORK', 'The Maintainer Board server is not reachable. Check the SSH tunnel and the server process.');
  }
  const text = await response.text();
  let data = null;
  if (text) {
    try {
      data = JSON.parse(text);
    } catch (err) {
      data = null;
    }
  }
  if (!response.ok) {
    const error = data && data.error ? data.error : {};
    throw new ApiError(
      response.status,
      error.code || `HTTP_${response.status}`,
      error.message || `Request failed (HTTP ${response.status}).`,
    );
  }
  return data;
}

export const api = {
  get: (path) => request('GET', path),
  post: (path, body = {}) => request('POST', path, body),
  patch: (path, body = {}) => request('PATCH', path, body),
  del: (path) => request('DELETE', path),
};

export function cardPath(id, suffix = '') {
  return `/api/cards/${encodeURIComponent(id)}${suffix}`;
}

/** Import History: AO's candidates, enriched from the local session stores. */
export const importHistory = {
  list: () => request('GET', '/api/ao/import-history'),
  search: (query) => request('GET', `/api/ao/import-history/search?q=${encodeURIComponent(query)}`),
  aiSearch: (query) => request('POST', '/api/ao/import-history/ai-search', { query }),
};

export function sessionPath(id, suffix = '') {
  return `/api/sessions/${encodeURIComponent(id)}${suffix}`;
}
