// 34. A prompt only the terminal can answer, on the demo core at desktop
// size: the right pane lists it and counts it, so it agrees with the sidebar
// and never says "Nothing needs you" beside a stuck session. There are no
// decision buttons, the card opens the session, and it goes when answered.
// Today doesn't say "All clear" above it.
//   node 34-demo-terminal-prompts.mjs
import { startDemo, login, check, summary, sleep, shot, scheme, axeCheck, DESKTOP, PHONE } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
await page.setViewport(DESKTOP);
await scheme(page, 'dark');
await login(page, demo);
await page.waitForSelector('.queue .qcard', { timeout: 10000 });

// The live store, as events would leave it. fn runs in the page with it.
const set = async (fn, arg) => {
  await page.evaluate(async () => { window.__api = await import('/js/api.js'); });
  await page.evaluate(fn, arg);
  await page.evaluate(() => window.__api.changed());
  await sleep(350);
};
const pane = () => page.evaluate(() => ({
  cards: document.querySelectorAll('.queue .qcard').length,
  decisions: document.querySelectorAll('.queue .qcard:not(.terminal)').length,
  terminal: [...document.querySelectorAll('.queue .qcard.terminal')].map((c) => c.innerText),
  count: document.querySelector('.queue-head .count').hidden ? 0 : +document.querySelector('.queue-head .count').textContent,
  sub: !!document.querySelector('.queue .qsub'),
  empty: !!document.querySelector('.queue .queue-empty'),
  buttons: document.querySelectorAll('.queue .qcard.terminal button').length,
  side: +(document.querySelector('.sidebar .side-group.need .side-sub b') || { textContent: '0' }).textContent,
}));

let p = await pane();
check('no terminal prompts: the pane holds only decisions', p.terminal.length === 0 && !p.sub && p.decisions === 4 && p.count === 4, JSON.stringify(p));

const sid = await page.evaluate(async () => {
  const { store } = await import('/js/api.js');
  return [...store.sessions.values()].find((s) => s.state === 'waiting' || s.state === 'working').id;
});
const command = 'echo "now: $(date +%s)"; sqlite3 ~/.pilothouse/pilothouse.db "select id,status from requests where id in (1,2,3,4,5,6,7,8,9)"';

// Beside open decisions: listed after them, under its own label.
await set(({ sid, command }) => {
  const { store } = window.__api;
  const s = store.sessions.get(sid);
  Object.assign(s, { state: 'blocked', block_kind: 'permission', state_detail: 'Waiting for permission: Run: x' });
  store.requests.set('r_terminal1', { id: 'r_terminal1', session_id: sid, agent: s.agent, status: 'stepped_aside', route: 'terminal',
    checkpoint: 'permission', tool: 'Bash', tool_kind: 'shell', command, summary: 'Run: x', created_at: new Date().toISOString() });
}, { sid, command });
p = await pane();
check('a terminal prompt joins the pane, after the decisions', p.terminal.length === 1 && p.decisions === 4 && p.sub, JSON.stringify(p));
check('it says where to answer and shows the command', /Asking in its terminal/.test(p.terminal[0]) && p.terminal[0].includes('sqlite3') && /Answer it on your computer/.test(p.terminal[0]), p.terminal[0]);
check('it offers no decision', p.buttons === 0);
check('the pane counts it', p.count === 5, String(p.count));
const order = await page.$$eval('.queue .qbody, .queue .qsub', (e) => e.map((x) => x.className));
check('decisions come first', order.indexOf('qsub') === 4, order.join(','));
await shot(page, '34-pane-mixed');

// On its own, which is the screen that said "Nothing needs you".
const clearOthers = (keep) => {
  const { store } = window.__api;
  for (const [id, r] of store.requests) if (r.status === 'open') store.requests.delete(id);
  for (const s of store.sessions.values()) if (s.state === 'blocked' && s.id !== keep) Object.assign(s, { state: 'working', block_kind: '' });
};
await set(clearOthers, sid);
p = await pane();
check('alone: the pane doesn’t say nothing needs you', !p.empty && p.terminal.length === 1 && p.cards === 1, JSON.stringify(p));
check('alone: the pane and the sidebar agree', p.count === 1 && p.side === 1, JSON.stringify(p));
const today = () => page.evaluate(() => ({
  clear: !!document.querySelector('main .lclear'),
  need: !!document.querySelector('main .g-need'),
  desk: document.querySelectorAll('main .g-desk .nrow').length,
  counts: document.querySelector('main .need')?.textContent || '',
}));
let t = await today();
check('Today doesn’t say all clear above it', !t.clear && !t.need && t.desk === 1, JSON.stringify(t));
check('Today’s top line counts it', t.counts.startsWith('1 need you'), t.counts);
await shot(page, '34-pane-alone');
await axeCheck(page, 'pane with a terminal prompt');

// A blocked session Pilothouse has no request for: a notification only.
await set((sid) => {
  const { store } = window.__api;
  store.requests.delete('r_terminal1');
  store.sessions.get(sid).state_detail = 'Claude needs your permission to use Bash';
}, sid);
p = await pane();
check('with no request it still shows, in the agent’s words', p.terminal.length === 1 && p.terminal[0].includes('Claude needs your permission'), JSON.stringify(p.terminal));
await set((sid) => { window.__api.store.sessions.get(sid).block_kind = 'auth'; }, sid);
p = await pane();
check('a sign-in says so', /Needs you to sign in/.test(p.terminal[0]), p.terminal[0]);
await set((sid) => { window.__api.store.sessions.get(sid).block_kind = 'permission'; }, sid);

// The card opens the session.
await page.click('.queue .qcard.terminal');
await sleep(400);
check('the card opens the session', new URL(page.url()).pathname === '/s/' + sid, page.url());
check('…and it stays in the pane there', (await pane()).terminal.length === 1);

// Answered in the terminal: gone at once.
await set((sid) => { Object.assign(window.__api.store.sessions.get(sid), { state: 'working', block_kind: '' }); }, sid);
p = await pane();
check('once answered it leaves the pane', p.terminal.length === 0 && p.empty && p.count === 0 && !p.sub, JSON.stringify(p));
await page.goto(demo.base + '/', { waitUntil: 'domcontentloaded' });
await page.waitForSelector('.queue .qcard', { timeout: 10000 });
check('a clean reload is back to the demo’s four', (await pane()).decisions === 4);

// Phones have no pane; Today does the same job there.
await page.setViewport(PHONE);
await sleep(200);
await set(clearOthers, '');
t = await today();
check('nothing waiting: Today is all clear again', t.clear && t.need && t.desk === 0, JSON.stringify(t));

await demo.stop();
process.exit(summary() ? 1 : 0);
