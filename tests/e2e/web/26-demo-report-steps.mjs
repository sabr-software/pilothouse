// 26. The report's "How it got there" on the demo core, at desktop size:
// newest step first and the turn's first step at the bottom; a shell step the agent
// described reads in its words, its command shown only when opened; a
// line that would repeat the label is left out; markdown in the agent's
// narration reads as plain words.
//   node 26-demo-report-steps.mjs
import { startDemo, login, goto, check, summary, sleep, scheme, shot, DESKTOP } from './demo-lib.mjs';

const demo = await startDemo();
const browser = await demo.browser();
const page = await browser.newPage();
await page.setViewport(DESKTOP);
await scheme(page, 'light');
await login(page, demo);
await goto(page, demo, '/s/s_infra0006');
await page.waitForSelector('.voyage-wrap .voyage .vs', { timeout: 15000 });
await sleep(500);

const steps = () => page.$$eval('.voyage-wrap .voyage > .vs', (vs) => vs.map((v) => v.innerText.replace(/\s+/g, ' ').trim()));
let list = await steps();
const at = (s) => list.findIndex((x) => x.includes(s));
check('the turn’s first step is at the bottom', list[list.length - 1].startsWith('Explored the code'), list[list.length - 1]);
check('its latest step is on top', list[0].startsWith('Edited main.tf') && list[0].includes('terraform/prod'), list[0]);
check('newest first: validate above format above the first search', at('Validate the staging configuration') >= 0 &&
  at('Validate the staging configuration') < at('Check the Terraform formatting') && at('Check the Terraform formatting') < at('Explored the code'), JSON.stringify(list));
check('a described step reads in the agent’s words', at('Check the Terraform formatting') >= 0);
check('…its command isn’t shown until opened', !list.some((x) => x.includes('terraform fmt -check')));
check('narration markdown reads as plain words', list.some((x) => x.includes('Staging is on Node 22; next the deploy.yml workflow.')) && !list.some((x) => /\*\*|`/.test(x)), JSON.stringify(list.filter((x) => x.includes('Staging'))));
await shot(page, '26-report-steps');

// Open the formatting step: the command and its output appear.
await page.evaluate(() => [...document.querySelectorAll('.voyage-wrap .vbtn')].find((b) => b.innerText.includes('Check the Terraform formatting')).click());
await sleep(400);
list = await steps();
const fmt = list.find((x) => x.includes('Check the Terraform formatting')) || '';
check('opened, the step shows its command and output', fmt.includes('terraform fmt -check -recursive') && fmt.includes('(no changes)'), fmt);

await demo.stop();
process.exit(summary() ? 1 : 0);
