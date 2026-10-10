"use strict";
// fastcontrol web client. Runs in a phone browser (served by a relay) and in
// the Android app (bundled assets; window.CodexAndroid is the native bridge).
// Keeps one WebSocket per paired Mac so the home screen can show all of them.

const $ = (id) => document.getElementById(id);
const Native = window.CodexAndroid || null;
const store = {
  get(k, d) { try { const v = localStorage.getItem(k); return v == null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch {} },
};
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const base = (p) => (p || "").split("/").filter(Boolean).pop() || p || "";
const svg = (d, cls = "") => `<svg class="ico ${cls}" viewBox="0 0 24 24">${d}</svg>`;
const I = {
  chev: svg('<path d="M9 6l6 6-6 6"/>', "sm chev"),
  down: svg('<path d="M6 9l6 6 6-6"/>', "sm chev"),
  folder: svg('<path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2z"/>', "folder"),
  term: svg('<rect x="3" y="4" width="18" height="16" rx="2.5"/><path d="M7 9l3 3-3 3M12 15h5"/>', "sm"),
  think: svg('<path d="M12 3a6 6 0 00-3.5 10.9V17h7v-3.1A6 6 0 0012 3zM9.5 21h5"/>', "sm"),
  edit: svg('<path d="M4 20h4L19 9l-4-4L4 16zM13.5 6.5l4 4"/>', "sm"),
  globe: svg('<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 010 18M12 3a14 14 0 000 18"/>', "sm"),
  plug: svg('<path d="M9 2v6M15 2v6M6 8h12v3a6 6 0 01-12 0zM12 17v5"/>', "sm"),
  agent: svg('<circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0116 0"/>', "sm"),
  warn: svg('<path d="M12 3l10 18H2zM12 10v5M12 18v.5"/>', "sm"),
  shield: svg('<path d="M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z"/>', "sm"),
  shieldok: svg('<path d="M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z"/><path d="M8.5 12l2.5 2.5 4.5-5"/>', "sm"),
  lock: svg('<rect x="5" y="10" width="14" height="10" rx="2"/><path d="M8 10V7a4 4 0 018 0v3"/>', "sm"),
  hand: svg('<path d="M8 13V5a1.5 1.5 0 013 0v6M11 11V4a1.5 1.5 0 013 0v7M14 11V6a1.5 1.5 0 013 0v8a7 7 0 01-7 7 6 6 0 01-5-3l-2.5-4a1.5 1.5 0 012.5-1.6L8 15"/>', "sm"),
  gear: svg('<circle cx="12" cy="12" r="3"/><path d="M12 2v3M12 19v3M4.2 4.2l2.1 2.1M17.7 17.7l2.1 2.1M2 12h3M19 12h3M4.2 19.8l2.1-2.1M17.7 6.3l2.1-2.1"/>'),
  squash: svg('<path d="M4 14h6v6M20 10h-6V4M14 10l7-7M3 21l7-7"/>'),
  box: svg('<path d="M3 7h18v13H3zM2 3h20v4H2zM10 12h4"/>'),
  mac: svg('<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>'),
  diff: svg('<path d="M12 3v8M8 7h8M8 17h8M5 21h14"/>'),
  copy: svg('<rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 012-2h10"/>'),
  share: svg('<path d="M4 12v7a2 2 0 002 2h12a2 2 0 002-2v-7M16 6l-4-4-4 4M12 2v13"/>'),
  download: svg('<path d="M12 3v12M7 10l5 5 5-5M4 16v4h16v-4"/>'),
  plus: svg('<path d="M12 5v14M5 12h14"/>'),
  minus: svg('<path d="M5 12h14"/>'),
  trash: svg('<path d="M3 6h18M8 6V4h8v2M6 6l1 14h10l1-14"/>'),
  close: svg('<path d="M6 6l12 12M18 6L6 18"/>'),
  check: svg('<path d="M5 12l5 5 9-10"/>', "sm check"),
  rocket: svg('<path d="M12 15l-3-3c1.5-4.5 4.5-8 10-9 -.5 5.5-4 8.5-9 10z"/><path d="M9 12H5l2.5-3.5H11M12 15v4l3.5-2.5V13"/><path d="M6.5 16.5C5 17 4.5 19.5 4.5 19.5S7 19 7.5 17.5"/>', "sm"),
  spark: svg('<path d="M12 3v4M12 17v4M3 12h4M17 12h4M6 6l2.5 2.5M15.5 15.5L18 18M6 18l2.5-2.5M15.5 8.5L18 6"/>', "sm"),
};

// ------------------------------------------------------------------ state --

const S = {
  provider: store.get("provider", "codex"),
  servers: store.get("servers", []),
  machine: store.get("machine", "all"), // "all" or a server id
  query: "",
  view: "home", // home | project | thread
  cwd: "", // project view
  sid: "", id: "", // thread view
  meta: null, turns: [], stick: true,
  open: new Set(), // expanded rows, keyed by turn and item
  minCards: new Set(), // collapsed approval and question cards
  attach: [], newAttach: [],
  sheet: null,
  showAllProjects: false, pinnedClosed: store.get("pinnedClosed", false), recentLimit: 40,
};
if (!S.servers.length && Native?.getServers) {
  try { S.servers = JSON.parse(Native.getServers()); } catch {}
}
const conns = new Map(); // server id -> Conn

function sid() { return Math.random().toString(36).slice(2, 10); }
function hostOf(url) { try { return new URL(url).hostname; } catch { return url; } }
function saveServers() {
  store.set("servers", S.servers);
  if (Native) try { Native.setServers(JSON.stringify(S.servers)); } catch {}
}
function addServer({ url, token, name, custom }) {
  url = url.replace(/\/+$/, "");
  let s = S.servers.find((x) => x.url === url);
  if (s) { s.token = token; if (name) { s.name = name; s.custom = !!custom; } }
  else { s = { id: sid(), url, token, name: name || hostOf(url), custom: !!(custom && name) }; S.servers.push(s); }
  saveServers();
  syncConns();
  return s;
}
const serverName = (id) => { const s = S.servers.find((x) => x.id === id); return s ? s.name || hostOf(s.url) : "Mac"; };

(function adoptPairingLink() {
  // Pairing link printed by a relay: http://mac:8787/#t=TOKEN
  const m = location.hash.match(/[#&]t=([0-9a-f]{32,})/);
  const fromRelay = /^https?:$/.test(location.protocol) && location.hostname !== "appassets.androidplatform.net";
  let legacy = null;
  try { legacy = localStorage.getItem("token"); legacy = legacy && legacy.replace(/"/g, ""); } catch {}
  if (fromRelay && (m || (legacy && !S.servers.length))) {
    addServer({ url: location.origin, token: m ? m[1] : legacy, name: "" });
    if (m) history.replaceState(null, "", location.pathname);
  }
})();

// ------------------------------------------------------------- connection --

class Conn {
  constructor(server) {
    this.s = server;
    this.ws = null; this.ok = false; this.desktop = false; this.state = "wait";
    this.threads = store.get("threads:" + server.id, []);
    this.backoff = 300; this.timer = 0; this.nonce = 0; this.pending = {};
    this.models = null; this.projects = null; this.home = ""; this.projAt = 0;
    this.base = server.url; // the address that last worked; alternates come from hello
    this.byCwd = {}; // full thread lists of opened projects
    this.dead = false;
    this.byProvider = {};
    this.capabilities = {};
    this.threads = store.get("threads:" + server.id + ":" + S.provider, []);
    this.connect();
  }
  // connect races every known address of the Mac (the last one that worked,
  // the paired one, and the Wi-Fi and Tailscale addresses from hello) and
  // keeps whichever socket opens first.
  connect() {
    clearTimeout(this.timer);
    if (this.dead) return;
    if (this.ws) { this.ws.onclose = null; try { this.ws.close(); } catch {} this.ws = null; }
    const urls = [this.base, this.s.url, ...(this.s.alts || [])].filter((u, i, a) => u && a.indexOf(u) === i);
    const my = (this.attempt = (this.attempt || 0) + 1);
    let winner = null, failed = 0;
    const sockets = urls.map((u) => {
      let w;
      try { w = new WebSocket(u.replace(/^http/, "ws") + "/ws?t=" + encodeURIComponent(this.s.token)); }
      catch { failed++; return null; }
      w.onopen = () => {
        if (winner || my !== this.attempt || this.dead) { w.onclose = null; w.close(); return; }
        winner = w;
        for (const o of sockets) if (o && o !== w) { o.onclose = null; try { o.close(); } catch {} }
        this.base = u;
        this.attach(w);
      };
      w.onclose = () => { if (!winner && my === this.attempt && ++failed === urls.length) this.unreachable(); };
      return w;
    });
    if (failed === urls.length) this.unreachable();
  }
  attach(ws) {
    mark("open:" + this.s.id);
    this.ws = ws; this.ok = true; this.backoff = 300;
    ws.onmessage = (e) => this.onMessage(JSON.parse(e.data));
    ws.onclose = () => {
      this.ok = false; this.ws = null;
      if (this.dead || this.paused) return;
      this.setState("off");
      this.timer = setTimeout(() => this.connect(), this.backoff);
      this.backoff = Math.min(this.backoff * 2, 5000);
    };
    this.send({ op: "events", on: true });
    if (Native?.startVoice) { this.speech = null; speechCapability(this); }
    this.refreshProjects(true);
    if (S.view === "project") this.send({ op: "project", cwd: S.cwd });
    if (S.view === "thread" && S.sid === this.s.id) this.send({ op: "open", id: S.id });
  }
  async unreachable() {
    this.ok = false;
    if (this.dead || this.paused) return;
    try {
      const r = await fetch(this.s.url + "/auth?t=" + encodeURIComponent(this.s.token));
      if (r.status === 401) { this.setState("rejected"); return; }
    } catch {}
    this.setState("off");
    this.timer = setTimeout(() => this.connect(), this.backoff);
    this.backoff = Math.min(this.backoff * 2, 5000);
  }
  // Project counts come from the whole database, refreshed at most every 30 s.
  refreshProjects(force) {
    if (force || Date.now() - this.projAt > 30000) { this.projAt = Date.now(); this.send({ op: "projects" }); }
  }
  pause() { this.paused = true; this.attempt = (this.attempt || 0) + 1; clearTimeout(this.timer); if (this.ws) { this.ws.onclose = null; try { this.ws.close(); } catch {} this.ws = null; } this.ok = false; }
  close() { this.dead = true; clearTimeout(this.timer); if (this.ws) { this.ws.onclose = null; try { this.ws.close(); } catch {} } }
  setState(st) { this.state = st; scheduleRender(); }
  send(m) { if (this.ws && this.ws.readyState === 1) { this.ws.send(JSON.stringify({ provider:S.provider, ...m })); return true; } return false; }
  call(m, timeout = 30000) {
    return new Promise((res) => {
      const nonce = "n" + ++this.nonce;
      if (!this.send({ ...m, nonce })) { res({ ok: false, msg: "Not Connected To " + serverName(this.s.id) }); return; }
      this.pending[nonce] = res;
      setTimeout(() => { if (this.pending[nonce]) { delete this.pending[nonce]; res({ ok: false, msg: "No Response From The Mac" }); } }, timeout);
    });
  }
  onMessage(m) {
    const provider = m.provider || "codex";
    if (["threads", "projects", "models"].includes(m.t)) {
      const cached = this.byProvider[provider] || (this.byProvider[provider] = {});
      if (m.t === "threads") { cached.threads = m.threads || []; store.set("threads:" + this.s.id + ":" + provider, cached.threads.slice(0,200)); }
      if (m.t === "projects") { cached.projects = m.projects || []; cached.home = m.home; }
      if (m.t === "models") cached.models = m;
      if (provider !== S.provider) return;
    }
    const here = provider === S.provider && S.view === "thread" && S.sid === this.s.id && m.id === S.id;
    switch (m.t) {
      case "hello":
        this.desktop = m.desktop;
        this.capabilities = m.providers || { codex:true, claude:false };
        if (Array.isArray(m.addrs) && JSON.stringify(m.addrs) !== JSON.stringify(this.s.alts || [])) { this.s.alts = m.addrs; saveServers(); }
        // Show the name the Mac reports, unless the user named it.
        if (m.host && !this.s.custom && this.s.name !== m.host) { this.s.name = m.host; saveServers(); scheduleRender(); }
        this.setState(m.desktop ? "on" : "nodesk");
        break;
      case "threads":
        mark("threads:" + this.s.id);
        this.threads = m.threads || [];
        store.set("threads:" + this.s.id, this.threads.filter((t) => !t.subagent).slice(0, 200));
        scheduleRender();
        break;
      case "open": if (here) { mark("thread:" + m.meta.mode); S.meta = m.meta; S.turns = m.turns; renderThread(m.more); } break;
      case "meta": if (here) { S.meta = m.meta; renderMeta(); } break;
      case "turn":
        if (!here) break;
        { const i = S.turns.findIndex((t) => t.key === m.turn.key); if (i >= 0) S.turns[i] = m.turn; else S.turns.push(m.turn); }
        dirtyTurn(m.turn.key);
        break;
      case "head":
        if (!here) break;
        { const t = S.turns.find((t) => t.key === m.key); if (t) { t.status = m.status; t.durationMs = m.durationMs; t.error = m.error; dirtyTurn(t.key); } }
        break;
      case "item":
        if (!here) break;
        { const t = S.turns.find((t) => t.key === m.key); if (t) { t.items[m.idx] = m.item; dirtyTurn(t.key); } }
        break;
      case "ack": {
        const p = this.pending[m.nonce];
        if (p) { delete this.pending[m.nonce]; p(m); } else if (!m.ok && here) toast(nice(m.msg));
        break;
      }
      case "event": onEvent(this, m); break;
      case "models": this.models = m; if (S.redraw) S.redraw(); break;
      case "projects": this.projects = m.projects; this.home = m.home; if (S.redraw) S.redraw(); scheduleRender(); break;
      case "project": if(provider===S.provider){this.byCwd[m.cwd] = m.threads || []; if (S.view === "project" && S.cwd === m.cwd) renderProject();} break;
      case "diff": if (here) showDiff(m); break;
    }
  }
}
function syncConns() {
  for (const [id, c] of conns) if (!S.servers.some((s) => s.id === id)) { c.close(); conns.delete(id); }
  for (const s of S.servers) {
    const c = conns.get(s.id);
    if (!c) conns.set(s.id, new Conn(s));
    else if (c.s.url !== s.url || c.s.token !== s.token) { c.close(); conns.set(s.id, new Conn(s)); }
    else c.s = s;
  }
  if (S.machine !== "all" && !conns.has(S.machine)) S.machine = "all";
}
const conn = () => conns.get(S.sid);
// Hidden for a while: drop the sockets so a backgrounded page costs nothing
// (the Android watch service keeps notifications going). Reconnect on return.
let hiddenTimer = 0;
document.addEventListener("visibilitychange", () => {
  clearTimeout(hiddenTimer);
  if (document.visibilityState !== "visible") {
    hiddenTimer = setTimeout(() => { for (const c of conns.values()) c.pause(); }, 15000);
    return;
  }
  for (const c of conns.values()) if (c.paused || !c.ws || c.ws.readyState > 1) { c.paused = false; c.backoff = 300; c.connect(); }
});
window.addEventListener("online", () => { for (const c of conns.values()) { c.backoff = 300; c.connect(); } });

function onEvent(c, e) {
  if (e.kind === "started") return;
  if (S.provider === (e.provider || "codex") && S.view === "thread" && S.id === e.id && document.visibilityState === "visible") return;
  const label = { completed: "Finished", failed: "Failed", attention: "Needs You" }[e.kind] || "";
  toast(`${label}: ${e.title}`, () => { selectProvider(e.provider || "codex",false);openThread(c.s.id, e.id, true); });
  if (e.kind === "attention") buzz(40);
}

// -------------------------------------------------------------- utilities --

// mark records the first time something happens, in ms since navigation, for
// benchmarks (read with window.perfMarks()).
const marks = {};
function mark(name) { if (!(name in marks)) marks[name] = Math.round(performance.now()); }
window.perfMarks = () => ({ ...marks, domInteractive: Math.round(performance.getEntriesByType("navigation")[0].domInteractive) });

const nice = (msg) => (msg || "Failed").replace(/^./, (c) => c.toUpperCase());
function buzz(ms = 12) {
  if (Native) { try { Native.vibrate(ms); } catch {} return; }
  if (navigator.vibrate) navigator.vibrate(ms);
}
function ago(ms) {
  const s = Math.max(0, (Date.now() - ms) / 1000);
  if (s < 60) return "Now";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  if (s < 86400 * 7) return Math.floor(s / 86400) + "d";
  if (s < 86400 * 30) return Math.floor(s / (86400 * 7)) + "w";
  return Math.floor(s / (86400 * 30)) + "mo";
}
function dur(ms) {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return s + "s";
  const m = Math.floor(s / 60);
  if (m < 60) return m + "m " + String(s % 60).padStart(2, "0") + "s";
  return Math.floor(m / 60) + "h " + String(m % 60).padStart(2, "0") + "m";
}
let toastTimer;
function toast(msg, onTap) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.toggle("tap", !!onTap);
  t.onclick = onTap ? () => { t.classList.add("hidden"); onTap(); } : null;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), onTap ? 4500 : 2200);
}
function copyText(text) {
  if (Native) { try { Native.copy(text); toast("Copied"); return; } catch {} }
  (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject()).then(() => toast("Copied"), () => toast("Copy Failed"));
}
// Elapsed timers tick in place, only while one is on screen.
setInterval(() => {
  if (document.visibilityState !== "visible") return;
  for (const el of document.querySelectorAll("[data-since]")) el.textContent = dur(Date.now() - +el.dataset.since);
}, 1000);

// ------------------------------------------------------------- navigation --
// History entries: {view, sid, id, cwd} plus {sheet}. Back pops the top one.

const reducedMotion = matchMedia("(prefers-reduced-motion: reduce)");
let pageAnimations = [];
function show(view) {
  const previous = S.view;
  const from = $(previous), to = $(view);
  const animate = previous !== view && from && !from.classList.contains("hidden") && !reducedMotion.matches;
  for (const a of pageAnimations) a.cancel();
  pageAnimations = [];
  S.view = view;
  for (const v of ["providers", "home", "project", "thread"]) {
    $(v).classList.toggle("hidden", v !== view);
    $(v).inert = v !== view;
  }
  if (!animate) return;
  const direction = ["providers", "home", "project", "thread"].indexOf(view) > ["providers", "home", "project", "thread"].indexOf(previous) ? 1 : -1;
  from.classList.remove("hidden");
  const timing = { duration: 180, easing: "cubic-bezier(0.22, 1, 0.36, 1)" };
  const outgoing = from.animate([{ transform: "translateX(0)", opacity: 1 }, { transform: `translateX(${-direction * 24}px)`, opacity: 0 }], timing);
  const incoming = to.animate([{ transform: `translateX(${direction * 40}px)`, opacity: 0.6 }, { transform: "translateX(0)", opacity: 1 }], timing);
  pageAnimations = [outgoing, incoming];
  outgoing.finished.then(() => { if (S.view !== previous) from.classList.add("hidden"); }, () => {});
}
window.addEventListener("popstate", (e) => {
  const st = e.state || {};
  if (st.provider && st.provider !== S.provider) selectProvider(st.provider,false);
  if (S.sheet && !st.sheet) closeSheet(true);
  if (st.view === "thread") { if (S.id !== st.id || S.view !== "thread") openThread(st.sid, st.id, false); }
  else if (st.view === "project") { leaveThread(); openProject(st.cwd, false); }
  else if (!st.sheet) { leaveThread(); show(st.view === "home" ? "home" : "providers"); renderHome(); }
});
function selectProvider(provider, push = true) {
  if (!['codex','claude'].includes(provider)) return;
  leaveThread();
  S.newDraft=null;S.newAttach=[];
  S.provider=provider;store.set('provider',provider);
  $('provider-switch').textContent=provider==='claude'?'Claude Code':'Codex';
 $('ask').querySelector('span').textContent=provider==='claude'?'Ask Claude':'Ask Codex';
  for (const c of conns.values()) {
    const cache=c.byProvider[provider] || {};
    c.threads=cache.threads || store.get('threads:'+c.s.id+':'+provider,[]);c.projects=cache.projects || null;c.models=cache.models || null;c.home=cache.home || '';c.byCwd={};
    c.send({op:'threads'});c.refreshProjects(true);c.send({op:'models'});
  }
  if(push)history.pushState({view:'home',provider},'');
  show('home');renderHome();
}
$('provider-switch').onclick=()=>{leaveThread();history.replaceState({view:'providers'},'');show('providers');};
$('provider-macs').onclick=()=>openSheet('macs');
for(const button of document.querySelectorAll('[data-provider]'))button.onclick=()=>selectProvider(button.dataset.provider);
for (const b of document.querySelectorAll(".back")) b.onclick = () => history.back();

// Every list render goes through one animation frame.
let renderQueued = 0;
function scheduleRender() {
  if (renderQueued) return;
  renderQueued = requestAnimationFrame(() => {
    renderQueued = 0;
    if (S.view === "home") renderHome();
    else if (S.view === "project") renderProject();
    else if (S.view === "thread") renderThreadHeader();
  });
}

// All threads visible under the current machine filter, tagged with their server.
function allThreads() {
  const out = [];
  for (const [id, c] of conns) {
    if (S.machine !== "all" && S.machine !== id) continue;
    for (const t of c.threads) if (!t.subagent) { t.sid = id; out.push(t); }
  }
  return out;
}
function findThread(sidv, id) {
  const c = conns.get(sidv);
  return c && c.threads.find((t) => t.id === id);
}

// ------------------------------------------------------------------- home --

$("search-btn").onclick = () => {
  const r = $("search-row");
  r.classList.toggle("hidden");
  if (!r.classList.contains("hidden")) $("search").focus();
  else { $("search").value = ""; S.query = ""; renderHome(); }
};
$("search").addEventListener("input", (e) => { S.query = e.target.value.toLowerCase().trim(); renderHome(); });
$("macs-btn").onclick = () => openSheet("macs");
$("ask").onclick = () => openSheet("new", {});

function renderMachines() {
  const dotCls = (c) => ({ on: "on", nodesk: "wait", off: "off", rejected: "off", bad: "off" }[c.state] || "wait");
  let h = S.servers.length > 1 ? `<button class="chip ${S.machine === "all" ? "on" : ""}" data-m="all">All Macs</button>` : "";
  for (const s of S.servers) {
    const c = conns.get(s.id);
    h += `<button class="chip ${S.machine === s.id || S.servers.length === 1 ? "on" : ""}" data-m="${s.id}"><span class="dot ${dotCls(c)}"></span>${esc(s.name || hostOf(s.url))}</button>`;
  }
  h += `<button class="chip" data-m="+">Add Mac</button>`;
  $("machines").innerHTML = h;
}
$("machines").addEventListener("click", (e) => {
  const b = e.target.closest("[data-m]");
  if (!b) return;
  if (b.dataset.m === "+") { openSheet("macs"); return; }
  S.machine = b.dataset.m; store.set("machine", S.machine);
  renderHome();
});

function statusBadge(t) {
  if (t.attention) return '<span class="badge need"><span class="dot"></span>Needs You</span>';
  if (t.running) return `<span class="badge run"><span class="dot"></span>${t.turnAt ? `<span data-since="${t.turnAt}">${dur(Date.now() - t.turnAt)}</span>` : "Running"}</span>`;
  return "";
}
function threadRow(t, { project = true } = {}) {
  const multi = S.servers.length > 1 && S.machine === "all";
  const bits = [statusBadge(t)];
  if (project) bits.push(`<span>${esc(base(t.cwd))}</span>`);
  if (multi) bits.push(`<span class="badge mac">${esc(serverName(t.sid))}</span>`);
  const msg = t.lastErr ? `<span class="msg err">${esc(t.lastErr)}</span>` : t.lastMsg ? `<span class="msg">${esc(t.lastMsg)}</span>` : "";
  return `<button class="row" data-sid="${t.sid}" data-id="${esc(t.id)}"><div class="main"><div class="t">${esc(t.title || t.preview || "Untitled")}</div>` +
    `<div class="s">${bits.join("")}${msg}</div></div><div class="when">${ago(t.recency)}</div></button>`;
}
function projectsOf(threads, withDB) {
  const by = new Map();
  if (withDB) {
    for (const [id, c] of conns) {
      if (S.machine !== "all" && S.machine !== id) continue;
      for (const p of c.projects || []) {
        const q = by.get(p.cwd) || { cwd: p.cwd, n: 0, recency: 0, running: 0, attention: 0, db: true };
        q.n += p.threads; q.recency = Math.max(q.recency, p.recency);
        by.set(p.cwd, q);
      }
    }
  }
  for (const t of threads) {
    const k = t.cwd || "";
    let p = by.get(k);
    if (!p) { p = { cwd: k, n: 0, recency: 0, running: 0, attention: 0 }; by.set(k, p); }
    if (!p.db) p.n++;
    p.recency = Math.max(p.recency, t.recency);
    if (t.running) p.running++;
    if (t.attention) p.attention++;
  }
  return [...by.values()].sort((a, b) => b.recency - a.recency);
}

function renderHome() {
  for (const c of conns.values()) c.refreshProjects(false);
  renderMachines();
  const list = $("home-list");
  const threads = allThreads();
  if (!S.servers.length) {
    list.innerHTML = `<div class="empty"><b>Pair A Mac</b>Run fastcontrol on your Mac and open the link it prints, or tap Add Mac.</div>`;
    return;
  }
  const q = S.query;
  if (q) {
    const hits = threads.filter((t) => `${t.title} ${t.preview} ${t.cwd} ${t.lastMsg || ""}`.toLowerCase().includes(q)).sort((a, b) => b.recency - a.recency).slice(0, 80);
    list.innerHTML = hits.length ? `<div class="sec">Results<span class="n">${hits.length}</span></div><div class="group">${hits.map((t) => threadRow(t)).join("")}</div>` : `<div class="empty"><b>No Matches</b>Nothing in titles, projects or last replies matches.</div>`;
    return;
  }
  const attention = threads.filter((t) => t.attention);
  const running = threads.filter((t) => t.running && !t.attention).sort((a, b) => (b.turnAt || 0) - (a.turnAt || 0));
  const pinned = threads.filter((t) => t.pinned);
  const recent = threads.filter((t) => !t.running && !t.attention).sort((a, b) => b.recency - a.recency);
  const projects = projectsOf(threads, true);
  const shownProjects = S.showAllProjects ? projects : projects.slice(0, 6);
  let h = "";
  if (attention.length) h += `<div class="sec">Needs You<span class="n">${attention.length}</span></div><div class="group">${attention.map((t) => threadRow(t)).join("")}</div>`;
  if (running.length) h += `<div class="sec">Running<span class="n">${running.length}</span></div><div class="group">${running.map((t) => threadRow(t)).join("")}</div>`;
  if (projects.length) {
    h += `<div class="sec">Projects<span class="n">${projects.length}</span></div><div class="group">` +
      shownProjects.map((p) => `<button class="row proj" data-cwd="${esc(p.cwd)}">${I.folder}<div class="main"><div class="t">${esc(base(p.cwd) || "Home")}</div>` +
        `<div class="s">${p.attention ? '<span class="badge need"><span class="dot"></span>Needs You</span>' : p.running ? `<span class="badge run"><span class="dot"></span>${p.running} Running</span>` : ""}<span>${p.n} ${p.n === 1 ? "Chat" : "Chats"}</span></div></div><div class="when">${ago(p.recency)}</div></button>`).join("") +
      (projects.length > 6 ? `<button class="show-more" data-x="projects">${S.showAllProjects ? "Show Fewer" : `Show All ${projects.length} Projects`}</button>` : "") + "</div>";
  }
  if (pinned.length) {
    h += `<button class="sec ${S.pinnedClosed ? "closed" : ""}" data-x="pinned">Pinned<span class="n">${pinned.length}</span>${I.down}</button>`;
    if (!S.pinnedClosed) h += `<div class="group">${pinned.map((t) => threadRow(t)).join("")}</div>`;
  }
  if (recent.length) {
    h += `<div class="sec">Recent<span class="n">${recent.length}</span></div><div class="group">${recent.slice(0, S.recentLimit).map((t) => threadRow(t)).join("")}` +
      (recent.length > S.recentLimit ? `<button class="show-more" data-x="recent">Show More</button>` : "") + "</div>";
  }
  if (!threads.length) {
    const c = [...conns.values()].find((c) => S.machine === "all" || c.s.id === S.machine);
    h = `<div class="empty"><b>${c && c.state === "rejected" ? "Pairing Rejected" : "Waiting For Your Mac"}</b>${c && c.state === "rejected" ? "The token was refused. Pair again with the link the relay prints." : "Make sure fastcontrol is running and this phone can reach it."}</div>`;
  }
  list.innerHTML = h;
}
$("home-list").addEventListener("click", (e) => {
  const r = e.target.closest("[data-id]");
  if (r) { openThread(r.dataset.sid, r.dataset.id, true); return; }
  const p = e.target.closest("[data-cwd]");
  if (p) { openProject(p.dataset.cwd, true); return; }
  const x = e.target.closest("[data-x]");
  if (!x) return;
  if (x.dataset.x === "projects") S.showAllProjects = !S.showAllProjects;
  if (x.dataset.x === "pinned") { S.pinnedClosed = !S.pinnedClosed; store.set("pinnedClosed", S.pinnedClosed); }
  if (x.dataset.x === "recent") S.recentLimit += 60;
  renderHome();
});
setInterval(() => { if (S.view !== "thread" && document.visibilityState === "visible") scheduleRender(); }, 30000);

// ---------------------------------------------------------------- project --

function openProject(cwd, push) {
  if (push) history.pushState({ view: "project", cwd, provider:S.provider }, "");
  S.cwd = cwd;
  $("p-search").value = "";
  for (const [id, c] of conns) if (S.machine === "all" || S.machine === id) c.send({ op: "project", cwd });
  show("project");
  renderProject();
  $("project-list").scrollTop = 0;
}
function renderProject() {
  // Full list from the database when it has arrived, with live state from the recent list.
  const byId = new Map(allThreads().filter((t) => t.cwd === S.cwd).map((t) => [t.sid + t.id, t]));
  for (const [id, c] of conns) {
    if (S.machine !== "all" && S.machine !== id) continue;
    for (const t of c.byCwd[S.cwd] || []) if (!t.subagent && !byId.has(id + t.id)) { t.sid = id; byId.set(id + t.id, t); }
  }
  const threads = [...byId.values()].sort((a, b) => b.recency - a.recency);
  const sids = [...new Set(threads.map((t) => t.sid))];
  $("p-title").textContent = base(S.cwd) || "Home";
  $("p-sub").innerHTML = sids.map((s) => { const c = conns.get(s); return `<span><span class="dot ${c && c.ok ? "on" : "off"}"></span> ${esc(serverName(s))}</span>`; }).join("") + `<span class="mono">${esc(S.cwd)}</span>`;
  const q = $("p-search").value.toLowerCase().trim();
  const rows = q ? threads.filter((t) => `${t.title} ${t.preview} ${t.lastMsg || ""}`.toLowerCase().includes(q)) : threads;
  $("project-list").innerHTML = rows.length ? `<div class="sec">Chats<span class="n">${rows.length}</span></div><div class="group">${rows.map((t) => threadRow(t, { project: false })).join("")}</div>`
    : `<div class="empty"><b>No Chats Here</b>Start one with New Chat.</div>`;
}
$("p-search").addEventListener("input", renderProject);
$("project-list").addEventListener("click", (e) => {
  const r = e.target.closest("[data-id]");
  if (r) openThread(r.dataset.sid, r.dataset.id, true);
});
$("p-new").onclick = () => {
  const t = allThreads().find((x) => x.cwd === S.cwd);
  openSheet("new", { cwd: S.cwd, sid: t ? t.sid : undefined });
};

// ----------------------------------------------------------------- thread --

function openThread(sidv, id, push) {
  if (!conns.has(sidv)) { toast("That Mac Is No Longer Paired"); return; }
  if (push) history.pushState({ view: "thread", sid: sidv, id, provider:S.provider }, "");
  if (S.view === "thread" && (S.sid !== sidv || S.id !== id)) leaveThread();
  if (S.id !== id) { S.open = new Set(); S.minCards = new Set(); S.attach = []; renderAttachments(); }
  resetResponses();
  clearImageCache();
  S.sid = sidv; S.id = id; S.meta = null; S.turns = []; S.stick = true;
  show("thread");
  renderThreadHeader();
  $("t-status").className = "status hidden";
  $("t-ring").innerHTML = "";
  $("feed").innerHTML = '<div class="empty">Loading</div>';
  $("dockcards").innerHTML = "";
  $("banner").classList.add("hidden");
  renderComposer();
  conn().send({ op: "open", id });
  if (Native) try { Native.setActiveThread(sidv, id, S.provider); } catch {}
}
function leaveThread() {
  cancelVoice();
  clearImageCache();
  resetResponses();
  if (S.id && conn()) conn().send({ op: "close", id: S.id });
  S.id = ""; S.meta = null; S.turns = [];
  if (Native) try { Native.setActiveThread("", "", ""); } catch {}
}
function renderThreadHeader() {
  const t = findThread(S.sid, S.id) || {};
  const m = S.meta || {};
  $("t-title").textContent = m.title || t.title || t.preview || "Thread";
  const c = conn();
  const cwd = m.cwd || t.cwd;
  const mac = S.servers.length > 1 ? `<span><span class="dot ${c && c.ok ? "on" : "off"}"></span> ${esc(serverName(S.sid))}</span>` : c && !c.ok ? '<span><span class="dot off"></span> Offline</span>' : "";
  $("t-sub").innerHTML = `<span class="provider-label">${S.provider==='claude'?'Claude Code':'Codex'}</span>` + (cwd ? `<span>${esc(base(cwd))}</span>` : "") + ((m.branch || t.branch) ? `<span class="mono">${esc(m.branch || t.branch)}</span>` : "") + mac;
}
$("menu-btn").onclick = () => openSheet("menu");

const feed = $("feed");
feed.addEventListener("scroll", () => {
  S.stick = feed.scrollHeight - feed.scrollTop - feed.clientHeight < 90;
  $("jump").classList.toggle("hidden", S.stick);
}, { passive: true });
$("jump-btn").onclick = () => { S.stick = true; feed.scrollTop = feed.scrollHeight; $("jump").classList.add("hidden"); };

function renderThread(keepPlace) {
  const fromBottom = feed.scrollHeight - feed.scrollTop;
  let h = S.meta && S.meta.hasOlder ? '<button class="load-earlier" id="earlier">Load Earlier</button>' : "";
  if (!S.turns.length) h += '<div class="empty">No Messages Yet</div>';
  resetResponses();
  feed.innerHTML = h;
  for (const t of S.turns) {
    const el = document.createElement("div");
    el.className = "turn"; el.dataset.key = t.key;
    feed.appendChild(el);
    renderTurn(el, t, false);
  }
  renderMeta();
  if (keepPlace) feed.scrollTop = feed.scrollHeight - fromBottom;
  else if (S.stick) feed.scrollTop = feed.scrollHeight;
}

// Turn updates arrive every 60 ms while streaming; render each turn once per frame.
const dirty = new Set();
let frame = 0;
function dirtyTurn(key) { dirty.add(key); if (!frame) frame = requestAnimationFrame(flushTurns); }
function flushTurns() {
  frame = 0;
  let cardsChanged = false;
  for (const key of dirty) {
    const t = S.turns.find((x) => x.key === key);
    if (!t) continue;
    let el = feed.querySelector(`.turn[data-key="${CSS.escape(key)}"]`);
    if (!el) {
      const empty = feed.querySelector(":scope > .empty");
      if (empty) empty.remove();
      el = document.createElement("div");
      el.className = "turn";
      el.dataset.key = key;
      feed.appendChild(el);
    }
    renderTurn(el, t, true);
    if (t === S.turns[S.turns.length - 1]) cardsChanged = true;
  }
  dirty.clear();
  // Drop optimistic bubbles once the real message is in the transcript.
  for (const p of feed.querySelectorAll(":scope > .u.pending")) {
    const last = S.turns[S.turns.length - 1];
    if (last && (last.items || []).some((i) => i && i.k === "user" && (i.text || "").startsWith(p.dataset.text))) {
      p.remove();
      feed.querySelector(":scope > .reply-wait-wrap")?.remove();
    }
  }
  if (cardsChanged) { renderCards(); renderStatus(); }
  if (S.stick) feed.scrollTop = feed.scrollHeight;
}

// Only incoming responses animate. History opens immediately, and no frame
// loop remains after the visible text catches up with the relay.
const responses = new Map();
let responseFrame = 0, responsePaintAt = 0;
function resetResponses() {
  cancelAnimationFrame(responseFrame); responseFrame = 0;
  responses.clear();
  dirty.clear();
}
function responseState(t, it, i, animate) {
  const key = t.key + ":" + (it.id || i);
  const target = it.text || "", now = performance.now();
  let r = responses.get(key);
  if (!r) {
    r = { key, shown: animate ? "" : target, target: "", plain: "", fades: [], el: null };
    responses.set(key, r);
  }
  if (r.target !== target) {
    r.target = target; r.from = r.shown.length; r.at = now;
    // Never type through a correction, stopped turn, or reduced-motion path.
    if (!animate || !target.startsWith(r.shown) || reducedMotion.matches || document.hidden || t.status === "interrupted") r.shown = target;
  }
  r.animate = animate;
  if (t.status === "interrupted" || reducedMotion.matches || document.hidden) r.shown = target;
  r.live = t.status === "inProgress" && i === (t.items || []).filter(Boolean).length - 1;
  if (r.shown !== r.target && !responseFrame) responseFrame = requestAnimationFrame(revealResponses);
  return r;
}
function paintResponse(r) {
  const el = r.el;
  if (!el) return;
  el.classList.toggle("live", r.live || r.shown !== r.target);
  if (el.dataset.shown === r.shown) return;
  el.dataset.shown = r.shown;
  el.innerHTML = md(r.shown);
  queueImages(el);
  const walker = document.createTreeWalker(el, NodeFilter.SHOW_TEXT);
  const nodes = [];
  while (walker.nextNode()) if (!walker.currentNode.parentElement.closest("button")) nodes.push(walker.currentNode);
  const plain = nodes.map((n) => n.data).join(""), now = performance.now();
  r.fades = plain.startsWith(r.plain) ? r.fades.filter((f) => now - f.at < 180) : [];
  if (r.animate && plain.startsWith(r.plain) && plain.length > r.plain.length && !reducedMotion.matches) r.fades.push({ start: r.plain.length, end: plain.length, at: now });
  r.plain = plain;
  let offset = 0;
  for (const node of nodes) {
    const text = node.data, end = offset + text.length;
    const fades = r.fades.filter((f) => f.end > offset && f.start < end);
    if (fades.length) {
      const frag = document.createDocumentFragment(); let pos = 0;
      for (const f of fades) {
        const start = Math.max(0, f.start - offset), stop = Math.min(text.length, f.end - offset);
        if (start > pos) frag.append(text.slice(pos, start));
        const span = document.createElement("span"); span.className = "stream-in";
        span.style.animationDelay = `-${Math.max(0, now - f.at)}ms`;
        span.textContent = text.slice(start, stop); frag.append(span); pos = stop;
      }
      if (pos < text.length) frag.append(text.slice(pos));
      node.replaceWith(frag);
    }
    offset = end;
  }
}
function revealResponses(now) {
  responseFrame = 0;
  let pending = false, changed = false;
  for (const r of responses.values()) {
    if (r.shown === r.target) continue;
    if (now - responsePaintAt >= 32 || document.hidden || reducedMotion.matches) {
      const progress = document.hidden || reducedMotion.matches ? 1 : Math.min(1, (now - r.at) / 220);
      let length = Math.min(r.target.length, Math.max(r.shown.length + 1, r.from + Math.ceil((r.target.length - r.from) * progress)));
      // Keep UTF-16 surrogate pairs intact, including emoji.
      if (length < r.target.length && /[\uDC00-\uDFFF]/.test(r.target[length])) length++;
      r.shown = r.target.slice(0, length); paintResponse(r); changed = true;
    }
    if (r.shown !== r.target) pending = true;
  }
  if (changed) { responsePaintAt = now; if (S.stick) feed.scrollTop = feed.scrollHeight; }
  if (pending) responseFrame = requestAnimationFrame(revealResponses);
}
function renderTurn(el, t, animate) {
  const old = new Map([...el.querySelectorAll("[data-response]")].map((p) => [p.dataset.response, p]));
  const template = document.createElement("template");
  template.innerHTML = turnHTML(t, animate);
  for (const next of template.content.querySelectorAll("[data-response]")) {
    const r = responses.get(next.dataset.response);
    const node = old.get(r.key) || next;
    if (node !== next) next.replaceWith(node);
    r.el = node; paintResponse(r);
  }
  el.replaceChildren(template.content);
  queueImages(el);
}
document.addEventListener("visibilitychange", () => {
  document.documentElement.classList.toggle("page-hidden", document.hidden);
  if (document.hidden) revealResponses(performance.now());
});
reducedMotion.addEventListener("change", () => { if (reducedMotion.matches) { show(S.view); revealResponses(performance.now()); } });
const waitingHTML = () => '<div class="reply-wait" role="status"><span class="thinking">Thinking</span></div>';

feed.addEventListener("click", (e) => {
  const t = e.target;
  const image = t.closest("[data-image]");
  if (image) { const img = image.querySelector("img"); openSheet("image", { src: image.dataset.image, path: img?.dataset.relayImage, server: img?.dataset.server }); return; }
  if (t.id === "earlier") { t.textContent = "Loading"; t.disabled = true; conn().send({ op: "more", id: S.id }); return; }
  if (t.classList.contains("copy")) { copyText(t.parentElement.querySelector("code").textContent); return; }
  const tog = t.closest("[data-tog]");
  if (tog) {
    const box = tog.parentElement, k = tog.dataset.tog;
    box.classList.toggle("open");
    if (box.classList.contains("open")) S.open.add(k); else S.open.delete(k);
    return;
  }
  const d = t.closest("[data-diff]");
  if (d) requestDiff(d.dataset.diff);
});

const firstLine = (s) => String(s || "").split("\n")[0];

// Step rows: one per command, edit, search, tool call or thought. Tap to expand.
function stepHTML(s, t, key, active = false) {
  if (hiddenTool(s)) return imagesHTML(s.images);
  const done = t.status && t.status !== "inProgress";
  const running = s.status === "inProgress" && !done;
  const open = S.open.has(key) ? " open" : "";
  let icon, label, body = "", x = "";
  switch (s.k) {
    case "cmd":
      icon = I.term; label = `<code>${esc(firstLine(s.title))}</code>`;
      body = `<pre>${s.body ? esc(s.body) : running ? "Running" : "No Output"}</pre>`;
      if (s.exit != null) x = `<span class="x ${s.exit === 0 ? "ok" : "bad"}">${s.exit === 0 ? "OK" : "Exit " + s.exit}</span>`;
      else if (running) x = '<span class="x run">Running</span>';
      else if (s.status === "inProgress" || s.status === "failed" || s.status === "declined") x = `<span class="x bad">${s.status === "declined" ? "Declined" : "Stopped"}</span>`;
      break;
    case "file": {
      const files = (s.title || "").split("\n").filter(Boolean);
      const lines = (s.body || "").split("\n");
      const add = lines.filter((l) => l.startsWith("+") && !l.startsWith("+++")).length, del = lines.filter((l) => l.startsWith("-") && !l.startsWith("---")).length;
      icon = I.edit; label = `Edited <code>${esc(files.map((f) => base(f.slice(2))).join(", "))}</code>`;
      x = `<span class="x ok">+${add}</span><span class="x bad">-${del}</span>`;
      body = `<pre>${diffLines(s.body || "")}</pre><button class="link-btn" data-diff="${esc(t.key)}">Open Full Diff</button>`;
      break;
    }
    case "reasoning": {
      const text = (s.text || "").trim();
      if (!text) return "";
      const head = text.match(/^\*\*(.+?)\*\*/);
      const rest = head ? text.slice(head[0].length).trim() : text;
      icon = I.think; label = esc(head ? head[1] : firstLine(text));
      body = rest ? `<div class="think md">${md(rest)}</div>` : "";
      break;
    }
    case "tool": icon = I.plug; label = `<code>${esc(s.title)}</code>`; body = s.body ? `<pre>${esc(s.body)}</pre>` : ""; if (running) x = '<span class="x run">Running</span>'; break;
    case "image": return `<div class="image-item"><div class="step-h"><span class="lbl">${esc(s.title || "Image")}</span></div>${imagesHTML(s.images)}</div>`;
    case "search": icon = I.globe; label = "Searched " + esc(s.title); break;
    case "agent": icon = I.agent; label = "Subagent " + esc(s.title); break;
    case "error": return `<div class="step err"><div class="step-h">${I.warn}<span class="lbl">${esc(s.text)}</span></div></div>`;
    case "info": return s.text ? `<div class="step info"><div class="step-h"><span class="lbl">${esc(s.text)}</span></div></div>` : "";
    default: return "";
  }
  const tog = body ? ` data-tog="${esc(key)}"` : "";
  return `<div class="step${open}"><button class="step-h"${tog}>${icon}<span class="lbl${running || active ? " step-progress" : ""}">${label}</span>${x}</button>${body ? `<div class="step-b">${body}</div>` : ""}${imagesHTML(s.images)}</div>`;
}

function hiddenTool(it) { return it?.k === "tool" && (it.title || "").replace(/__/g, ".").endsWith("cua_repl.js"); }
function turnHTML(t, animate = false) {
  const items = (t.items || []).filter((it) => it && (!hiddenTool(it) || it.images?.length));
  const running = t.status === "inProgress";
  const activeIndex = running ? items.findLastIndex((it) => it.k !== "user" && it.k !== "assistant" && !hiddenTool(it)) : -1;
  let out = t.startedAt ? `<div class="stamp">${new Date(t.startedAt).toLocaleString([], { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" })}</div>` : "";
  // Finished turns fold runs of more than three steps into one summary row.
  let run = [], ri = 0, userSeen = false;
  const flush = () => {
    if (!run.length) return;
    const rows = run.map(([s, i]) => stepHTML(s, t, t.key + ":" + i, running && i === activeIndex && items.at(-1)?.k !== "assistant")).join("");
    const n = run.filter(([s]) => s.k !== "reasoning" || s.text).length;
    if (!running && n > 3) {
      const k = t.key + ":g" + ri;
      const cmds = run.filter(([s]) => s.k === "cmd").length, edits = run.filter(([s]) => s.k === "file").length;
      const parts = [cmds && `${cmds} ${cmds === 1 ? "Command" : "Commands"}`, edits && `${edits} ${edits === 1 ? "Edit" : "Edits"}`].filter(Boolean);
      out += `<div class="steps${S.open.has(k) ? " open" : ""}"><button class="steps-sum" data-tog="${esc(k)}">${I.chev}${n} Steps${parts.length ? ", " + parts.join(", ") : ""}</button><div class="steps-body">${rows}</div></div>`;
    } else out += rows;
    run = []; ri++;
  };
  items.forEach((it, i) => {
    if (it.k === "user") {
      flush();
      out += `<div class="u${userSeen ? " steer" : ""}">${esc((it.text || "").replace(/\[Image\]/g, it.images?.length ? "" : "[Image]").trim())}${imagesHTML(it.images)}</div>`;
      userSeen = true;
    } else if (it.k === "assistant") {
      flush();
      const r = responseState(t, it, i, animate);
      out += `<div class="prose md" data-response="${esc(r.key)}"></div>`;
      out += imagesHTML(it.images);
    } else if (it.k === "image" || it.images?.length) { flush(); out += stepHTML(it, t, t.key + ":" + i); }
    else run.push([it, i]);
  });
  flush();
  if (running && !items.some((i) => i.k !== "user" && (i.text || i.title))) out += waitingHTML();
  const foot = [];
  if (!running && t.durationMs) foot.push(`<span>Worked For ${dur(t.durationMs)}</span>`);
  if (t.status === "interrupted") foot.push('<span class="err">Stopped</span>');
  if (t.status === "failed" || t.error) foot.push(`<span class="err">${esc(t.error || "Failed")}</span>`);
  if (t.hasDiff) foot.push(`<button class="link-btn" data-diff="${esc(t.key)}">View Changes</button>`);
  if (foot.length) out += `<div class="foot">${foot.join("")}</div>`;
  return out;
}

function diffLines(text) {
  return text.split("\n").map((l) => {
    const c = l.startsWith("+") && !l.startsWith("+++") ? "add" : l.startsWith("-") && !l.startsWith("---") ? "del" : l.startsWith("@@") ? "hunk" : "";
    return `<span class="dl ${c}">${esc(l) || " "}</span>`;
  }).join("");
}

function imageSrc(path) {
  if (/^https?:\/\//i.test(path)) return path;
  const c = conn();
  if (!c || !/^\//.test(path)) return "";
  return c.base + "/image?path=" + encodeURIComponent(path);
}
function imageHTML(path, alt = "Image") {
  path = path.replace(/^<|>$/g, "");
  const src = imageSrc(path);
  if (!src) return esc(alt);
  if (path.startsWith("/")) return `<button type="button" class="image-preview" disabled aria-label="Open Image"><img data-relay-image="${esc(path)}" data-server="${esc(S.sid)}" alt="${esc(alt)}" decoding="async"></button>`;
  return `<button type="button" class="image-preview" data-image="${esc(src)}" aria-label="Open Image"><img src="${esc(src)}" alt="${esc(alt)}" loading="lazy" decoding="async" referrerpolicy="no-referrer"></button>`;
}
function imagesHTML(paths) { return paths?.length ? `<div class="chat-images">${paths.map((p) => imageHTML(p)).join("")}</div>` : ""; }
// Chromium blocks plain-HTTP image elements in the HTTPS Android shell.
// Fetch authenticated bytes first, then display a local blob URL.
const imageCache = new Map(), observedImages = new Set();
let imageGeneration = 0;
const imageObserver = new IntersectionObserver((entries) => {
  for (const entry of entries) if (entry.isIntersecting) {
    imageObserver.unobserve(entry.target); observedImages.delete(entry.target);
    loadRelayImage(entry.target);
  }
}, { root: feed, rootMargin: "400px" });
function clearImageCache() {
  imageGeneration++;
  imageObserver.disconnect(); observedImages.clear();
  for (const entry of imageCache.values()) { entry.controller.abort(); if (entry.url) URL.revokeObjectURL(entry.url); }
  imageCache.clear();
}
function queueImages(root) {
  for (const img of observedImages) if (!img.isConnected) { imageObserver.unobserve(img); observedImages.delete(img); }
  for (const img of root.querySelectorAll("img[data-relay-image]:not([data-queued])")) {
    img.dataset.queued = "1"; observedImages.add(img); imageObserver.observe(img);
  }
}
async function loadRelayImage(img) {
  const generation = imageGeneration, server = img.dataset.server, path = img.dataset.relayImage;
  const key = server + ":" + path, c = conns.get(server);
  if (!c) return;
  let entry = imageCache.get(key);
  if (!entry) {
    entry = { controller: new AbortController(), url: "" };
    imageCache.set(key, entry);
    entry.promise = (async () => {
      const response = await fetch(c.base + "/image?path=" + encodeURIComponent(path), { headers: { Authorization: "Bearer " + c.s.token }, signal: entry.controller.signal });
      if (!response.ok) throw Error("Image Unavailable");
      const blob = await response.blob();
      if (generation !== imageGeneration) throw Error("Left Thread");
      entry.url = URL.createObjectURL(blob); return entry.url;
    })();
  }
  try {
    const url = await entry.promise;
    if (generation !== imageGeneration || !img.isConnected) return;
    img.src = url;
    const button = img.closest(".image-preview"); button.dataset.image = url; button.disabled = false;
  } catch {
    if (generation !== imageGeneration || !img.isConnected) return;
    const button = img.closest(".image-preview"); button.textContent = "Image Unavailable";
    imageCache.delete(key);
  }
}
feed.addEventListener("load", (e) => { if (e.target.tagName === "IMG" && S.stick) feed.scrollTop = feed.scrollHeight; }, true);
feed.addEventListener("error", (e) => {
  if (e.target.tagName === "IMG") { const button = e.target.closest(".image-preview"); if (button) { button.textContent = "Image Unavailable"; button.removeAttribute("data-image"); button.disabled = true; } }
}, true);

// Markdown: fenced code, inline code, bold, italics, links, headings, lists, quotes, tables.
function md(src) {
  const blocks = [];
  src = src.replace(/```[^\n]*\n([\s\S]*?)(```|$)/g, (_, code) => { blocks.push(code.replace(/\n$/, "")); return `\u0000${blocks.length - 1}\u0000`; });
  const inline = (s) => {
    const images = [];
    s = s.replace(/!\[([^\]]*)\]\(([^)\n]+)\)/g, (_, alt, path) => { images.push(imageHTML(path.trim(), alt || "Image")); return `\u0001${images.length - 1}\u0001`; });
    return esc(s)
    .replace(/`([^`\n]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
    .replace(/(^|[\s(])\*([^*\n]+)\*/g, "$1<i>$2</i>")
    .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>')
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, '<code title="$2">$1</code>')
    .replace(/\u0001(\d+)\u0001/g, (_, n) => images[+n] || "");
  };
  const lines = src.split("\n");
  let out = "", list = null, para = [];
  const endPara = () => { if (para.length) { out += `<p>${para.join("<br>")}</p>`; para = []; } };
  const endList = () => { if (list) { out += `</${list}>`; list = null; } };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const code = line.match(/^\u0000(\d+)\u0000$/);
    if (code) { endPara(); endList(); out += `<pre><button class="copy">Copy</button><code>${esc(blocks[+code[1]])}</code></pre>`; continue; }
    if (/^\s*\|.*\|\s*$/.test(line) && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1] || "")) {
      endPara(); endList();
      const cells = (l) => l.trim().replace(/^\||\|$/g, "").split("|").map((c) => inline(c.trim()));
      out += "<table><tr>" + cells(line).map((c) => `<th>${c}</th>`).join("") + "</tr>";
      i += 2;
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) { out += "<tr>" + cells(lines[i]).map((c) => `<td>${c}</td>`).join("") + "</tr>"; i++; }
      i--; out += "</table>";
      continue;
    }
    const li = line.match(/^\s*(?:[-*+]|(\d+)[.)])\s+(.*)$/);
    if (li) {
      endPara();
      const tag = li[1] ? "ol" : "ul";
      if (list !== tag) { endList(); out += `<${tag}>`; list = tag; }
      out += `<li>${inline(li[2])}</li>`;
      continue;
    }
    if (list && /^\s{2,}\S/.test(line)) { out = out.replace(/<\/li>$/, "<br>" + inline(line.trim()) + "</li>"); continue; }
    endList();
    const hd = line.match(/^(#{1,4})\s+(.*)$/);
    if (hd) { endPara(); out += `<h${hd[1].length}>${inline(hd[2])}</h${hd[1].length}>`; continue; }
    const bq = line.match(/^>\s?(.*)$/);
    if (bq) { endPara(); out += `<blockquote>${inline(bq[1])}</blockquote>`; continue; }
    if (line.trim() === "") { endPara(); continue; }
    para.push(inline(line));
  }
  endPara(); endList();
  return out;
}

// ------------------------------------------------------------- thread meta --

function renderStatus() {
  const m = S.meta;
  if (!m) return;
  const st = $("t-status");
  const need = (m.requests || []).length > 0 || !!pendingAsk();
  st.className = "status hidden";
  $("t-ring").classList.toggle("need", need);
}
function renderMeta() {
  const m = S.meta;
  if (!m) return;
  renderThreadHeader();
  renderStatus();
  const ring = $("t-ring");
  const known = m.ctxMax > 0 && m.ctxUsed != null;
  const f = known ? Math.max(0, Math.min(1, m.ctxUsed / m.ctxMax)) : 0, c = 2 * Math.PI * 10;
  ring.classList.toggle("hot", f > 0.8);
  ring.title = known ? `Context ${Math.round(f * 100)}% Used` : "Context Usage Unavailable";
  ring.setAttribute("aria-label", ring.title + ", Show Details");
  ring.innerHTML = `<svg viewBox="0 0 26 26"><circle class="bg" cx="13" cy="13" r="10"/>${known ? `<circle class="fg" cx="13" cy="13" r="10" stroke-dasharray="${(f * c).toFixed(1)} ${c.toFixed(1)}"/>` : ""}</svg>`;
  const b = $("banner");
  if (m.exec) {
    b.innerHTML = "<span>Starting Headless, Replies Queue</span>";
    b.classList.remove("hidden");
  } else if (m.mode !== "live") {
    b.innerHTML = `<span>${m.mode==='external'?'Managed On Mac':'Read Only'}</span><button class="btn" id="wake">${S.provider==='claude'?(m.mode==='external'?'Take Over':'Resume'):'Open On Mac'}</button>`;
    b.classList.remove("hidden");
    $("wake").onclick = async (e) => {
      e.target.disabled = true; e.target.textContent = "Opening";
      const a = await conn().call({ op: "wake", id: S.id }, 20000);
      if (!a.ok) toast(nice(a.msg));
      e.target.disabled = false; e.target.textContent = "Open On Mac";
    };
  } else b.classList.add("hidden");
  const earlier = $("earlier");
  if (earlier && !m.hasOlder) earlier.remove();
  renderCards();
  renderComposer();
}

// ------------------------------------------------- approvals and questions --
// Cards dock above the composer and can be minimized to read the thread.

// pendingAsk returns the question the assistant asked in the newest turn, if
// the user has not answered it yet (these questions do not block the turn).
function pendingAsk() {
  const t = S.turns[S.turns.length - 1];
  if (!t) return null;
  const items = (t.items || []).filter(Boolean);
  for (let i = items.length - 1; i >= 0; i--) {
    const it = items[i];
    if (it.k === "user" && i > 0) return null;
    if (it.k === "assistant" && it.asks && it.asks.length) return { item: it, key: t.key + ":" + it.id };
  }
  return null;
}

function cardHTML(key, title, sub, body) {
  const min = S.minCards.has(key);
  return `<div class="card${min ? " min" : ""}" data-card="${esc(key)}"><button class="card-h" data-min="${esc(key)}" aria-expanded="${!min}"><span class="dot"></span><span class="ttl">${esc(min && sub ? sub : title)}</span>${I.down}</button><div class="card-b">${body}</div></div>`;
}
let lastCards = "";
function renderCards() {
  const m = S.meta || {};
  let h = "";
  for (const q of m.requests || []) {
    const key = "r:" + q.id;
    if (q.kind === "command" || q.kind === "file") {
      const [cmd, ...why] = (q.body || "").split("\n\n");
      // A file request refers to the newest edit in the turn; show its diff.
      const edit = q.kind === "file" ? [...((S.turns[S.turns.length - 1] || {}).items || [])].reverse().find((i) => i && i.k === "file") : null;
      const files = edit ? (edit.title || "").split("\n").filter(Boolean).map((f) => base(f.slice(2))).join(", ") : "";
      const body = (q.kind === "command" ? `<pre>${esc(cmd)}</pre>${why.length ? `<div class="why">${esc(why.join("\n\n"))}</div>` : ""}`
        : `${q.body ? `<div class="why">${esc(q.body)}</div>` : ""}${edit ? `<pre>${diffLines(edit.body || "")}</pre>` : ""}`) +
        `<div class="btns"><button class="btn primary" data-req="${esc(q.id)}" data-d="accept">Approve</button><button class="btn" data-req="${esc(q.id)}" data-d="acceptForSession">Always</button><span class="grow"></span><button class="btn danger" data-req="${esc(q.id)}" data-d="decline">Decline</button></div>`;
      h += cardHTML(key, q.kind === "command" ? "Run Command" : "Apply Changes" + (files ? " To " + files : ""), q.kind === "command" ? firstLine(cmd) : files, body);
    } else if (q.kind === "question") {
      const body = (q.questions || []).map((x) => `<div class="qq" data-q="${esc(x.id)}">${x.header ? `<div class="q-h">${esc(x.header)}</div>` : ""}<div class="q">${esc(x.question)}</div>` +
        `<div class="opts">${(x.options || []).map((o, i) => `<button class="opt" data-pick="${esc(o)}"><span class="k">${i + 1}</span>${esc(o)}</button>`).join("")}</div>` +
        (x.other || !(x.options || []).length ? `<div class="other" style="margin-bottom:10px"><input data-other placeholder="Your Answer" type="${x.secret ? "password" : "text"}"></div>` : "") + "</div>").join("") +
        `<div class="btns"><button class="btn primary" data-answer="${esc(q.id)}">Send Answer</button></div>`;
      const q0 = (q.questions && q.questions[0]) || {};
      h += cardHTML(key, (q.questions || []).length === 1 ? q0.question : "Questions", q0.question || "", body.replace(`<div class="q">${esc(q0.question)}</div>`, (q.questions || []).length === 1 ? "" : `<div class="q">${esc(q0.question)}</div>`));
    } else {
      h += cardHTML(key, q.title || "Request", "", `<div class="why">Answer This One On The Mac</div><pre>${esc(q.body || "")}</pre>`);
    }
  }
  const ask = pendingAsk();
  if (ask) {
    const a = ask.item.asks[0];
    const body = `<div class="opts">${(a.options || []).map((o, i) => `<button class="opt" data-say="${esc(o)}"><span class="k">${i + 1}</span>${esc(o)}</button>`).join("")}</div>` +
      `<div class="other"><input id="ask-other" placeholder="Something Else" enterkeyhint="send"><button class="round send" data-sayother="1" aria-label="Send">${svg('<path d="M12 19V5M6 11l6-6 6 6"/>')}</button></div>`;
    h += cardHTML("a:" + ask.key, a.title, a.title, body);
  }
  // Re-rendering would wipe a half-typed answer, so skip identical markup.
  if (h === lastCards) return;
  lastCards = h;
  $("dockcards").innerHTML = h;
}
$("dockcards").addEventListener("click", async (e) => {
  const t = e.target;
  const mn = t.closest("[data-min]");
  if (mn) {
    const k = mn.dataset.min;
    if (S.minCards.has(k)) S.minCards.delete(k); else S.minCards.add(k);
    renderCards();
    return;
  }
  const say = t.closest("[data-say]");
  if (say) { sendText(say.dataset.say); return; }
  if (t.closest("[data-sayother]")) { const v = $("ask-other").value.trim(); if (v) sendText(v); return; }
  const pick = t.closest("[data-pick]");
  if (pick) {
    const qq = pick.closest(".qq");
    for (const b of qq.querySelectorAll(".opt")) b.classList.toggle("on", b === pick);
    qq.dataset.value = pick.dataset.pick;
    return;
  }
  const card = t.closest(".card");
  const q = card && ((S.meta && S.meta.requests) || []).find((r) => "r:" + r.id === card.dataset.card);
  if (!q) return;
  const dec = t.closest("[data-d]");
  if (dec) {
    buzz();
    card.style.opacity = 0.5;
    const a = await conn().call({ op: "approve", id: S.id, reqId: q.id, method: q.method, decision: dec.dataset.d });
    if (!a.ok) { card.style.opacity = 1; toast(nice(a.msg)); } else toast(dec.dataset.d === "decline" ? "Declined" : "Approved");
    return;
  }
  if (t.closest("[data-answer]")) {
    const answers = {};
    for (const el of card.querySelectorAll(".qq")) {
      const other = el.querySelector("[data-other]");
      const v = (other && other.value.trim()) || el.dataset.value;
      answers[el.dataset.q] = v ? [v] : [];
    }
    buzz();
    card.style.opacity = 0.5;
    const a = await conn().call({ op: "answer", id: S.id, reqId: q.id, answers });
    if (!a.ok) { card.style.opacity = 1; toast(nice(a.msg)); } else toast("Answer Sent");
  }
});
$("dockcards").addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.id === "ask-other") { e.preventDefault(); const v = e.target.value.trim(); if (v) sendText(v); }
});

// --------------------------------------------------------------- composer --

// Permission modes, as the desktop app presents them.
const PERMS = [
  { id: "default", icon: I.shield, name: "Default", short: "Default", d: "Sandboxed, asks to go further", set: { approvalPolicy: "on-request", sandbox: "workspaceWrite", reviewer: "user" } },
  { id: "auto", icon: I.shieldok, name: "Auto Review", short: "Auto Review", d: "A reviewer agent decides requests", set: { approvalPolicy: "on-request", sandbox: "workspaceWrite", reviewer: "auto_review" } },
  { id: "read", icon: I.lock, name: "Read Only", short: "Read Only", d: "Asks before edits and commands", set: { approvalPolicy: "on-request", sandbox: "readOnly", reviewer: "user" } },
  { id: "full", icon: I.rocket, name: "Full Access", short: "Full Access", d: "No sandbox, never asks", set: { approvalPolicy: "never", sandbox: "dangerFullAccess", reviewer: "user" } },
];
const CLAUDE_PERMS = [
 {id:'default',icon:I.shield,name:'Default',set:{permissionMode:'default'}},
 {id:'acceptEdits',icon:I.edit,name:'Accept Edits',set:{permissionMode:'acceptEdits'}},
 {id:'plan',icon:I.lock,name:'Plan Mode',set:{permissionMode:'plan'}},
 {id:'auto',icon:I.shieldok,name:'Auto Review',set:{permissionMode:'auto'}},
 {id:'bypassPermissions',icon:I.rocket,name:'Full Access',set:{permissionMode:'bypassPermissions'}}
];
function permOf(m) {
 if(S.provider==='claude')return CLAUDE_PERMS.find(p=>p.id===(m?.permissionMode||'default'))||CLAUDE_PERMS[0];
  if (!m || !m.approval) return null;
  if (m.approval === "never" && m.sandbox === "dangerFullAccess") return PERMS[3];
  if (m.reviewer === "auto_review") return PERMS[1];
  if (m.sandbox === "readOnly") return PERMS[2];
  if (m.sandbox === "workspaceWrite") return PERMS[0];
  return { id: "custom", icon: I.gear, name: "Custom", short: "Custom", d: `${m.approval}, ${m.sandbox || "default sandbox"}` };
}
function shortModel(slug) { return String(slug || "").replace(/^(gpt-|claude-)/i, "").replace(/(\d+)-(\d+)(?:-\d{8})?$/, "$1.$2").replace(/-/g, " ").replace(/\b\w/g, (c) => c.toUpperCase()); }
const EFFORT_D = { minimal: "Quickest Replies", low: "Fast Replies", medium: "Balanced", high: "More Thorough", xhigh: "Deep Reasoning", max: "Maximum Reasoning", ultra: "Several Agents In Parallel" };
function effortName(e) { return { xhigh: "Extra High", low: "Low", medium: "Medium", high: "High", max: "Max", ultra: "Ultra", minimal: "Minimal" }[e] || cap(e || ""); }
function cap(s) { return String(s).replace(/(^|[\s-])\w/g, (c) => c.toUpperCase()); }

const input = $("input");
function renderComposer() {
  const m = S.meta || {};
  const running = !!m.running;
  $("voice").classList.toggle("hidden", !Native || !Native.startVoice);
  $("stop").classList.toggle("hidden", !running);
  const sendBtn = $("send");
  sendBtn.classList.toggle("steer", running && !m.exec);
  sendBtn.setAttribute("aria-label", running ? (m.exec || S.provider==='claude' ? "Queue" : "Steer") : "Send");
  input.placeholder = running ? (m.exec || S.provider==='claude' ? 'Queue a Follow-Up' : 'Steer') : (S.provider==='claude' ? "Message Claude" : "Message Codex");
  const p = permOf(m);
  const pc = $("perm-chip");
  pc.innerHTML = p ? p.icon : I.shield;
  pc.className = "tool-chip icon" + (p && (p.id === "full" || p.id === "bypassPermissions") ? " warn" : "");
  pc.setAttribute("aria-label", "Permissions: " + (p ? p.name : "Unknown"));
  $("model-chip").innerHTML = `<b>${m.model ? esc(shortModel(m.model)) : "Model"}</b>${I.down}`;
  const mm = ((conn() && conn().models && conn().models.models) || []).find((x) => x.slug === m.model || x.resolved === m.model);
  const efforts = mm ? mm.efforts : ["low", "medium", "high", "xhigh", "max", "ultra"];
  const lvl = Math.max(0, efforts.indexOf(m.effort));
  $("effort-btn").innerHTML = gauge(efforts.length > 1 ? lvl / (efforts.length - 1) : 1);
  $("effort-btn").setAttribute("aria-label", "Intelligence: " + (m.effort ? effortName(m.effort) : "Unknown"));
  if (!mm && conn() && !conn().models) conn().send({ op: "models" });
}
function autosize() { input.style.height = "auto"; input.style.height = Math.min(input.scrollHeight, innerHeight * 0.38) + "px"; }
input.addEventListener("input", autosize);
input.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey && matchMedia("(pointer: fine)").matches) { e.preventDefault(); $("composer").requestSubmit(); }
});
$("composer").addEventListener("submit", (e) => { e.preventDefault(); sendText(input.value.trim()); });
$("perm-chip").onclick = () => openSheet("perms");
$("model-chip").onclick = () => openSheet("model");
$("effort-btn").onclick = () => openSheet("effort");
$("t-ring").onclick = () => openSheet("context");

// Audio stays on the phone until Transcribe is pressed. The transcript is a
// draft in the composer, never a message sent automatically to Codex.
const voiceState = { phase: "idle", id: "", server: null, base: "", levels: Array(40).fill(0.08) };
function renderVoice() {
  const active = voiceState.phase !== "idle";
  $("voice-panel").classList.toggle("hidden", !active);
  input.classList.toggle("hidden", active);
  $("composer").querySelector(".tools").classList.toggle("hidden", active);
  $("voice-label").textContent = voiceState.phase === "recording" ? "Listening" : voiceState.phase === "transcribing" ? "Transcribing" : "Starting";
  $("voice-transcribe").disabled = voiceState.phase !== "recording";
  $("voice-wave").classList.toggle("transcribing", voiceState.phase === "transcribing");
  $("voice-spinner").classList.toggle("hidden", voiceState.phase !== "transcribing");
}
function cancelVoice() {
  if (voiceState.phase === "idle") return;
  voiceState.phase = "idle"; voiceState.id = "";
  if (Native?.cancelVoice) Native.cancelVoice();
  renderVoice();
}
async function speechCapability(c) {
  if (c.speech && Date.now() - c.speechAt < (c.speech.available ? 30000 : 2000)) return c.speech;
  try {
    const r = await fetch(c.base + "/transcribe", { headers: { Authorization: "Bearer " + c.s.token }, signal: AbortSignal.timeout(5000) });
    c.speech = r.ok ? await r.json() : { available: false };
  } catch { c.speech = { available: false }; }
  c.speechAt = Date.now(); return c.speech;
}
$("voice").onclick = async () => {
  if (!Native?.startVoice || voiceState.phase !== "idle") return;
  voiceState.id = sid(); const request = voiceState.id;
  voiceState.phase = "preparing"; renderVoice();
  const candidates = [conn(), ...conns.values()].filter((c, i, all) => c?.ok && all.indexOf(c) === i);
  let c = candidates.find((candidate) => candidate.speech?.available);
  if (!c) {
    try { c = await Promise.any(candidates.map(async (candidate) => { if (!(await speechCapability(candidate)).available) throw Error("Unavailable"); return candidate; })); } catch {}
  }
  if (voiceState.id !== request) return;
  if (!c) { cancelVoice(); toast("No Paired Mac Has a Local Speech Model"); return; }
  voiceState.server = c.s; voiceState.base = c.base;
  voiceState.levels.fill(0.08);
  $("voice-time").textContent = "0:00";
  $("voice-wave").innerHTML = voiceState.levels.map(() => '<span style="transform:scaleY(0.08)"></span>').join("");
  buzz(); Native.startVoice(request);
};
$("voice-cancel").onclick = cancelVoice;
$("voice-transcribe").onclick = () => {
  if (voiceState.phase !== "recording") return;
  voiceState.phase = "transcribing"; renderVoice();
  Native.stopVoice(voiceState.server.id, voiceState.base);
};
window.onVoiceEvent = (e) => {
  if (e.id !== voiceState.id || voiceState.phase === "idle") return;
  if (e.type === "recording") { voiceState.phase = "recording"; renderVoice(); }
  else if (e.type === "level") {
    if (voiceState.phase !== "recording") return;
    voiceState.levels.shift(); voiceState.levels.push(Math.max(0.08, Math.min(1, Math.sqrt(e.level) * 2.4)));
    [...$("voice-wave").children].forEach((bar, i) => { bar.style.transform = `scaleY(${voiceState.levels[i]})`; });
    const sec = Math.floor(e.seconds); $("voice-time").textContent = `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, "0")}`;
  } else if (e.type === "transcript") {
    const text = (e.text || "").trim();
    input.value = [input.value.trimEnd(), text].filter(Boolean).join(" ");
    voiceState.phase = "idle"; voiceState.id = ""; renderVoice(); autosize(); input.focus();
    mark("voiceTranscript");
    window.lastVoiceTiming = { serverMs: e.durationMs, totalMs: e.totalMs };
  } else if (e.type === "error") { cancelVoice(); toast(nice(e.message)); }
};
window.onImageDownload = (e) => {
  if (S.sheet === "image") $("sheet").querySelector('[data-x="download"]').disabled = false;
  toast(e.message || (e.ok ? "Image Saved" : "Could Not Download Image"));
};
document.addEventListener("visibilitychange", () => {
  // The microphone permission dialog can briefly pause the WebView.
  if (document.hidden && voiceState.phase !== "preparing") cancelVoice();
});

// gauge draws a speedometer whose arc and needle show the intelligence level (0..1).
function gauge(f) {
  const r = 8, c = 2 * Math.PI * r, arc = c * 0.75;
  const a = (135 + 270 * f) * Math.PI / 180;
  return `<svg class="ico" viewBox="0 0 24 24"><circle cx="12" cy="13" r="${r}" fill="none" stroke="currentColor" stroke-opacity=".3" stroke-dasharray="${arc.toFixed(2)} ${c.toFixed(2)}" transform="rotate(135 12 13)"/>` +
    `<circle class="g-on" cx="12" cy="13" r="${r}" fill="none" stroke-dasharray="${Math.max(0.01, arc * f).toFixed(2)} ${c.toFixed(2)}" transform="rotate(135 12 13)"/>` +
    `<path d="M12 13L${(12 + 5 * Math.cos(a)).toFixed(2)} ${(13 + 5 * Math.sin(a)).toFixed(2)}"/></svg>`;
}

async function sendText(text) {
  if (voiceState.phase !== "idle") return;
  if (!S.id) return;
  if (S.attach.some((a) => a.uploading)) { toast("Waiting For Images To Upload"); return; }
  const images = S.attach.map((a) => a.path);
  if (!text && !images.length) return;
  const steering = S.meta && S.meta.running;
  buzz();
  // Show the message right away; the real one replaces it when the Mac echoes it.
  const pending = document.createElement("div");
  pending.className = "u pending" + (steering ? " steer" : "");
  pending.textContent = text + (images.length ? "\n[Image]" : "");
  pending.dataset.text = text;
  const empty = feed.querySelector(":scope > .empty");
  if (empty) empty.remove();
  feed.appendChild(pending);
  const waiting = !steering ? document.createElement("div") : null;
  if (waiting) { waiting.innerHTML = waitingHTML(); waiting.className = "reply-wait-wrap"; feed.appendChild(waiting); }
  feed.scrollTop = feed.scrollHeight;
  $("send").disabled = true;
  const fromInput = input.value.trim() === text;
  const keep = input.value;
  if (fromInput) { input.value = ""; autosize(); }
  S.stick = true;
  const a = await conn().call({ op: "send", id: S.id, text, images }, 40000);
  $("send").disabled = false;
  if (!a.ok) { pending.remove(); waiting?.remove(); if (fromInput) { input.value = keep; autosize(); } toast(nice(a.msg)); return; }
  S.attach = []; renderAttachments();
  if (a.queued) toast("Queued");
}
$("stop").onclick = async () => {
  buzz(20);
  const a = await conn().call({ op: "stop", id: S.id });
  toast(a.ok ? "Stopped" : nice(a.msg));
};

// applySettings changes thread settings right away, like the desktop menus.
async function applySettings(ch, label) {
  const m = S.meta || {};
  if (m.mode !== "live") toast("Opening On Mac");
  const a = await conn().call({ op: "settings", id: S.id, ...ch }, 30000);
  toast(a.ok ? label : nice(a.msg));
}

// Image attachments upload straight to the relay, downscaled on the phone.
let fileTarget = "thread";
$("attach").onclick = () => { fileTarget = "thread"; $("file").click(); };
$("file").addEventListener("change", (e) => {
  const files = [...e.target.files];
  e.target.value = "";
  for (const f of files) addAttachment(f, fileTarget);
});
async function addAttachment(file, target) {
  const a = { url: URL.createObjectURL(file), uploading: true, path: "" };
  const list = target === "new" ? S.newAttach : S.attach;
  const sidv = target === "new" ? S.newDraft && S.newDraft.sid : S.sid;
  const s = S.servers.find((x) => x.id === sidv);
  if (!s) return;
  list.push(a);
  redrawAttachments(target);
  try {
    const blob = await shrink(file);
    const r = await fetch(conns.get(s.id).base + "/upload", { method: "POST", headers: { Authorization: "Bearer " + s.token, "Content-Type": blob.type || "image/jpeg" }, body: blob });
    if (!r.ok) throw new Error(await r.text());
    a.path = (await r.json()).path;
    a.uploading = false;
  } catch (err) {
    list.splice(list.indexOf(a), 1);
    toast("Upload Failed: " + String(err.message || err).slice(0, 80));
  }
  redrawAttachments(target);
}
function shrink(file) {
  return new Promise((res) => {
    if (!/^image\/(jpeg|png|webp)$/.test(file.type) || file.size < 400e3) { res(file); return; }
    const img = new Image();
    img.onload = () => {
      const k = Math.min(1, 2048 / Math.max(img.width, img.height));
      const c = document.createElement("canvas");
      c.width = Math.round(img.width * k); c.height = Math.round(img.height * k);
      c.getContext("2d").drawImage(img, 0, 0, c.width, c.height);
      c.toBlob((b) => res(b || file), "image/jpeg", 0.86);
    };
    img.onerror = () => res(file);
    img.src = URL.createObjectURL(file);
  });
}
const attHTML = (list) => list.map((a, i) => `<div class="att${a.uploading ? " loading" : ""}"><img src="${a.url}" alt=""><button type="button" data-rm="${i}" aria-label="Remove Image">&times;</button></div>`).join("");
function renderAttachments() { $("attachments").innerHTML = attHTML(S.attach); }
function redrawAttachments(target) {
  if (target === "new") { const el = document.getElementById("nt-att"); if (el) el.innerHTML = attHTML(S.newAttach); }
  else renderAttachments();
}
$("attachments").addEventListener("click", (e) => {
  const b = e.target.closest("[data-rm]");
  if (b) { S.attach.splice(+b.dataset.rm, 1); renderAttachments(); }
});

// ------------------------------------------------------------------- diff --

function requestDiff(key) {
  openSheet("diff", {});
  conn().send({ op: "diff", id: S.id, key: key || "" });
}
function showDiff(m) {
  if (S.sheet !== "diff") return;
  const text = m.diff || "";
  const cwd = (S.meta && S.meta.cwd) || "";
  const files = [];
  let h = "";
  for (const l of text.split("\n")) {
    if (l.startsWith("+++") || l.startsWith("diff --git")) continue;
    if (l.startsWith("--- ")) {
      let name = l.slice(4).replace(/^a\//, "");
      if (cwd && name.startsWith(cwd + "/")) name = name.slice(cwd.length + 1);
      files.push(name);
      h += `<div class="dl file" id="df${files.length - 1}">${esc(name)}</div>`;
      continue;
    }
    const c = l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : l.startsWith("@@") ? "hunk" : "";
    h += `<div class="dl ${c}">${esc(l) || " "}</div>`;
  }
  const sh = $("sheet");
  const fl = sh.querySelector(".diff-files");
  fl.innerHTML = files.length > 1 ? files.map((f, i) => `<button class="chip" data-f="${i}">${esc(base(f))}</button>`).join("") : "";
  fl.classList.toggle("hidden", files.length < 2);
  sh.querySelector(".diff-body").innerHTML = text ? h : '<div class="empty"><b>No Changes</b>This turn did not change any files.</div>';
  const lines = text.split("\n");
  const adds = lines.filter((l) => l.startsWith("+") && !l.startsWith("+++")).length, dels = lines.filter((l) => l.startsWith("-") && !l.startsWith("---")).length;
  sh.querySelector("h2").textContent = `${files.length} ${files.length === 1 ? "File" : "Files"}, +${adds} -${dels}`;
}

// ----------------------------------------------------------------- sheets --

let sheetGeneration = 0;
function openSheet(name, opts = {}) {
  sheetGeneration++;
  if (!S.sheet) history.pushState({ ...(history.state || {}), sheet: name }, "");
  else history.replaceState({ ...(history.state || {}), sheet: name }, "");
  S.sheet = name; S.redraw = null;
  const sh = $("sheet");
  for (const animation of sh.getAnimations()) animation.cancel();
  sh.inert = false;
  $("scrim").style.pointerEvents = "";
  sh.className = "sheet" + (name === "diff" || name === "image" ? " full" : name === "effort" ? " effort" : "");
  sh.style.bottom = name === "effort" ? `${$("composer").offsetHeight + 24}px` : "";
  sh.onclick = null;
  $("scrim").classList.toggle("dark", name === "effort");
  $("scrim").classList.remove("hidden");
  SHEETS[name](sh, opts);
  if (S.sheet === name) sh.classList.remove("hidden");
}
function closeSheet(fromPop) {
  if (!S.sheet) return;
  S.sheet = null; S.redraw = null;
  const generation = ++sheetGeneration, sh = $("sheet"), scrim = $("scrim");
  sh.inert = true; scrim.style.pointerEvents = "none";
  const hide = () => { if (generation === sheetGeneration) { sh.classList.add("hidden"); scrim.classList.add("hidden"); } };
  if (reducedMotion.matches || document.hidden) hide();
  else {
    const transform = getComputedStyle(sh).transform;
    for (const animation of sh.getAnimations()) animation.cancel();
    sh.animate([{ transform, opacity: 1 }, { transform: sh.classList.contains("full") ? "translateX(32px)" : "translateY(20px)", opacity: 0 }], { duration: 130, easing: "ease-in", fill: "forwards" }).finished.then(hide, () => {});
  }
  if (!fromPop && history.state && history.state.sheet) history.back();
}
$("scrim").onclick = () => closeSheet();
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && S.sheet) closeSheet(); });
const mi = (act, icon, name, d = "", side = "", cls = "") => `<button class="mi ${cls}" data-act="${act}">${icon || ""}<span class="txt">${name}${d ? `<span class="d">${d}</span>` : ""}</span>${side}</button>`;

const SHEETS = {
  image(sh, { src, path, server }) {
    sh.innerHTML = `<header class="diff-head"><button class="icon-btn" data-x="close" aria-label="Back">${I.chev}</button><h2>Image</h2><button class="icon-btn" data-x="download" aria-label="Download Image">${I.download}</button></header><div class="image-stage"><img src="${esc(src)}" alt="Image" draggable="false" referrerpolicy="no-referrer"></div><div class="image-zoom"><button class="icon-btn" data-x="minus" aria-label="Zoom Out">${I.minus}</button><button class="chip" data-x="fit">Fit</button><button class="icon-btn" data-x="plus" aria-label="Zoom In">${I.plus}</button></div>`;
    const stage = sh.querySelector(".image-stage"), img = stage.querySelector("img"), fit = sh.querySelector('[data-x="fit"]');
    const pointers = new Map(); let scale = 1, x = 0, y = 0, previous = null;
    const clamp = () => {
      const maxX = Math.max(0, (img.offsetWidth * scale - stage.clientWidth) / 2), maxY = Math.max(0, (img.offsetHeight * scale - stage.clientHeight) / 2);
      x = Math.max(-maxX, Math.min(maxX, x)); y = Math.max(-maxY, Math.min(maxY, y));
      img.style.transform = `translate(${x}px, ${y}px) scale(${scale})`;
      fit.textContent = scale === 1 ? "Fit" : Math.round(scale * 100) + "%";
    };
    const zoom = (next, px = 0, py = 0) => { next = Math.max(1, Math.min(8, next)); const ratio = next / scale; x = px - (px - x) * ratio; y = py - (py - y) * ratio; scale = next; clamp(); };
    const gesture = () => {
      const p = [...pointers.values()];
      if (!p.length) return null;
      if (p.length === 1) return { x: p[0].x, y: p[0].y, distance: 0 };
      return { x: (p[0].x + p[1].x) / 2, y: (p[0].y + p[1].y) / 2, distance: Math.hypot(p[0].x - p[1].x, p[0].y - p[1].y) };
    };
    stage.onpointerdown = (e) => { e.preventDefault(); stage.setPointerCapture(e.pointerId); pointers.set(e.pointerId, { x: e.clientX, y: e.clientY }); previous = gesture(); };
    stage.onpointermove = (e) => {
      if (!pointers.has(e.pointerId)) return;
      pointers.set(e.pointerId, { x: e.clientX, y: e.clientY }); const next = gesture();
      if (previous?.distance && next.distance) { const r = stage.getBoundingClientRect(); zoom(scale * next.distance / previous.distance, next.x - r.left - r.width / 2, next.y - r.top - r.height / 2); }
      if (previous) { x += next.x - previous.x; y += next.y - previous.y; clamp(); }
      previous = next;
    };
    stage.onpointerup = stage.onpointercancel = (e) => { pointers.delete(e.pointerId); previous = gesture(); };
    stage.ondblclick = (e) => { const r = stage.getBoundingClientRect(); zoom(scale > 1 ? 1 : 2, e.clientX - r.left - r.width / 2, e.clientY - r.top - r.height / 2); };
    stage.onwheel = (e) => { e.preventDefault(); const r = stage.getBoundingClientRect(); zoom(scale * Math.exp(-e.deltaY * 0.002), e.clientX - r.left - r.width / 2, e.clientY - r.top - r.height / 2); };
    img.onload = clamp;
    sh.onclick = (e) => {
      const button = e.target.closest("[data-x]"); if (!button) return;
      const action = button.dataset.x;
      if (action === "close") closeSheet();
      else if (action === "plus") zoom(scale * 1.5);
      else if (action === "minus") zoom(scale / 1.5);
      else if (action === "fit") { x = y = 0; zoom(1); }
      else if (action === "download") {
        if (Native?.downloadImage && path && server) { button.disabled = true; Native.downloadImage(server, conns.get(server).base, path); }
        else if (Native?.downloadRemoteImage && /^https:\/\//.test(src)) { button.disabled = true; Native.downloadRemoteImage(src); }
        else { const a = document.createElement("a"); a.href = src; a.download = path ? base(path) : "Image.png"; a.click(); }
      }
    };
  },
  context(sh) {
    const m = S.meta || {}, known = m.ctxMax > 0 && m.ctxUsed != null;
    const last = S.turns[S.turns.length - 1];
    sh.innerHTML = '<div class="grip"></div><h2>Context And Activity</h2>' +
      `<div class="mi"><span class="txt">Context Used<span class="d">${known ? `${Math.round(m.ctxUsed / m.ctxMax * 100)}% (${m.ctxUsed.toLocaleString()} of ${m.ctxMax.toLocaleString()} Tokens)` : "Unavailable"}</span></span></div>` +
      `<div class="mi"><span class="txt">${m.running ? "Running" : "Last Turn"}<span class="d">${m.running && m.turnAt ? `<span data-since="${m.turnAt}">${dur(Date.now() - m.turnAt)}</span>` : last?.durationMs ? dur(last.durationMs) : "No Duration Available"}</span></span></div>`;
  },
  menu(sh) {
    const m = S.meta || {};
    const t = findThread(S.sid, S.id);
    sh.innerHTML = `<div class="grip"></div>` +
      mi("diff", I.diff, "View Latest Changes") +
      mi("compact", I.squash, "Compact Context", m.ctxMax ? `${Math.round((m.ctxUsed || 0) / m.ctxMax * 100)}% Used` : "") +
      (m.mode !== "live" ? mi("wake", I.mac, S.provider==='claude' ? (m.mode==='external'?'Take Over':'Resume') : "Open On Mac") : "") +
      mi("copy", I.copy, "Copy Thread ID") +
      (Native ? mi("share", I.share, "Share Last Reply") : "") +
      mi("archive", I.box, t ? "Archive Thread" : "Unarchive Thread", "", "", "danger");
    sh.onclick = async (e) => {
      const b = e.target.closest("[data-act]");
      if (!b) return;
      const act = b.dataset.act, c = conn();
      if (act === "diff") { openSheet("diff", {}); c.send({ op: "diff", id: S.id, key: "" }); return; }
      closeSheet();
      if (act === "copy") copyText(S.id);
      if (act === "share") {
        const last = [...S.turns].reverse().flatMap((t) => [...(t.items || [])].reverse()).find((i) => i && i.k === "assistant" && i.text);
        if (last) try { Native.share(last.text); } catch {}
      }
      if (act === "wake") { toast("Opening On Mac"); const a = await c.call({ op: "wake", id: S.id }, 20000); if (!a.ok) toast(nice(a.msg)); }
      if (act === "compact") { const a = await c.call({ op: "compact", id: S.id }, 60000); toast(a.ok ? "Compacting" : nice(a.msg)); }
      if (act === "archive") {
        const op = t ? "archive" : "unarchive", id = S.id;
        const a = await c.call({ op, id }, 30000);
        if (!a.ok) { toast(nice(a.msg)); return; }
        toast(op === "archive" ? "Archived, Tap To Undo" : "Unarchived", op === "archive" ? () => c.call({ op: "unarchive", id }) : null);
        if (op === "archive") history.back();
      }
    };
  },

  perms(sh) {
    const cur = permOf(S.meta);
    sh.innerHTML = `<div class="grip"></div><h2>Permissions</h2>` +
      (S.provider==='claude'?CLAUDE_PERMS:PERMS).map((p) => mi(p.id, p.icon, p.name, p.d, cur && cur.id === p.id ? I.check : "", p.id === "full" ? "warn" : "")).join("") +
      (cur && cur.id === "custom" ? mi("custom", I.gear, "Custom", esc(cur.d), I.check) : "");
    sh.onclick = (e) => {
      const b = e.target.closest("[data-act]");
      if (!b) return;
      const p = (S.provider==='claude'?CLAUDE_PERMS:PERMS).find((x) => x.id === b.dataset.act);
      closeSheet();
      if (p && (!cur || cur.id !== p.id)) applySettings(p.set, "Permissions: " + p.name);
    };
  },

  model(sh) {
    const draw = () => {
      const m = S.meta || {};
      const models = (conn().models && conn().models.models) || [];
      sh.innerHTML = `<div class="grip"></div><h2>Model</h2>` +
        (models.length ? models.map((x) => mi("m:" + x.slug, "", esc(x.name), esc(x.description || ""), (m.model === x.slug || m.model === x.resolved) ? I.check : "")).join("") : '<p class="hint">Loading Models</p>');
    };
    S.redraw = draw;
    if (!conn().models) conn().send({ op: "models" });
    draw();
    sh.onclick = (e) => {
      const b = e.target.closest("[data-act]");
      if (!b) return;
      const slug = b.dataset.act.slice(2);
      closeSheet();
      const m = S.meta || {};
      if (slug === m.model) return;
      const mm = ((conn().models && conn().models.models) || []).find((x) => x.slug === slug);
      const ch = { model: slug };
      // Keep the intelligence level when the new model supports it.
      if (mm && m.effort && !mm.efforts.includes(m.effort)) ch.effort = mm.defaultEffort;
      applySettings(ch, "Model: " + (mm ? mm.name : shortModel(slug)));
    };
  },

  // Intelligence: a large segmented slider floating above the composer, as in
  // the ChatGPT app. Drag or tap a stop; the level applies on release.
  effort(sh) {
    const m = S.meta || {};
    const mm = ((conn().models && conn().models.models) || []).find((x) => x.slug === m.model || x.resolved === m.model);
    const efforts = mm ? mm.efforts : ["low", "medium", "high", "xhigh"];
    let idx = Math.max(0, efforts.indexOf(m.effort));
    const start = idx;
    sh.innerHTML = `<div class="eo-label"><b id="eo-level"></b> Intelligence</div>` +
      `<div class="eo-track" id="eo-track" role="slider" aria-label="Intelligence" aria-valuemin="0" aria-valuemax="${efforts.length - 1}" tabindex="0">` +
      `<div class="eo-fill"></div>${efforts.map(() => '<span class="eo-dot"></span>').join("")}<div class="eo-knob"></div></div>`;
    const track = sh.querySelector("#eo-track");
    const fill = track.querySelector(".eo-fill"), knob = track.querySelector(".eo-knob");
    const dots = [...track.querySelectorAll(".eo-dot")];
    const geom = () => {
      const w = track.clientWidth, pad = 10, k = knob.offsetWidth;
      const x0 = pad + k / 2, x1 = w - pad - k / 2;
      return { pad, k, x0, step: efforts.length > 1 ? (x1 - x0) / (efforts.length - 1) : 0 };
    };
    const paint = () => {
      const g = geom();
      const cx = g.x0 + g.step * idx;
      knob.style.transform = `translateX(${cx - g.k / 2 - g.pad}px)`;
      fill.style.width = `${cx + g.k / 2 - g.pad}px`;
      dots.forEach((d, j) => { d.style.left = `${g.x0 + g.step * j - 5}px`; d.classList.toggle("in", j <= idx); });
      sh.querySelector("#eo-level").textContent = effortName(efforts[idx]);
      track.setAttribute("aria-valuenow", idx);
      track.setAttribute("aria-valuetext", effortName(efforts[idx]));
    };
    const at = (clientX) => {
      const g = geom(), r = track.getBoundingClientRect();
      const j = Math.round((clientX - r.left - g.x0) / (g.step || 1));
      return Math.max(0, Math.min(efforts.length - 1, j));
    };
    const set = (j) => { if (j !== idx) { idx = j; paint(); buzz(6); } };
    const commit = () => {
      setTimeout(() => { if (S.sheet === "effort") closeSheet(); }, 260);
      if (idx !== start) applySettings({ effort: efforts[idx] }, "Intelligence: " + effortName(efforts[idx]));
    };
    track.onpointerdown = (e) => { track.setPointerCapture(e.pointerId); track.classList.add("drag"); set(at(e.clientX)); };
    track.onpointermove = (e) => { if (track.hasPointerCapture(e.pointerId)) set(at(e.clientX)); };
    track.onpointerup = (e) => { track.classList.remove("drag"); if (track.hasPointerCapture(e.pointerId)) commit(); };
    track.onkeydown = (e) => {
      if (e.key === "ArrowRight" || e.key === "ArrowUp") { set(Math.min(efforts.length - 1, idx + 1)); e.preventDefault(); }
      if (e.key === "ArrowLeft" || e.key === "ArrowDown") { set(Math.max(0, idx - 1)); e.preventDefault(); }
      if (e.key === "Enter") commit();
    };
    requestAnimationFrame(() => { track.classList.add("still"); paint(); requestAnimationFrame(() => track.classList.remove("still")); });
  },

  diff(sh) {
    sh.innerHTML = `<div class="diff-head"><button class="icon-btn" data-x="close" aria-label="Close">${I.close}</button><h2>Loading Changes</h2></div><div class="diff-files hidden"></div><div class="diff-body"></div>`;
    sh.onclick = (e) => {
      if (e.target.closest("[data-x=close]")) closeSheet();
      const f = e.target.closest("[data-f]");
      if (f) { const el = sh.querySelector("#df" + f.dataset.f); if (el) el.scrollIntoView({ block: "start" }); }
    };
  },

  new(sh, opts) {
    const d = S.newDraft || (S.newDraft = { cwd: "", text: "", model: "", effort: "", sid: "" });
    if (opts.cwd) d.cwd = opts.cwd;
    if (opts.sid) d.sid = opts.sid;
    if (!d.sid || !conns.has(d.sid)) d.sid = S.machine !== "all" && conns.has(S.machine) ? S.machine : (S.servers[0] || {}).id;
    const c0 = conns.get(d.sid);
    if (!c0) { closeSheet(true); openSheet("macs"); return; }
    if (!d.cwd) d.cwd = (c0.threads.find((t) => !t.subagent) || {}).cwd || "";
    const draw = () => {
      const c = conns.get(d.sid);
      const projects = c.projects || projectsOf(c.threads, false);
      const models = (c.models && c.models.models) || [];
      const def = (c.models && c.models.default) || {};
      const model = models.find((x) => x.slug === (d.model || def.model));
      const shown = projects.slice(0, 30);
      if (d.cwd && !shown.some((p) => p.cwd === d.cwd)) shown.unshift({ cwd: d.cwd });
      const home = c.home || "";
      sh.innerHTML = `<div class="grip"></div><h2>New Chat</h2>` +
        (S.servers.length > 1 ? `<div class="field"><span class="lab">Mac</span><div class="seg">${S.servers.map((s) => `<button class="${s.id === d.sid ? "on" : ""}" data-sid="${s.id}">${esc(s.name || hostOf(s.url))}</button>`).join("")}</div></div>` : "") +
        `<div class="field"><span class="lab">Project</span><div class="proj-pick">${shown.map((p) => `<button class="${p.cwd === d.cwd ? "on" : ""}" data-cwd="${esc(p.cwd)}"><span class="nm">${esc(base(p.cwd))}</span><span class="pa">${esc(home ? p.cwd.replace(home, "~") : p.cwd)}</span></button>`).join("")}</div>` +
        `<input id="nt-cwd" style="margin-top:8px" placeholder="Or Type A Folder Path" value="${esc(d.cwd)}" autocomplete="off" autocapitalize="off" spellcheck="false"></div>` +
        `<div class="field"><span class="lab">Model</span><div class="seg" data-key="model"><button class="${!d.model ? "on" : ""}" data-v="">Default${def.model ? " (" + esc(shortModel(def.model)) + ")" : ""}</button>${models.map((x) => `<button class="${d.model === x.slug ? "on" : ""}" data-v="${esc(x.slug)}">${esc(shortModel(x.slug))}</button>`).join("")}</div></div>` +
        (model ? `<div class="field"><span class="lab">Intelligence</span><div class="seg" data-key="effort"><button class="${!d.effort ? "on" : ""}" data-v="">Default</button>${model.efforts.map((e) => `<button class="${d.effort === e ? "on" : ""}" data-v="${e}">${effortName(e)}</button>`).join("")}</div></div>` : "") +
        `<div class="field"><span class="lab">Prompt</span><textarea id="nt-text" placeholder="What Should ${S.provider==='claude'?'Claude':'Codex'} Do?">${esc(d.text)}</textarea></div>` +
        `<div class="attachments" id="nt-att">${attHTML(S.newAttach)}</div>` +
        `<div class="sheet-actions"><button class="btn" data-x="img">Attach Image</button><span class="grow"></span><button class="btn primary" data-x="start">Start Chat</button></div>`;
      const ta = sh.querySelector("#nt-text");
      ta.oninput = () => (d.text = ta.value);
      const cw = sh.querySelector("#nt-cwd");
      cw.oninput = () => (d.cwd = cw.value.trim());
    };
    S.redraw = draw;
    if (!c0.models) c0.send({ op: "models" });
    c0.send({ op: "projects" });
    draw();
    setTimeout(() => { const ta = sh.querySelector("#nt-text"); if (ta && !d.text) ta.focus({ preventScroll: true }); }, 200);
    sh.onclick = async (e) => {
      const sv = e.target.closest("[data-sid]");
      if (sv) {
        d.sid = sv.dataset.sid; d.model = ""; d.effort = ""; S.newAttach = [];
        const c2 = conns.get(d.sid);
        if (!c2.models) c2.send({ op: "models" });
        c2.send({ op: "projects" });
        d.cwd = (c2.threads.find((t) => !t.subagent) || {}).cwd || "";
        draw();
        return;
      }
      const cp = e.target.closest("[data-cwd]");
      if (cp) { d.cwd = cp.dataset.cwd; draw(); return; }
      const v = e.target.closest("[data-v]");
      if (v) { const k = v.parentElement.dataset.key; d[k] = v.dataset.v; if (k === "model") d.effort = ""; draw(); return; }
      const rm = e.target.closest("[data-rm]");
      if (rm) { S.newAttach.splice(+rm.dataset.rm, 1); redrawAttachments("new"); return; }
      const x = e.target.dataset.x;
      if (x === "img") { fileTarget = "new"; $("file").click(); }
      if (x === "start") {
        if (!d.text.trim()) { toast("Write A Prompt First"); return; }
        if (S.newAttach.some((a) => a.uploading)) { toast("Waiting For Images To Upload"); return; }
        e.target.disabled = true; e.target.textContent = "Starting";
        buzz();
        const a = await conns.get(d.sid).call({ op: "new", cwd: d.cwd, text: d.text.trim(), model: d.model || undefined, effort: d.effort || undefined, images: S.newAttach.map((x) => x.path) }, 60000);
        if (!a.ok) { toast(nice(a.msg)); e.target.disabled = false; e.target.textContent = "Start Chat"; return; }
        const target = d.sid;
        S.newDraft = null; S.newAttach = [];
        closeSheet(true);
        // The sheet's history entry becomes the thread.
        history.replaceState({ view: "thread", sid: target, id: a.thread, provider:S.provider }, "");
        openThread(target, a.thread, false);
      }
    };
  },

  macs(sh) {
    const draw = () => {
      const notif = Native ? (() => { try { return Native.notificationsEnabled(); } catch { return false; } })() : false;
      const stateText = { on: "Connected", nodesk: "Connected, Desktop App Not Running", off: "Not Reachable", rejected: "Token Rejected", bad: "Bad Address", wait: "Connecting" };
      sh.innerHTML = `<div class="grip"></div><h2>Macs</h2>` +
        S.servers.map((s) => {
          const c = conns.get(s.id);
          const link = `fastcontrol://pair?url=${encodeURIComponent(s.url)}&t=${encodeURIComponent(s.token)}&name=${encodeURIComponent(s.name || "")}`;
          return `<div class="mi"><span class="dot ${c && c.state === "on" ? "on" : c && c.state === "nodesk" ? "wait" : "off"}"></span><span class="txt">${esc(s.name || hostOf(s.url))}<span class="d">${esc(s.url)}, ${stateText[c ? c.state : "wait"]}</span></span>` +
            (!Native ? `<a class="btn" style="text-decoration:none;font-size:13px;padding:7px 10px" href="${esc(link)}">Open In App</a>` : "") +
            `<button class="icon-btn" data-del="${s.id}" aria-label="Remove Mac">${I.trash}</button></div>`;
        }).join("") +
        (Native ? `<div class="toggle"><div><div style="font-weight:650">Notifications</div><div class="hint" style="margin:0;font-size:13px;color:var(--faint)">Finished Turns And Approvals</div></div><button class="switch${notif ? " on" : ""}" data-x="notif" aria-label="Notifications"></button></div>` : "") +
        `<h3>Add A Mac</h3><div class="field"><input id="sv-url" placeholder="Pairing Link Or http://100.64.0.2:8787" autocomplete="off" autocapitalize="off" spellcheck="false" inputmode="url"></div>` +
        `<div class="field"><input id="sv-token" placeholder="Token" autocomplete="off" autocapitalize="off" spellcheck="false"></div>` +
        `<div class="field"><input id="sv-name" placeholder="Name (Optional)" autocomplete="off"></div>` +
        `<div class="sheet-actions"><span class="grow"></span><button class="btn primary" data-x="add">Add Mac</button></div>`;
      const u = sh.querySelector("#sv-url");
      u.oninput = () => {
        const m = u.value.match(/^(https?:\/\/[^/#?\s]+)\/?#t=([0-9a-f]+)/) || u.value.match(/^fastcontrol:\/\/pair\?(.*)$/);
        if (!m) return;
        if (m[2]) { u.value = m[1]; sh.querySelector("#sv-token").value = m[2]; }
        else { const p = new URLSearchParams(m[1]); u.value = p.get("url") || ""; sh.querySelector("#sv-token").value = p.get("t") || ""; sh.querySelector("#sv-name").value = p.get("name") || ""; }
      };
    };
    draw();
    sh.onclick = async (e) => {
      const del = e.target.closest("[data-del]");
      if (del) {
        const x = S.servers.find((s) => s.id === del.dataset.del);
        if (!x || !confirm(`Remove ${x.name || x.url}?`)) return;
        S.servers = S.servers.filter((s) => s !== x);
        saveServers(); syncConns(); draw(); renderHome();
        return;
      }
      const x = e.target.dataset.x;
      if (x === "notif") {
        const on = !e.target.classList.contains("on");
        try { Native.setNotifications(on); } catch {}
        e.target.classList.toggle("on", on);
        toast(on ? "Notifications On" : "Notifications Off");
      }
      if (x === "add") {
        let url = sh.querySelector("#sv-url").value.trim();
        const token = sh.querySelector("#sv-token").value.trim();
        const name = sh.querySelector("#sv-name").value.trim();
        if (!url || !token) { toast("Enter The Address And Token"); return; }
        if (!/^https?:\/\//.test(url)) url = "http://" + url;
        url = url.replace(/\/+$/, "");
        if (url.startsWith("http://") && !/:\d+$/.test(url)) url += ":8787";
        e.target.disabled = true; e.target.textContent = "Checking";
        let ok = false, msg = "";
        try { const r = await fetch(url + "/auth?t=" + encodeURIComponent(token)); ok = r.ok; msg = r.status === 401 ? "Wrong Token" : "Relay Error " + r.status; }
        catch { msg = "Cannot Reach " + url; }
        e.target.disabled = false; e.target.textContent = "Add Mac";
        if (!ok) { toast(msg); return; }
        const s = addServer({ url, token, name, custom: !!name });
        S.machine = s.id; store.set("machine", s.id);
        closeSheet(); renderHome();
      }
    };
  },
};

// ------------------------------------------------------- Android intents --

window.onAndroidIntent = function () {
  if (!Native) return;
  let it;
  try { const j = Native.consumeLaunchIntent(); if (!j) return; it = JSON.parse(j); } catch { return; }
  if (it.type === "pair" && it.url && it.token) {
    const s = addServer({ url: it.url, token: it.token, name: it.name || "" });
    S.machine = s.id; store.set("machine", s.id);
    toast("Paired With " + (s.name || s.url));
    if (S.view === "home") renderHome();
  } else if (it.type === "thread" && it.thread) {
    selectProvider(it.provider || "codex",false);
    // Reuse an open sheet's history entry; going back first would race the push.
    const reuse = !!S.sheet;
    if (reuse) closeSheet(true);
    if (reuse) { history.replaceState({ view: "thread", sid: it.server, id: it.thread }, ""); openThread(it.server, it.thread, false); }
    else openThread(it.server, it.thread, true);
  }
};

// ------------------------------------------------------------------- boot --

history.replaceState({view:"providers"}, "");
syncConns();
if (Native) saveServers();
show("providers");
renderHome();
mark("firstRender");
if (Native) window.onAndroidIntent();
if (!Native && "serviceWorker" in navigator && window.isSecureContext && location.protocol === "https:") navigator.serviceWorker.register("sw.js").catch(() => {});
