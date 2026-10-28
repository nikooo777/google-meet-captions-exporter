const assert = require("node:assert/strict");
const { randomUUID } = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const root = path.join(__dirname, "..");
const config = { bridgeEnabled: true, bridgeUrl: "http://127.0.0.1:6338", bridgeToken: "test-token" };

function captionRegion(text = "Earlier caption") {
  const turn = { querySelector: (selector) => selector === ".NWpY1d" ? { textContent: "Alice" } : container };
  const node = { nodeType: 3, textContent: text };
  const container = { childNodes: [node], matches: (selector) => selector === ".ygicle", closest: () => turn };
  node.parentElement = container;
  return { querySelectorAll: () => [turn], node };
}

function contentHarness({ joined = false, pathname = "/abc-defg-hij", respond, settings = config } = {}) {
  const messages = [];
  const observers = [];
  const timers = new Map();
  let listener;
  let region = captionRegion();
  const body = {};
  const document = {
    body,
    querySelector(selector) {
      if (selector.includes("Leave call")) return joined ? {} : null;
      return joined ? region : null;
    },
    querySelectorAll: () => [],
  };
  class Observer {
    constructor(callback) {
      this.callback = callback;
      observers.push(this);
    }
    observe(target) { this.target = target; }
    disconnect() { this.disconnected = true; }
  }
  const context = vm.createContext({
    document,
    location: { pathname },
    crypto: { randomUUID },
    Node: { TEXT_NODE: 3, ELEMENT_NODE: 1 },
    MutationObserver: Observer,
    setTimeout(callback) {
      const id = randomUUID();
      timers.set(id, callback);
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
    chrome: {
      runtime: {
        onMessage: { addListener: (callback) => { listener = callback; } },
        async sendMessage(message) {
          messages.push(message);
          return respond ? respond(message) : { ok: true };
        },
      },
      storage: {
        local: { get: async () => settings },
        onChanged: { addListener() {} },
      },
    },
  });
  vm.runInContext(fs.readFileSync(path.join(root, "content.js"), "utf8"), context);
  function update() { observers.find((observer) => observer.target === body).callback([]); }
  return {
    messages, observers, timers,
    get region() { return region; },
    setJoined(value) { joined = value; update(); },
    replaceRegion(value) { region = value; update(); },
    async tick() {
      const [id, callback] = timers.entries().next().value;
      timers.delete(id);
      callback();
      await settled();
    },
    request(type) {
      let response;
      listener({ type }, null, (value) => { response = value; });
      return response;
    },
  };
}

function settled() {
  return new Promise(setImmediate);
}

test("homepages and unjoined lobbies never push snapshots", async () => {
  const home = contentHarness({ pathname: "/" });
  const lobby = contentHarness();
  await settled();
  home.setJoined(true);
  await settled();
  assert.equal(home.messages.length, 0);
  assert.equal(lobby.messages.length, 0);
  assert.equal(home.timers.size, 0);
  assert.equal(lobby.timers.size, 0);
});

test("join streams and leaving sends a final inactive snapshot", async () => {
  const harness = contentHarness();
  await settled();
  harness.setJoined(true);
  await settled();
  assert.equal(harness.messages.length, 1);
  const first = harness.messages[0].snapshot;
  assert.equal(first.active, true);
  assert.equal(first.entries[0].text, "Earlier caption");
  assert.ok(first.session_id);
  harness.setJoined(false);
  await settled();
  const final = harness.messages.at(-1).snapshot;
  assert.equal(final.active, false);
  assert.equal(final.session_id, first.session_id);
  assert.ok(final.sequence > first.sequence);
  assert.equal(harness.timers.size, 0);
  assert.equal(harness.request("GET_STATUS").bridgeStreaming, false);
  harness.setJoined(true);
  await settled();
  assert.notEqual(harness.messages.at(-1).snapshot.session_id, first.session_id);
});

test("clear closes the old capture and starts a separate empty session", async () => {
  const harness = contentHarness({ joined: true });
  await settled();
  const first = harness.messages[0].snapshot;
  assert.equal(harness.request("CLEAR_TRANSCRIPT").ok, true);
  await settled();
  const ended = harness.messages.at(-2).snapshot;
  const current = harness.messages.at(-1).snapshot;
  assert.equal(ended.active, false);
  assert.equal(ended.session_id, first.session_id);
  assert.equal(ended.entries[0].text, "Earlier caption");
  assert.equal(current.active, true);
  assert.notEqual(current.session_id, first.session_id);
  assert.equal(current.entries.length, 0);
  assert.equal(harness.request("GET_TRANSCRIPT").text, "");
});

test("caption capture follows a replaced region", async () => {
  const harness = contentHarness({ joined: true });
  await settled();
  const oldRegion = harness.region;
  const oldObserver = harness.observers.find((observer) => observer.target === oldRegion);
  const replacement = captionRegion("Replacement caption");
  harness.replaceRegion(replacement);
  assert.equal(oldObserver.disconnected, true);
  assert.ok(harness.observers.some((observer) => observer.target === replacement));
  assert.match(harness.request("GET_TRANSCRIPT").text, /Replacement caption/);
});

test("late responses from a closed capture do not change its replacement", async () => {
  let release;
  let requests = 0;
  const harness = contentHarness({
    joined: true,
    respond() {
      if (++requests === 1) return new Promise((resolve) => { release = resolve; });
      return { ok: true, persistenceRevision: requests, persistenceErrors: {} };
    },
  });
  await settled();
  harness.request("CLEAR_TRANSCRIPT");
  await settled();
  release({ ok: false, reason: "persistence", error: "Old request failed", persistenceRevision: 1, persistenceErrors: { old: "Old request failed" } });
  await settled();
  assert.equal(harness.request("GET_STATUS").bridgeReason, null);
  assert.equal(Object.keys(harness.request("GET_STATUS").persistenceErrors).length, 0);
  assert.equal(harness.timers.size, 1);
});

test("pending save failures survive clear until confirmed recovery", async () => {
  let releaseFinal;
  let revision = 0;
  let errors = {};
  const harness = contentHarness({
    joined: true,
    respond(message) {
      if (!message.snapshot.active) {
        return new Promise((resolve) => { releaseFinal = resolve; });
      }
      return {
        ok: Object.keys(errors).length === 0,
        reason: Object.keys(errors).length ? "persistence" : undefined,
        persistenceRevision: ++revision,
        persistenceErrors: errors,
      };
    },
  });
  await settled();
  const original = harness.messages[0].snapshot;
  const key = original.meeting_id + "/" + original.session_id;
  harness.request("CLEAR_TRANSCRIPT");
  await settled();
  errors = { [key]: "Disk full" };
  releaseFinal({ ok: false, reason: "persistence", persistenceRevision: ++revision, persistenceErrors: errors });
  await settled();
  let status = harness.request("GET_STATUS");
  assert.equal(status.persistenceErrors[key], "Disk full");
  assert.equal(status.bridgeReason, null);
  assert.equal(status.bridgeStreaming, true);
  await harness.tick();
  assert.equal(harness.request("GET_STATUS").persistenceErrors[key], "Disk full");
  errors = {};
  await harness.tick();
  status = harness.request("GET_STATUS");
  assert.equal(Object.keys(status.persistenceErrors).length, 0);
  assert.equal(status.bridgeStreaming, true);
});

test("stale healthy responses cannot clear newer persistence failures", async () => {
  let release;
  let requests = 0;
  const harness = contentHarness({
    joined: true,
    respond() {
      if (++requests === 1) return new Promise((resolve) => { release = resolve; });
      return { ok: false, reason: "persistence", persistenceRevision: requests, persistenceErrors: { previous: "Disk full" } };
    },
  });
  await settled();
  harness.request("CLEAR_TRANSCRIPT");
  await settled();
  release({ ok: true, persistenceRevision: 1, persistenceErrors: {} });
  await settled();
  assert.equal(harness.request("GET_STATUS").persistenceErrors.previous, "Disk full");
});

function backgroundHarness(result, settings = config, status = 200) {
  const requests = [];
  let listener;
  const context = vm.createContext({
    URL,
    fetch: async (url, options) => {
      requests.push({ url, options });
      return { ok: status >= 200 && status < 300, status, json: async () => result };
    },
    chrome: {
      storage: { local: { get: async () => settings } },
      runtime: { onMessage: { addListener: (callback) => { listener = callback; } } },
    },
  });
  vm.runInContext(fs.readFileSync(path.join(root, "background.js"), "utf8"), context);
  return {
    requests,
    push(snapshot) {
      return new Promise((resolve) => listener({ type: "PUSH_TO_BRIDGE", snapshot }, { tab: { id: 1 } }, resolve));
    },
  };
}

test("background rejects missing meeting identities without creating tab meetings", async () => {
  const harness = backgroundHarness({ ok: true });
  const response = await harness.push({ meeting_id: "", entries: [] });
  assert.equal(response.reason, "not-in-meeting");
  assert.equal(harness.requests.length, 0);
});

test("persistence errors reach the content script even after successful ingestion", async () => {
  const harness = backgroundHarness({ ok: true, persistence_error: "Disk full" });
  const response = await harness.push({ meeting_id: "abc-defg-hij", session_id: "capture" });
  assert.equal(response.ok, false);
  assert.equal(response.reason, "persistence");
  assert.equal(response.error, "Disk full");
});

test("background forwards failures belonging to previously cleared captures", async () => {
  const harness = backgroundHarness({ ok: true, persistence_revision: 5, persistence_errors: { previous: "Disk full" } });
  const response = await harness.push({ meeting_id: "abc-defg-hij", session_id: "new-capture", entries: [] });
  assert.equal(response.reason, "persistence");
  assert.equal(response.persistenceRevision, 5);
  assert.equal(response.persistenceErrors.previous, "Disk full");
});

test("joined calls stream without any saved token or popup configuration", async () => {
  const harness = contentHarness({ joined: true, settings: {} });
  await settled();
  assert.equal(harness.messages.length, 1);
  assert.equal(harness.request("GET_STATUS").bridgeStreaming, true);
});

test("explicitly disabling the bridge prevents default streaming", async () => {
  const harness = contentHarness({ joined: true, settings: { bridgeEnabled: false } });
  await settled();
  assert.equal(harness.messages.length, 0);
});

test("background uses the default bridge URL and omits empty authorization", async () => {
  const harness = backgroundHarness({ ok: true }, {});
  const response = await harness.push({ meeting_id: "abc-defg-hij", session_id: "capture" });
  assert.equal(response.ok, true);
  assert.equal(harness.requests[0].url, "http://127.0.0.1:6338/ingest");
  assert.equal(harness.requests[0].options.headers.Authorization, undefined);
});

test("background sends a configured optional token", async () => {
  const harness = backgroundHarness({ ok: true });
  await harness.push({ meeting_id: "abc-defg-hij", session_id: "capture" });
  assert.equal(harness.requests[0].options.headers.Authorization, "Bearer test-token");
});

test("a token-protected daemon produces an authentication hint", async () => {
  const harness = backgroundHarness({}, {}, 401);
  const response = await harness.push({ meeting_id: "abc-defg-hij", session_id: "capture" });
  assert.equal(response.reason, "authentication");
  assert.match(response.error, /requires a token/);
});

test("popup shows outstanding save errors even when streaming is disabled", async () => {
  const elements = new Map();
  function element(id) {
    if (!elements.has(id)) {
      const classes = new Set();
      elements.set(id, {
        value: "", checked: false, textContent: "",
        addEventListener() {},
        classList: {
          add: (name) => classes.add(name),
          remove: (name) => classes.delete(name),
          toggle: (name, force) => force ? classes.add(name) : classes.delete(name),
          contains: (name) => classes.has(name),
        },
      });
    }
    return elements.get(id);
  }
  const context = vm.createContext({
    document: { getElementById: element },
    setInterval() {},
    chrome: {
      storage: { local: { get: async () => ({ ...config, bridgeEnabled: false }) } },
      tabs: {
        query: async () => [{ id: 1 }],
        sendMessage: async () => ({ entryCount: 0, persistenceErrors: { previous: "Disk full" } }),
      },
    },
  });
  vm.runInContext(fs.readFileSync(path.join(root, "popup.js"), "utf8"), context);
  await settled();
  assert.match(element("bridge-state").textContent, /Disk full/);
  assert.equal(element("bridge-state").classList.contains("bridge-error"), true);
  assert.equal(element("bridge-state").classList.contains("hidden"), false);
});
