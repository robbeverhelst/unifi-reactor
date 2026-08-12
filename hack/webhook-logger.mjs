#!/usr/bin/env node
// Spike tool (v0.0): dumb request logger for capturing UniFi Alarm Manager
// webhook deliveries during a real WAN failover. Every request is written
// verbatim to one JSON file; nothing is interpreted.
//
//   node hack/webhook-logger.mjs [port] [outDir]
//
// Defaults: port 8080, outDir testdata/unifi/webhooks/raw (gitignored — review
// and sanitize before committing anything from it).

import { createServer } from "node:http";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

const port = Number(process.argv[2] ?? 8080);
const outDir = process.argv[3] ?? "testdata/unifi/webhooks/raw";
mkdirSync(outDir, { recursive: true });

let seq = 0;
const server = createServer((req, res) => {
  const chunks = [];
  req.on("data", (c) => chunks.push(c));
  req.on("end", () => {
    const body = Buffer.concat(chunks);
    const now = new Date();
    const record = {
      receivedAt: now.toISOString(),
      remoteAddress: req.socket.remoteAddress,
      method: req.method,
      url: req.url,
      httpVersion: req.httpVersion,
      headers: req.headers,
      bodyBase64: body.toString("base64"),
      bodyUtf8: body.toString("utf8"),
    };
    const name = `${now.toISOString().replace(/[:.]/g, "-")}_${String(seq++).padStart(3, "0")}.json`;
    writeFileSync(join(outDir, name), JSON.stringify(record, null, 2));
    console.log(`[${record.receivedAt}] ${req.method} ${req.url} ${body.length}B from ${record.remoteAddress} -> ${name}`);
    res.writeHead(200, { "content-type": "application/json" });
    res.end('{"ok":true}');
  });
});

server.listen(port, "0.0.0.0", () => {
  console.log(`webhook-logger listening on 0.0.0.0:${port}, writing to ${outDir}`);
});
