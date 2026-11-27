"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function loadAppHarness({ withScanner = true } = {}) {
  const appSource = fs.readFileSync(path.join(__dirname, "..", "web", "app.js"), "utf8");
  const messagesSource = fs.readFileSync(path.join(__dirname, "..", "web", "messages.js"), "utf8");

  const storage = new Map();
  const elements = new Map();
  const toasts = [];
  let scanCalls = 0;
  let savedServers = [];

  class FakeEl {
    constructor(id = "") {
      this.id = id;
      this.innerHTML = "";
      this.textContent = "";
      this.value = "";
      this.dataset = {};
      this.style = { setProperty() {} };
      this.children = [];
      this.childNodes = [];
      this.className = "";
      this.hidden = false;
      this.inert = false;
      this.scrollTop = 0;
      this.scrollHeight = 0;
      this.clientHeight = 0;
      this.offsetHeight = 100;
      this.listeners = new Map();
      this.attrs = new Map();
      this._classes = new Set();
      this.classList = {
        add: (...cls) => cls.forEach((c) => this._classes.add(c)),
        remove: (...cls) => cls.forEach((c) => this._classes.delete(c)),
        toggle: (c, force) => {
          const on = force == null ? !this._classes.has(c) : !!force;
          if (on) this._classes.add(c);
          else this._classes.delete(c);
          return on;
        },
        contains: (c) => this._classes.has(c),
      };
    }
    addEventListener(type, fn) {
      if (!this.listeners.has(type)) this.listeners.set(type, []);
      this.listeners.get(type).push(fn);
    }
    removeEventListener() {}
    setAttribute(k, v) { this.attrs.set(k, String(v)); }
    getAttribute(k) { return this.attrs.get(k) ?? null; }
    removeAttribute(k) { this.attrs.delete(k); }
    querySelector() { return new FakeEl(); }
    querySelectorAll() { return []; }
    closest() { return null; }
    prepend() {}
    append() {}
    appendChild() {}
    focus() {}
    blur() {}
    getAnimations() { return []; }
    animate() { return { finished: Promise.resolve(), cancel() {} }; }
    getBoundingClientRect() { return { top: 0, bottom: 100, left: 0, right: 100, width: 100, height: 100 }; }
  }

  const getEl = (id) => {
    if (!elements.has(id)) elements.set(id, new FakeEl(id));
    return elements.get(id);
  };

  const nativeBridge = withScanner
    ? {
        getServers: () => JSON.stringify(savedServers),
        setServers: (json) => { savedServers = JSON.parse(json); },
        notificationsEnabled: () => true,
        getNotificationPrefs: () => JSON.stringify({ completed: true, attention: true, failed: true }),
        setNotificationPrefs: () => {},
        consumeLaunchIntent: () => "",
        appVersion: () => "0.2.20",
        scanPairingQr: () => { scanCalls++; },
      }
    : null;

  class FakeWebSocket {
    constructor() { this.readyState = 0; }
    send() {}
    close() {}
  }

  const context = {
    console,
    URL,
    URLSearchParams,
    Set,
    Map,
    Date,
    Math,
    JSON,
    Promise,
    AbortController,
    AbortSignal,
    setTimeout: () => 1,
    clearTimeout: () => {},
    setInterval: () => 1,
    clearInterval: () => {},
    requestAnimationFrame: () => 1,
    cancelAnimationFrame: () => {},
    performance: { now: () => 0 },
    innerHeight: 800,
    devicePixelRatio: 1,
    WebSocket: FakeWebSocket,
    IntersectionObserver: class { observe() {} unobserve() {} disconnect() {} },
    matchMedia: () => ({ matches: false, addEventListener() {} }),
    location: { protocol: "https:", hostname: "appassets.androidplatform.net", origin: "https://appassets.androidplatform.net", hash: "", pathname: "/index.html" },
    history: {
      state: { view: "home", provider: "codex" },
      pushState(s) { this.state = s; },
      replaceState(s) { this.state = s; },
      back() {},
    },
    navigator: { vibrate() {}, clipboard: { writeText: async () => {} } },
    localStorage: {
      getItem: (k) => (storage.has(k) ? storage.get(k) : null),
      setItem: (k, v) => storage.set(k, String(v)),
      removeItem: (k) => storage.delete(k),
    },
    addEventListener() {},
    removeEventListener() {},
    getComputedStyle: () => ({ transform: "none" }),
    document: {
      readyState: "loading",
      hidden: false,
      visibilityState: "visible",
      documentElement: new FakeEl("html"),
      activeElement: null,
      getElementById: getEl,
      querySelector: () => new FakeEl(),
      querySelectorAll: () => [],
      createElement: (tag) => new FakeEl(tag),
      addEventListener() {},
      removeEventListener() {},
    },
  };
  context.window = context;
  context.self = context;
  if (nativeBridge) context.CodexAndroid = nativeBridge;

  vm.createContext(context);
  vm.runInContext(messagesSource, context);
  vm.runInContext(appSource, context);

  // Intercept toast messages cleanly for assertions.
  vm.runInContext(`
    toast = (msg) => { __toasts.push(String(msg)); };
  `, Object.assign(context, { __toasts: toasts }));

  return {
    context,
    getEl,
    toasts,
    getScanCalls: () => scanCalls,
    getSavedServers: () => savedServers,
  };
}

test("Scan QR Code button renders on Android and calls Native.scanPairingQr", () => {
  const h = loadAppHarness({ withScanner: true });
  h.context.renderHome();
  assert.match(h.getEl("home-list").innerHTML, /data-scan-qr/);
  assert.match(h.getEl("home-list").innerHTML, /Scan QR Code/);

  h.context.openSheet("macs");
  const sh = h.getEl("sheet");
  assert.match(sh.innerHTML, /data-x="scan-qr"/);
  assert.match(sh.innerHTML, /Scan QR Code/);

  sh.onclick({
    target: {
      closest: (sel) => (sel === "[data-x]" ? { dataset: { x: "scan-qr" } } : null),
    },
  });
  assert.equal(h.getScanCalls(), 1);
});

test("Scan QR Code button is hidden in browser without Native.scanPairingQr", () => {
  const h = loadAppHarness({ withScanner: false });
  h.context.renderHome();
  assert.doesNotMatch(h.getEl("home-list").innerHTML, /data-scan-qr/);

  h.context.openSheet("macs");
  assert.doesNotMatch(h.getEl("sheet").innerHTML, /data-x="scan-qr"/);
});

test("onQrScanResult pairs secure fastcontrol://pair payload with pin and alts", () => {
  const h = loadAppHarness({ withScanner: true });
  h.context.openSheet("macs");
  const pin = "a".repeat(64);
  h.context.onQrScanResult({
    ok: true,
    url: "https://192.168.1.50:8787",
    token: "0123456789abcdef0123456789abcdef",
    name: "Windows Laptop",
    pin,
    alts: ["https://100.64.0.9:8787", "https://win.local:8787"],
  });

  const servers = h.getSavedServers();
  assert.equal(servers.length, 1);
  assert.equal(servers[0].url, "https://192.168.1.50:8787");
  assert.equal(servers[0].token, "0123456789abcdef0123456789abcdef");
  assert.equal(servers[0].name, "Windows Laptop");
  assert.equal(servers[0].pin, pin);
  assert.deepEqual(servers[0].alts, ["https://100.64.0.9:8787", "https://win.local:8787"]);
  assert.equal(vm.runInContext("S.sheet", h.context), null);
  assert.ok(h.toasts.some((m) => m.includes("Paired With Windows Laptop")));
});

test("onQrScanResult handles cancel silently and toasts permission errors", () => {
  const h = loadAppHarness({ withScanner: true });
  h.context.onQrScanResult({ ok: false, cancelled: true });
  assert.equal(h.toasts.length, 0);

  h.context.onQrScanResult({ ok: false, message: "Allow Camera Access To Scan Pairing QR Codes" });
  assert.equal(h.toasts.length, 1);
  assert.match(h.toasts[0], /Allow Camera Access/);
});
