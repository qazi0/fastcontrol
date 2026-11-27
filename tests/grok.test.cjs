const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const appSource = fs.readFileSync("web/app.js", "utf8");
const newChatSource = fs.readFileSync("web/new-chat.js", "utf8");
const agentsSource = fs.readFileSync("web/agents.js", "utf8");
const usageSource = fs.readFileSync("web/usage.js", "utf8");
const orgSource = fs.readFileSync("web/organization.js", "utf8");
const htmlSource = fs.readFileSync("web/index.html", "utf8");
const cssSource = fs.readFileSync("web/style.css", "utf8");
const swSource = fs.readFileSync("web/sw.js", "utf8");

function makeClassList(initial = []) {
  const set = new Set(initial);
  return {
    add: (...names) => names.forEach((n) => set.add(n)),
    remove: (...names) => names.forEach((n) => set.delete(n)),
    contains: (name) => set.has(name),
    toggle: (name, force) => {
      const next = force === undefined ? !set.has(name) : !!force;
      if (next) set.add(name);
      else set.delete(name);
      return next;
    },
    toString: () => [...set].join(" "),
  };
}

function makeElement(tag = "div", id = "") {
  const attrs = new Map();
  const listeners = new Map();
  return {
    tagName: tag.toUpperCase(),
    id,
    innerHTML: "",
    textContent: "",
    value: "",
    title: "",
    placeholder: "",
    disabled: false,
    className: "",
    scrollTop: 0,
    scrollHeight: 1000,
    clientHeight: 400,
    clientWidth: 320,
    offsetWidth: 52,
    selectionStart: 0,
    selectionEnd: 0,
    dataset: {},
    style: { setProperty() {} },
    classList: makeClassList(),
    childNodes: [],
    setAttribute(k, v) { attrs.set(k, String(v)); },
    getAttribute(k) { return attrs.has(k) ? attrs.get(k) : null; },
    removeAttribute(k) { attrs.delete(k); },
    hasAttribute(k) { return attrs.has(k); },
    addEventListener(type, fn) {
      if (!listeners.has(type)) listeners.set(type, []);
      listeners.get(type).push(fn);
    },
    dispatch(type, event = {}) {
      for (const fn of listeners.get(type) || []) fn(event);
    },
    focus() {},
    setSelectionRange(a, b) { this.selectionStart = a; this.selectionEnd = b; },
    querySelector() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
    prepend() {},
    remove() {},
    requestSubmit() { this._submitted = true; },
    getBoundingClientRect() { return { left: 0, top: 0, width: 320, height: 72, bottom: 72 }; },
    setPointerCapture() {},
    hasPointerCapture() { return true; },
  };
}

test("HTML, CSS, service worker, and guardrails support 4-provider Grok layout without em or en dashes", () => {
  assert.match(htmlSource, /data-provider="codex"/);
  assert.match(htmlSource, /data-provider="claude"/);
  assert.match(htmlSource, /data-provider="antigravity"/);
  assert.match(htmlSource, /data-provider="grok"[^>]*><span>Grok<\/span>/);
  assert.match(swSource, /const CACHE = "fastcontrol-v21";/);
  assert.match(cssSource, /@media\s*\(max-height:\s*680px\)\s*\{[\s\S]*\.provider-choice[\s\S]*\.provider-option/);
  assert.match(cssSource, /@media\s*\(max-width:\s*380px\)\s*\{[\s\S]*\.usage-body/);
  assert.match(cssSource, /@media\s*\(min-width:\s*620px\)\s*\{[\s\S]*\.usage-providers\s*\{\s*grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);[\s\S]*\.usage-provider:nth-child\(n\+3\)/);
  assert.match(cssSource, /\.card-b\s+\.plan-preview\s*\{/);

  const testSource = fs.readFileSync(__filename, "utf8");
  for (const [name, text] of [
    ["web/index.html", htmlSource],
    ["web/app.js", appSource],
    ["web/new-chat.js", newChatSource],
    ["web/agents.js", agentsSource],
    ["web/usage.js", usageSource],
    ["web/organization.js", orgSource],
    ["web/style.css", cssSource],
    ["web/sw.js", swSource],
    ["tests/grok.test.cjs", testSource],
  ]) {
    assert.equal(text.includes("\u2014"), false, `${name} must not contain em dashes`);
    assert.equal(text.includes("\u2013"), false, `${name} must not contain en dashes`);
  }
});

test("4-provider capability gating, switching, Choose Agent sheet, and Android intent routing", () => {
  const providerButtons = ["codex", "claude", "antigravity", "grok"].map((provider) => {
    const el = makeElement("button");
    el.dataset.provider = provider;
    return el;
  });
  const providerSwitch = makeElement("button", "provider-switch");
  const askSpan = makeElement("span");
  const askBtn = makeElement("button", "ask");
  askBtn.querySelector = (sel) => (sel === "span" ? askSpan : null);
  const elements = { "provider-switch": providerSwitch, ask: askBtn };
  const sent = [];
  const stored = new Map([["threads:m1:grok", [{ id: "grok-1", title: "Grok Thread" }]]]);
  const conn = {
    s: { id: "m1", name: "Studio", url: "http://100.67.66.88:8787" },
    ok: true,
    helloReceived: true,
    capabilities: { codex: true, claude: true, antigravity: true, grok: false },
    byProvider: {},
    threads: [],
    groups: [],
    send(msg) { sent.push({ ...msg, provider: ctx.S.provider }); },
    refreshProjects() { sent.push({ op: "projects", provider: ctx.S.provider }); },
  };
  const pushed = [];
  const opened = [];
  const ctx = vm.createContext({
    Map,
    Set,
    Array,
    JSON,
    esc: (s) => String(s ?? ""),
    I: { down: "<v/>", check: "<ok/>" },
    store: {
      get: (k, d) => (stored.has(k) ? stored.get(k) : d),
      set: (k, v) => stored.set(k, v),
    },
    $: (id) => elements[id] || makeElement("div", id),
    document: {
      querySelectorAll: (sel) => (sel === "#providers [data-provider]" ? providerButtons : []),
    },
    conns: new Map([["m1", conn]]),
    history: { pushState: (st) => pushed.push(st), replaceState() {} },
    leaveThread() {},
    show() {},
    renderHome() {},
    closeSheet() {},
    openThread: (sid, id, push) => opened.push({ sid, id, push, provider: ctx.S.provider }),
    toast() {},
    mi: (act, ico, name, d, side) => `<button data-act="${act}">${ico}|${name}|${d}|${side}</button>`,
    Native: {
      consumeLaunchIntent: () => JSON.stringify({ type: "thread", server: "m1", thread: "grok-99", provider: "grok" }),
    },
  });
  ctx.window = ctx;
  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const selStart = appSource.indexOf("function selectProvider(");
  const selEnd = appSource.indexOf("$('provider-switch').onclick=");
  const provStart = appSource.indexOf(" providers(sh) {");
  const provEnd = appSource.indexOf("  image(sh,", provStart);
  const intentStart = appSource.indexOf("window.onAndroidIntent =");
  const intentEnd = appSource.indexOf("// ------------------------------------------------------------------- boot --");
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\nconst S = { provider: 'codex', view: 'home', sid: '', id: '', sheet: null, redraw: null };\n" +
      appSource.slice(selStart, selEnd) +
      "\nconst SHEETS = {\n" +
      appSource.slice(provStart, provEnd) +
      "};\n" +
      appSource.slice(intentStart, intentEnd) +
      "\nglobalThis.S = S; globalThis.SHEETS = SHEETS; globalThis.AGENT_LOGOS = AGENT_LOGOS; globalThis.PROVIDERS = PROVIDERS;",
    ctx
  );

  assert.deepEqual(Array.from(ctx.PROVIDERS), ["codex", "claude", "antigravity", "grok"]);
  assert.ok(ctx.AGENT_LOGOS.grok && ctx.AGENT_LOGOS.grok.startsWith("M"));
  assert.equal(ctx.providerName("grok"), "Grok");
  assert.equal(ctx.providerShortName("grok"), "Grok");
  assert.equal(ctx.providerLogoKey("grok"), "grok");

  // Grok hidden when relay reports grok: false.
  assert.deepEqual(Array.from(ctx.availableProviders()), ["codex", "claude", "antigravity"]);
  ctx.syncProviderButtons();
  assert.equal(providerButtons[3].classList.contains("hidden"), true);

  const sh = makeElement("div", "sheet");
  ctx.S.sheet = "providers";
  ctx.SHEETS.providers(sh);
  assert.doesNotMatch(sh.innerHTML, /data-act="grok"/);

  // When hello enables grok, all 4 providers appear.
  conn.capabilities.grok = true;
  assert.deepEqual(Array.from(ctx.availableProviders()), ["codex", "claude", "antigravity", "grok"]);
  ctx.syncProviderButtons();
  assert.equal(providerButtons[3].classList.contains("hidden"), false);
  ctx.S.redraw();
  assert.match(sh.innerHTML, /data-act="grok"/);
  assert.match(sh.innerHTML, /Grok/);

  // Switching to Grok updates labels, loads cached threads, and fetches provider data.
  ctx.selectProvider("grok", true);
  assert.equal(ctx.S.provider, "grok");
  assert.match(providerSwitch.innerHTML, /Grok/);
  assert.equal(askSpan.textContent, "Ask Grok");
  assert.equal(conn.threads[0].id, "grok-1");
  assert.deepEqual(
    sent.map((m) => `${m.op}:${m.provider}`),
    ["threads:grok", "projects:grok", "models:grok"]
  );
  assert.equal(pushed.at(-1).provider, "grok");

  // Android thread intent switches to Grok and opens the thread.
  ctx.S.provider = "codex";
  ctx.S.sheet = null;
  ctx.onAndroidIntent();
  assert.equal(ctx.S.provider, "grok");
  assert.deepEqual(opened.at(-1), { sid: "m1", id: "grok-99", push: true, provider: "grok" });
});

test("Conn.onMessage defaults capabilities with grok: false and honors hello.providers.grok", () => {
  const sockets = [];
  class Socket {
    constructor(url) { this.url = url; this.readyState = 0; this.sent = []; sockets.push(this); }
    send(t) { this.sent.push(JSON.parse(t)); }
    close() { this.readyState = 3; }
    open() { this.readyState = 1; this.onopen?.(); }
    message(v) { this.onmessage?.({ data: JSON.stringify(v) }); }
  }
  let syncCalls = 0, redrawCalls = 0, metaCalls = 0;
  const ctx = vm.createContext({
    Native: null,
    WebSocket: Socket,
    URL,
    Set,
    Map,
    Date,
    JSON,
    Math,
    AbortSignal,
    fetch: async () => ({ status: 204 }),
    setTimeout: () => 1,
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    store: { get: (_, d) => d, set() {} },
    S: { provider: "grok", view: "thread", sid: "m1", id: "g1", meta: { mode: "tail" }, servers: [], sheet: "providers", redraw: () => { redrawCalls++; } },
    window: {},
    document: { querySelectorAll: () => [] },
    scheduleRender() {},
    saveServers() {},
    mark() {},
    serverName: () => "Mac",
    syncProviderButtons: () => { syncCalls++; },
    renderMeta: () => { metaCalls++; },
  });
  const start = appSource.indexOf("function computerStatus(");
  const end = appSource.indexOf("function syncConns()");
  vm.runInContext(appSource.slice(start, end) + ";globalThis.Conn=Conn;", ctx);

  const c = new ctx.Conn({ id: "m1", url: "http://127.0.0.1:8787", alts: [], token: "tok" });
  assert.deepEqual(JSON.parse(JSON.stringify(c.capabilities)), { codex: true, claude: false, antigravity: false, grok: false });
  sockets[0].open();
  sockets[0].message({ t: "hello", desktop: true, providers: { codex: true, claude: true, antigravity: true, grok: true }, addrs: [] });
  assert.deepEqual(JSON.parse(JSON.stringify(c.capabilities)), { codex: true, claude: true, antigravity: true, grok: true });
  assert.equal(syncCalls, 1);
  assert.equal(redrawCalls, 1);
  assert.equal(metaCalls, 1);
});

test("Antigravity banner shows Connecting To Chat spinner, Reconnect when degraded, and hides wake button when unavailable or readOnly", async () => {
  const els = {};
  for (const id of ["t-ring", "banner", "wake", "earlier", "voice", "stop", "send", "perm-chip", "model-chip", "effort-btn", "input", "slash-menu", "composer"]) {
    els[id] = makeElement(id === "input" ? "textarea" : "button", id);
  }
  const wakeCalls = [];
  const toasts = [];
  const connection = {
    state: "on",
    capabilities: { codex: true, claude: true, antigravity: true, grok: true },
    models: { models: [], default: {} },
    send() {},
    async call(payload) { wakeCalls.push(payload); return { ok: true }; },
  };
  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    JSON,
    Array,
    Native: null,
    I: { shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G", down: "V", check: "C", copy: "CP", diff: "DF", squash: "SQ", mac: "MC", share: "SH", box: "BX" },
    S: {
      provider: "antigravity",
      sid: "m1",
      id: "ag-1",
      meta: { mode: "tail", connecting: true, running: false },
      turns: [],
      sheet: null,
    },
    $: (id) => els[id],
    esc: (s) => String(s ?? ""),
    conn: () => connection,
    findThread: () => ({ id: "ag-1" }),
    serverName: () => "Studio",
    loadingHTML: (s) => `<span class="loading">${s}</span>`,
    renderStatus() {},
    renderCards() {},
    renderThreadHeader() {},
    gauge: (f) => `gauge(${f.toFixed(2)})`,
    matchMedia: () => ({ matches: false }),
    innerHeight: 800,
    voiceState: { phase: "idle" },
    window: {},
    toast: (m) => toasts.push(m),
    nice: (s) => s,
    closeSheet() {},
    openSheet() {},
    mi: (act, _ico, name) => `<button data-act="${act}">${name}</button>`,
  });

  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const metaStart = appSource.indexOf("function contextMetrics(");
  const metaEnd = appSource.indexOf("// ------------------------------------------------- approvals and questions --");
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  const menuStart = appSource.indexOf("  menu(sh) {");
  const menuEnd = appSource.indexOf("  rename(sh) {", menuStart);
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\n" +
      appSource.slice(metaStart, metaEnd) +
      "\n" +
      appSource.slice(compStart, compEnd) +
      "\nconst SHEETS = {\n" +
      appSource.slice(menuStart, menuEnd) +
      "};\nglobalThis.SHEETS = SHEETS;",
    ctx
  );

  // 1. Auto-attaching shows "Connecting To Chat" spinner, no wake button.
  ctx.renderMeta();
  assert.equal(els.banner.classList.contains("hidden"), false);
  assert.match(els.banner.innerHTML, /Connecting To Chat/);
  assert.doesNotMatch(els.banner.innerHTML, /id="wake"/);

  // 2. Live session hides banner completely.
  ctx.S.meta = { mode: "live", connecting: false, running: false };
  ctx.renderMeta();
  assert.equal(els.banner.classList.contains("hidden"), true);

  // 3. Degraded non-live Antigravity session with capability shows "Reconnect" and invokes op: "wake".
  ctx.S.meta = { mode: "tail", connecting: false, running: false };
  ctx.renderMeta();
  assert.equal(els.banner.classList.contains("hidden"), false);
  assert.match(els.banner.innerHTML, /Saved Chat/);
  assert.match(els.banner.innerHTML, /id="wake">Reconnect<\/button>/);
  await els.wake.onclick({ target: els.wake });
  assert.deepEqual(JSON.parse(JSON.stringify(wakeCalls[0])), { op: "wake", id: "ag-1" });

  // Menu also exposes Reconnect and calls op: "wake".
  const sh = makeElement("div", "sheet");
  ctx.SHEETS.menu(sh);
  assert.match(sh.innerHTML, /data-act="wake">Reconnect<\/button>/);
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-act]" ? { dataset: { act: "wake" } } : null) } });
  assert.deepEqual(JSON.parse(JSON.stringify(wakeCalls[1])), { op: "wake", id: "ag-1" });
  assert.equal(toasts.at(-1), "Reconnecting");

  // 4. Without antigravity capability, banner does NOT render a broken clickable wake button and menu hides wake.
  connection.capabilities.antigravity = false;
  ctx.renderMeta();
  assert.match(els.banner.innerHTML, /Saved Chat/);
  assert.doesNotMatch(els.banner.innerHTML, /id="wake"/);
  ctx.SHEETS.menu(sh);
  assert.doesNotMatch(sh.innerHTML, /data-act="wake"/);

  // 5. Read-only subagent shows Read Only Subagent with no wake button even when capability is true.
  connection.capabilities.antigravity = true;
  ctx.S.meta = { mode: "tail", connecting: false, readOnly: true };
  ctx.renderMeta();
  assert.match(els.banner.innerHTML, /Read Only Subagent/);
  assert.doesNotMatch(els.banner.innerHTML, /id="wake"/);
  ctx.SHEETS.menu(sh);
  assert.doesNotMatch(sh.innerHTML, /data-act="wake"/);
});

test("Grok modes, reasoning_effort, model formatting, external takeover, steering, and perms/effort sheets", () => {
  const els = {};
  for (const id of ["t-ring", "banner", "wake", "earlier", "voice", "stop", "send", "perm-chip", "model-chip", "effort-btn", "input", "slash-menu", "composer"]) {
    els[id] = makeElement(id === "input" ? "textarea" : "button", id);
  }
  const models = [
    { slug: "grok-4.7", name: "Grok 4.7", efforts: ["minimal", "low", "medium", "high", "xhigh"] },
    { slug: "grok-4.7-build-fast", name: "Grok 4.7 Fast", efforts: ["low", "medium", "high", "xhigh"] },
    { slug: "grok-4.6", name: "Grok 4.6", efforts: ["low", "medium", "high", "xhigh"] },
    { slug: "grok-4.5", name: "Grok 4.5", efforts: ["low", "medium", "high"] },
  ];
  const connection = {
    state: "on",
    capabilities: { codex: true, claude: true, antigravity: true, grok: true },
    models: { models, default: { model: "grok-4.7", effort: "high", mode: "default" } },
    send() {},
    call: async () => ({ ok: true }),
  };
  const applied = [];
  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    JSON,
    Array,
    Native: null,
    I: { shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G", down: "V", check: "C" },
    S: {
      provider: "grok",
      sid: "m1",
      id: "grok-thread",
      meta: {
        mode: "tail",
        running: true,
        model: "grok-4.7-build-fast",
        effort: "",
        permissionMode: "auto",
        ctxUsed: 64000,
        ctxMax: 256000,
      },
      turns: [],
      sheet: null,
    },
    $: (id) => els[id],
    esc: (s) => String(s ?? ""),
    conn: () => connection,
    serverName: () => "Mac",
    loadingHTML: (s) => s,
    renderStatus() {},
    renderCards() {},
    renderThreadHeader() {},
    gauge: (f) => `gauge(${f.toFixed(2)})`,
    matchMedia: () => ({ matches: false }),
    innerHeight: 800,
    voiceState: { phase: "idle" },
    window: {},
    toast() {},
    nice: (s) => s,
    closeSheet() {},
    buzz() {},
    setTimeout: (fn) => fn(),
    requestAnimationFrame: (fn) => fn(),
    mi: (act, _ico, name, d, side, cls) => `<button data-act="${act}" class="${cls || ""}">${name}|${d || ""}|${side || ""}</button>`,
    applySettings: (ch, label) => applied.push({ ch, label }),
  });

  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const metaStart = appSource.indexOf("function contextMetrics(");
  const metaEnd = appSource.indexOf("// ------------------------------------------------- approvals and questions --");
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  const permsStart = appSource.indexOf("  perms(sh) {");
  const permsEnd = appSource.indexOf("  model(sh) {", permsStart);
  const effortStart = appSource.indexOf("  effort(sh) {");
  const effortEnd = appSource.indexOf("  diff(sh) {", effortStart);
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\n" +
      appSource.slice(metaStart, metaEnd) +
      "\n" +
      appSource.slice(compStart, compEnd) +
      "\nconst SHEETS = {\n" +
      appSource.slice(permsStart, permsEnd) +
      "\n" +
      appSource.slice(effortStart, effortEnd) +
      "};\nglobalThis.GROK_PERMS = GROK_PERMS; globalThis.AGENT_LOGOS = AGENT_LOGOS; globalThis.SHEETS = SHEETS;",
    ctx
  );

  // Default modes omit opt-in bypassPermissions; allowFullAccess or modes unlocks it.
  assert.deepEqual(
    Array.from(ctx.grokPermsFor(ctx.S.meta, connection.models).map((p) => p.id)),
    ["default", "acceptEdits", "auto", "plan"]
  );
  assert.deepEqual(
    Array.from(ctx.grokPermsFor({ ...ctx.S.meta, allowFullAccess: true }, connection.models).map((p) => p.id)),
    ["default", "acceptEdits", "auto", "plan", "bypassPermissions"]
  );
  assert.deepEqual(
    Array.from(ctx.grokPermsFor({ permissionMode: "default" }, { modes: [{ id: "bypassPermissions", description: "Bypass All Prompts" }] }).map((p) => p.id)),
    ["default", "acceptEdits", "auto", "plan", "bypassPermissions"]
  );

  // Reasoning effort stops include Default plus minimal..xhigh.
  assert.deepEqual(
    Array.from(ctx.providerEfforts(null, "grok")),
    ["", "minimal", "low", "medium", "high", "xhigh"]
  );
  assert.deepEqual(
    Array.from(ctx.providerEfforts(models[0], "grok")),
    ["", "minimal", "low", "medium", "high", "xhigh"]
  );
  assert.deepEqual(
    Array.from(ctx.providerEfforts(models[3], "grok")),
    ["", "low", "medium", "high"]
  );

  // Clean model display formatting (catalog and fallback).
  assert.equal(ctx.shortModel("grok-4.7"), "Grok 4.7");
  assert.equal(ctx.shortModel("grok-4.7-build-fast"), "Grok 4.7 Fast");
  assert.equal(ctx.shortModel("grok-4.6"), "Grok 4.6");
  assert.equal(ctx.shortModel("grok-4.5"), "Grok 4.5");
  assert.equal(ctx.modelDisplayName("grok-4.7-build-fast", models), "Grok 4.7 Fast");

  // Render meta + composer while running in auto mode.
  ctx.renderMeta();
  assert.equal(els["t-ring"].title, "Context 25% Used");
  assert.match(els["t-ring"].getAttribute("aria-label"), /^Grok \(xAI\), Context 25% Used/);
  assert.ok(els["t-ring"].innerHTML.includes(ctx.AGENT_LOGOS.grok));
  assert.match(els.banner.innerHTML, /Saved Chat/);
  assert.match(els.banner.innerHTML, /id="wake">Reconnect<\/button>/);
  assert.equal(els.send.getAttribute("aria-label"), "Steer");
  assert.equal(els.input.placeholder, "Steer");
  assert.match(els["perm-chip"].className, /\bwarn\b/);
  assert.equal(els["perm-chip"].getAttribute("aria-label"), "Execution Mode: Auto");
  assert.match(els["model-chip"].innerHTML, /<b>Grok 4\.7 Fast<\/b>/);
  assert.equal(els["effort-btn"].getAttribute("aria-label"), "Intelligence: Default");

  // External mode offers Take Over; missing capability hides wake button.
  ctx.S.meta = { ...ctx.S.meta, mode: "external", running: false, permissionMode: "plan", effort: "minimal" };
  ctx.renderMeta();
  assert.match(els.banner.innerHTML, /Managed On Computer/);
  assert.match(els.banner.innerHTML, /id="wake">Take Over<\/button>/);
  assert.equal(els.input.placeholder, "Message Grok");
  assert.equal(els.send.getAttribute("aria-label"), "Send");
  assert.doesNotMatch(els["perm-chip"].className, /\bwarn\b/);
  assert.equal(els["effort-btn"].getAttribute("aria-label"), "Intelligence: Minimal");

  connection.capabilities.grok = false;
  ctx.renderMeta();
  assert.doesNotMatch(els.banner.innerHTML, /id="wake"/);

  // Perms sheet renders Execution Mode and applies acceptEdits.
  const permsSheet = makeElement("div", "sheet");
  ctx.SHEETS.perms(permsSheet);
  assert.match(permsSheet.innerHTML, /<h2>Execution Mode<\/h2>/);
  assert.match(permsSheet.innerHTML, /data-act="acceptEdits"/);
  assert.match(permsSheet.innerHTML, /data-act="auto" class="warn"/);
  permsSheet.onclick({ target: { closest: (sel) => (sel === "[data-act]" ? { dataset: { act: "acceptEdits" } } : null) } });
  assert.deepEqual(JSON.parse(JSON.stringify(applied[0])), {
    ch: { permissionMode: "acceptEdits" },
    label: "Execution Mode: Accept Edits",
  });

  // Effort sheet exposes Default..Extra High and applies selected stop.
  ctx.S.meta = { ...ctx.S.meta, model: "grok-4.7", effort: "" };
  const track = makeElement("div", "eo-track");
  const fill = makeElement("div");
  const knob = makeElement("div");
  const levelOut = makeElement("b", "eo-level");
  const dots = Array.from({ length: 6 }, () => makeElement("span"));
  track.querySelector = (sel) => ({ ".eo-fill": fill, ".eo-knob": knob }[sel] || null);
  track.querySelectorAll = () => dots;
  const effortSheet = makeElement("div", "sheet");
  effortSheet.querySelector = (sel) => ({ "#eo-track": track, "#eo-level": levelOut }[sel] || null);
  ctx.SHEETS.effort(effortSheet);
  assert.equal(levelOut.textContent, "Default");
  track.onkeydown({ key: "ArrowRight", preventDefault() {} });
  assert.equal(levelOut.textContent, "Minimal");
  track.onkeydown({ key: "Enter" });
  assert.deepEqual(JSON.parse(JSON.stringify(applied[1])), {
    ch: { effort: "minimal" },
    label: "Intelligence: Minimal",
  });
});

test("Grok effort slider and speedometer always sort ascending even when relay sends reversed efforts", () => {
  const els = {};
  for (const id of ["t-ring", "banner", "wake", "earlier", "voice", "stop", "send", "perm-chip", "model-chip", "effort-btn", "input", "slash-menu", "composer"]) {
    els[id] = makeElement(id === "input" ? "textarea" : "button", id);
  }
  const reversedModels = [
    { slug: "grok-4.7", name: "Grok 4.7", efforts: ["xhigh", "high", "medium", "low"] },
    { slug: "grok-4.8", name: "Grok 4.8", efforts: ["xhigh", "low", "minimal", "high", "low", "medium", "custom"] },
  ];
  const connection = {
    state: "on",
    threads: [{ cwd: "/work/grok" }],
    projects: [{ cwd: "/work/grok" }],
    home: "/work",
    capabilities: { codex: true, claude: true, antigravity: true, grok: true },
    models: { models: reversedModels, default: { model: "grok-4.7", effort: "", mode: "default" } },
    send() {},
    call: async () => ({ ok: true }),
  };
  const applied = [];
  const gauges = [];
  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    JSON,
    Array,
    Number,
    Object,
    String,
    Native: null,
    I: { shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G", down: "V", check: "C", close: "X" },
    S: {
      provider: "grok",
      sid: "m1",
      id: "grok-rev",
      sheet: "effort",
      servers: [{ id: "m1", name: "Studio", url: "http://127.0.0.1:8787" }],
      machine: "m1",
      newAttach: [],
      meta: { mode: "live", running: false, model: "grok-4.7", effort: "", permissionMode: "default" },
      turns: [],
    },
    $: (id) => els[id] || makeElement("div", id),
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
    hostOf: (u) => u,
    conn: () => connection,
    conns: new Map([["m1", connection]]),
    serverName: () => "Studio",
    loadingHTML: (s) => s,
    attHTML: () => "",
    projectsOf: () => [],
    attachmentBatches: new Map(),
    persistAttachmentBatches() {},
    store: { get: (_, d) => d, set() {} },
    renderStatus() {},
    renderCards() {},
    renderThreadHeader() {},
    gauge: (f) => { gauges.push(f); return `gauge(${f.toFixed(2)})`; },
    matchMedia: () => ({ matches: false }),
    innerHeight: 800,
    voiceState: { phase: "idle" },
    window: {},
    document: { activeElement: null },
    toast() {},
    nice: (s) => s,
    buzz() {},
    closeSheet() {},
    openSheet() {},
    setTimeout: (fn) => fn(),
    requestAnimationFrame: (fn) => fn(),
    applySettings: (ch, label) => applied.push({ ch, label }),
  });

  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const metaStart = appSource.indexOf("function contextMetrics(");
  const metaEnd = appSource.indexOf("// ------------------------------------------------- approvals and questions --");
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  const effortStart = appSource.indexOf("  effort(sh) {");
  const effortEnd = appSource.indexOf("  diff(sh) {", effortStart);
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\n" +
      appSource.slice(metaStart, metaEnd) +
      "\n" +
      appSource.slice(compStart, compEnd) +
      "\nconst SHEETS = {\n" +
      appSource.slice(effortStart, effortEnd) +
      "};\nglobalThis.SHEETS = SHEETS;\n" +
      newChatSource,
    ctx
  );

  // providerEfforts must normalize strictly ascending (Default -> Low -> Medium -> High -> Extra High).
  assert.deepEqual(Array.from(ctx.providerEfforts(reversedModels[0], "grok")), ["", "low", "medium", "high", "xhigh"]);
  assert.deepEqual(Array.from(ctx.providerEfforts(reversedModels[1], "grok")), ["", "minimal", "low", "medium", "high", "xhigh", "custom"]);

  // Speedometer gauge monotonically increases from Default (0) to Extra High (1).
  for (const eff of ["", "low", "medium", "high", "xhigh"]) {
    ctx.S.meta = { ...ctx.S.meta, effort: eff };
    ctx.renderComposer();
  }
  assert.deepEqual(gauges.map((v) => Number(v.toFixed(2))), [0, 0.25, 0.5, 0.75, 1]);

  // SHEETS.effort steps strictly ascending as user moves right.
  ctx.S.meta = { ...ctx.S.meta, effort: "" };
  const track = makeElement("div", "eo-track");
  const fill = makeElement("div");
  const knob = makeElement("div");
  const levelOut = makeElement("b", "eo-level");
  const dots = Array.from({ length: 5 }, () => makeElement("span"));
  track.querySelector = (sel) => ({ ".eo-fill": fill, ".eo-knob": knob }[sel] || null);
  track.querySelectorAll = () => dots;
  const effortSheet = makeElement("div", "sheet");
  effortSheet.querySelector = (sel) => ({ "#eo-track": track, "#eo-level": levelOut }[sel] || null);
  ctx.SHEETS.effort(effortSheet);
  const visited = [levelOut.textContent];
  for (let i = 0; i < 4; i++) {
    track.onkeydown({ key: "ArrowRight", preventDefault() {} });
    visited.push(levelOut.textContent);
  }
  assert.deepEqual(visited, ["Default", "Low", "Medium", "High", "Extra High"]);
  track.onkeydown({ key: "Enter" });
  assert.deepEqual(JSON.parse(JSON.stringify(applied[0])), { ch: { effort: "xhigh" }, label: "Intelligence: Extra High" });

  // New-chat slider (web/new-chat.js) also sorts reversed Grok efforts ascending.
  ctx.S.sheet = "new";
  ctx.S.newDraft = null;
  const newSheet = makeElement("div", "sheet");
  const sliderEl = makeElement("input", "nt-effort");
  const levelLabel = makeElement("output", "nt-level");
  newSheet.contains = () => false;
  newSheet.querySelector = (sel) => ({ "#nt-text": makeElement("textarea", "nt-text"), "#nt-cwd": makeElement("input", "nt-cwd"), "#nt-effort": sliderEl, "#nt-level": levelLabel }[sel] || null);
  ctx.SHEETS.new(newSheet);
  assert.match(newSheet.innerHTML, /<div class="setup-effort-ends"><span>Default<\/span><span>Extra High<\/span><\/div>/);
  const sliderStops = [];
  for (let i = 0; i <= 4; i++) {
    sliderEl.value = String(i);
    sliderEl.oninput();
    sliderStops.push(`${ctx.S.newDraft.effort}:${levelLabel.textContent}`);
  }
  assert.deepEqual(sliderStops, [":Default", "low:Low", "medium:Medium", "high:High", "xhigh:Extra High"]);
});

test("new-chat Grok flow renders models, reasoning_effort slider, Execution Mode, and sends permissionMode", async () => {
  const calls = [];
  const sent = [];
  const connection = {
    threads: [{ cwd: "/Users/dev/grok-app" }],
    projects: [{ cwd: "/Users/dev/grok-app" }],
    home: "/Users/dev",
    capabilities: { codex: true, claude: true, antigravity: true, grok: true },
    models: {
      models: [
        { slug: "grok-4.7", name: "Grok 4.7", efforts: ["minimal", "low", "medium", "high", "xhigh"] },
        { slug: "grok-4.7-build-fast", name: "Grok 4.7 Fast", efforts: ["low", "medium", "high", "xhigh"] },
        { slug: "grok-4.6", name: "Grok 4.6", efforts: ["low", "medium", "high", "xhigh"] },
        { slug: "grok-4.5", name: "Grok 4.5", efforts: ["low", "medium", "high"] },
      ],
      default: { model: "grok-4.7", effort: "high", mode: "default" },
    },
    send(msg) { sent.push(msg); },
    async call(payload) { calls.push(payload); return { ok: true, thread: "grok-new-1" }; },
  };

  let focusedEl = null;
  const textEl = makeElement("textarea", "nt-text");
  const cwdEl = makeElement("input", "nt-cwd");
  const sliderEl = makeElement("input", "nt-effort");
  const levelEl = makeElement("output", "nt-level");
  const modelBtn = makeElement("button", "nt-model");
  const sh = makeElement("div", "sheet");
  sh.contains = (el) => el === textEl;
  sh.querySelector = (sel) => ({
    "#nt-text": textEl,
    "#nt-cwd": cwdEl,
    "#nt-effort": sliderEl,
    "#nt-level": levelEl,
    "#nt-model": modelBtn,
  }[sel] || null);

  const opened = [];
  const ctx = vm.createContext({
    Map,
    Set,
    Number,
    Math,
    Array,
    CSS: { escape: (s) => s },
    document: { get activeElement() { return focusedEl; } },
    S: {
      provider: "grok",
      sheet: "new",
      machine: "all",
      servers: [{ id: "m1", name: "Studio", url: "http://127.0.0.1:8787" }],
      newDraft: null,
      newAttach: [],
      redraw: null,
    },
    SHEETS: {},
    conns: new Map([["m1", connection]]),
    I: { close: "X", down: "V", check: "OK" },
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
    hostOf: (u) => u,
    loadingHTML: (s) => s,
    attHTML: () => "",
    projectsOf: () => [],
    shortModel: (s) => s,
    effortName: (e) => ({ minimal: "Minimal", low: "Low", medium: "Medium", high: "High", xhigh: "Extra High" }[e] || "Default"),
    attachmentBatches: new Map(),
    persistAttachmentBatches() {},
    store: { set() {} },
    buzz() {},
    toast() {},
    nice: (s) => s,
    closeSheet() {},
    openSheet() {},
    selectProvider() {},
    openThread: (sid, id) => opened.push({ sid, id }),
    history: { replaceState() {} },
  });
  vm.runInContext(newChatSource, ctx);

  focusedEl = textEl;
  textEl.selectionStart = 2;
  textEl.selectionEnd = 4;
  sh.scrollTop = 120;

  ctx.SHEETS.new(sh, {});
  assert.match(sh.innerHTML, /New Grok Chat/);
  assert.match(sh.innerHTML, /What Should Grok Do\?/);
  assert.match(sh.innerHTML, /Grok 4\.7/);
  assert.match(sh.innerHTML, /Grok 4\.7 Fast/);
  assert.match(sh.innerHTML, /Execution Mode/);
  assert.match(sh.innerHTML, /data-new-mode="default"/);
  assert.match(sh.innerHTML, /data-new-mode="acceptEdits"/);
  assert.match(sh.innerHTML, /data-new-mode="auto"/);
  assert.match(sh.innerHTML, /data-new-mode="plan"/);
  assert.doesNotMatch(sh.innerHTML, /data-new-mode="bypassPermissions"/);

  // Select model, reasoning effort, execution mode, and verify scroll/caret stability.
  sh.scrollTop = 88;
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-new-model]" ? { dataset: { newModel: "grok-4.7-build-fast" } } : null) } });
  assert.equal(sh.scrollTop, 88);
  assert.equal(textEl.selectionStart, 2);
  assert.equal(textEl.selectionEnd, 4);
  sliderEl.value = "4";
  sliderEl.oninput();
  assert.equal(levelEl.textContent, "Extra High");
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-new-mode]" ? { dataset: { newMode: "acceptEdits" } } : null) } });
  textEl.value = "Review the ACP handler";
  textEl.oninput();
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-x]" ? { dataset: { x: "start" } } : null) } });

  assert.equal(calls.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), {
    op: "new",
    provider: "grok",
    cwd: "/Users/dev/grok-app",
    text: "Review the ACP handler",
    model: "grok-4.7-build-fast",
    effort: "xhigh",
    images: [],
    files: [],
    permissionMode: "acceptEdits",
  });
  assert.deepEqual(opened, [{ sid: "m1", id: "grok-new-1" }]);
});

test("Grok permission, exit_plan_mode, ask_user_question cards, slash commands, and scroll protection", async () => {
  const calls = [];
  const toasts = [];
  const dockcards = makeElement("div", "dockcards");
  const composer = makeElement("form", "composer");
  const voicePanel = makeElement("div", "voice-panel");
  const voiceBtn = makeElement("button", "voice");
  const input = makeElement("textarea", "input");
  const slashMenu = makeElement("div", "slash-menu");
  const restoredPlaces = [];
  const feed = makeElement("main", "feed");
  feed.getBoundingClientRect = () => ({ top: 0, bottom: 400 });
  feed.scrollTop = 180;

  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    Array,
    JSON,
    Native: null,
    I: { down: "V", check: "<chk/>", shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G" },
    S: {
      provider: "grok",
      sid: "m1",
      id: "grok-1",
      stick: false,
      minCards: new Set(),
      turns: [],
      sheet: null,
      meta: {
        requests: [],
        commands: [
          { name: "compact", description: "Compact conversation history" },
          { name: "plan", description: "Toggle plan mode" },
        ],
      },
    },
    feed,
    $: (id) => ({ dockcards, composer, "voice-panel": voicePanel, voice: voiceBtn, input, "slash-menu": slashMenu }[id] || makeElement("div", id)),
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
    firstLine: (s) => String(s || "").split("\n")[0],
    stepFilePath: (f) => (/^[A-Z?]\s+/.test(f) ? f.slice(2) : f),
    diffLines: (s) => s,
    md: (s) => `<p>${s}</p>`,
    store: { get: (_, d) => d, set() {} },
    voiceDraftKey: () => "vk",
    voiceState: { phase: "idle" },
    renderVoice() {},
    buzz() {},
    toast: (m) => toasts.push(m),
    nice: (s) => s,
    gauge: () => "",
    matchMedia: () => ({ matches: true }),
    innerHeight: 800,
    sendText() {},
    conn: () => ({ call: async (p) => { calls.push(p); return { ok: true }; } }),
    captureReadingPlace: () => ({ top: 180, bottom: 420, key: "t1" }),
    restoreReadingPlace: (_root, place) => restoredPlaces.push(place),
  });

  const cardsStart = appSource.indexOf("function pendingAsk(");
  const cardsEnd = appSource.indexOf("// --------------------------------------------------------------- composer --");
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  vm.runInContext(appSource.slice(cardsStart, cardsEnd) + "\n" + appSource.slice(compStart, compEnd), ctx);

  // 1. session/request_permission with Grok dynamic options.
  ctx.S.meta.requests = [
    {
      id: "gperm-1",
      method: "session/request_permission",
      kind: "command",
      title: "Run Shell Command",
      body: "go test ./...",
      options: [
        { id: "allow_once", label: "allow once", kind: "allow_once" },
        { id: "allow_edits_for_session", label: "allow edits for session", kind: "allow_always" },
        { id: "reject_once", label: "reject once", kind: "reject_once" },
      ],
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /data-opt="allow_once"[^>]*>Allow Once<\/button>/);
  assert.match(dockcards.innerHTML, /data-opt="allow_edits_for_session"[^>]*>Allow Edits For Session<\/button>/);
  assert.match(dockcards.innerHTML, /data-d="decline"[^>]*data-opt="reject_once"[^>]*>Reject Once<\/button>/);
  assert.equal(restoredPlaces.length, 1, "scroll position protected when permission card renders");

  await dockcards.dispatch("click", {
    target: {
      closest: (sel) =>
        sel === ".card"
          ? { dataset: { card: "r:gperm-1" }, style: {} }
          : sel === "[data-d]"
            ? { dataset: { d: "acceptForSession", opt: "allow_edits_for_session" } }
            : null,
    },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), {
    op: "approve",
    provider: "grok",
    id: "grok-1",
    reqId: "gperm-1",
    method: "session/request_permission",
    decision: "acceptForSession",
    optionId: "allow_edits_for_session",
  });

  // 2. x.ai/exit_plan_mode renders Markdown plan preview + options and protects scroll position.
  ctx.S.meta.requests = [
    {
      id: "plan-1",
      method: "x.ai/exit_plan_mode",
      kind: "plan",
      title: "Approve Implementation Plan",
      body: "# Migration Plan\n\n1. Update schema\n2. Run tests",
      options: [
        { id: "approve", label: "approve plan", kind: "allow_once" },
        { id: "reject", label: "keep planning", kind: "reject_once" },
      ],
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /Approve Implementation Plan/);
  assert.match(dockcards.innerHTML, /class="plan-preview md"/);
  assert.match(dockcards.innerHTML, /Migration Plan/);
  assert.match(dockcards.innerHTML, /data-opt="approve"[^>]*>Approve Plan<\/button>/);
  assert.match(dockcards.innerHTML, /data-d="decline"[^>]*data-opt="reject"[^>]*>Keep Planning<\/button>/);
  assert.equal(restoredPlaces.length, 2, "scroll position protected when exit_plan_mode card renders");

  await dockcards.dispatch("click", {
    target: {
      closest: (sel) =>
        sel === ".card"
          ? { dataset: { card: "r:plan-1" }, style: {} }
          : sel === "[data-d]"
            ? { dataset: { d: "accept", opt: "approve" } }
            : null,
    },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls[1])), {
    op: "approve",
    provider: "grok",
    id: "grok-1",
    reqId: "plan-1",
    method: "x.ai/exit_plan_mode",
    decision: "accept",
    optionId: "approve",
  });

  // 2b. Leading-underscore _x.ai/exit_plan_mode without options still renders plan preview and Approve Plan / Decline.
  ctx.S.meta.requests = [
    {
      id: "plan-2",
      method: "_x.ai/exit_plan_mode",
      kind: "other",
      body: "1. Wire Grok ACP\n2. Verify tests",
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /Review Plan/);
  assert.match(dockcards.innerHTML, /class="plan-preview md"/);
  assert.match(dockcards.innerHTML, /data-d="accept">Approve Plan<\/button>/);
  assert.match(dockcards.innerHTML, /data-d="decline">Decline<\/button>/);

  // 3. x.ai/ask_user_question supports single, multi-select, and free-text answers.
  ctx.S.meta.requests = [
    {
      id: "ask-1",
      method: "x.ai/ask_user_question",
      kind: "question",
      title: "Clarify Scope",
      questions: [
        {
          id: "mode",
          header: "Mode",
          question: "Which mode should be default?",
          options: [
            { label: "Default (Recommended)", description: "Ask before running tools" },
            { label: "Accept Edits", description: "Auto-approve file edits" },
          ],
        },
        {
          id: "checks",
          header: "Checks",
          question: "Which checks should run?",
          options: ["Unit Tests", "Syntax Check"],
          multiSelect: true,
        },
        {
          id: "note",
          header: "Notes",
          question: "Any extra guidance?",
          freeText: true,
        },
      ],
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /Clarify Scope/);
  assert.match(dockcards.innerHTML, /Ask before running tools/);
  assert.match(dockcards.innerHTML, /data-q="checks"[^>]*data-multi="1"/);
  assert.match(dockcards.innerHTML, /data-q="note"[\s\S]*data-other/);
  assert.equal(restoredPlaces.length, 4, "scroll position protected when ask_user_question renders");

  const singleQQ = makeElement("section");
  singleQQ.dataset = { q: "mode", value: "Default (Recommended)", questionKey: "k1" };
  const multiQQ = makeElement("section");
  multiQQ.dataset = { q: "checks", multi: "1", values: JSON.stringify(["Unit Tests", "Syntax Check"]), questionKey: "k2" };
  const freeQQ = makeElement("section");
  freeQQ.dataset = { q: "note", questionKey: "k3" };
  freeQQ.querySelector = (sel) => (sel === "[data-other]" ? { value: "Keep output concise" } : null);
  const answerCard = {
    dataset: { card: "r:ask-1" },
    style: {},
    querySelectorAll: (sel) => (sel === ".qq" ? [singleQQ, multiQQ, freeQQ] : []),
  };
  await dockcards.dispatch("click", {
    target: { closest: (sel) => (sel === ".card" ? answerCard : sel === "[data-answer]" ? {} : null) },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls[2])), {
    op: "answer",
    provider: "grok",
    id: "grok-1",
    reqId: "ask-1",
    answers: {
      mode: ["Default (Recommended)"],
      checks: ["Unit Tests", "Syntax Check"],
      note: ["Keep output concise"],
    },
  });

  // 4. Slash commands autocomplete works automatically from S.meta.commands for Grok.
  input.value = "/pl";
  input.selectionStart = 3;
  input.dispatch("input");
  assert.equal(slashMenu.classList.contains("hidden"), false);
  assert.match(slashMenu.innerHTML, /\/plan/);
});

test("Grok subagent view uses Grok labels, stays isolated across 4 providers, and remains strictly read-only", async () => {
  const requests = [];
  const sh = makeElement("div", "sheet");
  const agentFeed = makeElement("main", "agent-feed");
  const agentsBtn = makeElement("button", "agents-btn");
  const ctx = vm.createContext({
    Map,
    Date,
    JSON,
    base: (s) => String(s || "").split("/").pop(),
    esc: (s) => String(s ?? ""),
    svg: (d) => `<svg>${d}</svg>`,
    loadingHTML: (s) => s,
    I: { agent: "A", term: "T", chev: ">" },
    S: { provider: "grok", view: "thread", sid: "mac", id: "parent-1", sheet: "agents", open: new Set() },
    SHEETS: {},
    $: (id) => ({ "agents-btn": agentsBtn, "agent-feed": agentFeed, "agent-list": makeElement("div"), "agent-status": makeElement("div"), "agent-working": makeElement("div") }[id] || makeElement("div")),
    document: { hidden: false, addEventListener() {} },
    setInterval() {},
    requestAnimationFrame: () => 1,
    cancelAnimationFrame() {},
    bindTranscriptScroll() {},
    transcriptClick() {},
    clearAudio() {},
    responses: new Map(),
    conns: new Map([
      [
        "mac",
        {
          ok: true,
          s: { id: "mac" },
          call: async (req) => {
            requests.push(req);
            return {
              ok: true,
              agents: [{ id: "sub-g1", provider: "grok", agentName: "Explore Worker", running: true, agentState: "running" }],
            };
          },
        },
      ],
    ]),
  });
  ctx.window = ctx;
  vm.runInContext(agentsSource, ctx);

  assert.equal(vm.runInContext('agentLabel({ provider: "grok", agentName: "Audit Worker", title: "Fallback" })', ctx), "Audit Worker");
  assert.equal(vm.runInContext('agentLabel({ provider: "grok", title: "Task Title" })', ctx), "Task Title");
  assert.equal(vm.runInContext('agentLabel({ provider: "grok" })', ctx), "Subagent");

  await ctx.readAgents("mac", "parent-1", false, "grok");
  await ctx.readAgents("mac", "parent-1", false, "antigravity");
  assert.deepEqual(requests.slice(0, 2).map((r) => r.provider), ["grok", "antigravity"]);

  ctx.SHEETS.agents(sh, { server: "mac", parent: "parent-1", provider: "grok" });
  assert.match(sh.innerHTML, /<h2>Grok Subagents<\/h2>/);

  ctx.S.sheet = "agent";
  await ctx.SHEETS.agent(sh, {
    server: "mac",
    parent: "parent-1",
    provider: "grok",
    agent: { id: "sub-g1", provider: "grok", agentName: "Explore Worker" },
  });
  assert.match(sh.innerHTML, /Read Only/);
  assert.match(sh.innerHTML, /Managed By The Parent Chat/);
  assert.doesNotMatch(sh.innerHTML, /<textarea|id="composer"|id="send"|data-d="accept"/);
  assert.equal(requests.at(-1).op, "agentOpen");
  assert.equal(requests.at(-1).provider, "grok");
  assert.equal(ctx.onAgentMessage({ s: { id: "mac" } }, { id: "sub-g1", t: "meta", meta: { title: "Wrong" } }, "antigravity"), false);
  assert.equal(ctx.onAgentMessage({ s: { id: "mac" } }, { id: "sub-g1", t: "meta", meta: { title: "Right" } }, "grok"), true);
});

test("4-provider usage rendering displays Grok logo, exact USD cost, quotas, and graceful quotaError", async () => {
  const requests = [];
  const ctx = vm.createContext({
    Map,
    Date,
    Number,
    Math,
    Intl,
    AbortSignal,
    Error,
    AGENT_LOGOS: { openai: "M1 1", claude: "M2 2", antigravity: "M3 3", grok: "M4 4" },
    I: { down: "v", chev: ">" },
    esc: (s) => String(s ?? ""),
    serverName: () => "Studio",
    conns: new Map([["m1", { ok: true, base: "http://m1", s: { token: "tok" } }]]),
    S: { sheet: "context", sid: "m1", provider: "grok", servers: [{ id: "m1" }] },
    SHEETS: {},
    $: () => makeElement("div"),
    setInterval() {},
    document: { hidden: false },
    openSheet() {},
    fetch: (url) => new Promise((resolve) => requests.push({ url, resolve })),
  });
  vm.runInContext(usageSource, ctx);

  const grokProvider = {
    provider: "grok",
    plan: "supergrok_heavy",
    windows: [
      { label: "Weekly", used: 34, remaining: 66, resetAt: Math.floor(Date.now() / 1000) + 86400, minutes: 10080 },
    ],
    today: { tokens: 42500, costUsd: 1.42 },
    days30: { tokens: 1250000, costUsd: 18.75, input: 700000, cached: 400000, writes: 50000, output: 100000 },
    models: [
      { model: "grok-4.7", tokens: 950000, costUsd: 14.5 },
      { model: "grok-4.7-build-fast", tokens: 300000, costUsd: 4.25 },
    ],
  };

  const cardHTML = ctx.usageProvider(grokProvider);
  assert.match(cardHTML, /<h2>Grok<\/h2>/);
  assert.match(cardHTML, /d="M4 4"/);
  assert.match(cardHTML, /Supergrok Heavy/);
  assert.match(cardHTML, /<th scope="row">Cost<\/th>/);
  assert.match(cardHTML, /\$1\.42/);
  assert.match(cardHTML, /\$18\.75/);
  assert.match(cardHTML, /Recorded from local Grok session usage/);
  assert.match(cardHTML, /grok-4\.7-build-fast/);
  assert.match(cardHTML, /Weekly/);
  assert.match(cardHTML, /66<small>% Left<\/small>/);

  // Graceful non-fatal quotaError (for unsigned hosts like blakberrisigma) preserves local activity.
  const unsignedHTML = ctx.usageProvider({
    ...grokProvider,
    plan: "",
    windows: [],
    quotaError: "Sign In To Grok CLI To Read Plan Quota",
  });
  assert.match(unsignedHTML, /Sign In To Grok CLI To Read Plan Quota/);
  assert.match(unsignedHTML, /Local Activity/);
  assert.match(unsignedHTML, /\$18\.75/);
  assert.match(unsignedHTML, /grok-4\.7/);

  // Context weekly quota works for Grok and degrades cleanly on quotaError.
  const q = ctx.weeklyQuota({ providers: [grokProvider] }, "grok");
  assert.equal(q.used, 34);
  assert.equal(q.remaining, 66);
  assert.equal(q.tone, "");

  const target = makeElement("section", "context-weekly");
  target.querySelector = () => makeElement("button");
  const sheet = { querySelector: (sel) => (sel === "#context-weekly" ? target : null) };
  const pending = ctx.showContextWeekly(sheet, "m1", "grok");
  assert.equal(requests.length, 1);
  assert.match(requests[0].url, /provider=grok/);
  requests[0].resolve({ ok: true, json: async () => ({ providers: [grokProvider] }) });
  await pending;
  assert.match(target.innerHTML, /Weekly Usage/);
  assert.match(target.innerHTML, /66% Left/);

  // 4-provider full usage cards render all 4 providers cleanly.
  const allHTML = [
    { provider: "codex", today: {}, days30: {}, windows: [], models: [] },
    { provider: "claude", today: {}, days30: {}, windows: [], models: [] },
    { provider: "antigravity", today: {}, days30: {}, windows: [], models: [] },
    grokProvider,
  ].map((p) => ctx.usageProvider(p)).join("");
  assert.match(allHTML, /<h2>Codex<\/h2>/);
  assert.match(allHTML, /<h2>Claude Code<\/h2>/);
  assert.match(allHTML, /<h2>Antigravity<\/h2>/);
  assert.match(allHTML, /<h2>Grok<\/h2>/);
});

test("Grok organization requests preserve provider scoping and session trees", async () => {
  const calls = [];
  const ctx = vm.createContext({
    Map,
    Set,
    S: { provider: "grok", machine: "all", view: "project" },
    conns: new Map([
      [
        "m1",
        {
          groups: [{ id: "g-grok", name: "xAI Workspace", roots: ["/work/grok"] }],
          call: async (req) => { calls.push(req); return { ok: true, threads: [] }; },
        },
      ],
    ]),
    $: () => makeElement("div"),
    esc: (s) => String(s ?? ""),
    I: { folder: "F", chev: ">" },
    serverName: () => "Studio",
    history: { pushState() {} },
    show() {},
    toast() {},
    nice: (s) => s,
    allThreads: () => [],
    loadingHTML: (s) => s,
    scheduleRender() {},
    threadRow: (t) => `<button data-id="${t.id}">${t.title}</button>`,
  });
  ctx.window = ctx;
  vm.runInContext(orgSource, ctx);

  await ctx.openNativeGroup("m1", "g-grok", false);
  assert.equal(calls.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), { op: "group", id: "g-grok", provider: "grok" });

  const treeHTML = ctx.sessionTreeRows(
    [{ sid: "m1", id: "child-1", parentId: "root-1", title: "Forked Grok Chat", recency: 20 }],
    [
      { sid: "m1", id: "root-1", title: "Root Grok Chat", recency: 10 },
      { sid: "m1", id: "child-1", parentId: "root-1", title: "Forked Grok Chat", recency: 20 },
    ]
  );
  assert.match(treeHTML, /Root Grok Chat/);
  assert.match(treeHTML, /Forked Grok Chat/);
  assert.match(treeHTML, /session-tree-children/);
});
