// The whole of the operator interface's JavaScript. There is no second file, no bundler and
// no dependency: this is the file the browser is served, byte for byte, from
// /static/app.<digest>.js under `script-src 'self'`. docs/ui.md has the arrangement and the
// rules; the short version is below, because the rules are the point of the file.
//
// 1. Nothing here decides anything. The server decides. Every control this touches already
//    works with the file absent — the select-all buttons are real submit buttons posting to
//    a real endpoint — and all this does is save the round trip. A page that behaves one way
//    with script and another way without it is a bug on any screen and a vulnerability on
//    the consent screen, where a box that looks ticked has to be the box that gets submitted.
// 2. Nothing here is inline. No inline handler, no inline block, no eval, no new Function,
//    no dynamic import. internal/web/confirm_test.go fails the build on the first two, and
//    the policy has no 'unsafe-inline' and no 'unsafe-eval' to fall back on.
// 3. Nothing here writes markup. textContent, checked, hidden and dataset — never innerHTML.
//    Half of what is on a consent screen is a name an unauthenticated client registration
//    chose, and html/template is what makes that safe. Do not route around it.
// 4. Classes and attributes this toggles must already appear in a template. Tailwind scans
//    internal/web/templates/*.html and nothing else, so a utility named only here is a
//    utility that was never built. Write `data-[state=on]:bg-muted` on the element in the
//    template and set the attribute from here.
//
// Adding one: write the function, register it in `enhancements` under a name, and put
// data-enhance="<name>" on the element it applies to. Every page loads this file; a page
// with no data-enhance attribute gets one empty querySelectorAll and no behaviour change.

(() => {
  'use strict';

  const enhancements = {
    consent: consentForm,
  };

  document.addEventListener('DOMContentLoaded', () => {
    // Copy that is only true while this file is running. "Sends the page back to be redrawn"
    // describes the server round trip, which is the thing an enhancement removes; leaving it
    // on the page would make the UI describe behaviour it no longer has.
    for (const element of document.querySelectorAll('[data-js-text]')) {
      element.textContent = element.dataset.jsText;
    }

    for (const element of document.querySelectorAll('[data-enhance]')) {
      const enhance = enhancements[element.dataset.enhance];
      if (enhance) enhance(element);
    }
  });

  // The consent screen: select-all without the round trip, and a running account of what
  // Approve would grant.
  function consentForm(form) {
    const boxes = (name) =>
      Array.from(form.querySelectorAll('input[type="checkbox"][name="' + name + '"]'));

    // The four selections POST /authorize/reselect understands, doing in the browser exactly
    // what it does on the server. Anything outside this list is left to submit: an unknown
    // value is the server's to refuse, and it refuses it with a 400 rather than guessing.
    const selections = {
      'all-mailboxes': () => tickAll(boxes('accounts'), true),
      'no-mailboxes': () => tickAll(boxes('accounts'), false),
      'all-capabilities': () => tickAll(boxes('capabilities'), true),
      'no-capabilities': () => tickAll(boxes('capabilities'), false),
    };

    // The submit event rather than a click handler on each button, because implicit
    // submission — Enter in the grant-name field — reports the form's default button as the
    // submitter too. The button order that makes that Enter harmless (a deselect first,
    // never a select-all) therefore keeps meaning the same thing with this file loaded.
    form.addEventListener('submit', (event) => {
      const button = event.submitter;
      // Approve and Deny are decisions, and they belong to the server. So does anything this
      // does not recognise, including the case where a browser reports no submitter at all:
      // then nothing is prevented and the form posts exactly as it does with no script.
      if (!button || button.name !== 'reselect') return;
      const apply = selections[button.value];
      if (!apply) return;

      event.preventDefault();
      apply();
      summarise();
    });

    form.addEventListener('change', (event) => {
      if (event.target.matches('input[type="checkbox"], input[name="mode"]')) summarise();
    });

    // Restored from the back/forward cache, a browser puts back the ticks the operator left
    // rather than the ticks the server rendered. The summary is recomputed from the boxes
    // for the same reason it is recomputed on every change: it may only ever describe what
    // the form would actually submit.
    window.addEventListener('pageshow', summarise);

    const summary = form.querySelector('[data-consent-summary]');

    function summarise() {
      if (!summary) return;

      const mailboxes = boxes('accounts').filter((box) => box.checked);
      const capabilities = boxes('capabilities').filter((box) => box.checked);
      const privileged = capabilities.filter((box) => box.dataset.privileged === 'yes');
      // The mode's clause is read off the radio the server rendered rather than written
      // here. Rule 1 again: a sentence on this page has to be a sentence the server wrote,
      // and a copy of that wording living in this file would drift from grant.Mode without
      // anything failing.
      const mode = form.querySelector('input[name="mode"]:checked');

      if (mailboxes.length === 0 || capabilities.length === 0) {
        summary.dataset.privileged = 'no';
        summary.textContent =
          'Nothing to approve yet. Tick at least one mailbox and at least one capability.';
      } else {
        let text =
          'Approve grants ' +
          count(mailboxes.length, 'mailbox', 'mailboxes') +
          ' and ' +
          count(capabilities.length, 'capability', 'capabilities') +
          '.';
        if (privileged.length > 0) {
          text += ' Privileged: ' + list(privileged.map((box) => box.value)) + '.';
          // Said only where there is something privileged to say it about. The mode governs
          // how the privileged capabilities are exercised, so on a read-only grant it is a
          // sentence about nothing.
          if (mode && mode.dataset.modeBrief) {
            text += ' It ' + mode.dataset.modeBrief;
          }
        }
        summary.dataset.privileged = privileged.length > 0 ? 'yes' : 'no';
        summary.textContent = text;
      }
      // Hidden in the markup, so a browser with this file blocked or broken never sees a
      // summary at all rather than seeing one that stopped keeping up with the boxes.
      summary.hidden = false;
    }

    summarise();
  }

  function tickAll(boxes, checked) {
    for (const box of boxes) box.checked = checked;
  }

  function count(n, one, many) {
    return n + ' ' + (n === 1 ? one : many);
  }

  function list(items) {
    if (items.length <= 1) return items.join('');
    return items.slice(0, -1).join(', ') + ' and ' + items[items.length - 1];
  }
})();
