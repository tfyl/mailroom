// Builds the screenshots the README shows, from the same renders the UI review uses.
//
//   node scripts/readme-shots.mjs            # writes docs/images/*.png
//   CHROME=/path/to/chrome node scripts/readme-shots.mjs consent grants
//
// It runs the `shots` build tag to render each page state to standalone HTML, frames that
// page with scripts/readme-shots.css, and screenshots the frame. The composition is CSS, so
// there is no image library here and nothing to install: the only thing this needs that a
// Go checkout does not already have is a Chrome, and scripts/chrome.mjs drives that over the
// DevTools protocol with Node's own WebSocket rather than a browser-automation dependency.
//
// Nothing here is part of the server: the Go half of it is behind the `shots` build tag, this
// file is never compiled or shipped, and the pictures under docs/images are the only thing it
// leaves in the repository.

import { existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { findChrome, launch, open, run } from "./chrome.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.join(root, "docs", "images");

// The width the pages are rendered at. Narrower than the 1180 px the UI review renders at,
// and deliberately: a README column is about 850 px, so a wider render would be a wider
// picture shown smaller, and the text in it would be too small to read. The layout is the
// same at both — the only breakpoint the templates use is Tailwind's `sm` at 640 px — so
// this is the same page, in a narrower column.
const WIDTH = 940;

// Rendered at 2× and shown at 1×, so the text is sharp on a high-density display.
const SCALE = 2;

// Which states are shown, in the order the README shows them, and what each one is for. The
// crop is chosen per shot rather than shown whole: a 2,400 px page scaled into a README is
// unreadable, and every one of these pages says what it has to say in its first screen.
//
//   page   a state from internal/web/shots_test.go
//   url    what the address bar in the frame reads
//   y, h   the slice of the rendered page to show, in CSS pixels from its top
const SHOTS = [
  {
    name: "consent",
    page: "consent-compose",
    url: "mail.example.com/authorize",
    y: 0,
    // 132 px deeper than it was, which is the notice naming where an approved code is sent.
    // These two are consecutive slices of one page and the seam has to stay joined, so the
    // same 132 moves the start of `capabilities` below.
    h: 913,
  },
  {
    name: "capabilities",
    page: "consent-compose",
    url: "mail.example.com/authorize",
    y: 913,
    h: 800,
  },
  {
    name: "grants",
    page: "readme-grants",
    url: "mail.example.com/grants",
    y: 0,
    h: 1372,
  },
  {
    name: "held",
    page: "held",
    url: "mail.example.com/held",
    y: 0,
    // Two rows deeper than it was: each waiting action gained an Expires line, and the crop
    // has to end below the second card's buttons rather than through them. A cut that lands
    // halfway down a control reads as a rendering fault rather than as a cropped picture.
    h: 1075,
  },
  {
    name: "audit",
    page: "audit",
    url: "mail.example.com/audit",
    y: 0,
    h: 877,
  },
  {
    name: "mailboxes",
    page: "readme-accounts",
    url: "mail.example.com/accounts",
    y: 0,
    h: 1050,
  },
];

// --- the frame ----------------------------------------------------------------------------

function frameHTML(shot, pageFile, pageHeight) {
  const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="color-scheme" content="light dark">
<link rel="stylesheet" href="frame.css">
</head>
<body>
<div class="stage">
  <div class="window" style="width:${WIDTH}px">
    <div class="chrome"><span class="url">${esc(shot.url)}</span></div>
    <div class="viewport" style="height:${shot.h}px">
      <iframe src="${esc(pageFile)}" scrolling="no"
              style="width:${WIDTH}px;height:${pageHeight}px;top:${-shot.y}px"></iframe>
    </div>
  </div>
</div>
</body>
</html>`;
}

// --- driver -------------------------------------------------------------------------------

const only = process.argv.slice(2);
const shots = only.length ? SHOTS.filter((s) => only.includes(s.name)) : SHOTS;
if (!shots.length) {
  console.error(`no shot named ${only.join(", ")}; known: ${SHOTS.map((s) => s.name).join(", ")}`);
  process.exit(1);
}

const work = mkdtempSync(path.join(tmpdir(), "mailroom-readme-shots-"));
const pages = path.join(work, "pages");

console.log(`rendering page states to ${pages}`);
await run("go", ["test", "-tags", "shots", "./internal/web", "-run", "TestShots"], {
  cwd: root,
  env: { SHOTS_DIR: pages },
});
writeFileSync(path.join(pages, "frame.css"), readFileSync(path.join(root, "scripts", "readme-shots.css")));

mkdirSync(outDir, { recursive: true });
const browser = await launch(findChrome(), path.join(work, "profile"));
let total = 0;
try {
  for (const shot of shots) {
    const pageFile = `${shot.page}.html`;
    if (!existsSync(path.join(pages, pageFile))) {
      throw new Error(`${shot.page} is not a state in internal/web/shots_test.go`);
    }
    for (const dark of [false, true]) {
      // The page on its own first, only to find out how tall it renders. The frame then
      // gives the iframe exactly that height, so the crop is a window onto a page laid out
      // as if nothing were cropping it.
      const probe = await open(browser, { width: WIDTH, height: 900, dark, scale: SCALE });
      await probe.goto(`file://${path.join(pages, pageFile)}`);
      const pageHeight = await probe.measure("document.documentElement.scrollHeight");
      await probe.close();

      const framePath = path.join(pages, `frame-${shot.name}-${dark ? "dark" : "light"}.html`);
      writeFileSync(framePath, frameHTML(shot, pageFile, pageHeight));

      const page = await open(browser, { width: WIDTH + 200, height: shot.h + 200, dark, scale: SCALE });
      await page.goto(`file://${framePath}`);
      const box = await page.measure(
        "JSON.stringify((r => ({x:r.x,y:r.y,width:r.width,height:r.height}))" +
          "(document.querySelector('.stage').getBoundingClientRect()))",
      );
      const clip = JSON.parse(box);
      const { data } = await page.send("Page.captureScreenshot", {
        format: "png",
        captureBeyondViewport: true,
        optimizeForSpeed: false,
        clip: { ...clip, scale: 1 },
      });
      await page.close();

      const file = path.join(outDir, `${shot.name}-${dark ? "dark" : "light"}.png`);
      writeFileSync(file, Buffer.from(data, "base64"));
      const kb = statSync(file).size / 1024;
      total += statSync(file).size;
      console.log(
        `${path.relative(root, file).padEnd(34)} ${clip.width * SCALE}×${clip.height * SCALE}` +
          `  ${kb.toFixed(0)} KB`,
      );
    }
  }
} finally {
  browser.kill();
}
console.log(`${(total / 1024 / 1024).toFixed(2)} MB total`);
