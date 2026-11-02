// 31. Two real daemons linked with `pilothouse link`, and a browser paired
// with the hub, in headless Chrome at phone size:
//   - `link add` on the hub, `link join` on the laptop (declining trust);
//   - the laptop's session and held request show on the hub, labelled;
//   - the browser's device reaches the laptop untrusted, so Hold to allow
//     is refused there with the fix in the message; `link trust` on the
//     laptop, and the same hold allows it: the agent's hook gets "allow";
//   - `link ls` on both, then `link remove` on the hub drops the laptop.
// Builds the binary itself; everything lives in a temp dir.
//   node 31-link-real.mjs
import { spawn, execFileSync } from 'node:child_process';
import http from 'node:http';
import path from 'node:path';
import fs from 'node:fs';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import { launch, check, summary, sleep } from './lib.mjs';

const HERE = path.dirname(fileURLToPath(import.meta.url));
const ROOT = path.join(HERE, '..', '..', '..');
const DETACH = path.join(HERE, '..', 'claude', 'detach.py');
const TMP = fs.mkdtempSync(path.join(process.env.PH_TMP || os.tmpdir(), 'ph-link-'));
const BIN = path.join(TMP, 'pilothouse');
execFileSync('go', ['build', '-o', BIN, './cmd/pilothouse'], { cwd: ROOT, stdio: 'inherit' });

const PHONE = { width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true };
const port = (n) => 7620 + n + (process.pid % 50) * 2;

function machine(name, n) {
  const dir = path.join(TMP, name);
  const env = {
    ...process.env,
    PILOTHOUSE_HOME: path.join(dir, 'home'), PILOTHOUSE_USER_HOME: path.join(dir, 'uhome'),
    PILOTHOUSE_RUN_DIR: fs.mkdtempSync('/tmp/phl-'), PILOTHOUSE_NO_UPDATE_CHECK: '1',
  };
  for (const d of [env.PILOTHOUSE_HOME, env.PILOTHOUSE_USER_HOME]) fs.mkdirSync(d, { recursive: true, mode: 0o700 });
  const url = `http://127.0.0.1:${port(n)}`;
  fs.writeFileSync(path.join(env.PILOTHOUSE_HOME, 'config.yaml'),
    `machine: ${name}\nupdate_check: false\nkeep_awake: off\nweb:\n  addr: 127.0.0.1:${port(n)}\n  public_url: ${url}\nnotify:\n  desktop: false\n`, { mode: 0o600 });
  const proc = spawn(BIN, ['daemon', '--debug', '--log-file', path.join(dir, 'daemon.log')], { env, stdio: 'ignore' });
  return {
    name, env, url, proc, dir,
    // As the person (outside any agent's process tree).
    person: (args, input) => execFileSync('python3', input == null ? [DETACH, BIN, ...args] : [DETACH, 'sh', '-c', `printf '${input}' | "$0" "$@"`, BIN, ...args], { env, encoding: 'utf8' }),
    run: (args, input) => execFileSync(BIN, args, { env, encoding: 'utf8', input }),
    sock: (p) => new Promise((res, rej) => {
      http.get({ socketPath: path.join(env.PILOTHOUSE_RUN_DIR, 'pilothouse.sock'), path: p }, (r) => {
        let b = '';
        r.on('data', (c) => { b += c; });
        r.on('end', () => { try { res(JSON.parse(b)); } catch (e) { rej(new Error(b)); } });
      }).on('error', rej);
    }),
  };
}

const hub = machine('studio', 0);
const laptop = machine('laptop', 1);
let browser;
const hooks = [];
try {
  for (const m of [hub, laptop]) {
    let up = false;
    for (let i = 0; i < 100 && !up; i++) {
      up = await m.sock('/v1/health').then(() => true, () => false);
      if (!up) await sleep(100);
    }
    check(`${m.name} daemon started`, up);
  }

  // Link: add on the hub, join on the laptop, decline trust for now.
  const code = JSON.parse(hub.person(['link', 'add', '--json']));
  check('link add gives the hub URL and a code', code.url === hub.url && /^[0-9A-Z]{4}-/.test(code.code), JSON.stringify(code));
  let out = laptop.person(['link', 'join', code.url, code.code], 'n\\n');
  check('link join links the laptop', out.includes('Linked to studio'), out.trim());
  out = hub.person(['link', 'ls']);
  check('link ls on the hub lists the laptop', /laptop\s+http:\/\/127\.0\.0\.1:\d+\s+\S+\s+online/.test(out), out.trim());

  // An agent on the laptop asks for permission while you're away.
  laptop.person(['away']);
  const cwd = fs.mkdtempSync(path.join(TMP, 'repo-'));
  const hookIn = (e, extra = {}) => JSON.stringify({ hook_event_name: e, session_id: 'cc-link-1', cwd, transcript_path: null, ...extra });
  laptop.run(['hook', 'claude'], hookIn('SessionStart', { source: 'startup' }));
  const held = spawn(BIN, ['hook', 'claude'], { env: laptop.env, cwd, stdio: ['pipe', 'pipe', 'pipe'] });
  hooks.push(held);
  let hookOut = '';
  held.stdout.on('data', (c) => { hookOut += c; });
  held.stdin.end(hookIn('PermissionRequest', { tool_name: 'Bash', tool_input: { command: 'git push origin main' }, permission_suggestions: [] }));
  let req = null;
  for (let i = 0; i < 50 && !req; i++) {
    const snap = await laptop.sock('/v1/snapshot');
    req = (snap.requests || []).find((r) => r.status === 'open');
    if (!req) await sleep(100);
  }
  check('the laptop holds the request for devices', !!req && req.route === 'devices', JSON.stringify(req && { route: req.route, status: req.status }));

  // A browser paired with the hub (its own non-extractable key).
  browser = await launch();
  const page = await browser.newPage();
  await page.setViewport(PHONE);
  const errs = [];
  page.on('pageerror', (e) => errs.push(e.message));
  const login = hub.person(['open', '--print']).trim().split(/\s+/).find((w) => w.startsWith('http'));
  await page.goto(login, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.ledger', { timeout: 15000 });
  const sawRow = await page.waitForFunction(() => [...document.querySelectorAll('.ledger .mtag')].some((t) => t.textContent.trim() === 'laptop'),
    { timeout: 10000, polling: 200 }).then(() => true, () => false);
  check('Today on the hub shows the laptop\'s request, labelled', sawRow);
  check('machine chips: All, studio, laptop', (await page.$$eval('.mchip', (b) => b.map((x) => x.textContent.trim()).join('|'))) === 'All machines|studio|laptop');

  // Hold to allow on the request page: refused until the laptop trusts this device.
  const hold = async () => {
    await page.goto(new URL('/r/' + req.id, hub.url).href, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('.btn.hold', { timeout: 10000 });
    await sleep(700);
    const b = await (await page.$('.btn.hold')).boundingBox();
    await page.mouse.move(b.x + b.width / 2, b.y + b.height / 2);
    await page.mouse.down();
    await sleep(1300);
    await page.mouse.up();
    await sleep(1500);
    return page.$eval('body', (e) => e.innerText);
  };
  let body = '';
  for (let i = 0; i < 20; i++) { // the hub sends its new device to the laptop right after pairing
    const ds = laptop.person(['link', 'ls']);
    if (ds.includes('not trusted')) break;
    await sleep(250);
  }
  body = await hold();
  check('an untrusted device is refused on the laptop, with the fix', body.includes('pilothouse link trust'), body.slice(0, 300).replace(/\n/g, ' | '));
  check('the request is still open', (await laptop.sock('/v1/requests/' + req.id)).status === 'open');

  out = laptop.person(['link', 'trust', '--yes']);
  check('link trust on the laptop trusts the browser', /Trusted 1 device/.test(out), out.trim().split('\n').pop());
  body = await hold();
  for (let i = 0; i < 40 && !hookOut; i++) await sleep(100);
  check('Hold to allow on the hub allows it on the laptop', /"behavior"\s*:\s*"allow"/.test(hookOut), hookOut.trim().slice(0, 200));
  const closed = await laptop.sock('/v1/requests/' + req.id);
  check('decided by the browser\'s device', closed.status === 'decided' && closed.decision.behavior === 'allow' && /^device:d_/.test(closed.decision && closed.decision.by), JSON.stringify(closed.decision));

  // Unlink.
  out = hub.person(['link', 'remove', 'laptop']);
  check('link remove on the hub', out.includes('Unlinked laptop'), out.trim());
  out = laptop.person(['link', 'ls']);
  check('the laptop knows it was unlinked', out.includes('No linked machines'), out.trim());
  await page.goto(hub.url + '/', { waitUntil: 'domcontentloaded' });
  await sleep(1500);
  check('the hub no longer shows the laptop', !(await page.$('.mchip')));
  check('no page errors', !errs.length, errs.join(' | '));
} finally {
  for (const p of hooks) p.kill();
  if (browser) await browser.close();
  for (const m of [hub, laptop]) {
    m.proc.kill();
    fs.rmSync(m.env.PILOTHOUSE_RUN_DIR, { recursive: true, force: true });
  }
  await sleep(300);
  if (!process.env.KEEP) fs.rmSync(TMP, { recursive: true, force: true });
}
process.exit(summary() ? 1 : 0);
