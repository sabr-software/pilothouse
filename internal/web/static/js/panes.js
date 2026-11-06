// The desktop side panes (≥ 1024 px): the session list in the sidebar and
// the needs-you queue on the right. Phones never render them.

import { h, icon, toMs, ago, sync, fill, repoName, stateLabel, agentClass } from './dom.js';
import { askTitle, terminalWords, WORKING, DONE } from './views/today.js';
import { store } from './api.js';
import { panes, link, isWide } from './ui.js';
import { requestCard, pruneCards, settling } from './card.js';
import { avatar, sessionLine, machineTag } from './views/rows.js';

const qcards = new Map(); // request id → cached pane card (keeps a typed deny reason)
const tcards = new Map(); // session id → cached card for a prompt in its terminal

/** The pane's card: who and what in Today's words, then the decision itself. */
function queueCard(r) {
  const s = store.sessions.get(r.session_id);
  const perm = r.checkpoint !== 'question' && r.checkpoint !== 'plan';
  let c = qcards.get(r.id);
  if (!c) {
    const head = h('div', { class: 'qhead' });
    const body = h('div', { class: 'qbody' });
    c = { el: h('section', { class: 'qcard' + (perm ? ' perm' : ''), 'data-key': r.id }, head, body), head, body };
    qcards.set(r.id, c);
  }
  fill(c.head, h('span', { class: 'ag ' + agentClass(r.agent), 'aria-hidden': 'true' }), h('b', null, repoName(s, r)), machineTag(s),
    s && s.branch ? h('span', { class: 'br' }, s.branch) : null, h('time', null, ago(r.created_at)));
  // Questions and plans keep their full card here: number keys pick options,
  // d asks for changes (palette.js).
  const card = requestCard(r, perm ? { slot: 'queue', variant: 'ledger', animate: false } : { slot: 'queue', animate: false });
  if (card.parentNode !== c.body) fill(c.body, h('p', { class: 'qask' }, askTitle(r)), card);
  else c.body.firstChild.textContent = askTitle(r);
  return c.el;
}

/**
 * A session waiting on a prompt only its terminal can answer. There is nothing
 * to decide here, but it does need a person, so the pane shows it and the
 * card opens the session. r is the prompt, when Pilothouse has its details.
 */
function terminalCard(s, r) {
  let c = tcards.get(s.id);
  if (!c) {
    const head = h('div', { class: 'qhead' });
    const body = h('div', { class: 'qbody' });
    c = { el: link('/s/' + s.id, { class: 'qcard terminal', 'data-key': s.id }, head, body), head, body };
    tcards.set(s.id, c);
  }
  const said = r ? r.command || r.summary || r.tool : s.state_detail;
  fill(c.head, h('span', { class: 'ag ' + agentClass(s.agent), 'aria-hidden': 'true' }), h('b', null, repoName(s)), machineTag(s),
    s.branch ? h('span', { class: 'br' }, s.branch) : null, h('time', null, ago(r ? r.created_at : s.updated_at)));
  fill(c.body, h('p', { class: 'qask' }, terminalWords(s)),
    said ? h('p', { class: 'qsaid' + (r && r.command ? ' mono' : '') }, said) : null,
    h('p', { class: 'qhint' }, 'Answer it on your computer.'));
  return c.el;
}

let emptyQueue = null;
let terminalHead = null;

/** Re-renders both panes for the current route. */
export function renderPanes(path) {
  if (!isWide() || !store.loaded) return;
  const p = panes();

  // Grouped by status, in Today's words and order: needs you, working,
  // finished (new since you looked), idle, then the last few that ended.
  const sessions = [...store.sessions.values()].sort((a, b) => toMs(b.updated_at) - toMs(a.updated_at));
  const live = sessions.filter((s) => s.state !== 'exited');
  const asking = new Set([...store.requests.values()]
    .filter((r) => r.status === 'open' || r.status === 'stepped_aside').map((r) => r.session_id));
  const need = live.filter((s) => asking.has(s.id) || s.state === 'blocked');
  const working = live.filter((s) => !need.includes(s) && (WORKING.has(s.state) || s.state === 'starting'));
  const finished = live.filter((s) => !need.includes(s) && DONE.has(s.state) && (s.unseen || s.state === 'errored'));
  const grouped = new Set([...need, ...working, ...finished]);
  const groups = [
    ['need', 'Needs you', need],
    ['work', 'Working', working],
    ['fin', 'Finished', finished],
    ['idle', 'Idle', live.filter((s) => !grouped.has(s))],
    ['ended', 'Ended', sessions.filter((s) => s.state === 'exited').slice(0, 3)],
  ].filter(([, , list]) => list.length);
  const current = (/^\/s\/([\w-]+)/.exec(path || '') || [])[1];
  const row = (s) => link('/s/' + s.id, {
    class: 'side-session', 'aria-current': s.id === current ? 'page' : null, title: `${repoName(s)} — ${stateLabel(s)}`,
  }, avatar(s), h('span', { class: 'txt' }, h('b', null, repoName(s), machineTag(s)), h('small', null, sessionLine(s))));
  fill(p.sessions, groups.map(([key, label, list]) => h('section', { class: 'side-group ' + key, 'aria-label': `${label}: ${list.length}` },
    h('h3', { class: 'side-sub ' + key }, label, h('b', null, String(list.length))), list.map(row))),
  sessions.length ? null : h('p', { class: 'quiet' }, 'No sessions yet'));

  const open = [...store.requests.values()]
    .filter((r) => r.status === 'open' || settling.has(r.id))
    .sort((a, b) => toMs(a.expires_at) - toMs(b.expires_at));
  // The rest of the sidebar's "Needs you": nothing to answer here, because the
  // prompt is in the terminal. Leaving them out said "Nothing needs you" next
  // to a session that was stuck.
  const answerable = new Set(open.map((r) => r.session_id));
  const terminal = need.filter((s) => !answerable.has(s.id));
  const prompts = new Map([...store.requests.values()].filter((r) => r.status === 'stepped_aside')
    .sort((a, b) => toMs(a.created_at) - toMs(b.created_at)).map((r) => [r.session_id, r]));
  const n = open.filter((r) => r.status === 'open').length + terminal.length;
  p.queueCount.hidden = n === 0;
  p.queueCount.textContent = String(n);
  if (!open.length && !terminal.length) {
    emptyQueue = emptyQueue || h('div', { class: 'queue-empty' }, icon('check'),
      h('b', null, 'Nothing needs you'),
      h('span', null, 'Requests, questions and plans from your agents land here.'));
    sync(p.queue, emptyQueue);
  } else {
    terminalHead = terminalHead || h('h3', { class: 'qsub' }, 'At the computer');
    sync(p.queue, open.map(queueCard), terminal.length ? terminalHead : null, terminal.map((s) => terminalCard(s, prompts.get(s.id))));
  }
  pruneCards(new Set(open.map((r) => r.id)), 'queue');
  for (const id of qcards.keys()) if (!open.some((r) => r.id === id)) qcards.delete(id);
  for (const id of tcards.keys()) if (!terminal.some((s) => s.id === id)) tcards.delete(id);
}
