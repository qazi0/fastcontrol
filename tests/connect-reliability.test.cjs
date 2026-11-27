const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");

const appSource = fs.readFileSync("web/app.js", "utf8");

function connectionHarness(servers, options = {}) {
  const sockets = [];
  const timers = new Map();
  const intervals = new Map();
  const listeners = { doc: new Map(), win: new Map() };
  let nextTimer = 0;
  let saveCalls = 0;
  let renderCalls = 0;
  let now = options.now || 1_700_000_000_000;

  class Socket {
    constructor(url) {
      this.url = url;
      this.readyState = 0;
      this.sent = [];
      this.closed = false;
      sockets.push(this);
    }
    close() {
      this.closed = true;
      this.readyState = 3;
    }
    send(text) {
      this.sent.push(JSON.parse(text));
    }
    open() {
      this.readyState = 1;
      this.onopen?.();
    }
    drop() {
      this.readyState = 3;
      this.closed = true;
      this.onclose?.();
    }
    message(value) {
      this.onmessage?.({ data: JSON.stringify(value) });
    }
  }

  const FakeDate = class extends Date {
    static now() { return now; }
  };

  const state = { provider: "codex", view: "home", servers, machine: "all", sheet: null };
  const conns = new Map();
  const statusElements = [];

  const ctx = vm.createContext({
    Native: options.Native || null,
    WebSocket: Socket,
    PinnedRelaySocket: class {
      constructor(server, base) {
        this.url = base.replace(/^http/, "ws") + "/ws";
        this.server = server;
        this.base = base;
        this.readyState = 0;
        this.sent = [];
        this.closed = false;
        sockets.push(this);
      }
      send(text) { this.sent.push(JSON.parse(text)); }
      close() { this.closed = true; this.readyState = 3; }
      open() { this.readyState = 1; this.onopen?.(); }
      drop() { this.readyState = 3; this.closed = true; this.onclose?.(); }
      message(value) { this.onmessage?.({ data: JSON.stringify(value) }); }
    },
    URL,
    Set,
    Map,
    Date: FakeDate,
    JSON,
    Math,
    AbortSignal,
    fetch: options.fetch || (async () => ({ status: 204 })),
    setTimeout: (fn, delay) => {
      const id = ++nextTimer;
      timers.set(id, { fn, delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
    setInterval: (fn, delay) => {
      const id = ++nextTimer;
      intervals.set(id, { fn, delay });
      return id;
    },
    clearInterval: (id) => intervals.delete(id),
    store: { get: (k, d) => d, set() {} },
    S: state,
    conns,
    window: {
      addEventListener: (ev, fn) => listeners.win.set(ev, fn),
    },
    document: {
      visibilityState: "visible",
      addEventListener: (ev, fn) => listeners.doc.set(ev, fn),
      querySelectorAll: (sel) => (sel === "[data-computer-status]" ? statusElements : []),
    },
    scheduleRender: () => { renderCalls++; },
    saveServers: () => { saveCalls++; },
    mark: () => {},
    serverName: (id) => state.servers.find((s) => s.id === id)?.name || "Mac",
    hostOf: (url) => { try { return new URL(url).hostname; } catch { return url; } },
  });

  const start = appSource.indexOf("function computerStatus(");
  const end = appSource.indexOf("function onEvent(", start);
  vm.runInContext(
    appSource.slice(start, end) + ";globalThis.Conn=Conn;",
    ctx
  );

  ctx.syncConns();

  return {
    ctx,
    conns,
    sockets,
    timers,
    intervals,
    statusElements,
    get saveCalls() { return saveCalls; },
    get renderCalls() { return renderCalls; },
    advance(ms) { now += ms; },
    fire(delay, predicate) {
      const entry = [...timers.entries()].find(([, t]) => t.delay === delay && (!predicate || predicate(t)));
      assert.ok(entry, `missing timer for delay=${delay}; active=${[...timers.values()].map((t) => t.delay).join(",")}`);
      timers.delete(entry[0]);
      entry[1].fn();
    },
    hasTimer(delay) {
      return [...timers.values()].some((t) => t.delay === delay);
    },
  };
}

test("startup wait -> off -> silent retries stay off -> explicit Retry shows wait -> hello transitions to on", () => {
  const h = connectionHarness([
    { id: "m1", name: "Studio", url: "http://192.168.1.10:8787", alts: ["http://m1.local:8787"], token: "tok" },
  ]);
  const c = h.conns.get("m1");

  // Initial startup connect shows wait (yellow).
  assert.equal(c.state, "wait");
  assert.equal(c.ok, false);
  assert.equal(h.ctx.computerDotClass(c), "wait");
  assert.equal(h.ctx.computerStatus(c), "Connecting");
  assert.equal(h.sockets.length, 2);

  // Dead-IP connect timeout transitions directly to off.
  h.fire(3500);
  assert.equal(c.state, "off");
  assert.equal(c.ok, false);
  assert.equal(h.ctx.computerDotClass(c), "off");
  assert.equal(h.ctx.computerStatus(c), "Not Reachable");
  assert.ok(h.sockets.every((s) => s.closed));

  // First silent background retry stays off (no yellow loop).
  h.fire(300);
  assert.equal(h.sockets.length, 4);
  assert.equal(c.state, "off");
  assert.equal(h.ctx.computerDotClass(c), "off");
  assert.equal(h.ctx.computerStatus(c), "Not Reachable");

  // Second silent retry after failure also stays off.
  h.sockets[2].drop();
  h.sockets[3].drop();
  assert.equal(c.state, "off");
  h.fire(600);
  assert.equal(h.sockets.length, 6);
  assert.equal(c.state, "off");

  // Opening a socket during silent retry keeps indicator off until hello arrives.
  h.sockets[4].open();
  assert.equal(c.ok, false);
  assert.equal(c.state, "off");
  assert.equal(h.ctx.computerDotClass(c), "off");

  // If socket drops before hello, it remains off.
  h.sockets[4].drop();
  assert.equal(c.state, "off");

  // Explicit user Retry shows wait (yellow).
  c.connect(true);
  assert.equal(c.state, "wait");
  assert.equal(h.ctx.computerDotClass(c), "wait");
  assert.equal(h.ctx.computerStatus(c), "Connecting");

  // Winning socket open + hello transitions cleanly to on (green).
  const live = h.sockets.at(-2);
  live.open();
  assert.equal(c.ok, false);
  assert.equal(c.state, "wait");
  live.message({ t: "hello", desktop: true, heartbeat: true, addrs: ["http://192.168.1.10:8787", "http://m1.local:8787"] });
  assert.equal(c.ok, true);
  assert.equal(c.state, "on");
  assert.equal(h.ctx.computerDotClass(c), "on");
  assert.equal(h.ctx.computerStatus(c), "Connected");
});

test("transitions to off immediately without blocking behind slow /auth probe", async () => {
  let resolveAuth;
  const h = connectionHarness(
    [{ id: "m1", name: "Mac", url: "http://192.168.1.10:8787", alts: [], token: "tok" }],
    { fetch: () => new Promise((r) => { resolveAuth = r; }) }
  );
  const c = h.conns.get("m1");
  assert.equal(c.state, "wait");

  // Synchronously transitions to off and schedules retry even while /auth hangs.
  h.fire(3500);
  assert.equal(c.state, "off");
  assert.equal(h.ctx.computerDotClass(c), "off");
  assert.equal(h.hasTimer(300), true);
  assert.equal(typeof resolveAuth, "function");

  // If /auth returns 401 before the next attempt, state updates to rejected and stays rejected on silent retry.
  resolveAuth({ status: 401 });
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(c.state, "rejected");
  assert.equal(h.ctx.computerStatus(c), "Pairing Token Rejected");
  assert.equal(h.ctx.computerDotClass(c), "off");

  h.fire(300);
  assert.equal(c.state, "rejected");
  assert.equal(h.ctx.computerDotClass(c), "off");
});

test("silent retry socket that opens without hello times out without flashing yellow", () => {
  const h = connectionHarness([
    { id: "m1", name: "Mac", url: "http://192.168.1.10:8787", alts: [], token: "tok" },
  ]);
  const c = h.conns.get("m1");
  h.fire(3500);
  assert.equal(c.state, "off");
  h.fire(300);
  const silentSock = h.sockets.at(-1);
  silentSock.open();
  assert.equal(c.ok, false);
  assert.equal(c.state, "off");

  // 10s hello deadline fires, drops hung socket, and silently retries while staying off.
  h.fire(10000);
  assert.equal(silentSock.closed, true);
  assert.equal(c.state, "off");
  assert.equal(h.sockets.length, 3);
});

test("macs sheet initial render status dot and Title Case labels match refreshComputerStatus", () => {
  const servers = [
    { id: "s-wait", name: "Starting Mac", url: "http://192.168.1.11:8787", token: "t1" },
    { id: "s-on", name: "Online Mac", url: "http://192.168.1.12:8787", token: "t2" },
    { id: "s-nodesk", name: "Relay Only", url: "http://192.168.1.13:8787", token: "t3" },
    { id: "s-off", name: "Offline Mac", url: "http://192.168.1.14:8787", token: "t4" },
    { id: "s-rej", name: "Rejected Mac", url: "http://192.168.1.15:8787", token: "t5" },
  ];
  const conns = new Map([
    ["s-wait", { s: servers[0], ok: false, state: "wait", base: servers[0].url }],
    ["s-on", { s: servers[1], ok: true, state: "on", base: servers[1].url }],
    ["s-nodesk", { s: servers[2], ok: true, state: "nodesk", base: servers[2].url }],
    ["s-off", { s: servers[3], ok: false, state: "off", base: servers[3].url }],
    ["s-rej", { s: servers[4], ok: false, state: "rejected", base: servers[4].url }],
  ]);

  const statusNodes = [];
  const ctx = vm.createContext({
    Native: { notificationsEnabled: () => true, checkAppUpdates: () => {}, appVersion: () => "0.2.16" },
    S: { servers, sheet: "macs", connectionID: "" },
    conns,
    URL,
    Date,
    encodeURIComponent,
    esc: (s) => String(s ?? ""),
    hostOf: (u) => new URL(u).hostname,
    loadingHTML: (lbl) => lbl,
    mi: (act, ico, title, sub) => `<button data-act="${act}">${title}|${sub}</button>`,
    I: { trash: "", chev: "", plug: "", copy: "", gear: "", download: "" },
    recoveryChoices: () => [],
    recoveryJobs: new Map(),
    serverName: (id) => servers.find((s) => s.id === id)?.name || "Mac",
    document: { querySelectorAll: () => statusNodes },
  });

  const statusStart = appSource.indexOf("function computerStatus(");
  const statusEnd = appSource.indexOf("const CONNECT_TIMEOUT_MS", statusStart);
  const sheetStart = appSource.indexOf("  connection(sh,{server}) {");
  const sheetEnd = appSource.indexOf("// ------------------------------------------------------- Android intents --", sheetStart);
  vm.runInContext(
    appSource.slice(statusStart, statusEnd) +
      "\nconst sheets = {\n" +
      appSource.slice(sheetStart, sheetEnd) +
      "\nglobalThis.sheets = sheets;",
    ctx
  );

  const sh = {
    innerHTML: "",
    querySelector: () => ({ dataset: {} }),
  };
  ctx.sheets.macs(sh);

  assert.ok(sh.innerHTML.includes("Check For Updates"));

  const rows = sh.innerHTML.split('<div class="mi">').slice(1);
  for (const s of servers) {
    const c = conns.get(s.id);
    const row = rows.find((r) => r.includes(`data-computer-status="${s.id}"`));
    assert.ok(row, `missing macs row for ${s.id}`);
    const dotMatch = row.match(/^<span class="dot ([^"]+)"><\/span>/);
    const labelMatch = row.match(new RegExp(`data-computer-status="${s.id}">([^<]+)</span>`));
    assert.ok(dotMatch && labelMatch, `missing dot/label for ${s.id}`);
    const renderedDot = dotMatch[1];
    const renderedLabel = labelMatch[1];

    // Compare against refreshComputerStatus output.
    const dotEl = { className: "" };
    const statusEl = {
      dataset: { computerStatus: s.id },
      textContent: "",
      closest: () => ({ querySelector: () => dotEl }),
    };
    statusNodes.length = 0;
    statusNodes.push(statusEl);
    ctx.refreshComputerStatus(c);

    assert.equal(`dot ${renderedDot}`, dotEl.className, `dot mismatch for ${s.id}`);
    assert.equal(renderedLabel, statusEl.textContent, `label mismatch for ${s.id}`);
  }

  // Verify explicit dot mapping for wait state specifically.
  assert.match(sh.innerHTML, /<span class="dot wait"><\/span><span class="txt">Starting Mac/);
  assert.match(sh.innerHTML, /data-computer-status="s-off">Not Reachable</);
  assert.match(sh.innerHTML, /data-computer-status="s-rej">Pairing Token Rejected</);

  // Verify Connection sheet Retry triggers explicit wait connect(true) and uses Title Case labels.
  let retryArg;
  conns.get("s-off").connect = (arg) => { retryArg = arg; };
  const connSh = { innerHTML: "" };
  ctx.S.sheet = "connection";
  ctx.sheets.connection(connSh, { server: "s-off" });
  assert.ok(connSh.innerHTML.includes("Paired Address"));
  assert.ok(connSh.innerHTML.includes("Last Connected"));
  assert.ok(connSh.innerHTML.includes("Copy Mac Recovery Command"));
  connSh.onclick({ target: { closest: (sel) => sel === "[data-connection-retry]" } });
  assert.equal(retryArg, true);
});

test("non-destructive m.addrs merge retains .local, Tailscale MagicDNS, Tailscale IPs, and TLS invariants", () => {
  const h = connectionHarness([
    {
      id: "m5p",
      name: "M5P",
      url: "http://192.168.1.101:8787",
      alts: [
        "http://100.67.66.88:8787",
        "http://m5p.local:8787",
        "http://m5p.tail1000c2.ts.net:8787",
      ],
      token: "tok",
    },
    {
      id: "pinned",
      name: "Secure Mac",
      url: "https://100.125.53.124:8787",
      pin: "a".repeat(64),
      alts: [
        "https://blakberrisigma.local:8787",
        "https://blakberrisigma.tail1000c2.ts.net:8787",
      ],
      token: "tok2",
    },
  ]);

  const m5p = h.conns.get("m5p");
  const sock = h.sockets.find((s) => s.url.includes("192.168.1.101"));
  sock.open();

  // Hello advertises only new LAN IP and .local (Tailscale momentarily absent).
  sock.message({
    t: "hello",
    desktop: true,
    heartbeat: true,
    addrs: ["http://192.168.1.7:8787", "http://m5p.local:8787"],
  });
  assert.deepEqual(Array.from(m5p.s.alts), [
    "http://192.168.1.7:8787",
    "http://m5p.local:8787",
    "http://m5p.tail1000c2.ts.net:8787",
    "http://100.67.66.88:8787",
  ]);

  // Network update switches to Tailscale only: retains .local and LAN fallback.
  sock.message({
    t: "network",
    addrs: ["http://100.67.66.88:8787", "http://m5p.tail1000c2.ts.net:8787"],
  });
  assert.deepEqual(Array.from(m5p.s.alts), [
    "http://100.67.66.88:8787",
    "http://m5p.tail1000c2.ts.net:8787",
    "http://m5p.local:8787",
    "http://192.168.1.7:8787",
  ]);

  // Empty network flap does not wipe saved routes.
  sock.message({ t: "network", addrs: [] });
  assert.equal(m5p.s.alts.length, 4);
  assert.ok(m5p.s.alts.includes("http://m5p.local:8787"));
  assert.ok(m5p.s.alts.includes("http://m5p.tail1000c2.ts.net:8787"));

  // Ephemeral DHCP IP churn stays bounded (<= 8) while retaining stable hostnames and Tailscale IP.
  const churned = h.ctx.mergeRelayAddresses(
    {
      url: "http://192.168.1.101:8787",
      alts: [
        "http://192.168.1.20:8787",
        "http://192.168.1.21:8787",
        "http://192.168.1.22:8787",
        "http://192.168.1.23:8787",
        "http://192.168.1.24:8787",
        "http://192.168.1.25:8787",
        "http://100.67.66.88:8787",
        "http://m5p.local:8787",
        "http://m5p.tail1000c2.ts.net:8787",
      ],
    },
    ["http://192.168.1.7:8787", "http://192.168.1.8:8787"]
  );
  assert.equal(churned.length, 8);
  assert.equal(churned[0], "http://192.168.1.7:8787");
  assert.ok(churned.includes("http://m5p.local:8787"));
  assert.ok(churned.includes("http://m5p.tail1000c2.ts.net:8787"));
  assert.ok(churned.includes("http://100.67.66.88:8787"));

  // Pinned HTTPS server rejects HTTP downgrades, credentials, query strings, and non-root paths.
  const pinned = h.conns.get("pinned");
  const mergedSecure = h.ctx.mergeRelayAddresses(pinned.s, [
    "http://192.168.1.10:8787",
    "https://192.168.1.10:8787/",
    "https://user:pass@192.168.1.10:8787",
    "https://192.168.1.10:8787/?t=bad",
    "https://192.168.1.10:8787/evil",
    "https://100.125.53.124:8787",
  ]);
  assert.deepEqual(Array.from(mergedSecure), [
    "https://192.168.1.10:8787",
    "https://blakberrisigma.local:8787",
    "https://blakberrisigma.tail1000c2.ts.net:8787",
  ]);
});

test("native bridge network-change nudge reconnects offline hosts silently and probes live sockets", () => {
  const h = connectionHarness([
    { id: "offline", name: "Offline Mac", url: "http://192.168.1.10:8787", alts: [], token: "t1" },
    { id: "live", name: "Live Mac", url: "http://100.67.66.88:8787", alts: [], token: "t2" },
  ]);
  const offline = h.conns.get("offline");
  const live = h.conns.get("live");

  // Bring live online with heartbeat support; let offline time out to off.
  const liveSock = h.sockets.find((s) => s.url.includes("100.67.66.88"));
  liveSock.open();
  liveSock.message({ t: "hello", desktop: true, heartbeat: true, addrs: ["http://100.67.66.88:8787"] });
  h.fire(3500);
  assert.equal(offline.state, "off");
  assert.equal(live.state, "on");

  // Network lost with online: false does not spawn reconnects or flash yellow.
  const countBeforeLost = h.sockets.length;
  h.ctx.window.onNetworkChange({ reason: "lost", online: false });
  assert.equal(h.sockets.length, countBeforeLost);
  assert.equal(offline.state, "off");
  assert.equal(live.state, "on");

  // Network capabilities change (via onAndroidNetworkChange alias):
  // - offline immediately reconnects silently (no yellow flash!)
  // - live stays open and receives a liveness ping
  offline.backoff = 4800;
  const pingsBefore = liveSock.sent.filter((m) => m.op === "ping").length;
  h.ctx.window.onAndroidNetworkChange({ reason: "capabilities", online: true, vpn: true });
  assert.equal(offline.state, "off");
  assert.equal(offline.backoff, 300);
  assert.equal(h.sockets.length, countBeforeLost + 1);
  assert.equal(live.ws, liveSock);
  assert.equal(liveSock.sent.filter((m) => m.op === "ping").length, pingsBefore + 1);

  // If live responds with pong, probe cancels and socket stays connected.
  h.advance(100);
  liveSock.message({ t: "pong" });
  assert.equal(h.hasTimer(3000), false);
  assert.equal(live.state, "on");

  // If a later network change leaves live half-open (no pong), 3s probe triggers silent reconnect.
  h.advance(500);
  h.ctx.window.onNetworkChange({ reason: "link", online: true, wifi: true });
  assert.equal(h.hasTimer(3000), true);
  h.advance(3000);
  h.fire(3000);
  assert.equal(liveSock.closed, true);
  assert.equal(live.state, "off");
});

test("native mDNS discovered-URL hook merges safely, preserves identity/pin, and avoids unrelated flashes", () => {
  const h = connectionHarness([
    {
      id: "m5p",
      name: "My Custom M5P",
      custom: true,
      url: "http://192.168.1.101:8787",
      alts: ["http://100.67.66.88:8787", "http://m5p.tail1000c2.ts.net:8787"],
      token: "t1",
    },
    {
      id: "other",
      name: "Other Offline",
      url: "http://192.168.1.99:8787",
      alts: [],
      token: "t2",
    },
    {
      id: "secure",
      name: "Pinned Mac",
      custom: true,
      url: "https://100.125.53.124:8787",
      pin: "b".repeat(64),
      alts: ["https://studio.tail1000c2.ts.net:8787"],
      token: "t3",
    },
  ]);

  // All three time out to off.
  h.fire(3500);
  h.fire(3500);
  h.fire(3500);
  const m5p = h.conns.get("m5p");
  const other = h.conns.get("other");
  const secure = h.conns.get("secure");
  assert.equal(m5p.state, "off");
  assert.equal(other.state, "off");
  assert.equal(secure.state, "off");

  const socketsBefore = h.sockets.length;
  const savesBefore = h.saveCalls;

  // mDNS discovers new LAN IP for m5p.
  const ok = h.ctx.window.onRelayDiscovered({
    serverId: "m5p",
    url: "http://192.168.1.7:8787",
    alts: ["http://m5p.local:8787"],
    host: "m5p-ignored-name",
    tls: false,
    source: "mdns",
  });
  assert.equal(ok, true);
  assert.equal(m5p.s.id, "m5p");
  assert.equal(m5p.s.name, "My Custom M5P");
  assert.equal(m5p.s.custom, true);
  assert.deepEqual(Array.from(m5p.s.alts), [
    "http://192.168.1.7:8787",
    "http://m5p.local:8787",
    "http://m5p.tail1000c2.ts.net:8787",
    "http://100.67.66.88:8787",
  ]);
  assert.equal(h.saveCalls, savesBefore + 1);
  assert.equal(m5p.base, "http://192.168.1.7:8787");
  assert.equal(m5p.state, "off");
  // Unrelated offline host stays untouched and off.
  assert.equal(other.state, "off");
  const newSockets = h.sockets.slice(socketsBefore);
  assert.ok(newSockets.some((s) => s.url.startsWith("ws://192.168.1.7:8787/")));
  assert.ok(newSockets.every((s) => !s.url.includes("192.168.1.99")));

  // Replaying the exact same discovery while candidates are in flight is a no-op (no abort/save storm).
  const socketsAfterFirst = h.sockets.length;
  const savesAfterFirst = h.saveCalls;
  h.ctx.window.onRelayDiscovered({
    serverId: "m5p",
    url: "http://192.168.1.7:8787",
    alts: ["http://m5p.local:8787"],
    source: "mdns",
  });
  assert.equal(h.sockets.length, socketsAfterFirst);
  assert.equal(h.saveCalls, savesAfterFirst);

  // HTTP downgrade discovery for pinned HTTPS server is rejected completely.
  const rejected = h.ctx.window.onRelayDiscovered({
    id: "secure",
    url: "http://192.168.1.50:8787",
    tls: false,
    source: "mdns",
  });
  assert.equal(rejected, false);
  assert.deepEqual(Array.from(secure.s.alts), ["https://studio.tail1000c2.ts.net:8787"]);
  assert.equal(h.saveCalls, savesAfterFirst);

  // HTTPS discovery for pinned server is accepted and preserves pin and fallbacks.
  const accepted = h.ctx.window.onRelayDiscovered({
    id: "secure",
    url: "https://192.168.1.50:8787",
    urls: ["http://192.168.1.50:8787", "https://studio.local:8787"],
    tls: true,
    source: "mdns",
  });
  assert.equal(accepted, true);
  assert.equal(secure.s.pin, "b".repeat(64));
  assert.deepEqual(Array.from(secure.s.alts), [
    "https://192.168.1.50:8787",
    "https://studio.local:8787",
    "https://studio.tail1000c2.ts.net:8787",
  ]);
  assert.equal(secure.state, "off");

  // Unknown server ID is safely ignored.
  assert.equal(h.ctx.window.onRelayDiscovered({ serverId: "missing", url: "http://192.168.1.5:8787" }), false);
});

test("mergeNativeServers preserves background Store.kt discoveries on WebView cold start", () => {
  const h = connectionHarness([]);
  const local = [
    {
      id: "m5p",
      name: "My M5P",
      custom: true,
      url: "http://192.168.1.101:8787",
      alts: ["http://100.67.66.88:8787"],
      token: "tok",
    },
    {
      id: "sec",
      name: "Secure",
      url: "https://100.125.53.124:8787",
      pin: "c".repeat(64),
      alts: [],
      token: "tok2",
    },
  ];
  const native = [
    {
      id: "m5p",
      name: "Old Name",
      url: "http://192.168.1.101:8787",
      alts: ["http://192.168.1.7:8787", "http://m5p.local:8787"],
      token: "tok",
    },
    {
      id: "sec",
      name: "Secure",
      url: "https://100.125.53.124:8787",
      pin: "c".repeat(64),
      alts: ["http://192.168.1.10:8787", "https://192.168.1.10:8787"],
      token: "tok2",
    },
    {
      id: "deleted-locally",
      name: "Removed",
      url: "http://192.168.1.200:8787",
      alts: [],
      token: "tok3",
    },
  ];

  const changed = h.ctx.mergeNativeServers(local, native);
  assert.equal(changed, true);
  assert.equal(local.length, 2, "does not resurrect deleted servers");
  assert.equal(local[0].name, "My M5P");
  assert.deepEqual(Array.from(local[0].alts), [
    "http://192.168.1.7:8787",
    "http://m5p.local:8787",
    "http://100.67.66.88:8787",
  ]);
  assert.deepEqual(Array.from(local[1].alts), ["https://192.168.1.10:8787"]);
});
