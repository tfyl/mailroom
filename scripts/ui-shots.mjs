// Builds the UI review set under docs/ui/screenshots: every page in every state the review
// covers, light and dark, wide and narrow, plus the two contact sheets.
//
//   node scripts/ui-shots.mjs                       # the whole set
//   node scripts/ui-shots.mjs consent held-failed   # only those states, sheets left alone
//   CHROME=/path/to/chrome node scripts/ui-shots.mjs
//
// It runs the `shots` build tag itself to render each state to standalone HTML in a temporary
// directory, then screenshots that directory. There is no image library and nothing to
// install: the composition is CSS (scripts/ui-shots.css), and the only thing this needs that
// a Go checkout does not already have is a Chrome, which scripts/chrome.mjs drives over the
// DevTools protocol with Node's own WebSocket rather than a browser-automation dependency.
//
// It writes into pages/, narrow/, and the two contact sheets, and nowhere else. Two
// directories beside them are history and are never touched:
//
//   before/  the pages before the ergonomics pass, kept as the comparison that change is
//            argued from. Regenerating it would delete the only copy of the thing being
//            compared against and leave two identical sets.
//   fixes/   annotated one-off shots of particular defects. Nothing here can reproduce an
//            annotation, so overwriting one would lose it.
//
// Nothing here is part of the server: the Go half of it is behind the `shots` build tag, this
// file is never compiled or shipped, and the PNGs are the only thing it leaves behind.

import { existsSync, mkdirSync, mkdtempSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { readdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { findChrome, launch, open, run } from "./chrome.mjs";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.join(root, "docs", "ui", "screenshots");

// --- geometry -----------------------------------------------------------------------------

// The wide render. 1180 px is a desktop window with the page's own max-width doing the work,
// and the shot is taken at half a device pixel per CSS pixel: this set is 118 whole-page
// renders that a reviewer scrolls, several of them thousands of pixels tall, and at 1× it
// would be four times the bytes for detail nobody reads at that zoom. The README's pictures
// are the ones meant to be read, and readme-shots.mjs renders those at 2×.
const PAGE = { dir: "pages", width: 1180, scale: 0.5 };

// The narrow render, at 1× because it is small already. 420 px is below every breakpoint the
// templates use and is where a layout that only works wide falls apart.
const NARROW = { dir: "narrow", width: 420, scale: 1 };

// The viewport height both are rendered in. It is a floor rather than a frame: a page taller
// than this is captured whole, and a page shorter than it is captured at this height, so the
// pages that size themselves against the viewport — the sign-in column is `min-h-[58vh]` —
// are shown in a window somebody might actually have.
const VIEWPORT_HEIGHT = 900;

// The contact sheet. Five columns of the given width, which is what fixes the sheet's total
// width, and each cell shows the first CROP px of its page scaled to fit the column.
const SHEET = { pad: 16, cols: 5, col: 233, gutter: 12, crop: 390 };

// --- what goes where ----------------------------------------------------------------------

// The sheet, in the order it reads, grouped the way somebody looking for an inconsistency
// would want it grouped: a section is a set of pages that ought to look like each other.
// A state that reaches pages/ without being placed here is an error rather than a silent
// omission — see `place` below — because the sheet's whole claim is that it is everything.
const SECTIONS = [
  {
    title: "Signed out",
    states: ["login", "login-error", "login-proxy", "refused-invite", "refused-closed"],
  },
  {
    title: "Mailboxes",
    states: [
      "accounts-empty",
      "accounts-empty-unconfigured",
      "accounts",
      "accounts-disabled",
      "accounts-linked-nosending",
      "accounts-renamed",
      "accounts-manage-open",
      "accounts-rename-error",
      "accounts-google-open",
      "accounts-imap-open",
      "accounts-imap-error",
      "accounts-imap-error-form",
      "accounts-stress",
    ],
  },
  {
    title: "Consent",
    states: [
      "consent",
      "consent-compose",
      "consent-privileged",
      "consent-nojs",
      "consent-no-mailboxes",
      "consent-nothing-asked",
      "consent-stress",
    ],
  },
  {
    title: "Grants",
    states: [
      "grants-empty",
      "grants",
      "grants-saved",
      "grants-none-live",
      "grants-revoked-open",
      "grants-stress",
    ],
  },
  {
    title: "Editing a grant",
    states: [
      "grant-edit",
      "grant-edit-nothing",
      "grant-edit-refused",
      "grant-edit-expired",
      "grant-edit-no-mailboxes",
      "grant-edit-tightening",
      "grant-edit-widened",
      "grant-widen",
      "grant-widen-mode",
      "grant-widen-mixed",
      "grant-widen-expiry",
    ],
  },
  { title: "Held", states: ["held-empty", "held", "held-done", "held-failed", "held-stress"] },
  { title: "Revoking", states: ["revoke", "revoke-long", "revoke-stress"] },
  { title: "Audit", states: ["audit", "audit-empty", "audit-refused", "audit-refused-none"] },
  { title: "Invites", states: ["invites-empty", "invites", "invites-fresh", "invites-inert"] },
];

// States that are rendered into pages/ but deliberately left off the sheet, with the reason.
// The sheet shows the first screen of each page, so a state that differs from one already on
// it only below that line adds a cell nobody can tell apart from its neighbour.
const NOT_ON_SHEET = {
  "audit-open": "identical to `audit` above the fold — it is that page with every row disclosed",
  "held-open":
    "identical to `held` above the fold — it is that page with the closed-actions list disclosed",
};

// The states rendered narrow. Not all of them: 420 px is asking one question — does this
// layout survive a phone — and the answer is a property of a page, not of every state that
// page can be in. What is here is one state per page plus the states that carry the long
// alias, the long address and the long grant label, which are what actually break a narrow
// column.
const NARROW_STATES = [
  "login",
  "refused-invite",
  "accounts",
  "accounts-stress",
  "consent",
  "consent-compose",
  "consent-stress",
  "grants",
  "grants-revoked-open",
  "grants-stress",
  "grant-edit",
  "grant-edit-widened",
  "grant-widen",
  "grant-widen-mode",
  "held",
  "held-stress",
  "revoke",
  "revoke-long",
  "audit",
  "invites",
];

// --- the sheet ----------------------------------------------------------------------------

const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");

function sheetHTML(dark) {
  // The column is the outer box; the page inside it is scaled to the width within its border.
  const inner = SHEET.col - 2;
  const scale = inner / PAGE.width;
  const thumbHeight = Math.round(SHEET.crop * scale);
  const sections = SECTIONS.map(
    (s) => `<h2>${esc(s.title)}</h2>
<div class="grid">
${s.states
  .map(
    (name) => `  <div class="cell">
    <p class="name">${esc(name)}</p>
    <div class="thumb"><iframe src="${esc(name)}.html" scrolling="no"
      style="width:${PAGE.width}px;height:${SHEET.crop}px;transform:scale(${scale})"></iframe></div>
  </div>`,
  )
  .join("\n")}
</div>`,
  ).join("\n");
  return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="color-scheme" content="light dark">
<link rel="stylesheet" href="sheet.css">
<style>
:root {
  --pad: ${SHEET.pad}px;
  --cols: ${SHEET.cols};
  --col: ${SHEET.col}px;
  --gutter: ${SHEET.gutter}px;
  --thumb-h: ${thumbHeight}px;
}
</style>
</head>
<body>
<h1>mailroom — ${dark ? "Dark" : "Light"} theme</h1>
<p class="about">Every page in every state the review covers, rendered at ${PAGE.width} px.
Top ${SHEET.crop} px of each page.</p>
<hr>
${sections}
</body>
</html>`;
}

// --- driver -------------------------------------------------------------------------------

const work = mkdtempSync(path.join(tmpdir(), "mailroom-ui-shots-"));
const html = path.join(work, "pages");

console.log(`rendering page states to ${html}`);
await run("go", ["test", "-tags", "shots", "./internal/web", "-run", "TestShots"], {
  cwd: root,
  env: { SHOTS_DIR: html },
});

// Every state the Go half rendered, minus the ones that exist only to be cropped into the
// README. Those are framed differently and are not pages of the product in their own right;
// readme-shots.mjs owns them.
const rendered = (await readdir(html))
  .filter((f) => f.endsWith(".html"))
  .map((f) => f.slice(0, -".html".length))
  .filter((s) => !s.startsWith("readme-"))
  .sort();

// The two lists above name states by hand, so they can be wrong in both directions, and both
// directions are silent unless they are checked here: a state added to shots_test.go and not
// placed would quietly never reach the sheet, and a state renamed there would quietly stop
// being rendered while the old PNG stayed in the repository looking current.
const placed = new Set(SECTIONS.flatMap((s) => s.states));
for (const name of rendered) {
  if (!placed.has(name) && !(name in NOT_ON_SHEET)) {
    throw new Error(
      `${name} is a state in internal/web/shots_test.go that no section of the contact sheet ` +
        `claims. Add it to SECTIONS in this file, or to NOT_ON_SHEET with the reason.`,
    );
  }
}
for (const name of [...placed, ...Object.keys(NOT_ON_SHEET), ...NARROW_STATES]) {
  if (!rendered.includes(name)) {
    throw new Error(`${name} is named in this file but is not a state in internal/web/shots_test.go`);
  }
}

const only = process.argv.slice(2);
for (const name of only) {
  if (!rendered.includes(name)) {
    console.error(`no state named ${name}; known: ${rendered.join(", ")}`);
    process.exit(1);
  }
}
const wide = only.length ? rendered.filter((s) => only.includes(s)) : rendered;
const narrow = (only.length ? NARROW_STATES.filter((s) => only.includes(s)) : NARROW_STATES).slice();

writeFileSync(path.join(html, "sheet.css"), readFileSync(path.join(root, "scripts", "ui-shots.css")));
writeFileSync(path.join(html, "sheet-light.html"), sheetHTML(false));
writeFileSync(path.join(html, "sheet-dark.html"), sheetHTML(true));

mkdirSync(path.join(outDir, PAGE.dir), { recursive: true });
mkdirSync(path.join(outDir, NARROW.dir), { recursive: true });

const browser = await launch(findChrome(), path.join(work, "profile"));
let total = 0;

// Whole page, not a crop: this set exists to be scrolled, and a page cut off at its first
// screen is exactly the state of affairs the review is meant to catch.
async function shoot(file, url, { width, scale }, minHeight) {
  const page = await open(browser, { width, height: VIEWPORT_HEIGHT, dark: file.dark, scale });
  await page.goto(url);
  const height = Math.max(minHeight, await page.measure("document.documentElement.scrollHeight"));
  const { data } = await page.send("Page.captureScreenshot", {
    format: "png",
    captureBeyondViewport: true,
    optimizeForSpeed: false,
    clip: { x: 0, y: 0, width, height, scale: 1 },
  });
  await page.close();
  writeFileSync(file.path, Buffer.from(data, "base64"));
  const size = statSync(file.path).size;
  total += size;
  console.log(
    `${path.relative(root, file.path).padEnd(52)} ${Math.round(width * scale)}×${Math.round(height * scale)}`.padEnd(
      70,
    ) + `${(size / 1024).toFixed(0)} KB`,
  );
}

try {
  for (const [set, states] of [
    [PAGE, wide],
    [NARROW, narrow],
  ]) {
    for (const name of states) {
      for (const dark of [false, true]) {
        await shoot(
          { path: path.join(outDir, set.dir, `${name}-${dark ? "dark" : "light"}.png`), dark },
          `file://${path.join(html, `${name}.html`)}`,
          set,
          VIEWPORT_HEIGHT,
        );
      }
    }
  }

  if (only.length) {
    console.log(
      "\ncontact sheets left as they are: they are every state at once, so a run over a subset " +
        "cannot rebuild one. Run with no arguments to rebuild them.",
    );
  } else {
    for (const dark of [false, true]) {
      const name = `contact-sheet-${dark ? "dark" : "light"}`;
      await shoot(
        { path: path.join(outDir, `${name}.png`), dark },
        `file://${path.join(html, `sheet-${dark ? "dark" : "light"}.html`)}`,
        { width: SHEET.pad * 2 + SHEET.cols * SHEET.col + (SHEET.cols - 1) * SHEET.gutter, scale: 1 },
        0,
      );
    }
  }
} finally {
  browser.kill();
}
console.log(`${(total / 1024 / 1024).toFixed(2)} MB total`);
