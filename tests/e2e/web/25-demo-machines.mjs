// 25. Several machines in one place (pilothouse link), on the demo core with
// a pretend linked "laptop": machine labels, the Today filter, the report
// header, Settings → Machines, the desktop sidebar; axe in both themes.
//
//   PH_TMP=… PH_SHOTS=… node 25-demo-machines.mjs
import { startDemo, login, goto, watch, scheme, shot, check, summary, sleep, axeCheck, PHONE, DESKTOP, text } from './demo-lib.mjs';

const demo = await startDemo({ linked: true });
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'machines');
await page.setViewport(PHONE);
await login(page, demo);
await page.evaluate(() => localStorage.removeItem('ph.machine'));
await goto(page, demo, '/');
await sleep(300);

const chips = () => page.$$eval('.mchips .mchip', (b) => b.map((x) => ({ t: x.textContent.trim(), on: x.getAttribute('aria-pressed') === 'true' })));
const tags = () => page.$$eval('.ledger .mtag:not(.bg)', (t) => t.map((x) => x.textContent.trim()));
const rows = () => page.$$eval('.ledger [data-session], .ledger [data-request]', (r) => r.length);

let c = await chips();
check('Today shows machine chips: All, studio, laptop', c.map((x) => x.t).join('|') === 'All machines|studio|laptop', JSON.stringify(c));
check('All machines is selected at first', c[0] && c[0].on);
let t = await tags();
check('rows on the other machine carry its name; this machine\'s stay plain', t.length > 0 && t.every((x) => x === 'laptop'), t.join(','));
const all = await rows();

await page.click('.mchips .mchip:nth-child(3)');
await sleep(250);
t = await tags();
check('laptop narrows Today to its sessions (no labels needed)', t.length === 0 && (await rows()) < all, `tags=${t.join(',')} rows=${await rows()} of ${all}`);
check('the site build on laptop is listed', (await text(page, '.ledger')).includes('site'));
check('studio requests are hidden while narrowed', !(await page.$('.ledger [data-request="r_gitpush03"]')));
await shot(page, 'm-today-laptop-phone-light');
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector('.mchips .mchip');
await sleep(300);
c = await chips();
check('the choice survives a reload', c[2] && c[2].on, JSON.stringify(c));
await page.click('.mchips .mchip:nth-child(1)');
await sleep(200);
check('All machines shows everything again', (await rows()) === all);

// The report says where the session runs.
await goto(page, demo, '/s/s_site000002');
await sleep(300);
check('report header: on laptop', (await text(page, '.rmeta')).startsWith('on laptop'), await text(page, '.rmeta'));
await goto(page, demo, '/s/s_proofline');
await sleep(300);
check('report header of this machine has no machine', !(await text(page, '.rmeta')).includes('on studio'), await text(page, '.rmeta'));

// A laptop request says which machine it's on, and can't become a rule here.
const lsnap = await page.evaluate(() => fetch('/api/snapshot').then((r) => r.json()));
const onLaptop = (r) => (lsnap.sessions.find((x) => x.id === r.session_id) || {}).machine === 'laptop';
const lreq = lsnap.requests.find((r) => r.status === 'open' && onLaptop(r) && r.checkpoint === 'permission');
check('the demo has an open permission request on laptop', !!lreq);
if (lreq) {
  await goto(page, demo, '/r/' + lreq.id);
  await sleep(500);
  check('request page names the machine', (await text(page, '.dwho .mtag:not(.bg)')) === 'laptop', await text(page, '.dwho'));
  const rule = await page.$$eval('main button', (bs) => bs.filter((b) => /Make it a rule/.test(b.innerText) && b.offsetParent).length);
  check('no "Make it a rule" for another machine\'s request', rule === 0, String(rule));
}

// Settings → Machines.
await goto(page, demo, '/settings');
await sleep(300);
const st = await text(page, 'main');
check('Settings lists the machines', /Machines[\s\S]*studio[\s\S]*This computer[\s\S]*laptop[\s\S]*online/i.test(st), st.slice(st.indexOf('Machines'), st.indexOf('Machines') + 200));
check('Settings says how to link another', st.includes('pilothouse link add'));
check('Settings shows this device\'s key fingerprint', /^[0-9a-f]{4}( [0-9a-f]{4}){3}$/.test(await text(page, '.key-fp code')), await text(page, '.key-fp'));
await page.evaluate(() => [...document.querySelectorAll('.section-title, h2')].find((e) => e.textContent.trim() === 'Machines')?.scrollIntoView({ block: 'start' }));
await sleep(200);
await shot(page, 'm-settings-phone-light');

for (const s of ['light', 'dark']) {
  await scheme(page, s);
  await goto(page, demo, '/');
  await sleep(300);
  await axeCheck(page, `today with machines ${s}`);
  await goto(page, demo, '/settings');
  await sleep(300);
  await axeCheck(page, `settings machines ${s}`);
}

// Desktop: the sidebar labels sessions too.
await scheme(page, 'light');
await page.setViewport(DESKTOP);
await goto(page, demo, '/');
await sleep(400);
const side = await page.$$eval('.side-session .mtag:not(.bg)', (t) => t.map((x) => x.textContent.trim()));
check('desktop sidebar labels the other machine', side.length === 3 && side.every((x) => x === 'laptop'), side.join(','));
await shot(page, 'm-today-desktop-light');
const wide = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1);
check('no horizontal scroll on desktop', !wide);

check('no console or page errors', !logs.length, logs.join(' | '));
await demo.stop();
process.exit(summary() ? 1 : 0);
