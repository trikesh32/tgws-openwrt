import { connect } from "cloudflare:sockets";

// TCP only; literal Telegram IPv4 addresses prevent DNS rebinding and SSRF.
// Keep aligned with core.telegram.org/resources/cidr.txt and the router list.
const NETWORKS = [
  ["91.105.192.0", 23], ["91.108.4.0", 22], ["91.108.8.0", 22],
  ["91.108.12.0", 22], ["91.108.16.0", 22], ["91.108.20.0", 22],
  ["91.108.56.0", 22], ["149.154.160.0", 20], ["185.76.151.0", 24],
];
const MAX_PENDING = 1024 * 1024;
const MAX_FRAME = 256 * 1024;
const IDLE_MS = 5 * 60 * 1000;

function ipv4(s) {
  if (!/^(0|[1-9]\d{0,2})(\.(0|[1-9]\d{0,2})){3}$/.test(s)) return null;
  const parts = s.split(".").map(Number);
  if (parts.some(n => n > 255)) return null;
  return parts.reduce((n, octet) => ((n << 8) | octet) >>> 0, 0);
}

export function allowedDestination(dst, port) {
  const n = ipv4(dst);
  return n !== null && ["80", "443", "5222"].includes(port) &&
    NETWORKS.some(([base, bits]) => (n >>> (32 - bits)) === (ipv4(base) >>> (32 - bits)));
}

function authenticated(request, secret) {
  if (typeof secret !== "string" || !/^[A-Za-z0-9_-]{32,256}$/.test(secret)) return false;
  const expected = "Bearer " + secret;
  const actual = request.headers.get("Authorization") || "";
  if (actual.length !== expected.length) return false;
  let diff = 0;
  for (let i = 0; i < expected.length; i++) diff |= actual.charCodeAt(i) ^ expected.charCodeAt(i);
  return diff === 0;
}

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname !== "/apiws") return new Response("Not found", { status: 404 });
    if (request.method !== "GET") return new Response("Method not allowed", { status: 405 });
    if (!authenticated(request, env.TGWS_TOKEN)) return new Response("Unauthorized", { status: 401 });
    if ((request.headers.get("Upgrade") || "").toLowerCase() !== "websocket") {
      return new Response("Expected WebSocket", { status: 426 });
    }
    const dst = url.searchParams.get("dst") || "";
    const port = url.searchParams.get("port") || "443";
    if (!allowedDestination(dst, port)) return new Response("Destination denied", { status: 403 });

    let socket, connectTimer;
    try {
      socket = connect({ hostname: dst, port: Number(port) });
      socket.closed.catch(() => {});
      // Report failed upstream connects BEFORE upgrading: the router can still
      // retry its existing path without losing/replaying any client bytes.
      await Promise.race([
        socket.opened,
        new Promise((_, reject) => {
          connectTimer = setTimeout(() => reject(new Error("connect timeout")), 3000);
        }),
      ]);
    } catch {
      if (socket) socket.close().catch(() => {});
      return new Response("Upstream unavailable", { status: 502 });
    } finally {
      clearTimeout(connectTimer);
    }

    const [client, server] = Object.values(new WebSocketPair());
    server.accept();
    server.binaryType = "arraybuffer";
    const reader = socket.readable.getReader();
    const writer = socket.writable.getWriter();
    let stopped = false, idleTimer, queued = 0;
    let writes = Promise.resolve();
    const stop = (code = 1000) => {
      if (stopped) return;
      stopped = true;
      clearTimeout(idleTimer);
      try { server.close(code, ""); } catch {}
      reader.cancel().catch(() => {});
      writer.abort().catch(() => {});
      socket.close().catch(() => {});
    };
    const touch = () => {
      clearTimeout(idleTimer);
      idleTimer = setTimeout(() => stop(1000), IDLE_MS);
    };
    touch();
    server.addEventListener("close", () => stop());
    server.addEventListener("error", () => stop(1011));
    server.addEventListener("message", event => {
      if (stopped) return;
      if (!(event.data instanceof ArrayBuffer)) { stop(1003); return; }
      const bytes = new Uint8Array(event.data);
      if (bytes.byteLength > MAX_FRAME || queued + bytes.byteLength > MAX_PENDING) {
        stop(1009); return;
      }
      queued += bytes.byteLength;
      touch();
      // Event listeners are not awaited. Serialize writes explicitly and cap
      // queued bytes so a slow Telegram socket cannot grow an unbounded queue.
      writes = writes.then(async () => {
        try { if (!stopped) await writer.write(bytes); }
        finally { queued -= bytes.byteLength; }
      }).catch(() => stop(1011));
    });
    (async () => {
      try {
        while (!stopped) {
          const { value, done } = await reader.read();
          if (done) break;
          if (value?.byteLength) {
            touch();
            // TCP chunks and WebSocket frames have no MTProto significance.
            for (let i = 0; i < value.byteLength; i += 64 * 1024) {
              server.send(value.subarray(i, i + 64 * 1024));
            }
          }
        }
      } catch { stop(1011); }
      finally { stop(); }
    })();
    return new Response(null, { status: 101, webSocket: client });
  },
};
