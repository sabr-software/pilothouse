// 32. "Open in herdr" against a real daemon, a real holder and a real
// herdr server (a named test session, never your own):
//   - the button opens a herdr tab in the session's folder, and the tab is
//     attached: it shows the agent's output and typing there reaches it;
//   - a second tap reuses the tab; after a detach there it attaches again;
//   - a closed tab is replaced by a new one;
//   - once the agent exits the button goes away.
// Needs an isolated daemon (tests/e2e/claude/README.md) whose config has
//   desktop: { open: herdr, herdr_session: <name> }
// and that herdr session running: PH_BIN, PH_BASE, HERDR_SESSION, plus the
// PILOTHOUSE_* variables.
//   node 32-desktop-real.mjs
import { execFileSync } from 'node:child_process';
import path from 'node:path';
import fs from 'node:fs';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import { launch, check, summary, sleep } from './lib.mjs';

const BIN = process.env.PH_BIN;
const BASE = process.env.PH_BASE;
const HS = process.env.HERDR_SESSION;
if (!BIN || !BASE || !HS || !process.env.PILOTHOUSE_RUN_DIR) {
  console.error('set PH_BIN, PH_BASE, HERDR_SESSION and the PILOTHOUSE_* variables of an isolated daemon');
  process.exit(2);
}
const DETACH = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', 'claude', 'detach.py');
const person = (...args) => execFileSync('python3', [DETACH, BIN, ...args], { encoding: 'utf8' });
const herdr = (...args) => JSON.parse(execFileSync('herdr', ['--session', HS, ...args], { encoding: 'utf8' })).result;
const tabs = () => herdr('workspace', 'list').workspaces.flatMap((w) => herdr('tab', 'list', '--workspace', w.workspace_id).tabs);
const paneOf = (tab) => herdr('pane', 'list', '--workspace', tab.workspace_id).panes.find((p) => p.tab_id === tab.tab_id);
const read = (pane) => execFileSync('herdr', ['--session', HS, 'pane', 'read', pane, '--source', 'recent-unwrapped', '--lines', '60'], { encoding: 'utf8' });
const waitFor = async (fn, ms = 8000) => { for (const end = Date.now() + ms; Date.now() < end; await sleep(150)) { try { if (fn()) return true; } catch { /* not yet */ } } return false; };
const PHONE = { width: 390, height: 844, deviceScaleFactor: 2, isMobile: true, hasTouch: true };

const work = fs.mkdtempSync(path.join(os.tmpdir(), 'ph-desk-'));
const name = `desk-${Date.now() % 100000}`;
const sid = execFileSync(BIN, ['run', '-d', '-n', name, '--', '/bin/sh', '-c',
  'echo DESKREADY; while read l; do echo "typed:$l"; done'], { encoding: 'utf8', cwd: work }).trim();
const before = new Set(tabs().map((t) => t.tab_id));
const udd = fs.mkdtempSync(path.join(os.tmpdir(), 'ph-e2e-d-'));
const browser = await launch({ dir: udd });
try {
  const page = await browser.newPage();
  await page.emulate({ viewport: PHONE, userAgent: 'Mozilla/5.0 (iPhone) e2e' });
  const code = JSON.parse(person('pair', '--json', '--no-wait', '--no-qr')).code;
  await page.goto(`${BASE}/pair#${code}`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('#pair-name');
  const paired = page.waitForResponse((r) => r.url().endsWith('/api/pair'));
  await page.click('button[type=submit]');
  check('paired in the browser', (await paired).status() === 200);

  await page.goto(`${BASE}/s/${sid}`, { waitUntil: 'domcontentloaded' });
  const btn = await page.waitForSelector('main .desk-btn:not([hidden])', { timeout: 10000 }).catch(() => null);
  check('session page offers Open in herdr', !!btn);
  const toastText = () => page.$$eval('.toast', (ts) => ts.map((t) => t.innerText).join(' | ')).catch(() => '');
  const tap = async (want) => {
    const res = page.waitForResponse((r) => r.url().endsWith(`/api/sessions/${sid}/desktop`));
    await page.click('main .desk-btn');
    const r = await res;
    const body = await r.json().catch(() => ({}));
    await sleep(300);
    return { status: r.status(), body, toast: await toastText(), ok: r.status() === 200 && (!want || (await toastText()).includes(want)) };
  };

  // ---- First tap: a new tab, attached ----
  let r = await tap('Opened in herdr');
  check('first tap opens a herdr tab', r.ok, JSON.stringify(r));
  const opened = tabs().filter((t) => !before.has(t.tab_id));
  check('exactly one new tab, named after the session', opened.length === 1 && opened[0].label === name, JSON.stringify(opened));
  const tab = opened[0];
  const pane = tab && paneOf(tab);
  check('the tab starts in the session folder', pane && fs.realpathSync(pane.cwd) === fs.realpathSync(work), pane && pane.cwd);
  check('the tab shows the agent (replay)', await waitFor(() => read(pane.pane_id).includes('DESKREADY')));
  execFileSync('herdr', ['--session', HS, 'pane', 'send-text', pane.pane_id, 'from-herdr']);
  execFileSync('herdr', ['--session', HS, 'pane', 'send-keys', pane.pane_id, 'enter']);
  check('typing in the tab reaches the agent', await waitFor(() => read(pane.pane_id).includes('typed:from-herdr')));
  const act = await page.evaluate((id) => fetch(`/api/sessions/${id}`).then((x) => x.json()), sid);
  check('the session is still running', act.state !== 'exited', act.state);

  // ---- Second tap: same tab ----
  await sleep(3200);
  r = await tap('Already open in herdr');
  check('second tap reuses the tab', r.ok && r.body.reused === true && tabs().filter((t) => !before.has(t.tab_id)).length === 1, JSON.stringify(r));

  // ---- Detach in the tab, tap again: attaches again ----
  const attaches = () => read(pane.pane_id).split(`attach --replay ${sid}`).length - 1;
  const atPrompt = () => { const p = herdr('pane', 'process-info', '--pane', pane.pane_id).process_info; return p.foreground_process_group_id === p.shell_pid; };
  check('attached while open (shell not at its prompt)', !atPrompt());
  const before1 = attaches();
  execFileSync('herdr', ['--session', HS, 'pane', 'send-keys', pane.pane_id, 'ctrl+\\']);
  check('detached in the tab (shell back at its prompt)', await waitFor(atPrompt), read(pane.pane_id).slice(-300));
  await sleep(3200);
  r = await tap('Already open in herdr');
  check('tap after detach is accepted', r.ok, JSON.stringify(r));
  check('the attach command was typed again', await waitFor(() => attaches() > before1 && !atPrompt()), String(attaches()));
  execFileSync('herdr', ['--session', HS, 'pane', 'send-text', pane.pane_id, 'again']);
  execFileSync('herdr', ['--session', HS, 'pane', 'send-keys', pane.pane_id, 'enter']);
  check('the tab is attached again', await waitFor(() => read(pane.pane_id).includes('typed:again')), read(pane.pane_id).slice(-300));

  // ---- Close the tab, tap again: a new tab ----
  herdr('tab', 'close', tab.tab_id);
  await sleep(3200);
  r = await tap('Opened in herdr');
  const now = tabs().filter((t) => !before.has(t.tab_id));
  check('a closed tab is replaced', r.ok && !r.body.reused && now.length === 1 && now[0].tab_id !== tab.tab_id, JSON.stringify({ r, now }));

  // ---- The agent exits (end of input, typed in the new tab): no button, and the API refuses ----
  const pane2 = paneOf(now[0]);
  check('the new tab is attached too', await waitFor(() => read(pane2.pane_id).includes('DESKREADY')));
  execFileSync('herdr', ['--session', HS, 'pane', 'send-keys', pane2.pane_id, 'ctrl+d']);
  check('button hides after the agent exits', await page.waitForSelector('main .desk-btn', { hidden: true, timeout: 10000 }).then(() => true, () => false));
  const refused = await page.evaluate(async (id) => {
    const res = await self.PilothouseKeys.postSigned(`/api/sessions/${id}/desktop`, 'desktop', id, {});
    return res.status;
  }, sid);
  check('opening an ended session is refused', refused === 400, String(refused));

  // Clean up only the tabs this test made.
  for (const t of tabs().filter((x) => !before.has(x.tab_id))) herdr('tab', 'close', t.tab_id);
} finally {
  await browser.close();
  const s = JSON.parse(execFileSync(BIN, ['ls', '--all', '--json'], { encoding: 'utf8' })).sessions.find((x) => x.id === sid);
  if (s && s.pid && s.state !== 'exited') try { process.kill(s.pid); } catch { /* already gone */ }
}
process.exit(summary() ? 1 : 0);
