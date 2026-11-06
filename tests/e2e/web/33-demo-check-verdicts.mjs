// 33. "Did it check its work?" on the session report, in each state: a plain
// verdict with the checks behind it, no stamps, and a reply it drafts but
// never sends. The receipt is swapped in the browser, so every state shows
// on the real page in both themes and at both sizes.
//   PH_SHOTS=/tmp/shots node 33-demo-check-verdicts.mjs
import { startDemo, login, goto, check, summary, sleep, scheme, shot, axeCheck, PHONE, DESKTOP } from './demo-lib.mjs';

const at = new Date(Date.now() - 4 * 60000).toISOString();
const run = (name, command, status, extra = {}) => ({ name, command, at, status, fresh: true, ...extra });
const tests = run('pnpm test', 'pnpm vitest run 2>&1 | tail -3', 'passed', { count: 61 });
const lint = run('pnpm lint', 'pnpm lint', 'passed');
const build = run('pnpm build', 'pnpm build', 'passed');
const base = { turn: 2, since: at, searches: 2, reads: 4, edits: 3, files: 2, added: 40, removed: 6, blocked: 0, failed: 0 };

const states = [
  { key: 'ok', tone: 'ok', rc: { ...base, tests, lint, build }, title: 'Checked after the last edit', says: '61 tests, lint and build passed on the latest code.', rows: 3 },
  { key: 'failed', tone: 'bad', rc: { ...base, tests: { ...tests, status: 'failed', count: 0, failed_before: 1 }, lint, build }, title: 'Tests failed', says: 'isn’t finished', rows: 3, ask: 'Ask it to fix this' },
  { key: 'stale', tone: 'warn', rc: { ...base, tests: { ...tests, fresh: false }, lint: { ...lint, fresh: false } }, title: 'Edited after the last check', says: 'the latest code wasn’t checked', rows: 2, ask: 'Ask it to re-check' },
  { key: 'unclear', tone: 'warn', rc: { ...base, tests: { ...tests, status: 'unclear', count: 0, command: 'pnpm test 2>&1 | grep coverage' } }, title: 'Result not shown', says: 'no proof it passed', rows: 1, ask: 'Ask it to re-check' },
  { key: 'none', tone: 'none', rc: { ...base }, title: 'Not checked', says: 'you only have its word', rows: 0, ask: 'Ask it to check' },
  { key: 'readonly', tone: 'none', rc: { ...base, edits: 0, files: 0, added: 0, removed: 0 }, title: 'Nothing changed', says: 'It only looked', rows: 0 },
];

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
// Answer every read of the session's activity with this state's receipt.
await page.evaluateOnNewDocument(() => {
  const real = window.fetch;
  window.fetch = async (...args) => {
    const res = await real(...args);
    const url = String(args[0] && args[0].url ? args[0].url : args[0]);
    const rc = sessionStorage.getItem('__rc');
    if (!rc || !/\/api\/sessions\/[^/]+\/activity/.test(url) || !res.ok) return res;
    const body = await res.json();
    body.receipt = JSON.parse(rc);
    const step = sessionStorage.getItem('__step');
    if (step) for (const it of body.items || []) if ((it.summary || '').includes('pnpm test')) it.check_status = step;
    return new Response(JSON.stringify(body), { status: res.status, headers: res.headers });
  };
});
await page.setViewport(DESKTOP);
await scheme(page, 'light');
await login(page, demo);

const session = '/s/s_proofline';
async function show(rc) {
  await page.evaluate((v) => sessionStorage.setItem('__rc', JSON.stringify(v)), rc);
  await goto(page, demo, session);
  await page.waitForSelector('.verdict', { timeout: 15000 });
  await sleep(250);
}
const read = () => page.evaluate(() => ({
  tone: [...document.querySelector('.verdict').classList].filter((c) => c !== 'verdict').join(' '),
  title: document.querySelector('.verdict .vh').textContent,
  detail: document.querySelector('.verdict .vd').textContent,
  label: [...document.querySelectorAll('.view.rep .lbl')].map((l) => l.textContent),
  rows: document.querySelectorAll('.rchk').length,
  button: (document.querySelector('.verdict button') || {}).textContent || '',
  stamps: document.querySelectorAll('.stampw, .stamps').length,
  note: !!document.querySelector('.checknote'),
}));

for (const s of states) {
  await show(s.rc);
  const v = await read();
  check(`${s.key}: says “${s.title}”`, v.title === s.title && v.tone === s.tone, JSON.stringify(v));
  check(`${s.key}: says what it means`, v.detail.includes(s.says), v.detail);
  check(`${s.key}: shows the checks behind it`, v.rows === s.rows && v.note === (s.rows > 0), JSON.stringify(v));
  check(`${s.key}: ${s.ask ? `offers “${s.ask}”` : 'offers nothing'}`, v.button === (s.ask || ''), v.button);
  check(`${s.key}: no stamps, and asks the question in words`, v.stamps === 0 && v.label.includes('Did it check its work?') && !v.label.includes('Proof'), JSON.stringify(v.label));
  await shot(page, `33-${s.key}-desktop-light`);
}

// A run that failed before and failed again doesn't claim it passed.
await show(states[1].rc);
const note = await page.$eval('.rchk .note', (n) => n.textContent);
check('a failed run’s note doesn’t say it passed', note === 'Also failed once before this run', note);

// A run that recovered still says so.
await show({ ...base, tests: { ...tests, failed_before: 2 }, lint, build });
const fixed = await page.$eval('.rchk .note', (n) => n.textContent);
check('a recovered run’s note says it passed', fixed === 'Failed 2 times first, then passed', fixed);

// The timeline says what the verdict says, not what the exit status says.
const stepsNow = () => page.$$eval('.voyage-wrap .voyage > .vs', (vs) => vs.map((v) => v.innerText.replace(/\s+/g, ' ').trim()));
await page.evaluate(() => sessionStorage.setItem('__step', 'failed'));
await show(states[1].rc);
await page.waitForSelector('.voyage-wrap .voyage .vs', { timeout: 15000 });
let timeline = await stepsNow();
check('a hidden failure reads as failed in the timeline', timeline.some((x) => x.startsWith('Tests failed')) && !timeline.some((x) => x.startsWith('Tests passed')), JSON.stringify(timeline));
await page.evaluate(() => sessionStorage.setItem('__step', 'unclear'));
await show(states[3].rc);
await page.waitForSelector('.voyage-wrap .voyage .vs', { timeout: 15000 });
timeline = await stepsNow();
check('an unconfirmed run reads that way in the timeline', timeline.some((x) => x.startsWith('Tests ran, result not shown')) && !timeline.some((x) => x.startsWith('Tests passed')), JSON.stringify(timeline));
await page.evaluate(() => sessionStorage.removeItem('__step'));

// Today and the decision page say the same as the report.
const hidden = { ...base, tests: { ...tests, status: 'unclear', count: 0 }, lint: { ...lint, status: 'unclear' } };
await page.evaluate((v) => sessionStorage.setItem('__rc', JSON.stringify(v)), hidden);
await goto(page, demo, '/');
await page.reload({ waitUntil: 'domcontentloaded' }); // Today keeps a receipt until its session changes
await page.waitForFunction(() => (document.querySelector('.lrow[data-session="s_taskfaq08"] .l2') || {}).textContent?.includes('tests'), { timeout: 8000 });
const todayLine = await page.$eval('.lrow[data-session="s_taskfaq08"] .l2', (l) => l.textContent);
check('Today marks an unconfirmed result', todayLine.includes('? tests') && todayLine.includes('? lint') && !todayLine.includes('✓'), todayLine);
const snap = await page.evaluate(() => fetch('/api/snapshot').then((r) => r.json()));
const open = snap.requests.find((r) => r.status === 'open' && r.checkpoint === 'permission');
check('the demo has an open permission request', !!open);
await goto(page, demo, '/r/' + open.id);
await page.waitForSelector('.chk', { timeout: 10000 });
const decision = await page.$$eval('.chk', (cs) => cs.map((c) => c.innerText.replace(/\s+/g, ' ').trim()));
check('the decision page says the result wasn’t shown', decision.some((c) => c.startsWith('Tests ran, result not shown')) && decision.some((c) => c.startsWith('Lint ran, result not shown')), JSON.stringify(decision));

// The button drafts a reply and sends nothing.
let posted = 0;
page.on('request', (r) => { if (r.method() === 'POST' && r.url().includes('/reply')) posted += 1; });
await show(states[2].rc);
await page.click('.verdict button');
await sleep(300);
const draft = await page.$eval('#reply-text', (t) => ({ value: t.value, focused: document.activeElement === t }));
const send = await page.$eval('.reply-send', (b) => b.disabled);
check('the button drafts a reply naming what to re-run', draft.value.includes('You changed code after tests and lint ran') && draft.focused, JSON.stringify(draft));
check('…ready to send, and not sent', !send && posted === 0, `disabled ${send} posted ${posted}`);
await page.$eval('#reply-text', (t) => { t.value = 'Also keep the old name.'; });
await page.click('.verdict button');
await sleep(200);
const kept = await page.$eval('#reply-text', (t) => t.value);
check('…and doesn’t overwrite what you’d typed', kept.startsWith('Also keep the old name.') && kept.includes('You changed code after'), kept);

// Every state reads well on a phone and in the dark, and passes axe.
await page.setViewport(PHONE);
for (const s of states) {
  await show(s.rc);
  const wide = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth);
  check(`${s.key}: fits a phone`, !wide);
  await shot(page, `33-${s.key}-phone-light`);
}
await scheme(page, 'dark');
await page.setViewport(DESKTOP);
for (const s of states) {
  await show(s.rc);
  await axeCheck(page, `${s.key} (dark)`);
  await shot(page, `33-${s.key}-desktop-dark`);
}
await scheme(page, 'light');
for (const s of states) {
  await show(s.rc);
  await axeCheck(page, `${s.key} (light)`);
}

await demo.stop();
process.exit(summary() ? 1 : 0);
