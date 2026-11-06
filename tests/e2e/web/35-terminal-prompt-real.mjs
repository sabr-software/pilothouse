// 35. A prompt only the terminal can answer, against a real daemon and real
// hooks at desktop size, while you're at the computer:
//   - self-protection asks, so Claude Code prompts in its terminal and the
//     daemon steps aside: no decision in Pilothouse;
//   - the right pane lists it, counts it and agrees with the sidebar;
//   - Today doesn't say "All clear" above it;
//   - answering in the terminal clears it everywhere.
// Builds the binary itself; everything lives in a temp dir.
//   node 35-terminal-prompt-real.mjs
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
const TMP = fs.mkdtempSync(path.join(process.env.PH_TMP || os.tmpdir(), 'ph-term-'));
const BIN = path.join(TMP, 'pilothouse');
execFileSync('go', ['build', '-o', BIN, './cmd/pilothouse'], { cwd: ROOT, stdio: 'inherit' });

const port = 7720 + (process.pid % 50) * 2;
const BASE = `http://127.0.0.1:${port}`;
const env = {
  ...process.env,
  PILOTHOUSE_HOME: path.join(TMP, 'home'), PILOTHOUSE_USER_HOME: path.join(TMP, 'uhome'),
  PILOTHOUSE_RUN_DIR: fs.mkdtempSync('/tmp/pht-'), PILOTHOUSE_NO_UPDATE_CHECK: '1',
};
for (const d of [env.PILOTHOUSE_HOME, env.PILOTHOUSE_USER_HOME]) fs.mkdirSync(d, { recursive: true, mode: 0o700 });
fs.writeFileSync(path.join(env.PILOTHOUSE_HOME, 'config.yaml'),
  `machine: studio\nupdate_check: false\nkeep_awake: off\nweb:\n  addr: 127.0.0.1:${port}\n  public_url: ${BASE}\nnotify:\n  desktop: false\n`, { mode: 0o600 });
const daemon = spawn(BIN, ['daemon', '--debug', '--log-file', path.join(TMP, 'daemon.log')], { env, stdio: 'ignore' });

const person = (...args) => execFileSync('python3', [DETACH, BIN, ...args], { env, encoding: 'utf8' });
const sock = (p) => new Promise((res, rej) => {
  http.get({ socketPath: path.join(env.PILOTHOUSE_RUN_DIR, 'pilothouse.sock'), path: p }, (r) => {
    let b = '';
    r.on('data', (c) => { b += c; });
    r.on('end', () => { try { res(JSON.parse(b)); } catch { rej(new Error(b)); } });
  }).on('error', rej);
});

let browser;
try {
  let up = false;
  for (let i = 0; i < 100 && !up; i++) {
    up = await sock('/v1/health').then(() => true, () => false);
    if (!up) await sleep(100);
  }
  check('daemon started', up);
  person('here');

  const udd = fs.mkdtempSync(path.join(TMP, 'chrome-'));
  browser = await launch({ dir: udd });
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });
  const code = JSON.parse(person('pair', '--json', '--no-wait', '--no-qr')).code;
  await page.goto(`${BASE}/pair#${code}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#pair-name');
  const paired = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
  await page.click('button[type=submit]');
  check('paired in the browser', (await paired).status() === 200);
  await page.goto(`${BASE}/`, { waitUntil: 'domcontentloaded' });
  await page.waitForSelector('.queue .queue-empty', { timeout: 10000 });

  const cwd = fs.mkdtempSync(path.join(TMP, 'repo-'));
  const hook = (e, extra = {}) => execFileSync(BIN, ['hook', 'claude'], {
    env, cwd, encoding: 'utf8',
    input: JSON.stringify({ hook_event_name: e, session_id: 'cc-term-1', cwd, transcript_path: null, ...extra }),
  });
  const command = `sqlite3 ${env.PILOTHOUSE_HOME}/pilothouse.db "select id from requests"`;
  const tool = { tool_name: 'Bash', tool_input: { command }, tool_use_id: 'toolu_term1' };

  // What Claude Code sends when a hook makes it ask in the terminal.
  hook('SessionStart', { source: 'startup' });
  hook('UserPromptSubmit', { prompt: 'check the request' });
  const pre = hook('PreToolUse', tool);
  check('self-protection asks', pre.includes('"ask"') && pre.includes('self-protection'), pre.slice(0, 160));
  hook('PermissionRequest', { ...tool, permission_suggestions: [] });
  hook('Notification', { notification_type: 'permission_prompt', message: 'Claude needs your permission to use Bash' });

  const snap = await sock('/v1/snapshot');
  const req = (snap.requests || []).find((r) => r.command === command);
  check('the daemon steps aside for the terminal', req && req.status === 'stepped_aside' && req.route === 'terminal', JSON.stringify(req && { status: req.status, route: req.route }));
  check('the session is waiting on you', snap.sessions.some((s) => s.state === 'blocked'), JSON.stringify(snap.sessions.map((s) => s.state)));

  const view = () => page.evaluate(() => ({
    terminal: [...document.querySelectorAll('.queue .qcard.terminal')].map((c) => c.innerText),
    decisions: document.querySelectorAll('.queue .qcard:not(.terminal)').length,
    count: document.querySelector('.queue-head .count').hidden ? 0 : +document.querySelector('.queue-head .count').textContent,
    empty: !!document.querySelector('.queue .queue-empty'),
    side: +(document.querySelector('.sidebar .side-group.need .side-sub b') || { textContent: '0' }).textContent,
    clear: !!document.querySelector('main .lclear'),
    desk: document.querySelectorAll('main .g-desk .nrow').length,
  }));
  await page.waitForSelector('.queue .qcard.terminal', { timeout: 8000 }).catch(() => {});
  let v = await view();
  check('the pane lists the prompt instead of saying nothing needs you', v.terminal.length === 1 && !v.empty, JSON.stringify(v));
  check('it shows the command and where to answer', v.terminal[0]?.includes('sqlite3') && /Answer it on your computer/.test(v.terminal[0]), v.terminal[0]);
  check('it is not offered as a decision', v.decisions === 0 && !(await page.$('.queue .qcard.terminal button')));
  check('the pane and the sidebar agree', v.count === 1 && v.side === 1, JSON.stringify(v));
  check('Today doesn’t say all clear above it', !v.clear && v.desk === 1, JSON.stringify(v));
  if (process.env.PH_SHOTS) await page.screenshot({ path: path.join(process.env.PH_SHOTS, '35-real.png') });

  // You answer in the terminal, the tool runs.
  hook('PostToolUse', { ...tool, tool_response: { stdout: '' } });
  await page.waitForSelector('.queue .queue-empty', { timeout: 8000 }).catch(() => {});
  v = await view();
  check('answered in the terminal: gone from the pane', v.terminal.length === 0 && v.empty && v.count === 0, JSON.stringify(v));
  check('…and from the sidebar’s Needs you', v.side === 0, JSON.stringify(v));
  check('…and Today', v.desk === 0, JSON.stringify(v));
} catch (e) {
  check('ran to the end', false, e.message);
} finally {
  if (browser) await browser.close().catch(() => {});
  daemon.kill();
  fs.rmSync(env.PILOTHOUSE_RUN_DIR, { recursive: true, force: true });
  if (!process.env.KEEP) fs.rmSync(TMP, { recursive: true, force: true });
}
process.exit(summary() ? 1 : 0);
