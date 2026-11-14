// 11. Shell on the demo core: phone and desktop layouts, ⌘K palette,
// keyboard shortcuts and help, dark mode (system and manual), the rules
// page (presets, inbox rules, tester), and the service worker (precache,
// push → notification with actions, resolved → "Answered ✓", same tag).
//
//   PH_TMP=… PH_SHOTS=… node 11-demo-shell.mjs
import { startDemo, login, goto, watch, check, summary, sleep, shot, text, waitText, axeCheck, scheme, setSize, PHONE, DESKTOP } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
const logs = watch(page, 'shell');
await scheme(page, 'light');
await page.setViewport(DESKTOP);
await login(page, demo);
const visible = (sel) => page.$eval(sel, (e) => !!(e.offsetParent || e.getClientRects().length)).catch(() => false);
const path = () => page.evaluate(() => location.pathname);
const press = async (...keys) => { for (const k of keys) { await page.keyboard.press(k); await sleep(60); } };
/** Waits for a condition in the page instead of a fixed pause; false when it never holds. */
const until = (fn, ...args) => page.waitForFunction(fn, { timeout: 10000, polling: 50 }, ...args).then(() => true, () => false);

// ---------- Layouts ----------
check('desktop: sidebar, view and needs-you pane', await visible('.sidebar') && await visible('main') && await visible('.queue'));
check('desktop: no tab bar', !(await visible('.tabbar')));
check('desktop: the queue holds the four requests', (await page.$$('.queue .qcard')).length === 4);
check('desktop: Today lists them as rows and doesn’t repeat the cards', (await page.$$('main .g-need .nrow')).length === 4 && (await page.$$('main article.req')).length === 0);
check('desktop: sidebar lists sessions', (await page.$$('.sidebar .side-session')).length >= 8);
const sideGroups = await page.$$eval('.sidebar .side-group', (gs) => gs.map((g) => ({
  key: [...g.classList].find((c) => c !== 'side-group'),
  label: g.querySelector('.side-sub').firstChild.textContent,
  n: +g.querySelector('.side-sub b').textContent,
  rows: g.querySelectorAll('.side-session').length,
})));
const order = ['need', 'work', 'fin', 'idle', 'ended'];
check('desktop: sidebar groups sessions by status, in Today’s order', sideGroups.length >= 3 &&
  sideGroups.every((g, i) => i === 0 || order.indexOf(g.key) > order.indexOf(sideGroups[i - 1].key)), JSON.stringify(sideGroups));
check('desktop: each sidebar group counts its rows', sideGroups.every((g) => g.n === g.rows && g.rows > 0));
check('desktop: sidebar “Needs you” holds the sessions with the four requests', (sideGroups.find((g) => g.key === 'need') || {}).rows === 4, JSON.stringify(sideGroups));
await setSize(page, PHONE);
await until(() => document.querySelectorAll('main .g-need .nrow').length === 4 && !!document.querySelector('main article.req.ledger-dec'));
check('phone: tab bar with three tabs (Today, Rules, Settings), no side panes', await visible('.tabbar') && (await page.$$eval('.tabbar .tab', (t) => t.map((x) => x.innerText.replace(/\d+/g, '').trim()).join(','))) === 'Today,Rules,Settings' && !(await visible('.sidebar')) && !(await visible('.queue')));
check('phone: Today lists the four, with the oldest permission open in place', (await page.$$('main .g-need .nrow')).length === 4 && (await page.$$('main article.req.ledger-dec')).length === 1);
await setSize(page, DESKTOP);
// The shell (and with it the shortcuts) is back once the queue is.
await until(() => document.querySelectorAll('.queue .qcard').length === 4);

// ---------- Palette ----------
await page.keyboard.down('Control'); await page.keyboard.press('k'); await page.keyboard.up('Control');
await page.waitForSelector('dialog.palette[open]', { timeout: 3000 }).catch(() => {});
check('⌘K / Ctrl K opens the palette', await visible('dialog.palette[open]'));
check('palette: focus is in the search field', await page.evaluate(() => document.activeElement && document.activeElement.getAttribute('role') === 'combobox'));
await axeCheck(page, 'palette');
await page.keyboard.type('proof');
await until(() => { const o = document.querySelector('dialog.palette [aria-selected="true"]'); return !!o && o.innerText.includes('proofline'); });
const first = await text(page, 'dialog.palette [aria-selected="true"]');
check('palette: fuzzy match puts the session first', first.includes('proofline'), first);
await shot(page, 'h-palette-desktop-light');
await press('Enter');
await until(() => location.pathname === '/s/s_proofline' && !document.querySelector('dialog.palette[open]'));
check('palette: Enter jumps to the session', (await path()) === '/s/s_proofline', await path());
await press('/');
await page.waitForSelector('dialog.palette[open]');
await page.keyboard.type('git push');
await until(() => { const o = document.querySelector('dialog.palette [aria-selected="true"]'); return !!o && o.innerText.includes('git push origin main'); });
check('palette: open requests are searchable', (await text(page, 'dialog.palette [aria-selected="true"]')).includes('git push origin main'));
await press('Escape');
await until(() => !document.querySelector('dialog.palette[open]'));
check('palette: Esc closes it', !(await page.$('dialog.palette[open]')));

// ---------- Shortcuts ----------
await goto(page, demo, '/');
await press('j');
const sel1 = await page.$eval('.is-selected', (e) => e.dataset.request || e.dataset.session || '').catch(() => '');
check('j selects the first request in the queue', sel1 === 'r_gitpush03', sel1);
await press('j');
const sel2 = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
check('j moves to the next one', sel2 && sel2 !== sel1, sel2);
await press('k');
check('k moves back', (await page.$eval('.is-selected', (e) => e.dataset.request).catch(() => '')) === sel1);
await shot(page, 'h-keys-selected-desktop-light');
await press('a');
check('a once only asks to press it again', await waitText(page, 'Press a again', 3000));
await press('a');
check('a a allows the selected request', await waitText(page, 'Allowed', 3000, '.queue article[data-request="r_gitpush03"]'));
await sleep(1600);
const gp = await page.evaluate(() => fetch('/api/requests/r_gitpush03').then((r) => r.json()));
check('a sent a signed allow that the daemon accepted', gp.status === 'decided' && gp.decision.behavior === 'allow', gp.status);
// Question: select it and pick with number keys.
for (let i = 0; i < 6; i++) {
  const cur = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
  if (cur === 'r_question05') break;
  await press('j');
}
await press('1');
check('1 picks the first option of the selected question', (await page.$eval('.queue article[data-request="r_question05"] .opt', (b) => b.getAttribute('aria-checked'))) === 'true');
for (let i = 0; i < 6; i++) {
  const cur = await page.$eval('.is-selected', (e) => e.dataset.request || '').catch(() => '');
  if (cur === 'r_plan0006') break;
  await press('j');
}
await press('d');
check('d on a plan opens “Request changes” with the cursor in the feedback',
  await page.$eval('.queue article[data-request="r_plan0006"]', (c) => c.classList.contains('revising') && document.activeElement === c.querySelector('.revise-form textarea')).catch(() => false));
await press('Escape'); // closes the feedback form (handled by the textarea)
check('Esc in the feedback closes it', await page.$eval('.queue article[data-request="r_plan0006"]', (c) => !c.classList.contains('revising')).catch(() => false));
await page.evaluate(() => document.activeElement && document.activeElement.blur());
await press('Escape');
await press('?');
await page.waitForSelector('dialog.help[open]', { timeout: 2000 }).catch(() => {});
check('? opens the shortcuts help', await visible('dialog.help[open]'));
await axeCheck(page, 'help');
await shot(page, 'h-help-desktop-light');
await press('Escape');
await sleep(150);
await press('g', 's');
check('g s goes to Today (sessions live there now)', (await path()) === '/');
await press('g', 'r');
check('g r goes to rules', (await path()) === '/rules');
await press('n');
check('n starts a new task', (await path()) === '/new');
// r on a selected session (phone: j walks the list).
await setSize(page, PHONE);
await goto(page, demo, '/');
// j walks Today's rows; step past the requests to a session row.
let sid = '';
for (let i = 0; i < 12 && !sid; i++) {
  await press('j');
  sid = await page.$eval('.is-selected', (e) => e.dataset.session || '').catch(() => '');
}
await press('r');
await until(() => /^\/s\/s_\w+$/.test(location.pathname));
check('r on a selected row opens its session to reply', /^\/s\/s_\w+$/.test(await path()), `${sid} → ${await path()}`);
await setSize(page, DESKTOP);

// ---------- Dark mode ----------
await goto(page, demo, '/settings');
const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
await scheme(page, 'dark');
await sleep(100);
check('system dark: dark background', (await bg()) === 'rgb(18, 17, 14)', await bg());
await scheme(page, 'light');
await sleep(100);
check('system light: light background', (await bg()) === 'rgb(244, 240, 232)', await bg());
await page.click('[data-theme-mode="dark"]');
await sleep(100);
check('manual dark overrides a light system', (await bg()) === 'rgb(18, 17, 14)' && (await page.evaluate(() => document.documentElement.dataset.theme)) === 'dark');
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector('main .page-title');
check('the choice survives a reload (applied before first paint)', (await page.evaluate(() => document.documentElement.dataset.theme)) === 'dark' && (await bg()) === 'rgb(18, 17, 14)');
check('theme-color follows the choice', (await page.$$eval('meta[name="theme-color"]', (m) => m.map((x) => x.content))).every((c) => c === '#12110e'));
await shot(page, 'h-settings-desktop-manual-dark');
await page.click('[data-theme-mode="system"]');
await sleep(100);
check('System follows the device again', (await bg()) === 'rgb(244, 240, 232)' && !(await page.evaluate(() => document.documentElement.dataset.theme)));
await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
check('reduced motion: animations are off', (await page.evaluate(() => {
  const el = document.querySelector('.view');
  return parseFloat(getComputedStyle(el).animationDuration);
})) < 0.01);

// ---------- Rules page ----------
const signedRules = [];
page.on('response', (r) => { if (/\/api\/(rules|presets)\//.test(r.url()) && r.request().method() === 'POST') signedRules.push([r.url().replace(demo.base, ''), r.status()]); });
await goto(page, demo, '/rules');
await page.waitForSelector('#preset-protect-main .switch');
const pm = '#preset-protect-main .switch';
check('protect-main starts off', (await page.$eval(pm, (b) => b.getAttribute('aria-checked'))) === 'false');
await page.click(pm);
await sleep(600);
check('preset switch: signed POST succeeds', signedRules.some(([u, s]) => u === '/api/presets/protect-main' && s === 204), JSON.stringify(signedRules));
await page.reload({ waitUntil: 'domcontentloaded' });
await page.waitForSelector(pm);
check('preset switch: stays on after a reload', (await page.$eval(pm, (b) => b.getAttribute('aria-checked'))) === 'true');
const dock = '#rule-ask-docker-push .switch';
await page.click(dock);
await sleep(500);
check('inbox rule: switched on with a signed enable', signedRules.some(([u, s]) => u === '/api/rules/ask-docker-push/enable' && s === 204) && (await page.$eval(dock, (b) => b.getAttribute('aria-checked'))) === 'true');
check('read-only rules have no switch', !(await page.$('#rule-self-protection .switch')) && !(await page.$('#rule-ask-before-push .switch')));
await page.click('#rule-allow-go-test .icon-btn');
await page.waitForSelector('dialog.sheet[open]');
await axeCheck(page, 'confirm sheet');
await page.click('dialog.sheet[open] .btn.danger');
await sleep(700);
check('inbox rule: removed after confirming', !(await page.$('#rule-allow-go-test')) && signedRules.some(([u, s]) => u === '/api/rules/allow-go-test/remove' && s === 204));
await page.click('.tester input');
await page.keyboard.type('git push --force origin main');
await press('Enter');
await page.waitForSelector('.verdict');
const v1 = await text(page, '.verdict');
check('tester: a force-push to main is blocked once Protect main is on', v1.includes('Blocked') && v1.includes('protect-main-force'), v1.replace(/\s+/g, ' '));
await page.click('.examples button:nth-child(2)');
await sleep(300);
const v2 = await text(page, '.verdict');
check('tester: examples run (npm test → allowed by guarded autopilot)', v2.includes('Allowed'), v2.replace(/\s+/g, ' '));
await shot(page, 'h-rules-tester-desktop-light');
await axeCheck(page, 'rules desktop');
await page.goto(demo.base + '/rules#rule-protect-prod', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('#rule-protect-prod');
await sleep(300);
check('a card’s rule link lands on the rule', await page.$eval('#rule-protect-prod', (e) => e.classList.contains('is-selected')));
// A rule written by hand in policy.yaml can be named anything: a bare % in
// the address must not break the page, and a card's link encodes the name.
await page.goto(demo.base + '/rules#rule-100%-sure', { waitUntil: 'domcontentloaded' });
const pctOpened = await page.waitForSelector('#rule-protect-prod', { timeout: 10000 }).then(() => true, () => false);
await sleep(200);
check('a rule address with a bare % still opens the rules', pctOpened && !logs.some((l) => /URI/.test(l)), logs.filter((l) => /URI/.test(l)).join(' | '));
const ruleHref = await page.evaluate(async () => {
  const { requestCard } = await import('/js/card.js');
  const el = requestCard({ id: 'r_enc', session_id: 's_none', agent: 'claude', checkpoint: 'permission', tool: 'Bash', tool_kind: 'shell', command: 'ls', summary: 'Run: ls', status: 'open', policy_rule: 'ask 100% #1', created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 6e5).toISOString() }, { slot: 'enc', keep: true });
  const a = el.querySelector('a[href^="/rules#"]');
  return a && a.getAttribute('href');
});
check('a card’s rule link encodes the rule’s name', ruleHref === '/rules#rule-' + encodeURIComponent('ask 100% #1'), ruleHref);

// ---------- Service worker ----------
const swOK = await page.evaluate(async () => {
  const reg = await navigator.serviceWorker.ready;
  const keys = await caches.keys();
  const c = await caches.open('pilothouse-shell-v7');
  const n = (await c.keys()).length;
  return { active: !!reg.active, keys, n };
});
check('service worker: active, shell precached', swOK.active && swOK.keys.includes('pilothouse-shell-v7') && swOK.n >= 30, JSON.stringify(swOK));
const ctx = browser.defaultBrowserContext();
await ctx.overridePermissions(demo.base, ['notifications']);
const cdp = await page.createCDPSession();
const regs = [];
cdp.on('ServiceWorker.workerRegistrationUpdated', (e) => regs.push(...e.registrations));
await cdp.send('ServiceWorker.enable');
for (let i = 0; i < 100 && !regs.some((r) => r.scopeURL.startsWith(demo.base)); i++) await sleep(50);
const reg = regs.find((r) => r.scopeURL.startsWith(demo.base));
const push = (data) => cdp.send('ServiceWorker.deliverPushMessage', { origin: demo.base, registrationId: reg.registrationId, data: JSON.stringify(data) });
const notes = () => page.evaluate(async () => (await (await navigator.serviceWorker.ready).getNotifications()).map((n) => ({ tag: n.tag, title: n.title, body: n.body, actions: (n.actions || []).map((a) => a.action), data: n.data })));
// Reading the list while a notification is being put up makes Chrome forget
// that notification, so the suite reads only once the worker has finished
// what it was handed (`turn` in sw.js is its queue).
const swTarget = await browser.waitForTarget((t) => t.type() === 'service_worker' && t.url().startsWith(demo.base), { timeout: 10000 }).catch(() => null);
const worker = swTarget ? await swTarget.worker().catch(() => null) : null;
const workerIdle = () => (worker ? worker.evaluate('turn').then(() => {}, () => sleep(300)) : sleep(300));
/** The notifications once `ready(list)` holds; the last list read when it never does. */
const notesWhen = async (ready, ms = 10000) => {
  const end = Date.now() + ms;
  for (;;) {
    await workerIdle();
    const n = await notes();
    if (ready(n) || Date.now() > end) return n;
    await sleep(100);
  }
};
if (reg) {
  await push({ kind: 'request', title: 'pilothouse wants to run git push', body: 'git push origin main', tag: 'req-r_x1', url: '/r/r_x1', request: { id: 'r_x1', checkpoint: 'permission', summary: 'Run: git push origin main' }, actions: true });
  await push({ kind: 'request', title: 'notes asks 2 questions', body: 'Which search index?', tag: 'req-r_x2', url: '/r/r_x2', request: { id: 'r_x2', checkpoint: 'question' }, actions: true });
  let n = await notesWhen((l) => l.some((x) => x.tag === 'req-r_x1') && l.some((x) => x.tag === 'req-r_x2'));
  const perm = n.find((x) => x.tag === 'req-r_x1');
  const ques = n.find((x) => x.tag === 'req-r_x2');
  check('push: permission request shows Allow / Deny', perm && perm.actions.join() === 'allow,deny', JSON.stringify(perm));
  check('push: questions get no action buttons (they need the app)', ques && ques.actions.length === 0, JSON.stringify(ques));
  await push({ type: 'resolved', tag: 'req-r_x1', request_id: 'r_x1', status: 'decided', by: 'cli' });
  n = await notesWhen((l) => l.some((x) => x.tag === 'req-r_x1' && x.data && x.data.resolved));
  const done = n.filter((x) => x.tag === 'req-r_x1');
  check('push: resolved replaces it — same tag, “Answered ✓”, no buttons', done.length === 1 && done[0].body.startsWith('Answered ✓') && done[0].body.includes('at the computer') && done[0].actions.length === 0 && done[0].title.includes('git push'), JSON.stringify(done));
  // The daemon's own wording (internal/web/push.go resolvedPayload) wins.
  await push({ type: 'request', kind: 'request', title: 'infra wants to edit', body: 'Edit terraform/prod/main.tf', tag: 'req-r_x5', request_id: 'r_x5', request: { id: 'r_x5', checkpoint: 'permission', summary: 'Edit' }, actions: true });
  await notesWhen((l) => l.some((x) => x.tag === 'req-r_x5'));
  await push({ type: 'resolved', kind: 'resolved', title: '✓ Allowed', body: 'Edit terraform/prod/main.tf — from iPhone', tag: 'req-r_x5', url: '/r/r_x5', request_id: 'r_x5', status: 'decided', by: 'from iPhone' });
  n = await notesWhen((l) => l.some((x) => x.tag === 'req-r_x5' && x.data && x.data.resolved));
  const f5 = n.filter((x) => x.tag === 'req-r_x5');
  check('push: the daemon’s resolved text is shown as written', f5.length === 1 && f5[0].title === '✓ Allowed' && f5[0].body.endsWith('from iPhone') && f5[0].actions.length === 0, JSON.stringify(f5));
  // 0.1 servers tag with the bare id and nest nothing.
  await push({ kind: 'request', title: 'old', body: 'x', tag: 'r_x3', request_id: 'r_x3', actions: true });
  await notesWhen((l) => l.some((x) => x.tag === 'r_x3'));
  await push({ data: { type: 'resolved', request_id: 'r_x3', status: 'expired' } });
  n = await notesWhen((l) => l.some((x) => x.data && x.data.request_id === 'r_x3' && x.data.resolved));
  check('push: resolved finds a notification by request id and says what happened', n.some((x) => x.data && x.data.request_id === 'r_x3' && x.body.startsWith('Timed out')) && n.filter((x) => x.data && x.data.request_id === 'r_x3').length === 1, JSON.stringify(n.filter((x) => x.data && x.data.request_id === 'r_x3')));
  // A notification only ever opens this app: a link to somewhere else is dropped.
  await push({ kind: 'request', title: 'elsewhere', body: 'x', tag: 'req-r_x7', url: '//evil.example/x', request: { id: 'r_x7', checkpoint: 'permission' }, actions: true });
  await push({ kind: 'request', title: 'elsewhere', body: 'x', tag: 'req-r_x8', url: 'https://evil.example/r/r_x8', request_id: 'r_x8', actions: true });
  await push({ kind: 'request', title: 'here', body: 'x', tag: 'req-r_x9', url: '/s/s_proofline?from=push#now', request_id: 'r_x9', actions: true });
  n = await notesWhen((l) => ['req-r_x7', 'req-r_x8', 'req-r_x9'].every((t) => l.some((x) => x.tag === t)));
  const urls = ['req-r_x7', 'req-r_x8', 'req-r_x9'].map((t) => ((n.find((x) => x.tag === t) || {}).data || {}).url);
  check('push: a link to another site is dropped — the notification opens the request here', urls.join(' ') === '/r/r_x7 /r/r_x8 /s/s_proofline?from=push#now', JSON.stringify(urls));
  // Requests that arrive together each keep their notification. Three
  // rounds of six: before the worker took turns, a round lost one or two.
  const burst = [];
  for (let round = 0; round < 3; round++) {
    const tags = [1, 2, 3, 4, 5, 6].map((i) => `req-r_b${round}${i}`);
    burst.push(...tags);
    for (const t of tags) await push({ kind: 'request', title: 'together', body: 'x', tag: t, request_id: t.slice(4), actions: true });
    await sleep(700); // not read meanwhile: see workerIdle
    await workerIdle();
  }
  n = await notes();
  const lost = burst.filter((t) => !n.some((x) => x.tag === t));
  check('push: requests that arrive together each keep their notification', lost.length === 0, 'lost ' + lost.join(', '));
  // Answering in the app closes the request's notification, through the worker.
  await page.evaluate(async () => (await import('/js/push.js')).closeRequestNotifications('r_b01'));
  n = await notesWhen((l) => !l.some((x) => x.tag === 'req-r_b01'), 5000);
  check('answered in the app: its notification is closed, the others stay', !n.some((x) => x.tag === 'req-r_b01') && burst.filter((t) => n.some((x) => x.tag === t)).length === burst.length - 1 - lost.length, JSON.stringify(n.map((x) => x.tag)));
} else {
  check('push: service worker registration found over CDP', false, JSON.stringify(regs));
}

// What the service worker asks the page to open is checked again here.
const sent = await page.evaluate(async () => {
  const say = (url) => navigator.serviceWorker.dispatchEvent(new MessageEvent('message', { data: { type: 'navigate', url } }));
  const at = [];
  for (const url of ['//evil.example/x', '/\\evil.example/x', 'https://evil.example/', 'javascript:alert(1)']) { say(url); at.push(location.pathname); }
  say('/settings');
  await new Promise((r) => setTimeout(r, 100));
  at.push(location.pathname);
  return at;
});
check('a “navigate” message to another site is ignored; one to this app is followed', sent.join(' ') === '/rules /rules /rules /rules /settings' && !logs.some((l) => /SecurityError|pushState/.test(l)), JSON.stringify(sent) + ' ' + logs.filter((l) => /SecurityError|pushState/.test(l)).join(' | ').slice(0, 200));

// ---------- Keyboard focus after answering ----------
await goto(page, demo, '/');
await page.waitForSelector('.queue article.req .btn.deny', { timeout: 10000 });
const fid = await page.$eval('.queue article.req .btn.deny', (b) => { b.focus(); return b.closest('article.req').dataset.request; });
await press('Enter'); // opens "tell the agent why"
await page.keyboard.type('Not now');
await page.keyboard.down('Control'); await page.keyboard.press('Enter'); await page.keyboard.up('Control');
await waitText(page, 'Denied', 5000, `.queue article[data-request="${fid}"]`);
await sleep(120); // a frame or two: Chrome drops focus from a disabled control at the next frame
const where = () => page.evaluate(() => {
  const a = document.activeElement;
  if (!a || a === document.body) return 'body';
  const c = a.closest('article.req');
  return c ? 'card ' + c.dataset.request : a.tagName.toLowerCase();
});
const during = await where();
check('answering by keyboard keeps focus on the card while it shows the answer', during === 'card ' + fid, during);
await until((id) => !document.querySelector(`article[data-request="${id}"]`), fid);
await sleep(120);
const afterLeft = await where();
check('…and moves it to the next request (or the page) when the card leaves', afterLeft !== 'body' && afterLeft !== 'card ' + fid, afterLeft);

check('no console or page errors', logs.length === 0, logs.slice(0, 4).join(' | '));
await demo.stop();
process.exit(summary());
