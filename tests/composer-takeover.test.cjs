const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const root = path.join(__dirname, "..");
const htmlSource = fs.readFileSync(path.join(root, "web/index.html"), "utf8");
const cssSource = fs.readFileSync(path.join(root, "web/style.css"), "utf8");
const appSource = fs.readFileSync(path.join(root, "web/app.js"), "utf8");
const watchServiceSource = fs.readFileSync(
  path.join(root, "android/app/src/main/java/net/siraj/fastcontrol/RelayWatchService.kt"),
  "utf8"
);

function makeClassList(initial = []) {
  const set = new Set(initial);
  return {
    add: (...names) => names.forEach((n) => n && set.add(n)),
    remove: (...names) => names.forEach((n) => set.delete(n)),
    toggle: (name, force) => {
      if (force === undefined) {
        if (set.has(name)) { set.delete(name); return false; }
        set.add(name); return true;
      }
      if (force) set.add(name); else set.delete(name);
      return !!force;
    },
    contains: (name) => set.has(name),
    toString: () => [...set].join(" "),
  };
}

function makeElement(tag = "div", id = "") {
  const attrs = {};
  const el = {
    tagName: tag.toUpperCase(),
    id,
    classList: makeClassList(),
    className: "",
    innerHTML: "",
    textContent: "",
    value: "",
    disabled: false,
    dataset: {},
    style: {},
    children: [],
    childNodes: [],
    scrollHeight: 24,
    scrollTop: 0,
    setAttribute(k, v) { attrs[k] = String(v); },
    getAttribute(k) { return attrs[k] ?? null; },
    appendChild(child) { this.children.push(child); return child; },
    remove() { this.removed = true; },
    querySelector() { return null; },
    animate() {},
  };
  return el;
}

test("Composer places mode, model, and effort in slim composer-bar above composer pill", () => {
  const barIdx = htmlSource.indexOf('id="composer-bar"');
  const formIdx = htmlSource.indexOf('id="composer"');
  const toolsIdx = htmlSource.indexOf('<div class="tools">');
  assert.ok(barIdx > 0 && formIdx > barIdx && toolsIdx > formIdx);

  const barHtml = htmlSource.slice(barIdx, formIdx);
  const toolsHtml = htmlSource.slice(toolsIdx, htmlSource.indexOf("</div>", toolsIdx));
  assert.match(barHtml, /id="perm-chip"/);
  assert.match(barHtml, /id="model-chip"/);
  assert.match(barHtml, /id="effort-btn"/);
  assert.doesNotMatch(toolsHtml, /id="perm-chip"|id="model-chip"|id="effort-btn"/);
  assert.match(toolsHtml, /id="attach"/);
  assert.match(toolsHtml, /id="voice"/);
  assert.match(toolsHtml, /id="stop"/);
  assert.match(toolsHtml, /id="send"/);
  assert.match(cssSource, /\.composer-bar\s*\{[^}]*display:\s*flex;/);
  assert.match(cssSource, /\.composer-bar\s+#effort-btn\s*\{\s*margin-left:\s*auto;\s*\}/);
});

test("RelayWatchService foreground notification uses generic Computers wording and live connected count", () => {
  assert.doesNotMatch(watchServiceSource, /No Macs Paired|Watching 1 Mac|Watching \$total Macs/);
  assert.match(watchServiceSource, /"No Computers Paired"/);
  assert.match(watchServiceSource, /"\$online Of \$total Computers Connected"/);
  assert.match(watchServiceSource, /"\$total Computers Connected"/);
  assert.match(watchServiceSource, /"\$total Computers Offline"/);
});

test("Claude external mode hides Managed On Computer banner and handles idle vs running takeover on send", async () => {
  const els = {};
  for (const id of ["t-ring", "banner", "wake", "earlier", "voice", "stop", "send", "perm-chip", "model-chip", "effort-btn", "input", "slash-menu", "composer", "feed", "activity", "t-status"]) {
    els[id] = makeElement(id === "input" ? "textarea" : "div", id);
  }
  const calls = [];
  let openedSheet = null;
  const toasts = [];
  const connection = {
    state: "on",
    ok: true,
    capabilities: { codex: true, claude: true, antigravity: true, grok: true },
    models: { models: [{ slug: "claude-opus-4-6", name: "Claude Opus 4.6", efforts: ["low", "medium", "high"] }] },
    send() {},
    async call(msg) {
      calls.push(msg);
      return { ok: true, queued: false };
    },
  };

  const renderMetaStart = appSource.indexOf("function contextMetrics(");
  const renderMetaEnd = appSource.indexOf("// ------------------------------------------------- approvals and questions --");
  const sendStart = appSource.indexOf("async function sendText(");
  const sendEnd = appSource.indexOf('$("stop").onclick');
  const takeoverStart = appSource.indexOf("  takeover(sh, opts = {}) {");
  const takeoverEnd = appSource.indexOf("  rename(sh) {");

  const snippet = [
    appSource.slice(renderMetaStart, renderMetaEnd),
    appSource.slice(sendStart, sendEnd),
    "var TAKEOVER_SHEET = { " + appSource.slice(takeoverStart, takeoverEnd) + " };",
  ].join("\n");

  const ctx = vm.createContext({
    $: (id) => els[id] || null,
    document: {
      hidden: false,
      createElement: (tag) => makeElement(tag),
    },
    feed: els.feed,
    input: els.input,
    reducedMotion: { matches: true },
    Native: null,
    S: {
      sid: "s1",
      id: "claude-sess-1",
      provider: "claude",
      meta: { mode: "external", running: true, model: "claude-opus-4-6", effort: "high", permissionMode: "default" },
      turns: [],
      attach: [],
      stick: true,
    },
    voiceState: { phase: "idle" },
    attachmentBatches: new Map(),
    store: { get: () => null, set() {} },
    conn: () => connection,
    serverName: () => "m5p",
    esc: (s) => String(s ?? ""),
    nice: (s) => String(s ?? ""),
    toast: (m) => toasts.push(m),
    buzz() {},
    autosize() {},
    voiceDraftKey: () => "draft-1",
    messageHTML: (t) => t,
    captureReadingPlace: () => null,
    restoreReadingPlace() {},
    renderAttachments() {},
    persistAttachmentBatches() {},
    renderThreadHeader() {},
    renderStatus() {},
    renderCards() {},
    renderActivity() {},
    renderComposer() {},
    openSheet(name, opts) { openedSheet = { name, opts }; },
    closeSheet() { openedSheet = null; },
    providerLogoKey: () => "claude",
    AGENT_LOGOS: { claude: "" },
    loadingHTML: (s) => s,
    window: {},
    slashState: { open: false },
  });
  vm.runInContext(snippet, ctx);

  // 1. Running external Claude session hides the top banner completely.
  ctx.renderMeta();
  assert.ok(els.banner.classList.contains("hidden"));

  // 2. Sending while external Claude session is actively running opens confirmation sheet without sending yet.
  els.input.value = "continue from phone";
  await ctx.sendText("continue from phone");
  assert.equal(calls.length, 0);
  assert.equal(openedSheet?.name, "takeover");
  assert.equal(openedSheet?.opts?.computer, "m5p");

  // Confirming the sheet sends with takeover: true.
  const sh = makeElement("div", "sheet");
  ctx.TAKEOVER_SHEET.takeover(sh, openedSheet.opts);
  assert.match(sh.innerHTML, /Take Over Active Run\?/);
  assert.match(sh.innerHTML, /m5p/);
  await openedSheet.opts.onConfirm();
  assert.equal(calls.length, 1);
  assert.equal(calls[0].op, "send");
  assert.equal(calls[0].takeover, true);
  assert.equal(calls[0].text, "continue from phone");

  // 3. Idle external Claude session sends with takeover: true directly without opening a confirmation sheet.
  calls.length = 0;
  openedSheet = null;
  ctx.S.meta = { ...ctx.S.meta, mode: "external", running: false };
  ctx.renderMeta();
  assert.ok(els.banner.classList.contains("hidden"));
  await ctx.sendText("idle followup");
  assert.equal(openedSheet, null);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].op, "send");
  assert.equal(calls[0].takeover, true);
  assert.equal(calls[0].text, "idle followup");
});
