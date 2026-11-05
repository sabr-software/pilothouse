// 28. "Allow for session" on the decision card, on the demo core, at
// desktop size: a real button on the compact card and the request page,
// saying what it covers; a rule that asks every time says so instead of
// offering it; Interrupt isn't next to a decision, only while working.
//   node 28-demo-session-grant.mjs
import { startDemo, login, goto, check, summary, sleep, shot, scheme, DESKTOP } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
await page.setViewport(DESKTOP);
await scheme(page, 'light');
await login(page, demo);

// A request with no rule behind it, drawn by the app's own card.
const card = (req, variant) => page.evaluate(async (req, variant) => {
  const { requestCard } = await import('/js/card.js');
  const el = requestCard({ id: 'r_test' + variant, session_id: 's_infra0006', agent: 'claude', checkpoint: 'permission', status: 'open', route: 'devices',
    tool: 'Bash', tool_kind: 'shell', summary: 'Run: x', created_at: new Date().toISOString(), expires_at: new Date(Date.now() + 6e5).toISOString(), ...req },
  { slot: 'test' + variant, variant, animate: false });
  const box = document.createElement('div');
  box.append(el);
  document.body.append(box);
  const btn = el.querySelector('.dec-session');
  const out = { hidden: !btn || btn.hidden, label: btn && btn.textContent, covers: el.querySelector('.dec-covers').textContent, cls: btn && btn.className };
  box.remove();
  return out;
}, req, variant);

let c = await card({ command: 'git status; sed -n 1,9p a.ts', grant_covers: ['git status', 'reading files in this folder'] }, 'ledger');
check('the compact card shows Allow for session as a button', !c.hidden && c.label === 'Allow for session' && /\bbtn\b/.test(c.cls), JSON.stringify(c));
check('…and says what it covers', c.covers === 'Also allows git status, reading files in this folder until this session ends.', c.covers);
c = await card({ command: 'x', grant_covers: ['git status', 'a', 'b', 'c', 'd', 'e'] }, 'decision');
check('the request page too, with a long list shortened', !c.hidden && c.covers.includes('and 2 more'), c.covers);
c = await card({ tool: 'Edit', tool_kind: 'edit', grant_covers: ['every Edit call'] }, 'ledger');
check('a file tool says every call', c.covers.includes('every Edit call'), c.covers);
c = await card({ command: 'cat > a.txt', grant_covers: [] }, 'ledger');
check('a command that can’t be granted says so, with no button', c.hidden && c.covers.includes('can’t be allowed for the session'), JSON.stringify(c));
c = await card({ command: 'git push', policy_rule: 'ask-before-push', grant_covers: ['git push'] }, 'ledger');
check('a rule that always asks says so, with no button', c.hidden && c.covers.includes('Your rule ask-before-push asks every time'), JSON.stringify(c));

// On a real card in the app.
await goto(page, demo, '/s/s_infra0006');
await page.waitForSelector('.rnow.need .dec-covers', { timeout: 10000 });
check('the session page explains the rule on its card', (await page.$eval('.rnow.need .dec-covers', (e) => e.textContent)).includes('protect-prod'));
check('no Interrupt next to a decision', !(await page.$('.rnow.need #interrupt-btn')));
await shot(page, '28-session-card');

await demo.stop();
process.exit(summary() ? 1 : 0);
