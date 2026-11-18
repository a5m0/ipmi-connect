// Browser test of the console UI against test/e2e/fakeapp: sign-in, the consent dialog, the
// console socket and video, the security indicator, disconnect, replay and CSP.
// Run: node test/e2e/ui.mjs (Playwright and Chromium must be available).
import { spawn } from 'node:child_process';
import { createInterface } from 'node:readline';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

function fail(msg) { console.error(`FAIL: ${msg}`); process.exitCode = 1; throw new Error(msg); }
function ok(msg) { console.log(`ok: ${msg}`); }

import { execFileSync } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const bin = join(mkdtempSync(join(tmpdir(), 'ipmi-e2e-')), 'fakeapp');
execFileSync('go', ['build', '-o', bin, './test/e2e/fakeapp'], { stdio: 'inherit' });
const app = spawn(bin, [], { stdio: ['ignore', 'pipe', 'inherit'] });
const url = await new Promise((resolve, reject) => {
  const rl = createInterface({ input: app.stdout });
  rl.on('line', (l) => { if (l.startsWith('http://')) resolve(l.trim()); });
  app.on('exit', (c) => reject(new Error(`fakeapp exited ${c}`)));
  setTimeout(() => reject(new Error('fakeapp did not start')), 120000);
});
const origin = new URL(url).origin;

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ deviceScaleFactor: 1 });
  const problems = [];
  page.on('console', (m) => { if (m.type() === 'error') problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  let dialogs = 0;
  page.on('dialog', (d) => { dialogs++; d.accept(); });

  await page.goto(url);
  await page.waitForSelector('#connectView:not([hidden])');
  if (page.url().includes('#c=')) fail('the launch code stayed in the address bar');
  ok('signed in and showing the connection manager');
  if (!(await page.evaluate(() => sessionStorage.getItem('ipmi-connect-token')))) fail('token not in sessionStorage');
  if (await page.evaluate(() => localStorage.getItem('ipmi-connect-token'))) fail('token in localStorage');
  ok('token kept in sessionStorage only');

  await page.fill('#cAddress', '192.0.2.10');
  await page.fill('#cUser', 'ADMIN');
  await page.fill('#cPassword', 'test-password');
  await page.click('#btnConnect');
  await page.waitForSelector('#consentModal.open');
  const body = await page.textContent('#consentBody');
  if (!body.includes('AB:CD:EF:01')) fail('fingerprint not shown');
  if (!body.includes('<b>bmc</b>') || !body.includes('<script>')) fail('server text was not shown literally');
  if (await page.$('#consentBody b, #consentBody script')) fail('server text was rendered as HTML');
  if (!(await page.isDisabled('#btnConsentAccept'))) fail('accept enabled before every box is ticked');
  ok('consent dialog shows the fingerprint, escapes server text, needs every box');

  const boxes = await page.$$('#consentBody input[type=checkbox]');
  if (boxes.length !== 3) fail(`expected 3 consent boxes, got ${boxes.length}`);
  for (const b of boxes) await b.check();
  await page.click('#btnConsentAccept');
  await page.waitForSelector('#connectView', { state: 'hidden', timeout: 10000 });
  if (await page.inputValue('#cPassword')) fail('password left in the form after connecting');
  await page.waitForFunction(() => document.getElementById('statusDot').className.includes('connected'), null, { timeout: 10000 });
  await page.waitForFunction(() => {
    const c = document.getElementById('screenCanvas');
    return c.width === 320 && c.getContext('2d').getImageData(5, 5, 1, 1).data[0] === 200;
  }, null, { timeout: 10000 });
  ok('connected: socket open and video drawn');

  const badge = await page.textContent('#securityBadge');
  if (!/unencrypted/i.test(badge) || !(await page.isVisible('#securityBadge'))) fail(`security badge says "${badge}"`);
  await page.click('#securityBadge');
  const rows = await page.textContent('#securityRows');
  if (!rows.includes('None') || !rows.includes('Pinned certificate')) fail('security details incomplete');
  await page.click('#btnSecurityDone');
  ok('security indicator reports the unencrypted KVM and pinned certificate');

  // The type-text panel docks under the console and types its text on the host.
  await page.click('#btnPaste');
  await page.waitForSelector('#pastePanel:not([hidden])');
  await page.fill('#pasteText', 'echo hello');
  await page.check('#pasteEnter');
  await page.click('#btnPasteType');
  await page.waitForSelector('.toast >> text=Typed 11 characters', { timeout: 10000 });
  if (await page.inputValue('#pasteText')) fail('typed text left in the box without Keep text');
  if (!(await page.isVisible('#screenCanvas'))) fail('the type panel hid the console');
  await page.check('#pasteKeep');
  await page.fill('#pasteText', 'ls');
  await page.focus('#pasteText');
  await page.keyboard.press('Control+Enter');
  await page.waitForSelector('.toast >> text=Typed 3 characters', { timeout: 10000 });
  if (await page.inputValue('#pasteText') !== 'ls') fail('Keep text did not keep the text');
  await page.click('#btnClosePaste');
  if (await page.isVisible('#pastePanel')) fail('the type panel did not close');
  ok('type panel types its text (with Enter), keeps it on request, and closes');

  // Secondary buttons look like buttons, and selects are drawn by the page in the dark theme.
  const look = await page.evaluate(() => {
    const btn = getComputedStyle(document.getElementById('btnSecurityDone'));
    const sel = getComputedStyle(document.getElementById('cVendor'));
    return { border: btn.borderTopStyle !== 'none' && btn.borderTopColor !== 'rgba(0, 0, 0, 0)',
      bg: btn.backgroundColor !== 'rgba(0, 0, 0, 0)', appearance: sel.appearance, scheme: sel.colorScheme };
  });
  if (!look.border || !look.bg) fail(`secondary button has no outline or fill: ${JSON.stringify(look)}`);
  if (look.appearance !== 'none' || look.scheme !== 'dark') fail(`select not themed: ${JSON.stringify(look)}`);
  ok('secondary buttons are outlined and selects use the dark theme');

  // The socket refuses the long-lived token in its URL and a cross-origin page.
  const token = await page.evaluate(() => sessionStorage.getItem('ipmi-connect-token'));
  const wsRefused = await page.evaluate(async (t) => new Promise((res) => {
    const s = new WebSocket(`ws://${location.host}/ws?t=${t}`);
    s.onopen = () => res(false); s.onerror = () => res(true);
  }), token);
  if (!wsRefused) fail('socket accepted the token in its URL');
  ok('socket refuses the session token in the URL');

  const other = await browser.newPage();
  await other.goto('about:blank');
  const crossOrigin = await other.evaluate(async (o) => {
    try { const r = await fetch(`${o}/api/status`, { mode: 'no-cors' }); return r.type; } catch (e) { return 'blocked'; }
  }, origin);
  ok(`cross-origin fetch from another page: ${crossOrigin} (no token, so no data)`);
  await other.close();

  // Replaying the launch link fails.
  const replay = await browser.newPage();
  await replay.goto(url);
  await replay.waitForSelector('#overlayStatus');
  await replay.waitForFunction(() => document.getElementById('overlayStatus').textContent.includes('Not signed in'));
  ok('replayed launch link is refused');
  await replay.close();

  await page.click('#btnDisconnect');
  await page.waitForSelector('#connectView:not([hidden])', { timeout: 10000 });
  ok('disconnect returns to the connection manager');
  if (dialogs !== 1) fail(`expected 1 confirmation dialog, saw ${dialogs}`);

  const csp = problems.filter((p) => /Content Security Policy|Refused to/i.test(p));
  if (csp.length) fail(`CSP violations: ${csp.join(' | ')}`);
  const other_errors = problems.filter((p) => !/Failed to load resource|WebSocket connection to/.test(p));
  if (other_errors.length) fail(`page errors: ${other_errors.join(' | ')}`);
  ok('no CSP violations or script errors');
  if (process.env.SCREENSHOT) await page.screenshot({ path: process.env.SCREENSHOT });
} finally {
  await browser.close();
  app.kill('SIGINT');
}
