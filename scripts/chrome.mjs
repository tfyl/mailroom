// A headless Chrome, driven over the DevTools protocol with Node's own WebSocket.
//
// Shared by scripts/readme-shots.mjs and scripts/ui-shots.mjs, which want the same browser
// for different pictures. It is here rather than copied into both because the awkward parts —
// finding a Chrome, reading the endpoint off stderr, matching a reply to its request — are
// the parts that would quietly diverge, and a screenshot taken by a slightly different
// browser setup is a screenshot that cannot be compared with the one beside it.
//
// There is no dependency here and there is not meant to be one: the only thing either
// generator needs that a Go checkout does not already have is a Chrome. Nothing in this file
// is compiled, shipped or served — it exists to leave PNGs in docs/ and nothing else.

import { spawn } from "node:child_process";
import { once } from "node:events";
import { existsSync, readdirSync } from "node:fs";
import { homedir } from "node:os";
import path from "node:path";

export function findChrome() {
  if (process.env.CHROME) return process.env.CHROME;
  const candidates = [
    "/usr/bin/google-chrome",
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
    "/Applications/Chromium.app/Contents/MacOS/Chromium",
  ];
  // Whatever Playwright has already downloaded, if anything has.
  for (const base of [
    path.join(homedir(), ".cache", "ms-playwright"),
    path.join(homedir(), "Library", "Caches", "ms-playwright"),
  ]) {
    if (!existsSync(base)) continue;
    for (const dir of readdirSorted(base).filter((d) => d.startsWith("chromium-"))) {
      candidates.push(
        path.join(base, dir, "chrome-linux64", "chrome"),
        path.join(base, dir, "chrome-linux", "chrome"),
        path.join(base, dir, "chrome-mac", "Chromium.app", "Contents", "MacOS", "Chromium"),
      );
    }
  }
  const found = candidates.find((c) => existsSync(c));
  if (!found) {
    throw new Error("no Chrome found — set CHROME=/path/to/chrome");
  }
  return found;
}

function readdirSorted(dir) {
  return readdirSync(dir).sort().reverse();
}

export function run(cmd, args, { cwd, env } = {}) {
  return new Promise((resolve, reject) => {
    const p = spawn(cmd, args, { cwd, env: { ...process.env, ...env }, stdio: "inherit" });
    p.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`${cmd} exited ${code}`))));
  });
}

// --- the DevTools protocol, by hand -------------------------------------------------------

export async function launch(binary, profileDir) {
  const proc = spawn(
    binary,
    [
      "--headless=new",
      "--no-sandbox",
      "--disable-gpu",
      "--hide-scrollbars",
      "--force-color-profile=srgb",
      "--disable-lcd-text", // grey antialiasing: subpixel fringes are wrong outside a screen
      "--allow-file-access-from-files",
      "--no-first-run",
      "--no-default-browser-check",
      "--disable-extensions",
      "--disable-dev-shm-usage",
      `--user-data-dir=${profileDir}`,
      "--remote-debugging-port=0",
      "about:blank",
    ],
    { stdio: ["ignore", "ignore", "pipe"] },
  );
  const endpoint = await new Promise((resolve, reject) => {
    let buf = "";
    const timer = setTimeout(() => reject(new Error("no DevTools endpoint:\n" + buf)), 30_000);
    proc.stderr.on("data", (d) => {
      buf += d.toString();
      const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
      if (m) {
        clearTimeout(timer);
        resolve(m[1]);
      }
    });
    proc.on("exit", (code) => reject(new Error(`chrome exited ${code}\n${buf}`)));
  });
  const ws = new WebSocket(endpoint);
  await once(ws, "open");
  let seq = 0;
  const pending = new Map();
  const listeners = new Map();
  ws.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id !== undefined) {
      const p = pending.get(msg.id);
      pending.delete(msg.id);
      if (p) (msg.error ? p.reject(new Error(msg.error.message)) : p.resolve(msg.result));
      return;
    }
    for (const fn of listeners.get(msg.method) || []) fn(msg.params, msg.sessionId);
  });
  const send = (method, params = {}, sessionId) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params, sessionId }));
    });
  const on = (method, fn) => {
    if (!listeners.has(method)) listeners.set(method, []);
    listeners.get(method).push(fn);
    return () => listeners.get(method).splice(listeners.get(method).indexOf(fn), 1);
  };
  return { send, on, kill: () => proc.kill("SIGTERM") };
}

export async function open(browser, { width, height, dark, scale }) {
  const { targetId } = await browser.send("Target.createTarget", { url: "about:blank" });
  const { sessionId } = await browser.send("Target.attachToTarget", { targetId, flatten: true });
  const send = (m, p) => browser.send(m, p, sessionId);
  await send("Page.enable");
  await send("Runtime.enable");
  await send("Emulation.setDeviceMetricsOverride", {
    width,
    height,
    deviceScaleFactor: scale,
    mobile: false,
  });
  // The theme. Chrome's headless build ignores --force-prefers-color-scheme, and this UI has
  // no theme switch to click — prefers-color-scheme is the only signal it reads — so the
  // emulated media query is the only way to ask for the dark render. It applies to a frame's
  // subframes as well as to the frame itself, which is why a page and the sheet or window
  // holding it can never disagree.
  await send("Emulation.setEmulatedMedia", {
    features: [{ name: "prefers-color-scheme", value: dark ? "dark" : "light" }],
  });
  return {
    send,
    async goto(url) {
      const loaded = new Promise((resolve) => {
        const off = browser.on("Page.loadEventFired", (_p, sid) => {
          if (sid === sessionId) {
            off();
            resolve();
          }
        });
      });
      await send("Page.navigate", { url });
      await loaded;
      // One frame, so webfont-free system text and the shadow are painted before the capture.
      await send("Runtime.evaluate", {
        expression: "new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r)))",
        awaitPromise: true,
      });
    },
    async measure(expr) {
      const r = await send("Runtime.evaluate", { expression: expr, returnByValue: true });
      if (r.exceptionDetails) throw new Error(r.exceptionDetails.text);
      return r.result.value;
    },
    close: () => browser.send("Target.closeTarget", { targetId }),
  };
}
