import './style.css';
import { Events } from '@wailsio/runtime';
import {
  GetState, UIReady, Connect, Disconnect, RemoveServer, SetPlayerName, SetAutoConnect,
  CopyText, HideWindow, Quit, GetAutostart, SetAutostart,
} from '../bindings/github.com/Sniphs98/CatConnect/client/app.js';

const $ = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const icon = (name, cls = '') => `<svg class="i ${cls}"><use href="#i-${name}"/></svg>`;
const cap = (s) => (s ? s[0].toUpperCase() + s.slice(1) : s);
const EventsOn = (name, cb) => Events.On(name, (ev) => cb(ev.data));

function toast(text, kind = 'ok') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.innerHTML = `${icon(kind === 'error' ? 'x' : 'check')}<span>${esc(text)}</span>`;
  $('#toasts').append(el);
  setTimeout(() => { el.classList.add('out'); setTimeout(() => el.remove(), 300); }, kind === 'error' ? 5000 : 2000);
}
const guard = (fn) => async (...a) => {
  try { return await fn(...a); } catch (e) { toast(cap(String(e?.message ?? e)), 'error'); }
};

// ---------- views ----------

let view = 'home';
function show(v) {
  view = v;
  document.querySelectorAll('.view').forEach((el) => el.classList.toggle('active', el.dataset.view === v));
  $('#open-settings').classList.toggle('active', v === 'settings');
  if (v === 'settings') {
    $('#player-name').value = state.settings.playerName;
    $('#auto-connect').checked = state.settings.autoConnect;
    GetAutostart().then((on) => { $('#autostart').checked = on; }).catch(() => {});
  }
}

// ---------- rendering ----------

let state = { settings: {}, servers: [], session: null };

const STATE_TEXT = {
  connecting: ['Connecting …', 'accent live'],
  connected: ['Connected', 'ok live'],
  reconnecting: ['Reconnecting …', 'warn live'],
  error: ['Not connected', 'bad'],
};

function routePill(s) {
  if (s.state !== 'connected' || !s.route) return '';
  const ping = s.latencyMs > 0 ? ` · ${s.latencyMs < 1 ? '<1' : Math.round(s.latencyMs)} ms` : '';
  return s.route === 'direct'
    ? `<span class="pill ok" title="Direct peer-to-peer connection (${esc(s.routeInfo)})">${icon('bolt')}Direct${ping}</span>`
    : `<span class="pill warn" title="Through relay ${esc(s.routeInfo)}. Switches to direct when possible; games may lag until then">${icon('relay')}Relay${ping}</span>`;
}

function serverName(srv) {
  return srv.name || 'Server';
}

function renderSession(s) {
  const [label, cls] = STATE_TEXT[s.state] || [s.state, ''];
  const saved = state.servers.find((x) => x.link === s.link);
  const name = s.server || (saved && saved.name) || 'Server';
  let html = `
    <div class="card current">
      <div class="card-row">
        <div class="badge-icon">${icon('server')}</div>
        <div class="card-main">
          <div class="card-title">${esc(name)}</div>
          <div class="card-sub"><span class="pill ${cls}">${label}</span>${routePill(s)}</div>
        </div>
        <button class="btn ghost small" id="disconnect">Disconnect</button>
      </div>
      ${s.error && s.state !== 'connected' ? `<div class="card-sub err-line">${esc(cap(s.error))}</div>` : ''}
    </div>`;

  if (s.state === 'connected' || s.services.length) {
    html += `<div class="list-head"><h3>Games</h3></div>`;
    if (!s.services.length) {
      html += `<div class="empty"><strong>No games shared yet</strong>The server owner hasn't enabled any games.</div>`;
    }
    for (const g of s.services) {
      html += `
        <div class="card game">
          <div class="card-row">
            <div class="badge-icon">${icon('game')}</div>
            <div class="card-main">
              <div class="card-title">${esc(g.name)}</div>
              <div class="card-sub">${(g.ports || []).map((p) => `<span class="pill">${esc(p)}</span>`).join('')}
                ${g.active > 0 ? '<span class="pill ok live">In game</span>' : ''}</div>
            </div>
          </div>
          ${g.error
            ? `<div class="card-sub err-line">${esc(g.error)}. Close the program using it and reconnect.</div>`
            : `<div class="linkbox"><code>${esc(g.address)}</code>
                 <button class="btn primary small" data-copy="${esc(g.address)}">${icon('copy', 'sm')}<span>Copy</span></button></div>
               ${g.join ? `<div class="hint">${esc(g.join)}</div>` : ''}`}
        </div>`;
    }
  }
  if (s.udpDrops > 0) {
    html += `<div class="card-sub err-line">${s.udpDrops} game packets were too large for the tunnel and were dropped. If the game lags, tell the server owner.</div>`;
  }
  return html;
}

function render(st) {
  state = st;
  document.body.classList.add(`platform-${st.platform}`);
  $('#app-version').textContent = st.version;
  const s = st.session;
  let html = '';
  if (s) html += renderSession(s);

  const others = st.servers.filter((x) => !s || x.link !== s.link);
  if (others.length) {
    html += `<div class="list-head"><h3>${s ? 'Other servers' : 'Your servers'}</h3></div>`;
    for (const srv of others) {
      html += `
        <div class="card saved">
          <div class="card-row">
            <div class="badge-icon">${icon('server')}</div>
            <div class="card-main"><div class="card-title">${esc(serverName(srv))}</div>
              <div class="card-sub mono">${esc(srv.link.slice(0, 34))}…</div></div>
            <button class="btn primary small" data-connect="${esc(srv.link)}">Join</button>
            <button class="icon-btn" data-remove="${esc(srv.link)}" title="Remove">${icon('x')}</button>
          </div>
        </div>`;
    }
  }
  if (!s && !others.length) {
    html += `<div class="empty welcome"><strong>Join a game server</strong>Ask your friend for their CatConnect link and paste it above, or just click the link.</div>`;
  }

  // Keep the DOM (and hover/focus) when nothing visible changed.
  const root = $('#home');
  if (root.dataset.html !== html) {
    root.dataset.html = html;
    root.innerHTML = html;
    root.querySelector('#disconnect')?.addEventListener('click', guard(() => Disconnect()));
    root.querySelectorAll('[data-copy]').forEach((b) => b.addEventListener('click', guard(async () => {
      await CopyText(b.dataset.copy);
      toast(`Copied ${b.dataset.copy}`);
    })));
    root.querySelectorAll('[data-connect]').forEach((b) => b.addEventListener('click', guard(() => Connect(b.dataset.connect))));
    root.querySelectorAll('[data-remove]').forEach((b) => b.addEventListener('click', guard(() => RemoveServer(b.dataset.remove))));
  }
}

// ---------- actions ----------

$('#join-form').addEventListener('submit', guard(async (ev) => {
  ev.preventDefault();
  const input = $('#link-input');
  if (!input.value.trim()) { input.focus(); return; }
  await Connect(input.value);
  input.value = '';
  show('home');
}));

document.addEventListener('paste', (ev) => {
  if (document.activeElement?.tagName === 'INPUT') return;
  const text = ev.clipboardData?.getData('text') ?? '';
  if (/^\s*catconnect:/i.test(text)) {
    $('#link-input').value = text.trim();
    $('#link-input').focus();
  }
});

$('#open-settings').addEventListener('click', () => show(view === 'settings' ? 'home' : 'settings'));
$('#close-settings').addEventListener('click', () => show('home'));
$('#hide-window').addEventListener('click', () => HideWindow());
$('#quit').addEventListener('click', () => Quit());
$('#name-form').addEventListener('submit', guard(async (ev) => {
  ev.preventDefault();
  await SetPlayerName($('#player-name').value);
  toast('Saved. Used from your next connection');
}));
$('#auto-connect').addEventListener('change', guard((ev) => SetAutoConnect(ev.target.checked)));
$('#autostart').addEventListener('change', guard(async (ev) => {
  await SetAutostart(ev.target.checked);
  toast(ev.target.checked ? 'CatConnect will start with your computer' : 'Autostart turned off');
}));

EventsOn('state', render);
EventsOn('toast', (m) => toast(cap(m.text), m.kind));
EventsOn('disconnect', guard(() => Disconnect()));

show('home');
(async () => {
  render(await GetState());
  await UIReady();
})();
