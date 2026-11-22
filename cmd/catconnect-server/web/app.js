'use strict';

const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

let state = null;   // last /api/state
let draft = null;   // {name, services} being edited
let dirty = false;

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: { 'Content-Type': 'application/json', 'X-CatConnect': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function toast(text, kind = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = text;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), kind === 'error' ? 5000 : 2200);
}

function ago(iso) {
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

const presetName = (id) => state?.presets.find((p) => p.id === id)?.name;

// ---------- editing ----------

function setDirty(v) {
  dirty = v;
  $('#savebar').hidden = !v;
  if (!v) $('#save-error').textContent = '';
}

function startDraft() {
  if (!draft) draft = { name: state.name, services: structuredClone(state.services) };
}

function edit(fn) {
  startDraft();
  fn(draft);
  setDirty(true);
}

const portsText = (arr) => (arr || []).join(', ');
const parsePorts = (s) => s.split(/[,\s]+/).map((x) => x.trim()).filter(Boolean);

function renderGames() {
  const services = (draft || state).services;
  const root = $('#games');
  if (!services.length) {
    root.innerHTML = `<div class="empty"><strong>No games yet</strong>Pick a game above and click Add game.</div>`;
    return;
  }
  // Keep focus while typing: only rebuild when the list's shape changes.
  const shape = services.map((s) => s.id + (s.enabled ? 1 : 0)).join('|');
  if (root.dataset.shape === shape && root.children.length) return;
  root.dataset.shape = shape;
  root.innerHTML = '';
  services.forEach((svc, i) => {
    const el = document.createElement('div');
    el.className = `game${svc.enabled ? '' : ' off'}`;
    const preset = presetName(svc.preset);
    el.innerHTML = `
      <div class="game-top">
        <input class="name" value="${esc(svc.name)}" maxlength="40" aria-label="Game name"/>
        ${preset ? (preset !== svc.name ? `<span class="pill">${esc(preset)}</span>` : '') : '<span class="pill">Custom</span>'}
        <label class="switch" title="Serve this game"><input type="checkbox" ${svc.enabled ? 'checked' : ''}/><span class="track"></span></label>
        <button class="icon-btn" title="Remove">✕</button>
      </div>
      <div class="game-grid">
        <label class="field"><span>Game server address</span><input class="host mono" placeholder="192.168.1.20" value="${esc(svc.host)}" spellcheck="false"/></label>
        <label class="field"><span>TCP ports</span><input class="tcp mono" placeholder="e.g. 25565" value="${esc(portsText(svc.tcp))}"/></label>
        <label class="field"><span>UDP ports</span><input class="udp mono" placeholder="e.g. 7777" value="${esc(portsText(svc.udp))}"/></label>
      </div>
      ${svc.join ? `<div class="game-hint">Players join with: ${esc(svc.join.replace('{addr}', 'localhost:' + firstPort(svc)).replace('{port}', firstPort(svc)))}</div>` : ''}`;
    el.querySelector('.name').addEventListener('input', (e) => edit((d) => { d.services[i].name = e.target.value; }));
    el.querySelector('.host').addEventListener('input', (e) => edit((d) => { d.services[i].host = e.target.value; }));
    el.querySelector('.tcp').addEventListener('input', (e) => edit((d) => { d.services[i].tcp = parsePorts(e.target.value); }));
    el.querySelector('.udp').addEventListener('input', (e) => edit((d) => { d.services[i].udp = parsePorts(e.target.value); }));
    el.querySelector('.switch input').addEventListener('change', (e) => { edit((d) => { d.services[i].enabled = e.target.checked; }); renderGames(); });
    el.querySelector('.icon-btn').addEventListener('click', () => { edit((d) => { d.services.splice(i, 1); }); renderGames(); });
    root.append(el);
  });
}

function firstPort(svc) {
  const p = (svc.tcp && svc.tcp[0]) || (svc.udp && svc.udp[0]) || '';
  return String(p).split('-')[0];
}

function renderPlayers() {
  const root = $('#players');
  const clients = state.clients;
  const online = clients.filter((c) => c.online).length;
  $('#players-sub').textContent = clients.length ? `${online} online` : '';
  let html = '';
  if (state.udpDrops > 0) {
    html += `<div class="notice">${state.udpDrops} game packets were too large for the tunnel (over 1232 bytes) and were dropped. If a game lags or disconnects, this is likely why.</div>`;
  }
  if (!clients.length) {
    html += `<div class="empty"><strong>Nobody has connected yet</strong>Players appear here once they open your link in the CatConnect app.</div>`;
  }
  for (const c of clients) {
    const route = c.online && c.route
      ? (c.route === 'direct' ? `<span class="pill ok" title="${esc(c.routeInfo)}">Direct</span>` : `<span class="pill warn" title="Relay ${esc(c.routeInfo)}">Relay</span>`)
      : '';
    html += `
      <div class="player">
        <span class="dot${c.online ? ' on' : ''}"></span>
        <div class="main">
          <div class="pname">${esc(c.name || 'Unnamed player')}</div>
          <div class="meta"><span class="mono">${esc(c.shortKey)}</span>
            <span>${c.online ? (c.activeConns ? `${c.activeConns} in game` : 'online') : 'last seen ' + ago(c.lastSeen)}</span>
            ${route}${c.blocked ? '<span class="pill bad">Blocked</span>' : ''}</div>
        </div>
        <button class="btn ghost small${c.blocked ? '' : ' danger-text'}" data-key="${esc(c.key)}" data-block="${c.blocked ? '0' : '1'}">${c.blocked ? 'Unblock' : 'Block'}</button>
      </div>`;
  }
  root.innerHTML = html;
  root.querySelectorAll('button[data-key]').forEach((b) => b.addEventListener('click', async () => {
    try {
      await api('POST', '/api/block', { key: b.dataset.key, blocked: b.dataset.block === '1' });
      await refresh();
    } catch (e) { toast(e.message, 'error'); }
  }));
}

function render() {
  $('#link').textContent = state.link;
  $('#link').title = state.link;
  $('#version').textContent = state.version;
  if (!dirty) $('#server-name').value = state.name;
  renderGames();
  renderPlayers();
}

function renderPresetSelect() {
  const sel = $('#preset-select');
  sel.innerHTML = state.presets.map((p) => `<option value="${esc(p.id)}">${esc(p.name)}</option>`).join('') +
    '<option value="">Custom game…</option>';
}

async function refresh() {
  try {
    const first = !state;
    state = await api('GET', '/api/state');
    if (first) renderPresetSelect();
    if (!dirty) draft = null;
    render();
    $('#status').className = 'pill ok live';
    $('#status').textContent = 'Online';
  } catch (e) {
    $('#status').className = 'pill bad';
    $('#status').textContent = 'Unreachable';
  }
}

// ---------- actions ----------

$('#copy-link').addEventListener('click', async () => {
  try {
    await navigator.clipboard.writeText(state.link);
    toast('Link copied');
  } catch {
    const r = document.createRange();
    r.selectNodeContents($('#link'));
    getSelection().removeAllRanges();
    getSelection().addRange(r);
    toast('Press Ctrl+C to copy');
  }
});

$('#server-name').addEventListener('input', (e) => edit((d) => { d.name = e.target.value; }));

$('#add-game').addEventListener('click', () => {
  const id = $('#preset-select').value;
  const p = state.presets.find((x) => x.id === id);
  const used = (draft || state).services;
  const lastHost = used.length ? used[used.length - 1].host : '';
  edit((d) => {
    d.services.push(p
      ? { id: '', name: p.name, preset: p.id, tcp: [...p.tcp], udp: [...p.udp], join: p.join, host: lastHost, enabled: true }
      : { id: '', name: 'My game', preset: '', tcp: [], udp: [], join: '', host: lastHost, enabled: true });
    d.services[d.services.length - 1].id = 'new-' + Math.random().toString(36).slice(2, 8);
  });
  renderGames();
  const inputs = document.querySelectorAll('#games .host');
  inputs[inputs.length - 1]?.focus();
});

$('#discard').addEventListener('click', () => {
  draft = null;
  setDirty(false);
  $('#games').dataset.shape = '';
  render();
});

$('#save').addEventListener('click', async () => {
  const btn = $('#save');
  btn.disabled = true;
  $('#save-error').textContent = '';
  try {
    const services = draft.services.map((s) => ({ ...s, id: s.id.startsWith('new-') ? '' : s.id }));
    await api('PUT', '/api/config', { name: draft.name, services });
    draft = null;
    setDirty(false);
    $('#games').dataset.shape = '';
    await refresh();
    toast('Saved. Connected players see the change right away');
  } catch (e) {
    $('#save-error').textContent = e.message;
  } finally {
    btn.disabled = false;
  }
});

$('#reset-link').addEventListener('click', async () => {
  if (!confirm('Create a new link? The current link stops working and everyone needs the new one.')) return;
  try {
    await api('POST', '/api/reset-link');
    await refresh();
    toast('New link created');
  } catch (e) { toast(e.message, 'error'); }
});

window.addEventListener('beforeunload', (e) => { if (dirty) e.preventDefault(); });

refresh();
setInterval(refresh, 3000);
