// perfAgentPanel builds the explained page's side panel. model is the
// page's model (fact_rows, prompts); hooks is {frameById(id), elementFor(id),
// onSelect(id)}: frameById returns a frame object or undefined, elementFor
// the drawn element of a frame or null, and onSelect is called with the id of
// a frame the viewer picked inside the panel. It returns {show(frame, path),
// factRows(f), explanation(parent, e), segments(parent, segs)}; path is the
// list of path items from the root's child down to the frame, each with at
// least id, ref, name and share.
function perfAgentPanel(model, hooks) {
  'use strict';
  const $ = (id) => document.getElementById(id);
  const chart = $('chart');

  const make = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  const clear = (e) => {
    while (e.firstChild) e.removeChild(e.firstChild);
  };

  const segments = (parent, segs) => {
    for (const s of segs || []) {
      if (s.kind === 'claim') {
        const e = make('span', 'claim', s.text);
        e.title = s.title;
        parent.appendChild(e);
      } else if (s.kind === 'node') {
        const b = make('button', 'cite', s.text);
        b.type = 'button';
        b.dataset.node = s.node;
        b.title = s.title;
        parent.appendChild(b);
        parent.appendChild(document.createTextNode(' '));
        parent.appendChild(make('code', '', s.name));
      } else if (s.kind === 'unchecked') {
        const m = make('mark', 'unchecked', s.text);
        m.title = s.title;
        parent.appendChild(m);
      } else {
        parent.appendChild(document.createTextNode(s.text));
      }
    }
  };

  const section = (cls, heading) => {
    const s = make('section', cls ? 'sec ' + cls : 'sec');
    s.appendChild(make('h5', '', heading));
    return s;
  };

  const explanation = (parent, e) => {
    const interp = section('interp', 'Agent interpretation — prose, not verified');
    const p = make('p');
    segments(p, e.prose);
    interp.appendChild(p);
    parent.appendChild(interp);
    if (e.hypotheses && e.hypotheses.length) {
      const hs = section('', "Hypotheses — the agent's, not measured");
      const ul = make('ul', 'hyp');
      for (const h of e.hypotheses) {
        const li = make('li');
        li.appendChild(make('b', '', 'Hypothesis:'));
        li.appendChild(document.createTextNode(' '));
        segments(li, h);
        ul.appendChild(li);
      }
      hs.appendChild(ul);
      parent.appendChild(hs);
    }
    if (e.next && e.next.length) {
      const ns = section('', "Next measurement — the agent's suggestion, not verified");
      const p2 = make('p');
      segments(p2, e.next);
      ns.appendChild(p2);
      parent.appendChild(ns);
    }
  };

  const maxPromptName = 200;
  const promptName = (name) => {
    const chars = Array.from(name);
    return chars.length > maxPromptName ? chars.slice(0, maxPromptName).join('') + '…' : name;
  };
  const prompt = (p, f) => p.parts.map((part) => {
    if (part.slot === 'id') return f.id;
    if (part.slot === 'name') return promptName(f.name);
    return part.s || '';
  }).join('');

  const exactTitle = (c, unit, den, rounding, zero) => {
    let s = c + ' ' + unit;
    if (den) s += ' of ' + den;
    if (rounding === 'exact') s += ', exact';
    else if (rounding) s += ', rounded to ' + rounding;
    if (zero) s += ', measured zero';
    return s;
  };
  const factRowsWith = (f, parent) => {
    const rows = model.fact_rows.map((spec, i) => {
      const val = f.facts[i];
      let note = spec.n || '';
      if (spec.pn) note = parent ? 'of ' + parent.name : '';
      let title = val.v;
      if (val.c) title = exactTitle(val.c, spec.u, spec.d === 'parent' ? 'parent:' + f.parent : spec.d, spec.r, val.z);
      return {k: spec.k, v: val.v, t: title, n: note};
    });
    return rows.concat(f.extra || []);
  };
  const factRows = (f) => factRowsWith(f, f.parent ? hooks.frameById(f.parent) : undefined);

  let currentEl = null;
  const show = (f, path) => {
    if (!f) return;
    if (currentEl) currentEl.classList.remove('cur');
    currentEl = hooks.elementFor(f.id) || null;
    if (currentEl) currentEl.classList.add('cur');
    $('sel-ref').textContent = f.ref;
    $('sel-name').textContent = f.name;
    $('sel-id').textContent = f.id;

    const parent = (f.parent && (hooks.frameById(f.parent) || (path || []).find((g) => g.id === f.parent))) || undefined;
    const facts = $('facts');
    clear(facts);
    for (const r of factRowsWith(f, parent)) {
      facts.appendChild(make('span', 'k', r.k));
      const v = make('span', 'v', r.v);
      v.title = r.t;
      facts.appendChild(v);
      facts.appendChild(make('span', 'p', r.n || ''));
    }

    const exp = $('exp');
    clear(exp);
    const list = f.explanation || [];
    for (const e of list) explanation(exp, e);
    $('noexp').hidden = list.length > 0;

    if (chart) for (const c of chart.querySelectorAll('.frame.cite')) c.classList.remove('cite');
    for (const b of exp.querySelectorAll('button.cite')) {
      const ce = hooks.elementFor(b.dataset.node);
      if (ce && b.dataset.node !== f.id) ce.classList.add('cite');
    }

    const pathEl = $('path');
    clear(pathEl);
    for (const g of path || []) {
      const li = make('li');
      const b = make('button');
      b.type = 'button';
      b.dataset.node = g.id;
      b.appendChild(make('span', 'ref', g.ref));
      b.appendChild(make('span', 'pn', g.name));
      b.appendChild(make('span', 'c', g.share));
      li.appendChild(b);
      pathEl.appendChild(li);
    }

    const chips = $('chips');
    clear(chips);
    for (const p of model.prompts) {
      const b = make('button', 'chip', p.label);
      b.type = 'button';
      b.dataset.prompt = prompt(p, f);
      chips.appendChild(b);
    }
    $('copybox').hidden = true;
  };

  // Frames picked inside the panel (a reference in the explanation or a
  // path entry) go to onSelect.
  const picked = (ev) => {
    const b = ev.target.closest('[data-node]');
    if (b) hooks.onSelect(b.dataset.node);
  };
  $('exp').addEventListener('click', picked);
  $('path').addEventListener('click', picked);

  return {show, factRows, explanation, segments};
}
