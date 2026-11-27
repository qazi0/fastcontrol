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
  };
}

test("HTML, CSS, service worker, and guardrails support 3-provider Antigravity layout", () => {
  assert.match(htmlSource, /data-provider="antigravity"/);
  assert.match(htmlSource, /id="slash-menu"/);
  assert.match(htmlSource, /Usage And Limits/);
  assert.match(swSource, /const CACHE = "fastcontrol-v23";/);
  assert.match(cssSource, /\.slash-menu\s*\{\s*position:\s*absolute;/);
  assert.match(cssSource, /\.usage-providers\s*\{\s*display:\s*grid;\s*grid-template-columns:\s*minmax\(0,\s*1fr\);/);
  assert.match(cssSource, /grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/);
  assert.match(cssSource, /\.opt\.multi\.on\s+\.choice-mark/);
  assert.match(cssSource, /\.model-chip\s*\{[^}]*overflow:\s*hidden;/);
  for (const [name, text] of [
    ["web/index.html", htmlSource],
    ["web/app.js", appSource],
    ["web/new-chat.js", newChatSource],
    ["web/agents.js", agentsSource],
    ["web/usage.js", usageSource],
    ["web/organization.js", orgSource],
    ["web/style.css", cssSource],
    ["web/sw.js", swSource],
    ["web/messages.js", fs.readFileSync("web/messages.js", "utf8")],
  ]) {
    assert.equal(text.includes("\u2014"), false, `${name} must not contain em dashes`);
    assert.equal(text.includes("\u2013"), false, `${name} must not contain en dashes`);
  }
});

test("Antigravity capability gating, provider switching, and Choose Agent sheet", () => {
  const providerButtons = ["codex", "claude", "antigravity"].map((provider) => {
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
  const stored = new Map([["threads:m1:antigravity", [{ id: "ag-1", title: "Antigravity Chat" }]]]);
  const conn = {
    s: { id: "m1", name: "Studio", url: "http://100.67.66.88:8787" },
    ok: true,
    helloReceived: true,
    capabilities: { codex: true, claude: true, antigravity: false },
    byProvider: {},
    threads: [],
    groups: [],
    send(msg) { sent.push({ ...msg, provider: ctx.S.provider }); },
    refreshProjects() { sent.push({ op: "projects", provider: ctx.S.provider }); },
  };
  const pushed = [];
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
    mi: (act, ico, name, d, side) => `<button data-act="${act}">${ico}|${name}|${d}|${side}</button>`,
  });
  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const selStart = appSource.indexOf("function selectProvider(");
  const selEnd = appSource.indexOf("$('provider-switch').onclick=");
  const provStart = appSource.indexOf(" providers(sh) {");
  const provEnd = appSource.indexOf("  image(sh,", provStart);
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\nconst S = { provider: 'codex', sheet: null, redraw: null };\n" +
      appSource.slice(selStart, selEnd) +
      "\nconst SHEETS = {\n" +
      appSource.slice(provStart, provEnd) +
      "};\nglobalThis.S = S; globalThis.SHEETS = SHEETS; globalThis.AGENT_LOGOS = AGENT_LOGOS;",
    ctx
  );

  assert.ok(ctx.AGENT_LOGOS.antigravity && ctx.AGENT_LOGOS.antigravity.startsWith("M"));
  // Capability gating: Antigravity hidden when relay reports antigravity: false.
  assert.deepEqual(Array.from(ctx.availableProviders()), ["codex", "claude"]);
  ctx.syncProviderButtons();
  assert.equal(providerButtons[2].classList.contains("hidden"), true);

  const sh = makeElement("div", "sheet");
  ctx.S.sheet = "providers";
  ctx.SHEETS.providers(sh);
  assert.doesNotMatch(sh.innerHTML, /data-act="antigravity"/);

  // When hello enables antigravity, button unhides and Choose Agent sheet shows all 3.
  conn.capabilities.antigravity = true;
  assert.deepEqual(Array.from(ctx.availableProviders()), ["codex", "claude", "antigravity"]);
  ctx.syncProviderButtons();
  assert.equal(providerButtons[2].classList.contains("hidden"), false);
  ctx.S.redraw();
  assert.match(sh.innerHTML, /data-act="antigravity"/);
  assert.match(sh.innerHTML, /Antigravity/);

  // Switching to Antigravity updates labels, loads cached threads, and fetches provider data.
  ctx.selectProvider("antigravity", true);
  assert.equal(ctx.S.provider, "antigravity");
  assert.match(providerSwitch.innerHTML, /Antigravity/);
  assert.equal(askSpan.textContent, "Ask Antigravity");
  assert.equal(conn.threads[0].id, "ag-1");
  assert.deepEqual(
    sent.map((m) => `${m.op}:${m.provider}`),
    ["threads:antigravity", "projects:antigravity", "models:antigravity"]
  );
  assert.equal(pushed.at(-1).provider, "antigravity");
});

test("Conn.onMessage defaults capabilities with antigravity: false and honors hello.providers.antigravity", () => {
  const sockets = [];
  class Socket {
    constructor(url) { this.url = url; this.readyState = 0; this.sent = []; sockets.push(this); }
    send(t) { this.sent.push(JSON.parse(t)); }
    close() { this.readyState = 3; }
    open() { this.readyState = 1; this.onopen?.(); }
    message(v) { this.onmessage?.({ data: JSON.stringify(v) }); }
  }
  let syncCalls = 0, redrawCalls = 0;
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
    S: { provider: "codex", view: "home", servers: [], sheet: "providers", redraw: () => { redrawCalls++; } },
    window: {},
    document: { querySelectorAll: () => [] },
    scheduleRender() {},
    saveServers() {},
    mark() {},
    serverName: () => "Mac",
    syncProviderButtons: () => { syncCalls++; },
  });
  const start = appSource.indexOf("function computerStatus(");
  const end = appSource.indexOf("function syncConns()");
  vm.runInContext(appSource.slice(start, end) + ";globalThis.Conn=Conn;", ctx);

  const c = new ctx.Conn({ id: "m1", url: "http://127.0.0.1:8787", alts: [], token: "tok" });
  assert.deepEqual(JSON.parse(JSON.stringify(c.capabilities)), { codex: true, claude: false, antigravity: false, grok: false });
  sockets[0].open();
  sockets[0].message({ t: "hello", desktop: true, providers: { codex: true, claude: true, antigravity: true }, addrs: [] });
  assert.deepEqual(JSON.parse(JSON.stringify(c.capabilities)), { codex: true, claude: true, antigravity: true, grok: false });
  assert.equal(syncCalls, 1);
  assert.equal(redrawCalls, 1);
});

test("Antigravity modes, opt-in Full Access gating, catalog model display, steering, and context meter", () => {
  const els = {};
  for (const id of ["t-ring", "banner", "wake", "earlier", "voice", "stop", "send", "perm-chip", "model-chip", "effort-btn", "input", "slash-menu", "composer"]) {
    els[id] = makeElement(id === "input" ? "textarea" : "button", id);
  }
  const models = [
    { slug: "gemini-4-neon", name: "Gemini 4 Neon", efforts: ["low", "medium", "high", "xhigh"] },
    { slug: "gemini-pro-orchestrator", name: "Gemini Pro Orchestrator", efforts: ["low", "medium", "high", "xhigh"] },
    { slug: "gpt-oss-120b-medium", name: "GPT-OSS 120B (Medium)", efforts: ["low", "medium", "high"] },
  ];
  const connection = {
    state: "on",
    capabilities: { codex: true, claude: true, antigravity: true, grok: false },
    models: { models, default: { model: "gemini-4-neon", effort: "high", mode: "default" } },
    send() {},
    call: async () => ({ ok: true }),
  };
  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    JSON,
    Array,
    Native: null,
    I: { shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G", down: "V", check: "C" },
    S: {
      provider: "antigravity",
      sid: "m1",
      id: "ag-thread",
      meta: {
        mode: "tail",
        running: true,
        model: "gemini-pro-orchestrator",
        effort: "",
        permissionMode: "auto",
        contextUsed: 130169,
        contextWindow: 1048576,
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
  });

  const logoStart = appSource.indexOf("const AGENT_LOGOS =");
  const logoEnd = appSource.indexOf("// ------------------------------------------------------------------ state --");
  const metaStart = appSource.indexOf("function contextMetrics(");
  const metaEnd = appSource.indexOf("// ------------------------------------------------- approvals and questions --");
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  vm.runInContext(
    appSource.slice(logoStart, logoEnd) +
      "\n" +
      appSource.slice(metaStart, metaEnd) +
      "\n" +
      appSource.slice(compStart, compEnd) +
      "\nglobalThis.ANTIGRAVITY_PERMS = ANTIGRAVITY_PERMS; globalThis.AGENT_LOGOS = AGENT_LOGOS;",
    ctx
  );

  // Turbo (full-access) is visible by default for Antigravity unless explicitly disabled on both meta and models.
  const defaultPerms = ctx.antigravityPermsFor(ctx.S.meta, connection.models);
  assert.deepEqual(
    Array.from(defaultPerms.map((p) => p.id)),
    ["default", "accept-edits", "auto", "plan", "full-access"]
  );
  const turbo = defaultPerms.find((p) => p.id === "full-access");
  assert.equal(turbo.name, "Turbo");
  assert.equal(turbo.short, "Turbo");
  assert.equal(turbo.d, "Auto-Approves All Edits And Commands");
  // Explicitly disabled on both meta and models hides full-access.
  assert.deepEqual(
    Array.from(ctx.antigravityPermsFor({ ...ctx.S.meta, allowFullAccess: false }, { ...connection.models, allowFullAccess: false }).map((p) => p.id)),
    ["default", "accept-edits", "auto", "plan"]
  );
  assert.deepEqual(
    Array.from(ctx.antigravityPermsFor({ permissionMode: "default", allowFullAccess: false }, { allowFullAccess: false, modes: [{ id: "full-access", description: "Turbo Mode" }] }).map((p) => p.id)),
    ["default", "accept-edits", "auto", "plan", "full-access"]
  );
  // Antigravity thought_level efforts include Default plus low/medium/high/xhigh.
  assert.deepEqual(
    Array.from(ctx.providerEfforts(models[0], "antigravity")),
    ["", "low", "medium", "high", "xhigh"]
  );

  ctx.renderMeta();
  // Context ring reads ACP contextUsed/contextWindow and uses Antigravity logo.
  assert.equal(els["t-ring"].title, "Context 12% Used");
  assert.match(els["t-ring"].getAttribute("aria-label"), /^Antigravity, Context 12% Used/);
  assert.ok(els["t-ring"].innerHTML.includes(ctx.AGENT_LOGOS.antigravity));
  // Banner offers Reconnect for non-live degraded/saved sessions when ACP capability is available.
  assert.match(els.banner.innerHTML, /Reconnect/);
  // Composer supports live steering while running, full display model name, warn badge on auto, and Default effort.
  assert.equal(els.send.getAttribute("aria-label"), "Steer");
  assert.equal(els.input.placeholder, "Steer");
  assert.match(els["perm-chip"].className, /\bwarn\b/);
  assert.equal(els["perm-chip"].getAttribute("aria-label"), "Execution Mode: Auto");
  assert.match(els["model-chip"].innerHTML, /<b>Gemini Pro Orchestrator<\/b>/);
  assert.equal(els["effort-btn"].getAttribute("aria-label"), "Intelligence: Default");
  assert.equal(ctx.shortModel("gpt-oss-120b-medium"), "GPT OSS 120B Medium");
  assert.equal(ctx.shortModel("gemini-3.8-flash"), "Gemini 3.8 Flash");

  // When idle in plan mode: placeholder is Message Antigravity and no warn class.
  ctx.S.meta.running = false;
  ctx.S.meta.permissionMode = "plan";
  ctx.S.meta.effort = "xhigh";
  ctx.renderComposer();
  assert.equal(els.input.placeholder, "Message Antigravity");
  assert.equal(els.send.getAttribute("aria-label"), "Send");
  assert.doesNotMatch(els["perm-chip"].className, /\bwarn\b/);
  assert.equal(els["effort-btn"].getAttribute("aria-label"), "Intelligence: Extra High");
});

test("new-chat Antigravity flow renders models, thought_level, and Execution Mode and sends permissionMode", async () => {
  const calls = [];
  const sent = [];
  const connection = {
    threads: [{ cwd: "/Users/dev/workspace" }],
    projects: [{ cwd: "/Users/dev/workspace" }],
    home: "/Users/dev",
    capabilities: { codex: true, claude: true, antigravity: true },
    models: {
      models: [
        { slug: "gemini-4-neon", name: "Gemini 4 Neon", efforts: ["low", "medium", "high", "xhigh"] },
        { slug: "gemini-pro-orchestrator", name: "Gemini Pro Orchestrator", efforts: ["low", "medium", "high", "xhigh"] },
      ],
      default: { model: "gemini-4-neon", effort: "high", mode: "default" },
    },
    send(msg) { sent.push(msg); },
    async call(payload) { calls.push(payload); return { ok: true, thread: "ag-new-1" }; },
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
      provider: "antigravity",
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
    effortName: (e) => ({ low: "Low", medium: "Medium", high: "High", xhigh: "Extra High" }[e] || "Default"),
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

  // Caret + scroll preservation check during redraw.
  focusedEl = textEl;
  textEl.selectionStart = 3;
  textEl.selectionEnd = 5;
  sh.scrollTop = 140;

  ctx.SHEETS.new(sh, {});
  assert.match(sh.innerHTML, /New Antigravity Chat/);
  assert.match(sh.innerHTML, /What Should Antigravity Do\?/);
  assert.match(sh.innerHTML, /Gemini 4 Neon/);
  assert.match(sh.innerHTML, /Gemini Pro Orchestrator/);
  assert.match(sh.innerHTML, /Execution Mode/);
  assert.match(sh.innerHTML, /data-new-mode="default"/);
  assert.match(sh.innerHTML, /data-new-mode="accept-edits"/);
  assert.match(sh.innerHTML, /data-new-mode="auto"/);
  assert.match(sh.innerHTML, /data-new-mode="plan"/);
  assert.match(sh.innerHTML, /data-new-mode="full-access"[^>]*>Turbo</);

  // Select model, effort, mode, and prompt.
  sh.scrollTop = 95;
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-new-model]" ? { dataset: { newModel: "gemini-pro-orchestrator" } } : null) } });
  assert.equal(sh.scrollTop, 95, "redraw preserves sheet scrollTop");
  sliderEl.value = "4";
  sliderEl.oninput();
  assert.equal(levelEl.textContent, "Extra High");
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-new-mode]" ? { dataset: { newMode: "accept-edits" } } : null) } });
  textEl.value = "Audit the relay reconnect flow";
  textEl.oninput();
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-x]" ? { dataset: { x: "start" } } : null) } });

  assert.equal(calls.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), {
    op: "new",
    provider: "antigravity",
    cwd: "/Users/dev/workspace",
    text: "Audit the relay reconnect flow",
    model: "gemini-pro-orchestrator",
    effort: "xhigh",
    images: [],
    files: [],
    permissionMode: "accept-edits",
  });
  assert.deepEqual(opened, [{ sid: "m1", id: "ag-new-1" }]);

  // Codex new-chat keeps existing behavior without Execution Mode or permissionMode.
  ctx.S.provider = "codex";
  ctx.S.newDraft = null;
  ctx.SHEETS.new(sh, {});
  assert.match(sh.innerHTML, /New Codex Chat/);
  assert.doesNotMatch(sh.innerHTML, /data-new-mode=/);
  textEl.value = "Hello Codex";
  textEl.oninput();
  await sh.onclick({ target: { closest: (sel) => (sel === "[data-x]" ? { dataset: { x: "start" } } : null) } });
  assert.equal("permissionMode" in calls[1], false);
});

test("dynamic request_permission options, elicitation multi-select & free-text cards, and scroll protection", async () => {
  const calls = [];
  const toasts = [];
  const dockcards = makeElement("div", "dockcards");
  const composer = makeElement("form", "composer");
  const voicePanel = makeElement("div", "voice-panel");
  const voiceBtn = makeElement("button", "voice");
  const restoredPlaces = [];
  const feed = makeElement("main", "feed");
  feed.getBoundingClientRect = () => ({ top: 0, bottom: 400 });
  feed.scrollTop = 210;

  const ctx = vm.createContext({
    Map,
    Set,
    Array,
    JSON,
    Native: null,
    I: { down: "V", check: "<chk/>" },
    S: {
      provider: "antigravity",
      sid: "m1",
      id: "ag-1",
      stick: false,
      minCards: new Set(),
      turns: [],
      meta: { requests: [] },
    },
    feed,
    $: (id) => ({ dockcards, composer, "voice-panel": voicePanel, voice: voiceBtn }[id] || makeElement("div", id)),
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
    conn: () => ({ call: async (p) => { calls.push(p); return { ok: true }; } }),
    captureReadingPlace: () => ({ top: 210, bottom: 390, key: "t1" }),
    restoreReadingPlace: (_root, place) => restoredPlaces.push(place),
  });

  const start = appSource.indexOf("function pendingAsk(");
  const end = appSource.indexOf("// --------------------------------------------------------------- composer --");
  vm.runInContext(appSource.slice(start, end), ctx);

  // 1. Dynamic ACP session/request_permission options.
  ctx.S.meta.requests = [
    {
      id: "perm-1",
      method: "session/request_permission",
      kind: "command",
      title: "Run Build Command",
      body: "npm test\n\nNeeds permission to run tests",
      options: [
        { id: "allow_once", label: "allow once", kind: "allow_once" },
        { id: "allow_session", label: "allow for session", kind: "allow_always" },
        { id: "reject_once", label: "deny", kind: "reject_once" },
      ],
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /data-opt="allow_once"[^>]*>Allow Once<\/button>/);
  assert.match(dockcards.innerHTML, /data-opt="allow_session"[^>]*>Allow For Session<\/button>/);
  assert.match(dockcards.innerHTML, /class="btn danger"[^>]*data-d="decline"[^>]*data-opt="reject_once"[^>]*>Deny<\/button>/);
  assert.equal(restoredPlaces.length, 1, "scroll position protected when card appears while reading history");

  const cardMock = { dataset: { card: "r:perm-1" }, style: {} };
  const allowBtn = { dataset: { d: "acceptForSession", opt: "allow_session" } };
  await dockcards.dispatch("click", {
    target: {
      closest: (sel) => (sel === ".card" ? cardMock : sel === "[data-d]" ? allowBtn : null),
    },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), {
    op: "approve",
    provider: "antigravity",
    id: "ag-1",
    reqId: "perm-1",
    method: "session/request_permission",
    decision: "acceptForSession",
    optionId: "allow_session",
  });

  // 2. Backward-compatible Codex request without q.options omits optionId.
  ctx.S.provider = "codex";
  ctx.S.meta.requests = [{ id: "perm-codex", method: "commandExecution", kind: "command", body: "ls -la" }];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /data-d="accept">Approve<\/button>/);
  assert.doesNotMatch(dockcards.innerHTML, /data-opt=/);
  await dockcards.dispatch("click", {
    target: {
      closest: (sel) =>
        sel === ".card"
          ? { dataset: { card: "r:perm-codex" }, style: {} }
          : sel === "[data-d]"
            ? { dataset: { d: "accept" } }
            : null,
    },
  });
  assert.equal("optionId" in calls[1], false);

  // 3. ACP elicitation/create with multi-select + free-text + secret fields.
  ctx.S.provider = "antigravity";
  ctx.S.meta.requests = [
    {
      id: "elic-1",
      method: "elicitation/create",
      kind: "question",
      title: "Configure Deployment",
      questions: [
        { id: "targets", header: "Targets", question: "Select targets", options: ["macOS", "Linux", "Android"], multiSelect: true },
        { id: "notes", header: "Release Notes", question: "Provide release summary", freeText: true },
        { id: "token", header: "Secret", question: "Optional token", freeText: true, secret: true },
      ],
    },
  ];
  ctx.renderCards();
  assert.match(dockcards.innerHTML, /Configure Deployment/);
  assert.match(dockcards.innerHTML, /data-q="targets"[^>]*data-multi="1"/);
  assert.match(dockcards.innerHTML, /role="checkbox" aria-checked="false"/);
  assert.match(dockcards.innerHTML, /data-q="notes"[\s\S]*data-other/);
  assert.match(dockcards.innerHTML, /type="password"/);

  // Simulate toggling two multi-select checkboxes and entering free-text, then submitting.
  const opt1 = makeElement("button"); opt1.dataset.pick = "macOS";
  const opt2 = makeElement("button"); opt2.dataset.pick = "Android";
  const multiQQ = makeElement("section");
  multiQQ.dataset = { q: "targets", multi: "1", questionKey: "k1" };
  multiQQ.querySelectorAll = (sel) => (sel === ".opt.on" ? [opt1, opt2].filter((o) => o.classList.contains("on")) : [opt1, opt2]);
  opt1.closest = (sel) => (sel === ".qq" ? multiQQ : null);
  opt2.closest = (sel) => (sel === ".qq" ? multiQQ : null);

  await dockcards.dispatch("click", { target: { closest: (sel) => (sel === "[data-pick]" ? opt1 : null) } });
  await dockcards.dispatch("click", { target: { closest: (sel) => (sel === "[data-pick]" ? opt2 : null) } });
  assert.equal(multiQQ.dataset.values, JSON.stringify(["macOS", "Android"]));

  const freeQQ = makeElement("section");
  freeQQ.dataset = { q: "notes", questionKey: "k2" };
  freeQQ.querySelector = (sel) => (sel === "[data-other]" ? { value: "  Ship Antigravity support  " } : null);
  const answerCard = {
    dataset: { card: "r:elic-1" },
    style: {},
    querySelectorAll: (sel) => (sel === ".qq" ? [multiQQ, freeQQ] : []),
  };
  await dockcards.dispatch("click", {
    target: { closest: (sel) => (sel === ".card" ? answerCard : sel === "[data-answer]" ? {} : null) },
  });
  assert.deepEqual(JSON.parse(JSON.stringify(calls[2])), {
    op: "answer",
    provider: "antigravity",
    id: "ag-1",
    reqId: "elic-1",
    answers: {
      targets: ["macOS", "Android"],
      notes: ["Ship Antigravity support"],
    },
  });
});

test("slash-command autocomplete filters, navigates, inserts, and dismisses cleanly", () => {
  const input = makeElement("textarea", "input");
  const menu = makeElement("div", "slash-menu");
  const composer = makeElement("form", "composer");
  const buttons = [];
  menu.querySelectorAll = () => buttons;

  const ctx = vm.createContext({
    Map,
    Set,
    Math,
    Array,
    Native: null,
    I: { shield: "S", edit: "E", shieldok: "A", lock: "P", rocket: "F", gear: "G", down: "V" },
    S: {
      provider: "antigravity",
      sheet: null,
      meta: {
        commands: [
          { name: "goal", description: "Run an autonomous goal", inputHint: "<objective>" },
          { name: "compact", description: "Summarize conversation context" },
          { name: "schedule", description: "Schedule a recurring prompt" },
          { name: "plan", description: "Draft a step-by-step plan" },
        ],
      },
    },
    $: (id) => ({ input, "slash-menu": menu, composer }[id] || makeElement("div", id)),
    esc: (s) => String(s ?? ""),
    conn: () => null,
    gauge: () => "",
    matchMedia: () => ({ matches: true }),
    innerHeight: 800,
    voiceState: { phase: "idle" },
    sendText() {},
  });
  const compStart = appSource.indexOf("const PERMS =");
  const compEnd = appSource.indexOf('$("perm-chip").onclick');
  vm.runInContext(appSource.slice(compStart, compEnd), ctx);

  // Query parsing.
  assert.equal(ctx.slashQuery("/", 1), "");
  assert.equal(ctx.slashQuery("/go", 3), "go");
  assert.equal(ctx.slashQuery("/goal run tests", 15), null);
  assert.equal(ctx.slashQuery("hello /goal", 11), null);

  // Prefix matches sort ahead of description matches.
  const filtered = ctx.filterSlashCommands(ctx.S.meta.commands, "p");
  assert.equal(filtered[0].name, "plan");
  assert.ok(filtered.some((c) => c.name === "compact"));

  // Typing "/" opens menu and Escape dismisses.
  input.value = "/co";
  input.selectionStart = 3;
  input.dispatch("input");
  assert.equal(menu.classList.contains("hidden"), false);
  assert.match(menu.innerHTML, /\/compact/);
  assert.equal(input.getAttribute("aria-expanded"), "true");

  // Pressing Enter inserts "/compact " and dismisses without submitting form.
  let prevented = false;
  input.dispatch("keydown", { key: "Enter", shiftKey: false, preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(input.value, "/compact ");
  assert.equal(input.selectionStart, 9);
  assert.equal(menu.classList.contains("hidden"), true);
  assert.equal(composer._submitted, undefined);

  // Escape dismisses when open, and blur also dismisses.
  input.value = "/";
  input.selectionStart = 1;
  input.dispatch("input");
  assert.equal(menu.classList.contains("hidden"), false);
  input.dispatch("keydown", { key: "Escape", preventDefault() {}, stopPropagation() {} });
  assert.equal(menu.classList.contains("hidden"), true);

  input.value = "/g";
  input.selectionStart = 2;
  input.dispatch("input");
  assert.equal(menu.classList.contains("hidden"), false);
  input.dispatch("blur");
  assert.equal(menu.classList.contains("hidden"), true);

  // Inserting preserves trailing text.
  const applied = ctx.applySlashCommand("/go fix tests", 3, "goal");
  assert.deepEqual(JSON.parse(JSON.stringify(applied)), { value: "/goal fix tests", caret: 6 });
});

test("Antigravity subagent view uses Antigravity labels, stays isolated, and remains strictly read-only", async () => {
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
    S: { provider: "antigravity", view: "thread", sid: "mac", id: "parent-1", sheet: "agents", open: new Set() },
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
              agents: [{ id: "sub-1", provider: "antigravity", agentName: "Research Worker", running: true, agentState: "running" }],
            };
          },
        },
      ],
    ]),
  });
  ctx.window = ctx;
  vm.runInContext(agentsSource, ctx);

  assert.equal(vm.runInContext('agentLabel({ provider: "antigravity", agentName: "Planner Worker", title: "Fallback" })', ctx), "Planner Worker");
  assert.equal(vm.runInContext('agentLabel({ provider: "antigravity", title: "Subtask Title" })', ctx), "Subtask Title");
  assert.equal(vm.runInContext('agentLabel({ provider: "antigravity" })', ctx), "Subagent");

  // Verify cache isolation across all 3 providers.
  const agList = await ctx.readAgents("mac", "parent-1", false, "antigravity");
  const claudeList = await ctx.readAgents("mac", "parent-1", false, "claude");
  assert.equal(agList.agents[0].id, "sub-1");
  assert.equal(claudeList.agents[0].id, "sub-1");
  assert.deepEqual(requests.slice(0, 2).map((r) => r.provider), ["antigravity", "claude"]);

  ctx.SHEETS.agents(sh, { server: "mac", parent: "parent-1", provider: "antigravity" });
  assert.match(sh.innerHTML, /<h2>Antigravity Subagents<\/h2>/);

  ctx.S.sheet = "agent";
  await ctx.SHEETS.agent(sh, {
    server: "mac",
    parent: "parent-1",
    provider: "antigravity",
    agent: { id: "sub-1", provider: "antigravity", agentName: "Research Worker" },
  });
  assert.match(sh.innerHTML, /Read Only/);
  assert.match(sh.innerHTML, /Managed By The Parent Chat/);
  assert.doesNotMatch(sh.innerHTML, /<textarea|id="composer"|id="send"|data-d="accept"/);
  assert.equal(requests.at(-1).op, "agentOpen");
  assert.equal(requests.at(-1).provider, "antigravity");
  assert.equal(ctx.onAgentMessage({ s: { id: "mac" } }, { id: "sub-1", t: "meta", meta: { title: "Wrong" } }, "codex"), false);
  assert.equal(ctx.onAgentMessage({ s: { id: "mac" } }, { id: "sub-1", t: "meta", meta: { title: "Right" } }, "antigravity"), true);
});

test("3-provider usage rendering, Antigravity multi-group weekly quotas, and non-fatal quotaError", async () => {
  const requests = [];
  const ctx = vm.createContext({
    Map,
    Date,
    Number,
    Math,
    Intl,
    AbortSignal,
    Error,
    AGENT_LOGOS: { openai: "M1 1", claude: "M2 2", antigravity: "M3 3" },
    I: { down: "v", chev: ">" },
    esc: (s) => String(s ?? ""),
    serverName: () => "Studio",
    conns: new Map([["m1", { ok: true, base: "http://m1", s: { token: "tok" } }]]),
    S: { sheet: "context", sid: "m1", provider: "antigravity", servers: [{ id: "m1" }] },
    SHEETS: {},
    $: () => makeElement("div"),
    setInterval() {},
    document: { hidden: false },
    openSheet() {},
    fetch: (url) => new Promise((resolve) => requests.push({ url, resolve })),
  });
  vm.runInContext(usageSource, ctx);

  // Full 3-provider rendering with Antigravity multi-group windows + sentinel resetAt=0.
  const agProvider = {
    provider: "antigravity",
    plan: "google_ai_ultra",
    windows: [
      { label: "Gemini Weekly", used: 15, remaining: 85, resetAt: 0, minutes: 10080 },
      { label: "Gemini 5-Hour", used: 0, remaining: 100, resetAt: 0, minutes: 300 },
      { label: "Claude And GPT Weekly", used: 82, remaining: 18, resetAt: Math.floor(Date.now() / 1000) + 7200, minutes: 10080 },
      { label: "Claude And GPT 5-Hour", used: 25, remaining: 75, resetAt: Math.floor(Date.now() / 1000) + 3600, minutes: 300 },
    ],
    today: { tokens: 12000, costUsd: null, unpricedTokens: 12000 },
    days30: { tokens: 450000, costUsd: null, unpricedTokens: 450000, input: 300000, cached: 100000, writes: 0, output: 50000 },
    models: [{ model: "gemini-4-neon", tokens: 450000, costUsd: null }],
  };
  const cardHTML = ctx.usageProvider(agProvider);
  assert.match(cardHTML, /<h2>Antigravity<\/h2>/);
  assert.match(cardHTML, /d="M3 3"/);
  assert.match(cardHTML, /Google AI Ultra/);
  assert.match(cardHTML, /Gemini Weekly/);
  assert.match(cardHTML, /Gemini 5-Hour/);
  assert.match(cardHTML, /Claude And GPT Weekly/);
  assert.match(cardHTML, /Claude And GPT 5-Hour/);
  assert.match(cardHTML, /No Active Window/);
  assert.match(cardHTML, /gemini-4-neon/);

  // Non-fatal quotaError keeps Local Activity and 30-Day Breakdown visible.
  const degradedHTML = ctx.usageProvider({
    ...agProvider,
    windows: [],
    quotaError: "Sign In To Antigravity CLI To Read Account Quotas",
  });
  assert.match(degradedHTML, /Sign In To Antigravity CLI To Read Account Quotas/);
  assert.match(degradedHTML, /Local Activity/);
  assert.match(degradedHTML, /gemini-4-neon/);

  // Multi-group weeklyQuota picks tightest weekly group for tone and exposes all weekly windows.
  const q = ctx.weeklyQuota({ providers: [agProvider] }, "antigravity");
  assert.equal(q.window.label, "Claude And GPT Weekly");
  assert.equal(q.used, 82);
  assert.equal(q.remaining, 18);
  assert.equal(q.tone, "caution");
  assert.equal(q.windows.length, 2);

  // Context sheet renders both weekly groups for Antigravity.
  const target = makeElement("section", "context-weekly");
  target.querySelector = () => makeElement("button");
  const sheet = { querySelector: (sel) => (sel === "#context-weekly" ? target : null) };
  const pending = ctx.showContextWeekly(sheet, "m1", "antigravity");
  assert.equal(requests.length, 1);
  assert.match(requests[0].url, /provider=antigravity/);
  requests[0].resolve({ ok: true, json: async () => ({ providers: [agProvider] }) });
  await pending;
  assert.match(target.innerHTML, /Gemini Weekly/);
  assert.match(target.innerHTML, /Claude And GPT Weekly/);
  assert.match(target.innerHTML, /85% Left/);
  assert.match(target.innerHTML, /18% Left/);
});

test("diff viewer accepts both m.text and m.diff, and file:// brain image artifacts resolve safely", () => {
  const diffFiles = makeElement("div");
  const diffBody = makeElement("div");
  const diffHeading = makeElement("h2");
  const sheet = {
    querySelector: (sel) => ({ ".diff-files": diffFiles, ".diff-body": diffBody, h2: diffHeading }[sel]),
  };
  const ctx = vm.createContext({
    URL,
    decodeURIComponent,
    encodeURIComponent,
    S: { sheet: "diff", sid: "m1", meta: { cwd: "/work" }, diffSource: { cwd: "/work" } },
    conns: new Map([["m1", { base: "http://127.0.0.1:8787" }]]),
    $: () => sheet,
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
  });
  const imgStart = appSource.indexOf("function imageSrc(");
  const imgEnd = appSource.indexOf("function isAudioPath(");
  const diffStart = appSource.indexOf("function showDiff(");
  const diffEnd = appSource.indexOf("// ----------------------------------------------------------------- sheets --");
  vm.runInContext(appSource.slice(imgStart, imgEnd) + "\n" + appSource.slice(diffStart, diffEnd), ctx);

  ctx.showDiff({ text: "--- a/web/app.js\n+++ b/web/app.js\n@@ -1 +1 @@\n-old\n+new\n" });
  assert.match(diffBody.innerHTML, /web\/app\.js/);
  assert.equal(diffHeading.textContent, "1 File, +1 -1");

  const imgHTML = ctx.imageHTML("file:///Users/siraj/.gemini/antigravity/brain/conv-1/diagram.png", "Diagram", "m1");
  assert.match(imgHTML, /data-relay-image="\/Users\/siraj\/\.gemini\/antigravity\/brain\/conv-1\/diagram\.png"/);
});

test("Antigravity organization requests preserve provider scoping", async () => {
  const calls = [];
  const ctx = vm.createContext({
    Map,
    Set,
    S: { provider: "antigravity", machine: "all", view: "project" },
    conns: new Map([
      [
        "m1",
        {
          groups: [{ id: "g1", name: "Core Work", roots: ["/work"] }],
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
    threadRow: (t) => `<button>${t.title}</button>`,
  });
  ctx.window = ctx;
  vm.runInContext(orgSource, ctx);

  await ctx.openNativeGroup("m1", "g1", false);
  assert.equal(calls.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), { op: "group", id: "g1", provider: "antigravity" });
});

test("Antigravity thread labels clean cached XML, model catalog skips caching empty errors, and new-chat retries cleanly", () => {
  const msgCtx = vm.createContext({});
  vm.runInContext(fs.readFileSync("web/messages.js", "utf8") + "\nglobalThis.MessageContent = MessageContent;", msgCtx);

  // Thread row & header cleaning.
  const tTitleEl = makeElement("div", "t-title");
  const tSubEl = makeElement("div", "t-sub");
  const rowCtx = vm.createContext({
    Map,
    Set,
    Date,
    String,
    MessageContent: msgCtx.MessageContent,
    S: {
      sid: "m1",
      id: "ag-1",
      machine: "m1",
      servers: [{ id: "m1", name: "Studio" }],
      meta: { title: "<USER_REQUEST>\nHeader Title\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\ntime\n</ADDITIONAL_METADATA>" },
    },
    $: (id) => ({ "t-title": tTitleEl, "t-sub": tSubEl }[id] || makeElement("div", id)),
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
    ago: () => "1m",
    dur: () => "1s",
    serverName: () => "Studio",
    computerDotClass: () => "on",
    conn: () => ({ ok: true, state: "on" }),
    findThread: () => ({ id: "ag-1", title: "", preview: "<USER_REQUEST>\nPreview fallback" }),
  });
  const rowStart = appSource.indexOf("function cleanThreadText(");
  const rowEnd = appSource.indexOf("function projectsOf(");
  const hdrStart = appSource.indexOf("function renderThreadHeader(");
  const hdrEnd = appSource.indexOf('$("t-title").onclick');
  vm.runInContext(appSource.slice(rowStart, rowEnd) + "\n" + appSource.slice(hdrStart, hdrEnd), rowCtx);

  const rowHTML = rowCtx.threadRow({
    sid: "m1",
    id: "ag-1",
    title: "<SYSTEM_MESSAGE>\nwakeup\n</SYSTEM_MESSAGE>",
    preview: "<USER_REQUEST>\nFix Antigravity XML\n</USER_REQUEST>\n<ADDITIONAL_METADATA>meta</ADDITIONAL_METADATA>",
    cwd: "/work/app",
    recency: Date.now(),
  });
  assert.match(rowHTML, /Fix Antigravity XML/);
  assert.doesNotMatch(rowHTML, /USER_REQUEST|ADDITIONAL_METADATA|SYSTEM_MESSAGE|wakeup/);

  rowCtx.renderThreadHeader();
  assert.equal(tTitleEl.textContent, "Header Title");

  // Conn.onMessage never caches empty/errored model catalog over valid models.
  const sockets = [];
  class Socket {
    constructor(url) { this.url = url; this.readyState = 1; this.sent = []; sockets.push(this); }
    send(t) { this.sent.push(JSON.parse(t)); }
    close() { this.readyState = 3; }
    open() { this.readyState = 1; this.onopen?.(); }
    message(v) { this.onmessage?.({ data: JSON.stringify(v) }); }
  }
  let redrawCount = 0;
  const connCtx = vm.createContext({
    Native: null,
    WebSocket: Socket,
    URL,
    Set,
    Map,
    Date,
    JSON,
    Math,
    Array,
    AbortSignal,
    fetch: async () => ({ status: 204 }),
    setTimeout: () => 1,
    clearTimeout() {},
    setInterval: () => 1,
    clearInterval() {},
    store: { get: (_, d) => d, set() {} },
    S: { provider: "antigravity", view: "home", servers: [], sheet: "new", redraw: () => { redrawCount++; } },
    window: {},
    document: { querySelectorAll: () => [] },
    scheduleRender() {},
    saveServers() {},
    mark() {},
    serverName: () => "Studio",
    syncProviderButtons() {},
  });
  vm.runInContext(appSource.slice(appSource.indexOf("function computerStatus("), appSource.indexOf("function syncConns()")) + ";globalThis.Conn=Conn;", connCtx);
  const c = new connCtx.Conn({ id: "m1", url: "http://127.0.0.1:8787", alts: [], token: "tok" });
  sockets[0].open();
  sockets[0].message({ t: "models", provider: "antigravity", models: [] });
  assert.equal(c.byProvider.antigravity.models, undefined);
  assert.equal(c.models.models.length, 0);
  sockets[0].message({ t: "models", provider: "antigravity", models: [{ slug: "gemini-4-neon", name: "Gemini 4 Neon", efforts: ["low", "high"] }] });
  assert.equal(c.byProvider.antigravity.models.models.length, 1);
  // Subsequent empty error does not wipe out valid cached models.
  sockets[0].message({ t: "models", provider: "antigravity", models: [], error: "temporary timeout" });
  assert.equal(c.byProvider.antigravity.models.models.length, 1);
  assert.equal(c.models.models.length, 1);
  assert.ok(redrawCount >= 3);

  // SHEETS.new shows Retry on empty catalog, re-requests on computer switch, and preserves draft text/caret on async models arrival.
  const sent1 = [], sent2 = [];
  const c1 = { threads: [{ cwd: "/work/a" }], projects: [], home: "/work", capabilities: { antigravity: true }, models: null, send: (m) => sent1.push(m) };
  const c2 = { threads: [{ cwd: "/work/b" }], projects: [], home: "/work", capabilities: { antigravity: true }, models: { models: [] }, send: (m) => sent2.push(m) };
  const textEl = makeElement("textarea", "nt-text");
  const cwdEl = makeElement("input", "nt-cwd");
  const compEl = makeElement("select", "nt-computer");
  let restoredSelection = null;
  textEl.setSelectionRange = (a, b) => { restoredSelection = [a, b]; };
  const newSheet = makeElement("div", "sheet");
  newSheet.contains = (el) => el === textEl;
  newSheet.querySelector = (sel) => ({ "#nt-text": textEl, "#nt-cwd": cwdEl, "#nt-computer": compEl }[sel] || null);
  const newCtx = vm.createContext({
    Map,
    Set,
    Array,
    Number,
    Object,
    String,
    CSS: { escape: (x) => x },
    I: { close: "X", down: "V", check: "C" },
    S: {
      provider: "antigravity",
      sheet: "new",
      machine: "m1",
      servers: [{ id: "m1", name: "Mac 1", url: "http://1" }, { id: "m2", name: "Mac 2", url: "http://2" }],
      newAttach: [],
      newDraft: null,
    },
    SHEETS: {},
    conns: new Map([["m1", c1], ["m2", c2]]),
    document: { activeElement: textEl },
    esc: (s) => String(s ?? ""),
    base: (p) => String(p || "").split("/").pop(),
    hostOf: (u) => u,
    loadingHTML: (s) => s,
    attHTML: () => "",
    projectsOf: () => [],
    shortModel: (s) => s,
    effortName: (s) => s,
    attachmentBatches: new Map(),
    persistAttachmentBatches() {},
    store: { get: (_, d) => d, set() {} },
    closeSheet() {},
    openSheet() {},
  });
  vm.runInContext(newChatSource, newCtx);
  newCtx.SHEETS.new(newSheet);
  assert.match(newSheet.innerHTML, /Loading Models/);

  // Empty response triggers Retry UI instead of infinite spinner.
  c1.models = { models: [] };
  newCtx.S.redraw();
  assert.match(newSheet.innerHTML, /data-model-retry/);
  assert.match(newSheet.innerHTML, /No models available yet\./);

  // User types a prompt while models arrive asynchronously -> text and caret preserved.
  textEl.value = "Deploy release";
  textEl.selectionStart = 6;
  textEl.selectionEnd = 6;
  textEl.oninput();
  c1.models = { models: [{ slug: "gemini-4-neon", name: "Gemini 4 Neon", efforts: ["low", "high"] }], default: { model: "gemini-4-neon" } };
  newCtx.S.redraw();
  assert.equal(newCtx.S.newDraft.text, "Deploy release");
  assert.deepEqual(restoredSelection, [6, 6]);
  assert.match(newSheet.innerHTML, /Gemini 4 Neon/);

  // Switching computers clears stale empty catalog and requests models for m2.
  compEl.value = "m2";
  compEl.onchange();
  assert.equal(c2.models, null);
  assert.ok(sent2.some((m) => m.op === "models" && m.provider === "antigravity"));
});

test("Notification Controls UI defaults, toggle persistence to Native bridge, browser filtering, and boot sync", () => {
  const storage = new Map();
  const nativeSaved = [];
  let nativeReturned = "";
  let nativeMaster = true;
  const toasts = [];
  const browserNotifs = [];
  const buzzes = [];

  class FakeNotification {
    constructor(title, opts) { browserNotifs.push({ title, body: opts?.body }); }
  }
  FakeNotification.permission = "granted";

  function buildCtx(withNative) {
    const ctx = vm.createContext({
      Map,
      Set,
      Array,
      JSON,
      String,
      Object,
      Notification: FakeNotification,
      Native: withNative ? {
        notificationsEnabled: () => nativeMaster,
        setNotifications: (v) => { nativeMaster = !!v; },
        getNotificationPrefs: () => nativeReturned,
        setNotificationPrefs: (j) => { nativeSaved.push(j); nativeReturned = j; },
      } : null,
      store: {
        get: (k, d) => (storage.has(k) ? JSON.parse(storage.get(k)) : d),
        set: (k, v) => storage.set(k, JSON.stringify(v)),
      },
      S: { provider: "antigravity", view: "home", sid: "m1", id: "", servers: [], sheet: "macs" },
      conns: new Map(),
      recoveryJobs: new Map(),
      I: { trash: "T", download: "D", chev: ">", plug: "P", copy: "C", gear: "G" },
      esc: (s) => String(s ?? ""),
      hostOf: (u) => u,
      computerDotClass: () => "on",
      computerStatus: () => "Connected",
      cleanThreadText: (s) => String(s || ""),
      toast: (msg) => toasts.push(msg),
      buzz: (ms) => buzzes.push(ms),
      selectProvider() {},
      openThread() {},
      saveServers() {},
      syncConns() {},
      renderHome() {},
      openSheet() {},
      closeSheet() {},
      openUsage() {},
      mi: () => "",
      document: { visibilityState: "hidden" },
    });
    const evStart = appSource.indexOf("function onEvent(");
    const evEnd = appSource.indexOf("// -------------------------------------------------------------- utilities --");
    const macsStart = appSource.indexOf("  macs(sh) {");
    const macsEnd = appSource.indexOf("// ------------------------------------------------------- Android intents --", macsStart);
    vm.runInContext(
      appSource.slice(evStart, evEnd) +
        "\nconst SHEETS = {\n" +
        appSource.slice(macsStart, macsEnd) +
        "\nglobalThis.SHEETS = SHEETS;",
      ctx
    );
    return ctx;
  }

  // 1. Defaults are all true when nothing is stored.
  const ctx = buildCtx(true);
  assert.deepEqual(JSON.parse(JSON.stringify(ctx.loadNotificationPrefs())), { completed: true, attention: true, failed: true });

  // 2. Render Notification Controls in SHEETS.macs with Title Case labels and toggle Turn Completed off.
  const sheet = makeElement("div", "sheet");
  ctx.SHEETS.macs(sheet);
  assert.match(sheet.innerHTML, /Turn Completed/);
  assert.match(sheet.innerHTML, /Notify When An Agent Finishes A Turn/);
  assert.match(sheet.innerHTML, /Questions And Approvals/);
  assert.match(sheet.innerHTML, /Notify When An Agent Needs Input Or Permission/);
  assert.match(sheet.innerHTML, /Turn Failed/);
  assert.match(sheet.innerHTML, /Notify When A Turn Fails/);
  assert.match(sheet.innerHTML, /role="switch" aria-checked="true"[^>]*data-notify-pref="completed"/);

  sheet.onclick({
    target: {
      dataset: {},
      closest: (sel) => (sel === "[data-notify-pref]" ? { dataset: { notifyPref: "completed" } } : null),
    },
  });
  assert.equal(nativeSaved.at(-1), '{"completed":false,"attention":true,"failed":true}');
  assert.equal(storage.get("fc_notify_prefs"), '{"completed":false,"attention":true,"failed":true}');
  assert.match(sheet.innerHTML, /role="switch" aria-checked="false"[^>]*data-notify-pref="completed"/);

  // 3. Browser / web onEvent suppresses completed notifications while still allowing attention & failed.
  const webCtx = buildCtx(false);
  webCtx.loadNotificationPrefs();
  webCtx.onEvent({ s: { id: "m1" } }, { kind: "completed", provider: "antigravity", id: "t1", title: "Finished Task" });
  assert.equal(toasts.filter((t) => t.startsWith("Finished:")).length, 0);
  assert.equal(browserNotifs.length, 0);

  webCtx.onEvent({ s: { id: "m1" } }, { kind: "attention", provider: "antigravity", id: "t1", title: "Needs Approval" });
  assert.equal(toasts.filter((t) => t.startsWith("Needs You:")).length, 1);
  assert.equal(browserNotifs.length, 1);
  assert.equal(browserNotifs[0].body, "Needs Approval");
  assert.equal(buzzes.length, 1);

  // 4. Boot sync: Native.getNotificationPrefs wins over localStorage if present.
  storage.set("fc_notify_prefs", '{"completed":true,"attention":true,"failed":true}');
  nativeReturned = '{"completed":false,"attention":true,"failed":false}';
  const bootCtx = buildCtx(true);
  assert.deepEqual(JSON.parse(JSON.stringify(bootCtx.loadNotificationPrefs())), { completed: false, attention: true, failed: false });
  assert.equal(storage.get("fc_notify_prefs"), '{"completed":false,"attention":true,"failed":false}');

  // 5. Boot sync: when Native has no saved prefs, localStorage is pushed to Native.
  nativeReturned = "";
  nativeSaved.length = 0;
  storage.set("fc_notify_prefs", '{"completed":false,"attention":false,"failed":true}');
  const pushCtx = buildCtx(true);
  assert.deepEqual(JSON.parse(JSON.stringify(pushCtx.loadNotificationPrefs())), { completed: false, attention: false, failed: true });
  assert.equal(nativeSaved.at(-1), '{"completed":false,"attention":false,"failed":true}');
});

test("Swipe-down-to-dismiss sheets respects scrollTop>0, interactive controls, thresholds, and resets styles", () => {
  const sh = makeElement("div", "sheet");
  sh.offsetHeight = 400;
  sh.style = { transform: "", transition: "", bottom: "" };
  sh.getAnimations = () => [];
  const scrim = makeElement("div", "scrim");
  scrim.style = { opacity: "", pointerEvents: "" };

  const listeners = new Map();
  sh.addEventListener = (type, fn) => {
    if (!listeners.has(type)) listeners.set(type, []);
    listeners.get(type).push(fn);
  };
  const emit = (type, ev) => { for (const fn of listeners.get(type) || []) fn(ev); };

  let closed = 0;
  let now = 1000;
  const ctx = vm.createContext({
    Set,
    Map,
    Math,
    Date: { now: () => now },
    setTimeout: (fn) => { fn(); return 1; },
    clearTimeout() {},
    S: { sheet: "macs", view: "home", provider: "antigravity" },
    $: (id) => ({ sheet: sh, scrim, composer: { offsetHeight: 60 }, jump: makeElement("button") }[id] || makeElement("div", id)),
    history: { state: {}, pushState() {}, replaceState() {}, back() {} },
    document: { hidden: true },
    reducedMotion: { matches: true },
    window: {},
    SHEETS: { macs() {} },
    closeSheet() { closed++; ctx.S.sheet = null; ctx.resetSheetSwipeStyles(sh, scrim); },
  });
  const start = appSource.indexOf("function resetSheetSwipeStyles(");
  const end = appSource.indexOf("function openSheet(");
  vm.runInContext(appSource.slice(start, end), ctx);
  ctx.bindSheetSwipe(sh, scrim);

  const bodyTarget = { scrollTop: 0, parentElement: sh, closest: () => null };
  const scrolledTarget = { scrollTop: 42, parentElement: sh, closest: () => null };
  const rangeTarget = { scrollTop: 0, parentElement: sh, closest: (sel) => (sel.includes('input[type="range"]') ? {} : null) };
  const gripTarget = { scrollTop: 0, parentElement: sh, closest: (sel) => (sel.includes(".grip") ? {} : null) };

  // 1. Scrollable content with scrollTop > 0 must NOT trigger sheet dismiss.
  emit("pointerdown", { pointerId: 1, button: 0, clientX: 100, clientY: 100, target: scrolledTarget });
  now += 50;
  emit("pointermove", { pointerId: 1, clientX: 100, clientY: 260, preventDefault() {} });
  emit("pointerup", { pointerId: 1 });
  assert.equal(closed, 0);
  assert.equal(sh.style.transform, "");

  // 2. Interactive range slider must NOT trigger sheet dismiss.
  emit("pointerdown", { pointerId: 2, button: 0, clientX: 100, clientY: 100, target: rangeTarget });
  now += 50;
  emit("pointermove", { pointerId: 2, clientX: 100, clientY: 260, preventDefault() {} });
  emit("pointerup", { pointerId: 2 });
  assert.equal(closed, 0);

  // 3. Small downward drag snaps back without closing and resets inline styles.
  emit("pointerdown", { pointerId: 3, button: 0, clientX: 100, clientY: 100, target: bodyTarget });
  now += 200;
  emit("pointermove", { pointerId: 3, clientX: 100, clientY: 145, preventDefault() {} });
  assert.equal(sh.style.transform, "translateY(45px)");
  assert.ok(Number(scrim.style.opacity) < 1);
  emit("pointerup", { pointerId: 3 });
  assert.equal(closed, 0);
  assert.equal(sh.style.transform, "");
  assert.equal(scrim.style.opacity, "");

  // 4. Full-screen sheet (.full) ignores swipe-down dismiss.
  sh.classList.add("full");
  emit("pointerdown", { pointerId: 4, button: 0, clientX: 100, clientY: 50, target: gripTarget });
  now += 40;
  emit("pointermove", { pointerId: 4, clientX: 100, clientY: 250, preventDefault() {} });
  emit("pointerup", { pointerId: 4 });
  assert.equal(closed, 0);
  sh.classList.remove("full");

  // 5. Dragging grip past dismiss threshold closes sheet cleanly via touch events.
  let prevented = false;
  emit("touchstart", { touches: [{ identifier: 7, clientX: 100, clientY: 60 }], target: gripTarget });
  now += 40;
  emit("touchmove", { touches: [{ identifier: 7, clientX: 102, clientY: 200 }], cancelable: true, preventDefault: () => { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(sh.style.transform, "translateY(140px)");
  emit("touchend", {});
  assert.equal(closed, 1);
  assert.equal(sh.style.transform, "");
  assert.equal(scrim.style.opacity, "");
});

test("window.handleAndroidBack unwinds slash menu, nested sheets, thread, project, and returns false at home root", () => {
  const sh = makeElement("div", "sheet");
  sh.style = { transform: "", transition: "", bottom: "" };
  sh.getAnimations = () => [];
  const scrim = makeElement("div", "scrim");
  scrim.style = { opacity: "", pointerEvents: "" };
  const replacedStates = [];
  const pushedStates = [];
  let historyBackCalls = 0;
  let leftThreads = 0;
  let renderedHome = 0;
  let openedProjects = [];
  let openedSheets = [];

  const ctx = vm.createContext({
    Set,
    Map,
    Math,
    Date,
    setTimeout: (fn) => { fn(); return 1; },
    clearTimeout() {},
    S: { view: "home", provider: "antigravity", sid: "", id: "", cwd: "", group: null, sheet: null, sheetOpts: null },
    slashState: { open: false },
    closeSlashMenu() { ctx.slashState.open = false; },
    $: (id) => ({ sheet: sh, scrim, composer: { offsetHeight: 60 }, jump: makeElement("button") }[id] || makeElement("div", id)),
    history: {
      state: { view: "home", provider: "antigravity" },
      pushState(st) { this.state = st; pushedStates.push(st); },
      replaceState(st) { this.state = st; replacedStates.push(st); },
      back() { historyBackCalls++; },
    },
    document: { hidden: true },
    reducedMotion: { matches: true },
    feed: { scrollHeight: 0, clientHeight: 0, scrollTop: 0 },
    Native: null,
    appUpdate: { phase: "idle" },
    appUpdateInstallWanted: false,
    SHEETS: {
      macs(_s, opts) { openedSheets.push(["macs", opts]); },
      connection(_s, opts) { openedSheets.push(["connection", opts]); },
    },
    show(v) { ctx.S.view = v; },
    renderHome() { renderedHome++; },
    leaveThread() { leftThreads++; ctx.S.id = ""; },
    openProject(cwd, push) {
      if (push) ctx.pushNavSnapshot();
      ctx.S.group = null;
      ctx.S.cwd = cwd;
      ctx.S.view = "project";
      openedProjects.push(cwd);
    },
    openThread(sid, id, push) {
      if (push) ctx.pushNavSnapshot();
      ctx.S.sid = sid;
      ctx.S.id = id;
      ctx.S.view = "thread";
    },
  });
  ctx.window = ctx;
  const start = appSource.indexOf("let sheetGeneration = 0;");
  const end = appSource.indexOf('$("scrim").onclick');
  vm.runInContext(appSource.slice(start, end), ctx);

  // Navigate home -> project -> thread, open nested sheets macs -> connection, then open slash menu.
  ctx.openProject("/work/fastcontrol", true);
  ctx.openThread("m1", "ag-1", true);
  ctx.openSheet("macs", {});
  ctx.openSheet("connection", { server: "m1" });
  ctx.slashState.open = true;

  // 1. Slash menu closes first.
  assert.equal(ctx.window.handleAndroidBack(), true);
  assert.equal(ctx.slashState.open, false);
  assert.equal(ctx.S.sheet, "connection");

  // 2. Nested sheet returns to parent sheet (macs).
  assert.equal(ctx.window.handleAndroidBack(), true);
  assert.equal(ctx.S.sheet, "macs");

  // 3. Parent sheet closes cleanly without calling history.back().
  assert.equal(ctx.window.handleAndroidBack(), true);
  assert.equal(ctx.S.sheet, null);
  assert.equal(historyBackCalls, 0);

  // 4. Thread navigates back to parent project.
  assert.equal(ctx.window.handleAndroidBack(), true);
  assert.equal(leftThreads, 1);
  assert.equal(ctx.S.view, "project");
  assert.equal(openedProjects.at(-1), "/work/fastcontrol");

  // 5. Project navigates back to home.
  assert.equal(ctx.window.handleAndroidBack(), true);
  assert.equal(ctx.S.view, "home");
  assert.equal(renderedHome, 1);

  // 6. Root home with no modal returns false (triggers Android double-back toast).
  assert.equal(ctx.window.handleAndroidBack(), false);
  assert.equal(historyBackCalls, 0);

  // 7. On-screen .back buttons delegate to window.handleAndroidBack() before falling back to history.back(),
  //    and popstate to home clears navStack cleanly.
  assert.match(appSource, /querySelectorAll\("\.back"\)\) b\.onclick = \(\) => \{ if \(!window\.handleAndroidBack\?\.\(\)\) history\.back\(\); \};/);
  assert.match(appSource, /else if \(!st\.sheet\) \{ navStack = \[\]; leaveThread\(\); show\("home"\); renderHome\(\); \}/);

  // 8. Swipe-dismiss closeSheet preserves dragged translateY into exit animation before hiding and resetting styles.
  const animFrames = [];
  let finishAnim;
  const animSh = makeElement("div", "sheet");
  animSh.offsetHeight = 400;
  animSh.style = { transform: "translateY(140px)", transition: "none", bottom: "" };
  animSh.getAnimations = () => [];
  animSh.animate = (frames) => {
    animFrames.push(frames);
    return { finished: new Promise((res) => { finishAnim = res; }) };
  };
  const animScrim = makeElement("div", "scrim");
  animScrim.style = { opacity: "0.65", pointerEvents: "" };
  const animCtx = vm.createContext({
    Set,
    Map,
    Math,
    Date,
    setTimeout: (fn) => { fn(); return 1; },
    clearTimeout() {},
    S: { view: "home", provider: "antigravity", sid: "", id: "", sheet: "macs", sheetOpts: {} },
    $: (id) => ({ sheet: animSh, scrim: animScrim, composer: { offsetHeight: 60 }, jump: makeElement("button") }[id] || makeElement("div", id)),
    history: { state: {}, pushState() {}, replaceState() {}, back() {} },
    document: { hidden: false },
    reducedMotion: { matches: false },
    getComputedStyle: () => ({ transform: "none" }),
    feed: { scrollHeight: 0, clientHeight: 0, scrollTop: 0 },
    Native: null,
    appUpdate: { phase: "idle" },
    appUpdateInstallWanted: false,
    SHEETS: {},
  });
  animCtx.window = animCtx;
  vm.runInContext(appSource.slice(start, end), animCtx);
  animCtx.closeSheet(true);
  assert.equal(animFrames.length, 1);
  assert.equal(animFrames[0][0].transform, "translateY(140px)");
  assert.equal(animFrames[0][1].transform, "translateY(400px)");
  assert.equal(animSh.style.transform, "translateY(140px)");
  finishAnim();
  return Promise.resolve().then(() => {
    assert.equal(animSh.style.transform, "");
    assert.equal(animScrim.style.opacity, "");
    assert.equal(animSh.classList.contains("hidden"), true);
  });
});
