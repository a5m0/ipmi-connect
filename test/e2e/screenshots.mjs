// Captures the documentation screenshots of the connection manager, the consent dialog and the
// security details from test/e2e/fakeapp in documentation mode (no real BMC is contacted; the
// address is in the 192.0.2.0/24 documentation range). Run: node test/e2e/screenshots.mjs
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createInterface } from 'node:readline';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');
const out = (name) => join('docs', 'images', name);

const bin = join(mkdtempSync(join(tmpdir(), 'ipmi-shots-')), 'fakeapp');
execFileSync('go', ['build', '-o', bin, './test/e2e/fakeapp'], { stdio: 'inherit' });
const app = spawn(bin, [], { stdio: ['ignore', 'pipe', 'inherit'], env: { ...process.env, FAKEAPP_DOCS: '1' } });
const url = await new Promise((resolve, reject) => {
  createInterface({ input: app.stdout }).on('line', (l) => { if (l.startsWith('http://')) resolve(l.trim()); });
  app.on('exit', (c) => reject(new Error(`fakeapp exited ${c}`)));
});

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 2 });
  page.on('dialog', (d) => d.accept());
  await page.goto(url);
  await page.waitForSelector('#connectView:not([hidden])');

  // A saved device, so the manager shows how profiles look (no password is stored).
  await page.evaluate(async () => {
    const token = sessionStorage.getItem('ipmi-connect-token');
    await fetch('/api/profiles', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
      body: JSON.stringify({ name: 'Rack 3 storage (X8DTL)', address: '192.0.2.10', user: 'ADMIN', vendor: 'supermicro' }),
    });
  });
  await page.reload();
  await page.waitForSelector('#connectView:not([hidden])');
  await page.waitForFunction(() => document.querySelectorAll('#profileList button').length > 0);

  await page.fill('#cAddress', '192.0.2.10');
  await page.fill('#cUser', 'ADMIN');
  await page.selectOption('#cVendor', 'supermicro');
  await page.fill('#cPassword', 'test-password');
  await page.click('.connect-head h2'); // move focus off the form
  await page.screenshot({ path: out('connection-manager.png') });

  await page.click('#btnConnect');
  await page.waitForSelector('#consentModal.open');
  await page.waitForTimeout(300); // let the dialog finish appearing
  await page.screenshot({ path: out('consent.png') });

  for (const box of await page.$$('#consentBody input[type=checkbox]')) await box.check();
  await page.click('#btnConsentAccept');
  await page.waitForSelector('#connectView', { state: 'hidden', timeout: 10000 });
  await page.waitForSelector('#securityBadge:not([hidden])');
  await page.click('#securityBadge');
  await page.waitForSelector('#securityModal.open');
  await page.waitForTimeout(300);
  await page.screenshot({ path: out('security.png') });
  console.log('wrote docs/images/connection-manager.png, consent.png, security.png');
} finally {
  await browser.close();
  app.kill('SIGINT');
}
