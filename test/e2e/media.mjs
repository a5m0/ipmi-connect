// Browser test of the virtual media panel and the stats bar against test/e2e/fakeapp, whose
// virtual media is the real manager over a fake BMC that keeps reading mapped drives.
// Run: node test/e2e/media.mjs (Playwright and Chromium must be available).
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { basename, dirname, join } from 'node:path';
import { createInterface } from 'node:readline';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

function fail(msg) { console.error(`FAIL: ${msg}`); process.exitCode = 1; throw new Error(msg); }
function ok(msg) { console.log(`ok: ${msg}`); }

// Images live in a folder under the temporary directory, which fakeapp allows.
const images = mkdtempSync(join(tmpdir(), 'ipmi-media-'));
writeFileSync(join(images, 'installer.iso'), Buffer.alloc(4 << 20));
writeFileSync(join(images, 'scratch.img'), Buffer.alloc(1474560));

const bin = join(mkdtempSync(join(tmpdir(), 'ipmi-e2e-')), 'fakeapp');
execFileSync('go', ['build', '-o', bin, './test/e2e/fakeapp'], { stdio: 'inherit' });
const app = spawn(bin, [], { stdio: ['ignore', 'pipe', 'inherit'], env: { ...process.env, FAKEAPP_CONNECTED: '1' } });
const url = await new Promise((resolve, reject) => {
  const rl = createInterface({ input: app.stdout });
  rl.on('line', (l) => { if (l.startsWith('http://')) resolve(l.trim()); });
  app.on('exit', (c) => reject(new Error(`fakeapp exited ${c}`)));
  setTimeout(() => reject(new Error('fakeapp did not start')), 120000);
});

const browser = await chromium.launch();
try {
  const page = await browser.newPage();
  const problems = [];
  page.on('console', (m) => { if (m.type() === 'error') problems.push(m.text()); });
  page.on('pageerror', (e) => problems.push(e.message));
  const dialogs = [];
  page.on('dialog', (d) => { dialogs.push(d.message()); d.accept(); });

  await page.goto(url);
  await page.waitForFunction(() => document.getElementById('statusDot').className.includes('connected'), null, { timeout: 15000 });

  // Ping: the server measures the round trip to the BMC; fakeapp varies it every frame.
  await page.waitForFunction(() => /Ping: \d/.test(document.getElementById('statLatency').textContent), null, { timeout: 10000 });
  const firstPing = await page.textContent('#statLatency');
  await page.waitForFunction((p) => document.getElementById('statLatency').textContent !== p, firstPing, { timeout: 10000 });
  ok(`ping shows and updates (${firstPing} -> ${await page.textContent('#statLatency')})`);

  // Choose a file in the in-app picker: open our folder, then the file.
  async function pickFile(name) {
    await page.waitForSelector('#pickerModal.open');
    // The allowed folder that holds our images (the temporary directory).
    await page.waitForFunction((dir) => [...document.getElementById('pickerRoots').options].some((o) => dir.startsWith(o.value)), images);
    const root = await page.$eval('#pickerRoots', (s, dir) => [...s.options].map((o) => o.value).filter((v) => dir.startsWith(v)).sort((a, b) => b.length - a.length)[0], images);
    if ((await page.inputValue('#pickerRoots')) !== root || !(await page.textContent('#pickerPath')).endsWith(basename(images))) {
      await page.selectOption('#pickerRoots', root);
      await page.click(`#pickerList li.dir >> text="${basename(images)}/"`);
    }
    await page.click(`#pickerList li.file >> text="${name}"`);
    await page.waitForFunction(() => !document.getElementById('pickerModal').classList.contains('open'));
  }
  const drive = (t) => `#mediaDrives [data-drive="${t}"]`;

  await page.click('#btnMedia');
  await page.waitForSelector('#mediaModal.open');
  await page.waitForSelector(`${drive('floppy')} .media-insert`, { timeout: 10000 });
  for (const t of ['cd', 'removable', 'floppy']) {
    if (!(await page.isVisible(`${drive(t)} .media-insert`))) fail(`no Insert button on the ${t} drive`);
    if (!/empty/i.test(await page.textContent(drive(t)))) fail(`${t} drive not shown as empty`);
  }
  if (!(await page.textContent('#mediaModal')).match(/boot menu/i)) fail('no hint on how to boot from a drive');
  ok('every drive is listed as empty with an Insert button, and the panel says how to boot from it');

  // Insert: one click and a file choice map the image.
  await page.click(`${drive('cd')} .media-insert`);
  await pickFile('installer.iso');
  await page.waitForFunction((sel) => /installer\.iso/.test(document.querySelector(sel).textContent)
    && document.querySelector(`${sel} .media-eject`), drive('cd'), { timeout: 10000 });
  if (await page.isVisible('#mediaError')) fail(`inserting showed an error: ${await page.textContent('#mediaError')}`);
  ok('Insert maps the chosen ISO into the CD drive in one step');

  // Transfer: the stats bar and the drive show a live, non-zero rate that keeps changing.
  await page.waitForFunction(() => /VM ↓ [1-9]/.test(document.getElementById('statMedia').textContent), null, { timeout: 10000 });
  const readA = await page.textContent(`${drive('cd')} .media-counters`);
  await page.waitForFunction(([sel, a]) => document.querySelector(`${sel} .media-counters`).textContent !== a, [drive('cd'), readA], { timeout: 10000 });
  ok(`transfer rate shows in the stats bar (${await page.textContent('#statMedia')}) and the drive counters update`);

  // Eject, then insert again from the recent list with one click.
  await page.click(`${drive('cd')} .media-eject`);
  await page.waitForFunction((sel) => /empty/i.test(document.querySelector(sel).textContent), drive('cd'), { timeout: 10000 });
  const recent = '#mediaImages tr:has-text("installer.iso")';
  if (!(await page.isVisible(recent))) fail('ejected image not in the recent list');
  await page.click(`${recent} .media-reinsert`);
  await page.waitForFunction((sel) => /installer\.iso/.test(document.querySelector(sel).textContent), drive('cd'), { timeout: 10000 });
  ok('Eject empties the drive and the recent list re-inserts with one click');

  // A writable disk needs the box ticked and a confirmation.
  await page.check(`${drive('removable')} .media-writable input`);
  if (!dialogs.some((d) => /write/i.test(d))) fail('ticking Writable did not ask for confirmation');
  await page.click(`${drive('removable')} .media-insert`);
  await pickFile('scratch.img');
  await page.waitForFunction((sel) => /scratch\.img/.test(document.querySelector(sel).textContent)
    && /writable/i.test(document.querySelector(sel).textContent), drive('removable'), { timeout: 10000 });
  ok('a writable disk is confirmed, inserted and labelled writable');

  // An ISO cannot go into the floppy drive: the error is shown in the panel.
  await page.click(`${drive('floppy')} .media-insert`);
  await pickFile('installer.iso');
  await page.waitForFunction(() => /cannot|already/.test(document.getElementById('mediaError').textContent), null, { timeout: 10000 });
  if (/scratch|installer/.test(await page.textContent(drive('floppy'))) && !/empty/i.test(await page.textContent(drive('floppy')))) fail('the ISO went into the floppy drive');
  ok(`a wrong image type is refused with a message: "${await page.textContent('#mediaError')}"`);

  if (process.env.SCREENSHOT) { await page.waitForTimeout(600); await page.screenshot({ path: process.env.SCREENSHOT }); }
  const errs = problems.filter((p) => !/Failed to load resource|WebSocket connection to/.test(p));
  if (errs.length) fail(`page errors: ${errs.join(' | ')}`);
  ok('no script errors');
} finally {
  await browser.close();
  // fakeapp must shut down cleanly with drives mapped; if it hangs, dump its goroutines.
  const exited = new Promise((resolve) => app.on('exit', resolve));
  app.kill('SIGINT');
  const timer = setTimeout(() => { console.error('FAIL: fakeapp did not exit'); process.exitCode = 1; app.kill('SIGQUIT'); }, 5000);
  await exited;
  clearTimeout(timer);
  rmSync(images, { recursive: true, force: true });
  rmSync(dirname(bin), { recursive: true, force: true });
}
