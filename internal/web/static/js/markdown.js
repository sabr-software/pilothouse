// A small, safe Markdown renderer for agent plans and messages. It builds
// DOM nodes from text — never innerHTML — so nothing an agent writes can
// become markup, script or a live link.
//
// Supported: ATX headings, paragraphs, - * + and 1. lists (nested by
// indent, [ ] / [x] task items), > quotes, ``` / ~~~ code blocks with a
// language label, --- rules, pipe tables, **strong**, *em* / _em_,
// ~~del~~, `code`, and links/autolinks shown as text: "label (url)".
//
// Exported API (stable; workstream I may use it for agent text):
//   renderMarkdown(text, opts?) → <div class="md">
//     opts.baseLevel      the HTML level of the text's top heading level (default 3,
//                         for text inside a card under a page h1 and a section h2)
//     opts.dropTitle      drop a leading level-1 heading (shown elsewhere)
//   inlineMarkdown(text) → nodes[] (inline formatting only)
//   markdownTitle(text) → the leading "# Title", or ''

import { h, icon, add } from './dom.js';

const MAX_INPUT = 200000;

// What a regex's \s matches. charCodeAt past the end is NaN: not a space.
function isSpace(c) {
  return c === 32 || (c >= 9 && c <= 13) || c === 0xa0 || c === 0x1680 || (c >= 0x2000 && c <= 0x200a) ||
    c === 0x2028 || c === 0x2029 || c === 0x202f || c === 0x205f || c === 0x3000 || c === 0xfeff;
}

/**
 * An ATX heading line ("## Text ##") → {level, text}, or null. Read by
 * hand, like everything an agent's text can make slow: a regex with a
 * lazy middle and optional spaces around it took seconds on a heading
 * with a long run of spaces.
 */
function heading(line) {
  let i = 0;
  while (i < 3 && isSpace(line.charCodeAt(i))) i++;
  let level = 0;
  while (line.charCodeAt(i + level) === 35) level++;
  if (level < 1 || level > 6 || !isSpace(line.charCodeAt(i + level))) return null;
  let a = i + level;
  while (isSpace(line.charCodeAt(a))) a++;
  // Without the closing "##" and the spaces around it.
  let b = line.length;
  while (b > a && isSpace(line.charCodeAt(b - 1))) b--;
  while (b > a && line.charCodeAt(b - 1) === 35) b--;
  while (b > a && isSpace(line.charCodeAt(b - 1))) b--;
  return { level, text: line.slice(a, b) };
}

/** The text of a leading level-1 heading, or ''. */
export function markdownTitle(text) {
  const src = String(text || '');
  const from = src.search(/\S/); // the first line with anything on it
  if (from < 0) return '';
  let to = from;
  for (let c; to < src.length && (c = src.charCodeAt(to)) !== 10 && c !== 13; to++);
  const head = heading(src.slice(from, to));
  return head && head.level === 1 ? head.text : '';
}

// ---------- Inline ----------
//
// Inline tokens. The earliest one wins; at the same place, the first in
// this order: `code`, [link](url), <autolink>, **strong**, __strong__,
// ~~del~~, *em*, _em_.
//
// An agent writes this text, so the scan has to stay quick whatever it
// holds: a long run of openers that never close ("`a`[b `a`[b …") must
// not freeze the page that asks for an answer. So no regex is run over
// "the rest of the string" again and again. Each kind has a finder that
// walks forward by hand, keeps the match it found until the scan has
// passed it, and remembers when nothing further can match. A whole
// paragraph costs about one pass per kind.

/** Longer inline text than this is shown as written (a safety valve). */
const MAX_INLINE = 20000;

// charCodeAt past the end is NaN (c !== c).
const notSpace = (c) => c === c && !isSpace(c);
const notSpaceOrStar = (c) => c !== 42 && notSpace(c);
const opensWord = (c) => c === 40 || isSpace(c); // "(" or a space before _ and __
const endsWord = (c) => c !== c || isSpace(c) || (c < 128 && '.,;:!?)'.includes(String.fromCharCode(c)));

/**
 * Wraps find(from) → {at, end, a, b} | null, where null means "nothing
 * from here to the end". The match is kept while the scan is before it.
 */
function finder(find) {
  let hit; // undefined: not looked yet; null: nothing left
  return (pos) => {
    if (hit === null) return null;
    if (hit === undefined || hit.at < pos) hit = find(pos);
    return hit;
  };
}

/** `code`: no backtick or line break inside. */
function codeFinder(s) {
  let nl = -1;
  return finder((pos) => {
    let i = s.indexOf('`', pos);
    while (i >= 0) {
      const j = s.indexOf('`', i + 1);
      if (j < 0) return null;
      if (nl <= i) { nl = s.indexOf('\n', i + 1); if (nl < 0) nl = s.length; }
      if (j > i + 1 && nl > j) return { at: i, end: j + 1, a: i + 1, b: j };
      i = j;
    }
    return null;
  });
}

/** [label](url), [label](<url>), [label](url "title"). */
function linkFinder(s) {
  const at = (k) => s.charCodeAt(k);
  const inUrl = (c) => c === c && c !== 41 && c !== 62 && !isSpace(c); // not ) > or a space
  let nl = -1; // the next line break and "]", looked up once per stretch
  let close = -1;
  // URLs starting in [deadLo, deadHi) run to the same end and lead nowhere.
  let deadLo = -1;
  let deadHi = -1;
  let noQuote = false; // no closing " further on
  /** What follows "](": the URL, an optional "title" and ")". */
  function tail(k) {
    while (isSpace(at(k))) k++;
    const u = at(k) === 60 && inUrl(at(k + 1)) ? k + 1 : k; // "<" before the URL
    if (u >= deadLo && u < deadHi) return null;
    let e = u;
    while (inUrl(at(e))) e++;
    if (e === u) return null;
    const p = at(e) === 62 ? e + 1 : e;
    let q = p;
    while (isSpace(at(q))) q++;
    if (at(q) === 41) return { url: s.slice(u, e), end: q + 1 };
    if (q > p && at(q) === 34 && !noQuote) {
      const z = s.indexOf('"', q + 1);
      if (z < 0) {
        noQuote = true;
      } else {
        let r = z + 1;
        while (isSpace(at(r))) r++;
        if (at(r) === 41) return { url: s.slice(u, e), end: r + 1 };
      }
    }
    deadLo = u;
    deadHi = e;
    return null;
  }
  return finder((pos) => {
    let i = s.indexOf('[', pos);
    while (i >= 0) {
      if (close <= i) close = s.indexOf(']', i + 1);
      const j = close;
      if (j < 0) return null;
      if (nl <= i) { nl = s.indexOf('\n', i + 1); if (nl < 0) nl = s.length; }
      if (nl < j) { i = s.indexOf('[', nl + 1); continue; }
      if (j > i + 1 && at(j + 1) === 40) {
        const t = tail(j + 2);
        if (t) return { at: i, end: t.end, a: i + 1, b: j, url: t.url };
      }
      // Every "[" before this "]" would end at it too.
      i = s.indexOf('[', j + 1);
    }
    return null;
  });
}

const SCHEME_RE = /(?:https?|mailto):/y;

/** <https://…> and <mailto:…>. */
function autoFinder(s) {
  return finder((pos) => {
    let i = s.indexOf('<', pos);
    while (i >= 0) {
      let k = i + 1;
      SCHEME_RE.lastIndex = k;
      if (SCHEME_RE.test(s)) {
        const from = SCHEME_RE.lastIndex;
        k = from;
        let c;
        while ((c = s.charCodeAt(k)) === c && c !== 62 && !isSpace(c)) k++;
        if (k > from && c === 62) return { at: i, end: k + 1, a: i + 1, b: k };
        // Any "<" before k would run to the same end.
      }
      i = s.indexOf('<', k);
    }
    return null;
  });
}

/** "No span opens after here", kept like a match that is never reached. */
const NOT_YET = { at: Infinity };

/**
 * A span between two marks d: **strong**, ~~del~~, *em*, and __strong__ /
 * _em_ (word: true — these need a space or "(" before and a space,
 * punctuation or the end after, so snake_case is left alone; right after
 * another token counts as a start too). The text can't start or end with
 * a space: inner(c) says what may sit next to the marks.
 */
function spanFinder(s, d, inner, word) {
  const L = d.length;
  const at = (k) => s.charCodeAt(k);
  let closers = null; // where a closing mark may sit, in order
  function closerFrom(min) {
    if (!closers) {
      closers = [];
      for (let j = s.indexOf(d, 1); j >= 0; j = s.indexOf(d, j + 1)) {
        if (inner(at(j - 1)) && (!word || endsWord(at(j + L)))) closers.push(j);
      }
    }
    let lo = 0;
    let hi = closers.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (closers[mid] < min) lo = mid + 1; else hi = mid;
    }
    return lo < closers.length ? closers[lo] : -1;
  }
  /** The span opening at i; null: no later opener can close either. */
  function open(i) {
    const c = closerFrom(i + L + 1);
    return c < 0 ? null : { at: i, end: c + L, a: i + L, b: c };
  }
  function find(pos) {
    for (let i = s.indexOf(d, pos); i >= 0; i = s.indexOf(d, i + 1)) {
      if (!inner(at(i + L)) || (word && i !== pos && !opensWord(at(i - 1)))) continue;
      const m = open(i);
      if (m || !word || i === pos) return m;
      break;
    }
    // A word mark before the ones tried can still open a span, once the
    // scan stops right at it (after another token).
    return word ? NOT_YET : null;
  }
  if (!word) return finder(find);
  let hit;
  return (pos) => {
    if (hit === null) return null;
    if (hit === undefined || hit.at < pos) hit = find(pos);
    // A mark right here opens a span whatever is before it.
    else if (hit.at > pos && s.startsWith(d, pos) && inner(at(pos + L))) hit = open(pos);
    return hit === NOT_YET ? null : hit;
  };
}

const INLINE = [
  ['code', codeFinder],
  ['link', linkFinder],
  ['auto', autoFinder],
  ['strong', (s) => spanFinder(s, '**', notSpace, false)],
  ['strong', (s) => spanFinder(s, '__', notSpace, true)],
  ['del', (s) => spanFinder(s, '~~', notSpace, false)],
  ['em', (s) => spanFinder(s, '*', notSpaceOrStar, false)],
  ['em', (s) => spanFinder(s, '_', notSpace, true)],
];

/** Inline formatting as nodes. */
export function inlineMarkdown(text, depth = 0) {
  const s = String(text || '');
  if (!s) return [];
  if (depth > 6 || s.length > MAX_INLINE) return [s];
  const out = [];
  const finders = INLINE.map(([kind, make]) => [kind, make(s)]);
  let pos = 0;
  while (pos < s.length) {
    let best = null;
    let kind = '';
    for (const [k, next] of finders) {
      const m = next(pos);
      if (m && (!best || m.at < best.at)) { best = m; kind = k; }
    }
    if (!best) break;
    if (best.at > pos) out.push(s.slice(pos, best.at));
    const inner = s.slice(best.a, best.b);
    switch (kind) {
      case 'code':
        out.push(h('code', null, inner));
        break;
      case 'link':
        out.push(h('span', { class: 'md-link' }, inlineMarkdown(inner, depth + 1)));
        if (best.url !== inner) out.push(' ', h('span', { class: 'md-url' }, '(' + best.url + ')'));
        break;
      case 'auto':
        out.push(h('span', { class: 'md-url' }, inner));
        break;
      case 'strong':
        out.push(h('strong', null, inlineMarkdown(inner, depth + 1)));
        break;
      case 'del':
        out.push(h('del', null, inlineMarkdown(inner, depth + 1)));
        break;
      case 'em':
        out.push(h('em', null, inlineMarkdown(inner, depth + 1)));
        break;
    }
    pos = best.end;
  }
  if (pos < s.length) out.push(s.slice(pos));
  return out;
}

const LIST_RE = /^(\s*)([-*+]|\d{1,9}[.)])\s+(.*)$/;
const FENCE_RE = /^\s{0,3}(`{3,}|~{3,})\s*([\w+#.-]*)/;
const HR_RE = /^\s{0,3}([-*_])(\s*\1){2,}\s*$/;
const TABLE_DASH_RE = /^:?-{2,}:?$/;

function splitRow(line) {
  let l = line.trim();
  if (l.startsWith('|')) l = l.slice(1);
  if (l.endsWith('|')) l = l.slice(0, -1);
  return l.split('|').map((c) => c.trim());
}

/** The line under a table's head: "|---|:--:|" (cells of dashes). */
function isTableSep(line) {
  return line.includes('-') && splitRow(line).every((c) => TABLE_DASH_RE.test(c));
}

function indentOf(s) {
  let n = 0;
  for (const c of s) {
    if (c === ' ') n++;
    else if (c === '\t') n += 4;
    else break;
  }
  return n;
}

function blocks(lines, opts, depth) {
  const out = [];
  let i = 0;
  const offset = opts.offset;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    const fence = FENCE_RE.exec(line);
    if (fence) {
      const marker = fence[1];
      const body = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith(marker)) body.push(lines[i++]);
      i++; // closing fence (or end)
      const pre = h('pre', { tabindex: '0' });
      if (fence[2]) add(pre, h('span', { class: 'lang' }, fence[2]));
      add(pre, h('code', null, body.join('\n')));
      out.push(pre);
      continue;
    }

    const head = heading(line);
    if (head) {
      // Never above the level this text starts at: a heading that only
      // appears inside a quote or a list isn't counted in the offset.
      const level = Math.max(opts.base, Math.min(6, head.level + offset));
      out.push(h('h' + level, null, inlineMarkdown(head.text)));
      i++;
      continue;
    }

    if (HR_RE.test(line)) {
      out.push(h('hr'));
      i++;
      continue;
    }

    if (/^\s{0,3}>/.test(line)) {
      const body = [];
      while (i < lines.length && /^\s{0,3}>/.test(lines[i])) body.push(lines[i++].replace(/^\s{0,3}>\s?/, ''));
      out.push(h('blockquote', null, depth < 4 ? blocks(body, opts, depth + 1) : body.join(' ')));
      continue;
    }

    if (line.includes('|') && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const headCells = splitRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && lines[i].includes('|') && lines[i].trim()) rows.push(splitRow(lines[i++]));
      out.push(h('table', null,
        h('thead', null, h('tr', null, headCells.map((c) => h('th', { scope: 'col' }, inlineMarkdown(c))))),
        h('tbody', null, rows.map((r) => h('tr', null, headCells.map((_, j) => h('td', null, inlineMarkdown(r[j] || ''))))))));
      continue;
    }

    if (LIST_RE.test(line)) {
      const [el, next] = list(lines, i, opts, depth);
      out.push(el);
      i = next;
      continue;
    }

    // Paragraph: until a blank line or another block starts.
    const para = [];
    while (i < lines.length && lines[i].trim() && !FENCE_RE.test(lines[i]) && !heading(lines[i]) &&
      !/^\s{0,3}>/.test(lines[i]) && !(para.length && LIST_RE.test(lines[i])) && !(para.length && HR_RE.test(lines[i]))) {
      para.push(lines[i++].trim());
    }
    if (!para.length) { para.push(lines[i++].trim()); }
    out.push(h('p', null, inlineMarkdown(para.join(' '))));
  }
  return out;
}

function list(lines, start, opts, depth) {
  const first = LIST_RE.exec(lines[start]);
  const base = indentOf(first[1]);
  const ordered = /\d/.test(first[2]);
  const el = h(ordered ? 'ol' : 'ul');
  if (ordered && parseInt(first[2], 10) !== 1) el.setAttribute('start', String(parseInt(first[2], 10)));
  let i = start;
  while (i < lines.length) {
    const m = LIST_RE.exec(lines[i]);
    if (!m || indentOf(m[1]) !== base || /\d/.test(m[2]) !== ordered) break;
    const body = [m[3]];
    i++;
    // Continuation and nested lines: indented deeper than the marker.
    while (i < lines.length) {
      const l = lines[i];
      if (!l.trim()) {
        const nextIdx = i + 1;
        if (nextIdx < lines.length && indentOf(lines[nextIdx]) > base && lines[nextIdx].trim()) { body.push(''); i++; continue; }
        break;
      }
      const lm = LIST_RE.exec(l);
      if (indentOf(l) <= base && (lm || !body.length)) break;
      if (indentOf(l) <= base && !lm) {
        body[body.length - 1] += ' ' + l.trim();
        i++;
        continue;
      }
      body.push(l.slice(Math.min(indentOf(l), base + 2)));
      i++;
    }
    let text = body[0];
    let task = null;
    const t = /^\[( |x|X)\]\s+(.*)$/.exec(text);
    if (t) { task = t[1] !== ' '; text = t[2]; }
    const li = h('li', task === null ? null : { class: 'task' + (task ? ' done' : '') });
    if (task !== null) add(li, h('span', { class: 'box', 'aria-hidden': 'true' }, task ? icon('check') : null), h('span', { class: 'sr-only' }, task ? 'Done: ' : 'To do: '));
    const rest = body.slice(1);
    if (rest.some((l) => l.trim())) {
      const nested = depth < 6 ? blocks(rest, opts, depth + 1) : [rest.join(' ')];
      add(li, task !== null ? h('span', null, inlineMarkdown(text), nested) : [inlineMarkdown(text), nested]);
    } else {
      add(li, task !== null ? h('span', null, inlineMarkdown(text)) : inlineMarkdown(text));
    }
    add(el, li);
  }
  return [el, i];
}

/** Renders Markdown into a <div class="md">. */
export function renderMarkdown(text, opts = {}) {
  let src = String(text || '');
  if (src.length > MAX_INPUT) src = src.slice(0, MAX_INPUT) + '\n\n…';
  let lines = src.replace(/\r\n?/g, '\n').split('\n');
  if (opts.dropTitle) {
    const k = lines.findIndex((l) => l.trim());
    if (k >= 0 && /^\s*#\s+/.test(lines[k])) lines = lines.slice(k + 1);
  }
  // Map the text's own top level to baseLevel, so headings stay in order
  // within the page (a plan's "##" under the card is not an h2).
  let top = 6;
  let fenced = false;
  for (const l of lines) {
    if (FENCE_RE.test(l)) fenced = !fenced;
    const m = !fenced && heading(l);
    if (m) top = Math.min(top, m.level);
  }
  const base = Math.max(1, Math.min(6, Math.floor(Number(opts.baseLevel)) || 3));
  const o = { ...opts, base, offset: base - top };
  return h('div', { class: 'md' }, blocks(lines, o, 0));
}
