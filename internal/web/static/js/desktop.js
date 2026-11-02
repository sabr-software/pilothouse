// "Open in herdr / Terminal": shows a session on the computer itself
// (internal/web/desktop.go). Only sessions Pilothouse holds the terminal
// of (started with `pilothouse run`, or as a task) can be opened.
//
//   GET  /api/desktop                → {target, label, machine} ({} when it can't)
//   POST /api/sessions/{id}/desktop  signed `desktop`, payload {}
//        → {target, label, where?, reused?, machine}

import { h, icon, fill } from './dom.js';
import { api, store } from './api.js';
import { toast } from './ui.js';
import { postAction, actionError, spinner } from './views/session.js';

const TTL = 15000;
let info = null;
let infoAt = 0;
let pending = null;
const waiting = new Set(); // update functions to call when info arrives

/** Where this computer would open a session; null until known. */
function desktopInfo() {
  if (info && Date.now() - infoAt < TTL) return info;
  if (!pending) {
    pending = api('GET', '/api/desktop')
      .then((d) => { info = d || {}; })
      .catch(() => { info = info || {}; })
      .finally(() => {
        infoAt = Date.now();
        pending = null;
        for (const f of waiting) f();
      });
  }
  return info;
}

/** "Sadats-Mac-Studio.attlocal.net" → "Sadats-Mac-Studio". */
function shortMachine(m) {
  return String(m).replace(/\.(local|lan|home|attlocal\.net|[a-z0-9-]+\.ts\.net)$/i, '');
}

/** True when s can be opened on the computer from here. */
function openable(s, d) {
  if (!s || !d || !d.target) return false;
  if (s.state === 'exited' || s.mode !== 'wrapped' || !(s.caps && s.caps.watch)) return false;
  // A session reported by another computer opens there, not here. Both
  // names come from the daemon, so they match for its own sessions.
  return !s.machine || !d.machine || s.machine === d.machine;
}

/**
 * A button that opens a session on the computer. variant 'tool' is the
 * compact terminal-bar button. Returns {el, update()}; el is hidden while
 * the session can't be opened.
 */
export function desktopButton(sessionId, { variant = 'button' } = {}) {
  let busy = false;
  const label = h('span');
  const btn = h('button', {
    class: variant === 'tool' ? 'term-tool desk-tool' : 'btn small secondary desk-btn', type: 'button', hidden: true,
  });
  const update = () => {
    const s = store.sessions.get(sessionId);
    const d = desktopInfo();
    const ok = openable(s, d);
    btn.hidden = !ok;
    if (!ok || busy) return;
    const where = d.machine ? ` on ${d.machine}` : '';
    btn.title = `Open this session in ${d.label}${where}`; // the full name, for hover
    label.textContent = variant === 'tool' ? d.label : `Open in ${d.label}`;
    // On a phone, say which computer: the tab opens there, not here.
    const on = variant === 'tool' || !d.machine ? null : h('span', { class: 'desk-on' }, 'on ' + shortMachine(d.machine));
    fill(btn, icon('monitor', 'sm'), label, on);
  };
  waiting.add(update);
  btn.addEventListener('click', async () => {
    if (busy) return;
    busy = true;
    btn.disabled = true;
    fill(btn, spinner('sm'), h('span', null, 'Opening…'));
    const res = await postAction(`/api/sessions/${encodeURIComponent(sessionId)}/desktop`, 'desktop', sessionId, {});
    busy = false;
    btn.disabled = false;
    if (res.ok) {
      const r = res.data || {};
      const on = r.machine ? ` on ${shortMachine(r.machine)}` : '';
      const ws = r.where ? ` · ${r.where}` : '';
      toast(r.reused ? `Already open in ${r.label}${on} — brought to the front` : `Opened in ${r.label}${ws}${on}`, 'ok');
    } else {
      toast(actionError(res, 'This computer can’t open sessions on its screen.'), 'bad');
      info = null; // herdr may have quit: ask again
    }
    update();
  });
  update();
  return { el: btn, update, destroy: () => waiting.delete(update) };
}
