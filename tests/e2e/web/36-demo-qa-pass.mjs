// 36. Regression checks from the QA pass of 2026-11-18, on the demo core.
// Each block names the defect it guards against.
//
//   PH_TMP=… PH_SHOTS=… node 36-demo-qa-pass.mjs
import { execFileSync } from 'node:child_process';
import { startDemo, login, watch, check, summary, sleep, scheme, PHONE } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'qa');
await scheme(page, 'light');
await page.setViewport(PHONE);
await login(page, demo);
const until = (fn, ...args) => page.waitForFunction(fn, { timeout: 10000, polling: 50 }, ...args).then(() => true, () => false);

// QA-1: the connection banner kept "Reconnecting…" in the page (faded out)
// while connected, so a screen reader could read it and every re-render
// re-announced it through its polite live region.
await until(() => document.querySelector('.conn') && !document.querySelector('.conn').classList.contains('connecting'));
const banner = await page.$eval('.banner', (b) => ({ text: b.textContent.trim(), shown: b.classList.contains('show'), hiddenFromAT: b.getAttribute('aria-hidden') === 'true' }));
check('QA-1: connected, the connection banner is hidden from assistive tech', !banner.shown && banner.hiddenFromAT, JSON.stringify(banner));
check('no console or page errors', logs.length === 0, logs.slice(0, 4).join(' | '));
// QA-R: every route, at every width, in both themes: the page never scrolls
// sideways, nothing sticks out past the viewport, and axe finds nothing
// serious.
import { goto, setSize, axe } from './demo-lib.mjs';
const hrefs = await page.$$eval('main a[href^="/s/"], main a[href^="/r/"]', (as) => [...new Set(as.map((a) => a.getAttribute('href')))]);
const routes = ['/', '/rules', '/settings', '/insights', '/tasks', '/new', ...hrefs];
const widths = [[320, 640], [390, 844], [606, 667], [768, 1024], [1280, 800]];
const sideways = () => page.evaluate(() => {
  const vw = document.documentElement.clientWidth;
  const out = [];
  for (const e of document.querySelectorAll('main *, .topbar *, .tabbar *')) {
    const r = e.getBoundingClientRect();
    if (!r.width || !r.height) continue;
    if (e.closest('svg')) continue; // decorative art, clipped on purpose
    const cs = getComputedStyle(e);
    if (cs.position === 'fixed' || cs.visibility === 'hidden' || cs.display === 'none') continue;
    if (r.right > vw + 1 || r.left < -1) {
      // fine when an ancestor scrolls horizontally on purpose
      let p = e.parentElement, scrolls = false;
      while (p && p !== document.body) { const o = getComputedStyle(p).overflowX; if ((o === 'auto' || o === 'scroll' || o === 'hidden') && p.scrollWidth > p.clientWidth) { scrolls = true; break; } p = p.parentElement; }
      if (!scrolls) out.push(`${e.tagName.toLowerCase()}.${String(e.className).split(' ')[0]} ${Math.round(r.left)}..${Math.round(r.right)}`);
    }
  }
  return { page: document.scrollingElement.scrollWidth > vw + 1 ? `${document.scrollingElement.scrollWidth}>${vw}` : '', out: out.slice(0, 4) };
});
const seen = new Set();
for (const theme of ['light', 'dark']) {
  await scheme(page, theme);
  for (const [w, h] of widths) {
    await setSize(page, w < 800 ? { width: w, height: h, isMobile: true, hasTouch: true, deviceScaleFactor: 2 } : { width: w, height: h, deviceScaleFactor: 1 });
    for (const r of routes) {
      await goto(page, demo, r);
      await sleep(250);
      const o = await sideways();
      check(`QA-R ${theme} ${w}px ${r}: no sideways scroll or overflow`, !o.page && o.out.length === 0, JSON.stringify(o));
      // QA-3: a page title squeezed to nothing by a sibling in the header row
      // overlapped the text under it (New task).
      const head = await page.evaluate(() => {
        const t = document.querySelector('main .page-title'); if (!t || getComputedStyle(t).position === 'absolute') return null; // Today's is for screen readers only
        const r = t.getBoundingClientRect(); const sub = document.querySelector('main .page-sub');
        const sr = sub && sub.getBoundingClientRect();
        return { w: Math.round(r.width), overlap: !!sr && r.bottom > sr.top + 1 && r.right > sr.left && r.left < sr.right && r.top < sr.bottom };
      });
      if (head) check(`QA-3 ${theme} ${w}px ${r}: page title has room and doesn't overlap its subtitle`, head.w >= 40 && !head.overlap, JSON.stringify(head));
      const sev = (await axe(page)).filter((x) => x.impact === 'serious' || x.impact === 'critical');
      const key = r.replace(/\/(s|r)\/.*/, '/$1/*') + '|' + sev.map((x) => x.id).join(',');
      if (sev.length && !seen.has(key + w + theme)) { seen.add(key + w + theme); console.log('  axe', theme, w, r, JSON.stringify(sev)); }
      check(`QA-R ${theme} ${w}px ${r}: axe has nothing serious`, sev.length === 0, sev.map((x) => x.id + '×' + x.n).join(', '));
    }
  }
}
await scheme(page, 'light');
await setSize(page, PHONE);

// QA-4: saving an empty device name did nothing at all and left the field
// blank although the saved name hadn't changed.
await goto(page, demo, '/settings');
await page.waitForSelector('#device-name');
const savedName = await page.$eval('#device-name', (i) => i.value);
await page.$eval('#device-name', (i) => { i.value = ''; });
await page.click('.name-form button[type=submit]');
const toastSeen = await until(() => [...document.querySelectorAll('.toast')].some((t) => /name/i.test(t.textContent)));
check('QA-4: an empty device name says so', toastSeen);
check('QA-4: …and the field is not left blank', (await page.$eval('#device-name', (i) => i.value)) !== '' || (await page.evaluate(() => document.activeElement && document.activeElement.id)) === 'device-name');
check('QA-4: the saved name is unchanged', (await page.evaluate(() => fetch('/api/me').then((r) => r.json()).then((m) => m.device.name))) === savedName);

// QA-5: the reply box advertised a "send" return key on phones, but Enter in
// it inserts a newline (⌘/Ctrl+Enter or the Send button sends), so the key's
// label promised something it didn't do.
await goto(page, demo, '/s/s_pilothouse01');
await page.waitForSelector('#reply-text');
const hint = await page.$eval('#reply-text', (t) => t.getAttribute('enterkeyhint'));
await page.type('#reply-text', 'a');
await page.keyboard.press('Enter');
const afterEnter = await page.$eval('#reply-text', (t) => t.value);
check('QA-5: the reply box doesn’t label its return key "send" while Enter adds a line', !(hint === 'send' && afterEnter.includes('\n')), `enterkeyhint=${hint} value=${JSON.stringify(afterEnter)}`);
await page.$eval('#reply-text', (t) => { t.value = ''; t.dispatchEvent(new Event('input', { bubbles: true })); });

// QA-7: signing out used the browser's own confirm() box, the only one in
// the app (every other confirmation is a sheet), which can't be styled, is
// suppressed in some embedded browsers, and froze automated browsers.
const nativeDialogs = [];
page.on('dialog', async (d) => { nativeDialogs.push(d.message()); await d.dismiss(); });
await goto(page, demo, '/settings');
await page.waitForSelector('button.setting.danger');
await page.click('button.setting.danger');
const sheet = await until(() => !!document.querySelector('.sheet, [role=dialog]') && /sign out/i.test(document.body.innerText));
check('QA-7: sign out asks in the app’s own sheet, not a native dialog', sheet && nativeDialogs.length === 0, `native=${JSON.stringify(nativeDialogs)} sheet=${sheet}`);
// Cancel keeps the device signed in.
await page.evaluate(() => { const b = [...document.querySelectorAll('.sheet button, [role=dialog] button')].find((x) => /^cancel$/i.test(x.textContent.trim())); if (b) b.click(); });
await sleep(400);
check('QA-7: Cancel keeps this device signed in', (await page.evaluate(() => fetch('/api/me').then((r) => r.status))) === 200 && (await page.evaluate(() => location.pathname)) === '/settings');
// Confirming signs out and returns to pairing.
await page.click('button.setting.danger');
await until(() => /sign out/i.test(document.body.innerText) && !!document.querySelector('.sheet button.danger, [role=dialog] button.danger'));
await page.evaluate(() => document.querySelector('.sheet button.danger, [role=dialog] button.danger').click());
const toPair = await until(() => location.pathname === '/pair');
check('QA-7: confirming signs out and goes to pairing', toPair && nativeDialogs.length === 0, await page.evaluate(() => location.pathname));
page.removeAllListeners('dialog');
await login(page, demo); // back in for the checks that follow

// Losing the daemon shows it, readable. (CDP offline doesn't cut an open
// stream, so stop the devserver itself.)
execFileSync('pkill', ['-f', '--', `-addr 127.0.0.1:${new URL(demo.base).port}`]);
const sawBanner = await page.waitForFunction(() => { const b = document.querySelector('.banner'); return b.classList.contains('show') && b.getAttribute('aria-hidden') === 'false' && /Reconnecting|Can.t reach/.test(b.textContent); }, { timeout: 45000 }).then(() => true, () => false);
check('QA-1: daemon gone, the banner shows its message to assistive tech', sawBanner, await page.$eval('.banner', (b) => b.className + ' aria-hidden=' + b.getAttribute('aria-hidden') + ' ' + b.textContent));

await demo.stop();
process.exit(summary());
