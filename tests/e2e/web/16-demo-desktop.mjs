// 16. "Open in herdr" on the demo core (which pretends herdr is running):
// the button shows on sessions Pilothouse holds the terminal of, opens the
// session with a signed action, says where, and reuses the tab on a second
// tap; observed sessions have no button; the terminal bar has a compact one.
//
//   PH_TMP=… PH_SHOTS=… node 16-demo-desktop.mjs
import { startDemo, watch, check, summary, sleep, shot, waitText, scheme, PHONE } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
await page.setViewport(PHONE);
await scheme(page, 'light');
const logs = watch(page, 'P');

const link = await (await fetch(demo.admin + '/v1/login-link', { method: 'POST' })).json();
await page.goto(link.url.replace(/^http:\/\/[^/]+/, demo.base), { waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .page-title', { timeout: 15000 });

const snap = await page.evaluate(() => fetch('/api/snapshot').then((r) => r.json()));
const machine = (await page.evaluate(() => fetch('/api/desktop').then((r) => r.json()))).machine;
const wrapped = snap.sessions.find((s) => s.mode === 'wrapped' && s.caps && s.caps.watch && s.state !== 'exited');
const observed = snap.sessions.find((s) => s.mode === 'observed' && s.state !== 'exited');
check('demo has a wrapped and an observed session', wrapped && observed);

const go = async (path) => {
  await page.evaluate((p) => { history.pushState({ inApp: true }, '', p); dispatchEvent(new PopStateEvent('popstate')); }, path);
  await sleep(600);
};
const deskBtn = () => page.$('main .desk-btn:not([hidden])');

// ---- Session page ----
await go('/s/' + wrapped.id);
await page.waitForSelector('main .desk-btn:not([hidden])', { timeout: 8000 }).catch(() => null);
const btn = await deskBtn();
check('session page shows Open in herdr', btn && (await btn.evaluate((b) => b.innerText)).includes('Open in herdr'));
check('button names the computer', btn && (await btn.evaluate((b) => b.title)) === `Open this session in herdr on ${machine}`);
await shot(page, '16-desktop-button');
await btn.click();
check('toast says where it opened', await waitText(page, `Opened in herdr · ${wrapped.title} on ${machine.replace(/\.(local|lan|home|attlocal\.net)$/i, '')}`, 8000));
await sleep(300);
check('button is ready again', !(await btn.evaluate((b) => b.disabled)) && (await btn.evaluate((b) => b.innerText)).includes('Open in herdr'));
await shot(page, '16-desktop-opened');
await sleep(3500); // let the first toast go
await btn.click();
check('second tap reuses the tab', await waitText(page, 'Already open in herdr', 8000));

// ---- Observed session: no button ----
await go('/s/' + observed.id);
await sleep(800);
check('observed session has no button', !(await deskBtn()));

// ---- Terminal bar ----
await go('/s/' + wrapped.id + '/terminal');
const tool = await page.waitForSelector('.desk-tool:not([hidden])', { timeout: 8000 }).catch(() => null);
check('terminal bar has a herdr button', tool && (await tool.evaluate((b) => b.innerText)).trim() === 'herdr');
await shot(page, '16-desktop-terminal');

// ---- Desktop width ----
await page.setViewport({ width: 1280, height: 860 });
await go('/s/' + wrapped.id);
check('desktop layout shows the button', !!(await page.waitForSelector('main .desk-btn:not([hidden])', { timeout: 8000 }).catch(() => null)));
await shot(page, '16-desktop-wide');

check('no console errors', logs.length === 0, logs.join('\n'));
await demo.stop();
process.exit(summary() ? 1 : 0);
