// 27. Pairing and the signing key, end to end on the demo core, the way
// people actually hit them on a desktop browser:
//   - a sign-in link opened again in a signed-in browser just opens the app;
//   - /pair reloaded while signed in with a working key goes to Today;
//   - Pair again from Settings: the done screen proves signing works and
//     stays put (the old device's revocation doesn't bounce it back to the
//     pair screen); the new device can sign;
//   - the same (now used) code typed again: an error, and the key that
//     works is kept;
//   - Settings shows this device's key beside the computer's and Check
//     proves it; a mismatched key says so there and offers Pair again.
//   node 27-demo-pairing.mjs
import { startDemo, check, summary, sleep, shot, waitText, scheme, DESKTOP } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await (await browser.createBrowserContext()).newPage();
await page.setViewport(DESKTOP);
await scheme(page, 'light');
const errs = [];
page.on('pageerror', (e) => errs.push(e.message));

const post = async (p) => (await fetch(demo.admin + p, { method: 'POST' })).json();
const loginLink = async () => (await post('/v1/login-link')).url.replace(/^http:\/\/[^/]+/, demo.base);
const me = () => page.evaluate(() => fetch('/api/me').then((r) => r.json()));
const spki = () => page.evaluate(() => self.PilothouseKeys.load().then((r) => r && r.spki));
const path = () => page.evaluate(() => location.pathname);
const body = () => page.$eval('body', (b) => b.innerText);
const inApp = (p) => page.evaluate((p) => { history.pushState({ inApp: true }, '', p); dispatchEvent(new PopStateEvent('popstate')); }, p);
const today = () => page.waitForSelector('main .page-title', { timeout: 15000 }).then(() => true, () => false);

// ---- Sign in, then open the same link again ----
const link = await loginLink();
await page.goto(link, { waitUntil: 'domcontentloaded' });
check('a sign-in link signs in', await today());
const first = await me();
const key1 = await spki();
await page.goto(link, { waitUntil: 'domcontentloaded' });
check('the same link again: straight to the app', await today() && !(await body()).includes('Couldn’t sign in'), (await body()).slice(0, 120));
check('…same device, same key', (await me()).device.id === first.device.id && (await spki()) === key1);

// ---- /pair reloaded while signed in with a working key ----
await page.goto(demo.base + '/pair', { waitUntil: 'domcontentloaded' });
await today();
check('/pair with a working key goes to Today', (await path()) === '/', await path());

// ---- Pair again from Settings ----
await inApp('/settings');
await page.waitForSelector('main .view.settings', { timeout: 10000 });
check('Settings always offers Pair again', await page.$$eval('main a[href="/pair"]', (as) => as.some((a) => a.innerText.includes('Pair again'))));
await page.evaluate(() => [...document.querySelectorAll('main a[href="/pair"]')].find((a) => a.innerText.includes('Pair again')).click());
await page.waitForSelector('#pair-code', { timeout: 5000 });
const code = (await post('/v1/pair')).code;
await page.evaluate(() => { window.__samePage = true; });
await page.type('#pair-code', code);
await page.click('button[type=submit]');
check('pair again → paired', await waitText(page, 'You’re paired', 8000));
check('the done screen proves signing works', await waitText(page, 'Signing works', 8000));
await sleep(2000); // the old device's revocation arrives on the live stream meanwhile
check('…and stays there: no bounce back to the pair screen', (await body()).includes('You’re paired') && await page.evaluate(() => window.__samePage === true) && !(await page.$('#pair-code')));
await shot(page, '27-paired-again');
const second = await me();
check('a new device replaced the old one', second.device.id !== first.device.id);
await page.evaluate(() => [...document.querySelectorAll('button')].find((b) => b.innerText.includes('Open inbox')).click());
check('Open inbox → Today', await today() && (await path()) === '/');
const key2 = await spki();

// ---- The same, now used, code typed again ----
await inApp('/pair');
await page.waitForSelector('#pair-code', { timeout: 5000 });
await page.type('#pair-code', code);
await page.click('button[type=submit]');
check('a used code is refused', await waitText(page, 'That code didn’t work', 8000));
check('…and the key that works is kept', (await spki()) === key2 && (await me()).device.public_key === key2);
const signed = await page.evaluate((id) => self.PilothouseKeys.check(id).then((r) => r.ok), second.device.id);
check('…which still signs', signed);

// ---- Settings: the key, plainly ----
await inApp('/settings');
await page.waitForSelector('main .view.settings', { timeout: 10000 });
await sleep(300);
const fps = await page.$$eval('main .key-fp code', (cs) => cs.map((c) => c.textContent));
check('Settings shows this device’s key beside the computer’s, matching', fps.length === 2 && /^[0-9a-f]{4}( [0-9a-f]{4}){3}$/.test(fps[0]) && fps[0] === fps[1], JSON.stringify(fps));
await page.evaluate(() => [...document.querySelectorAll('main button')].find((b) => b.innerText.trim() === 'Check').click());
check('Check proves it signs', await waitText(page, 'your computer just verified a signature', 8000, 'main'));
await shot(page, '27-settings-key');

// ---- A mismatched key ----
await page.evaluate(() => self.PilothouseKeys.generate().then((r) => self.PilothouseKeys.save(r)));
await page.goto(demo.base + '/settings', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .view.settings', { timeout: 15000 });
check('mismatch: Settings says so', await waitText(page, 'Doesn’t match the key your computer has', 8000, 'main'));
const fps2 = await page.$$eval('main .key-fp code', (cs) => cs.map((c) => c.textContent));
check('…the two keys differ on screen', fps2.length === 2 && fps2[0] !== fps2[1], JSON.stringify(fps2));
check('…Pair again is the main action', await page.$$eval('main .key-setting a.btn.primary[href="/pair"]', (as) => as.length === 1), JSON.stringify(await page.$$eval('main a[href="/pair"]', (as) => as.map((a) => a.className + ':' + a.innerText))));
await page.goto(demo.base + '/pair', { waitUntil: 'domcontentloaded' });
await sleep(1500);
check('mismatch: /pair stays on the pair screen', !!(await page.$('#pair-code')) && (await path()) === '/pair');
await shot(page, '27-settings-mismatch');

check('no page errors', !errs.length, errs.join(' | '));
await demo.stop();
process.exit(summary() ? 1 : 0);
