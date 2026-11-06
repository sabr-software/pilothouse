// What a turn's checks add up to, in plain words. The receipt's facts come
// from the commands themselves; this says what they mean for someone
// deciding whether the work is done, and what to do if it isn't.

const CHECKS = [['tests', 'Tests'], ['lint', 'Lint'], ['build', 'Build']];

/** A check's state: ok | bad | run | stale | unclear. */
export function checkState(c) {
  if (c.status === 'failed') return 'bad';
  if (c.status === 'running') return 'run';
  if (c.status === 'unclear') return 'unclear';
  return c.fresh ? 'ok' : 'stale';
}

export const CHECK_ICON = { ok: 'check', bad: 'x', run: 'hourglass', stale: 'alert', unclear: 'alert' };

/** A check's own line: "Tests passed · 61", "Lint ran before the last edit". */
export function checkTitle(name, c) {
  switch (checkState(c)) {
    case 'bad': return `${name} failed`;
    case 'run': return `${name} running`;
    case 'unclear': return `${name} ran, result not shown`;
    case 'stale': return `${name} ran before the last edit`;
    default: return `${name} passed` + (c.count ? ` · ${c.count}` : '');
  }
}

/** ['Tests', 'Lint', 'Build'] → "Tests, lint and build". */
function joined(names) {
  const low = names.map((n, i) => (i ? n.toLowerCase() : n));
  return low.length < 2 ? low.join('') : `${low.slice(0, -1).join(', ')} and ${low[low.length - 1]}`;
}

const lower = (s) => s.charAt(0).toLowerCase() + s.slice(1);
const count = (n, one, many) => `${n} ${n === 1 ? one : many}`;

/**
 * verdict(receipt, working) → { tone, icon, title, detail, ask } or null.
 * tone: ok | bad | run | warn | none. ask is { label, text }: a reply that
 * would settle it, offered once the agent has stopped. working: the agent
 * is still going, so what's missing may yet come.
 */
export function verdict(rc, working) {
  const ran = CHECKS.filter(([k]) => rc[k]).map(([k, name]) => ({ k, name, c: rc[k], state: checkState(rc[k]) }));
  const inState = (st) => ran.filter((x) => x.state === st);
  const names = (xs) => joined(xs.map((x) => x.name));
  const offer = (label, text) => (working ? null : { label, text });

  if (!ran.length) {
    if (rc.edits) {
      const files = rc.files ? count(rc.files, 'file', 'files') : 'files';
      return working
        ? { tone: 'none', icon: 'flask', title: 'Not checked yet', detail: `It has changed ${files} and run no tests, lint or build so far.`, ask: null }
        : { tone: 'none', icon: 'flask', title: 'Not checked', detail: `It changed ${files} but ran no tests, lint or build, so you only have its word that this works.`,
          ask: offer('Ask it to check', 'Run the tests, lint and build that apply to this change and show me the result.') };
    }
    if (!rc.searches && !rc.reads) return null;
    const looked = [rc.searches ? count(rc.searches, 'search', 'searches') : '', rc.reads ? count(rc.reads, 'read', 'reads') : ''].filter(Boolean).join(', ');
    return { tone: 'none', icon: 'search', title: 'Nothing changed', detail: `It only looked: ${looked}.`, ask: null };
  }

  const bad = inState('bad');
  if (bad.length) {
    const edited = bad.some((x) => !x.c.fresh);
    const detail = edited ? 'It changed code after that run without running it again, so any fix is unproven.'
      : working ? 'It’s still working, so this may change.' : 'The last run didn’t pass, so this isn’t finished.';
    return { tone: 'bad', icon: 'x', title: `${names(bad)} failed`, detail,
      ask: offer('Ask it to fix this', `${names(bad)} failed. Fix the cause, run ${lower(names(bad))} again and show me the result.`) };
  }
  const run = inState('run');
  if (run.length) return { tone: 'run', icon: 'hourglass', title: `${names(run)} running`, detail: 'The result will show here when it finishes.', ask: null };
  const stale = inState('stale');
  if (stale.length) {
    return { tone: 'warn', icon: 'alert', title: working ? 'Edited since the last check' : 'Edited after the last check',
      detail: working ? `${names(stale)} passed, but it has changed code since. It may run them again.`
        : `${names(stale)} passed, but it changed code afterwards, so the latest code wasn’t checked.`,
      ask: offer('Ask it to re-check', `You changed code after ${lower(names(stale))} ran. Run ${lower(names(stale))} again on the latest code and show me the result.`) };
  }
  const unclear = inState('unclear');
  if (unclear.length) {
    return { tone: 'warn', icon: 'alert', title: 'Result not shown',
      detail: `${names(unclear)} ran, but the output doesn’t show how it went, so there’s no proof it passed.`,
      ask: offer('Ask it to re-check', `Run ${lower(names(unclear))} again without trimming the output and show me the result.`) };
  }
  const said = joined(ran.map((x) => (x.k === 'tests' && x.c.count ? count(x.c.count, 'test', 'tests') : x.name)));
  if (!rc.edits) return { tone: 'ok', icon: 'check', title: 'Checks passed', detail: `${said} passed. No files changed this turn.`, ask: null };
  const skipped = CHECKS.filter(([k]) => !rc[k]).map(([, name]) => name);
  return { tone: 'ok', icon: 'check', title: 'Checked after the last edit',
    detail: `${said} passed on the latest code.` + (skipped.length ? ` ${joined(skipped)} didn’t run.` : ''), ask: null };
}
