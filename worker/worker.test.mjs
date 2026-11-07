import { test } from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import vm from "node:vm";

const source = await readFile(new URL("./worker.mjs", import.meta.url), "utf8");
const secret = "a".repeat(64);
async function fixture(connectImpl) {
  let pair;
  class Peer {
    handlers = {}; sent = []; code = null;
    accept() {}
    addEventListener(name, callback) { this.handlers[name] = callback; }
    send(data) { this.sent.push(new Uint8Array(data)); }
    close(code) { this.code = code; }
    emit(name, data) { this.handlers[name]?.({ data }); }
  }
  const context = vm.createContext({
    URL, ArrayBuffer, Uint8Array, setTimeout, clearTimeout,
    Response: class { constructor(body, options) { this.body = body; Object.assign(this, options); } },
    WebSocketPair: class { constructor() { pair = [new Peer(), new Peer()]; return pair; } },
  });
  const module = new vm.SourceTextModule(source, { context });
  await module.link(async name => {
    assert.equal(name, "cloudflare:sockets");
    return new vm.SyntheticModule(["connect"], function() { this.setExport("connect", connectImpl); }, { context });
  });
  await module.evaluate();
  return { handler: module.namespace.default, allowed: module.namespace.allowedDestination, pair: () => pair };
}
function request(query = "dst=149.154.167.51&port=443", token = secret) {
  return new Request("https://relay.example/apiws?" + query, {
    headers: { Upgrade: "websocket", Authorization: "Bearer " + token },
  });
}
const tick = () => new Promise(resolve => setImmediate(resolve));

test("deny missing/wrong secrets and non-Telegram destinations before connect", async () => {
  const f = await fixture(() => { throw new Error("must not dial"); });
  for (const token of ["", "b".repeat(64)]) {
    assert.equal((await f.handler.fetch(request(undefined, token), { TGWS_TOKEN: secret })).status, 401);
  }
  assert.equal((await f.handler.fetch(request(), {})).status, 401);
  for (const query of ["dst=127.0.0.1", "dst=169.254.169.254", "dst=example.com", "dst=149.154.167.51&port=22", "dst=149.154.167.051", "dst=149.154.176.1", "dst=91.108.24.1", "dst=::1"]) {
    assert.equal((await f.handler.fetch(request(query), { TGWS_TOKEN: secret })).status, 403, query);
  }
  assert.equal(f.allowed("149.154.175.255", "5222"), true);
  assert.equal(f.allowed("149.154.159.255", "443"), false);
});

test("TCP connect failure returns 502 before WebSocket upgrade", async () => {
  let closed = false;
  const f = await fixture(() => ({ opened: Promise.reject(new Error("blocked")), closed: Promise.resolve(), close: async () => { closed = true; } }));
  assert.equal((await f.handler.fetch(request(), { TGWS_TOKEN: secret })).status, 502);
  assert.equal(f.pair(), undefined);
  assert.equal(closed, true);
});

test("preserve original port, binary stream order and clean up on close", async () => {
  let upstream, target, closed = false;
  const writes = [];
  const f = await fixture(dst => {
    target = dst;
    return {
      opened: Promise.resolve(), closed: Promise.resolve(), close: async () => { closed = true; },
      readable: new ReadableStream({ start(c) { upstream = c; } }),
      writable: new WritableStream({ async write(data) { await tick(); writes.push([...data]); } }),
    };
  });
  const response = await f.handler.fetch(request("dst=149.154.167.51&port=5222"), { TGWS_TOKEN: secret });
  assert.equal(response.status, 101);
  assert.equal(target.hostname, "149.154.167.51"); assert.equal(target.port, 5222);
  const server = f.pair()[1];
  server.emit("message", Uint8Array.of(1, 2).buffer);
  server.emit("message", Uint8Array.of(3, 4).buffer);
  upstream.enqueue(Uint8Array.of(9, 8, 7));
  for (let i = 0; i < 5; i++) await tick();
  assert.deepEqual(writes, [[1, 2], [3, 4]]);
  assert.deepEqual([...server.sent[0]], [9, 8, 7]);
  server.emit("close");
  assert.equal(closed, true);
});

test("bound queued bytes when upstream stops reading", async () => {
  let release;
  const f = await fixture(() => ({
    opened: Promise.resolve(), closed: Promise.resolve(), close: async () => {},
    readable: new ReadableStream(),
    writable: new WritableStream({ write() { return new Promise(resolve => { release = resolve; }); } }),
  }));
  await f.handler.fetch(request(), { TGWS_TOKEN: secret });
  const server = f.pair()[1];
  for (let i = 0; i < 18; i++) server.emit("message", new ArrayBuffer(64 * 1024));
  assert.equal(server.code, 1009);
  release?.();
});
