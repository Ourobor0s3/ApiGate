// Thin JSON wrapper over the ApiGate HTTP API. Every endpoint is same-origin
// (the Go server embeds this page), non-2xx responses raise an Error.

async function json(url, options) {
  const res = await fetch(url, options);
  let data = null;
  try { data = await res.json(); } catch { /* non-JSON body */ }
  if (!res.ok) throw new Error(data?.message || 'HTTP ' + res.status);
  return data;
}

function post(url, body) {
  return json(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

export const api = {
  dashboard: () => json('/dashboard'),
  newsQuota: () => json('/api/newsquota'),
  secrets: {
    list: () => json('/api/secrets'),
    save: (name, value) => post('/api/secrets', { name, value }),
    remove: (name) => json('/api/secrets?name=' + encodeURIComponent(name), { method: 'DELETE' }),
  },
  checks: {
    list: () => json('/api/checks'),
    add: (url) => post('/api/checks', { url }),
    remove: (url) => json('/api/checks?url=' + encodeURIComponent(url), { method: 'DELETE' }),
    run: () => post('/api/checks/run', {}),
  },
};