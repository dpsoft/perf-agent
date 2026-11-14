(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);
  const model = JSON.parse($('model').textContent);
  const frames = model.frames;
  const byId = new Map();
  frames.forEach((f, i) => byId.set(f.id, i));
  const chart = $('chart');
  const els = new Map();
  for (const el of chart.querySelectorAll('.frame')) els.set(Number(el.dataset.i), el);

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
  const factRows = (f) => {
    const parent = f.parent ? frames[byId.get(f.parent)] : undefined;
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

  let current = -1;
  const select = (i, scroll) => {
    const f = frames[i];
    if (!f) return;
    const prev = els.get(current);
    if (prev) prev.classList.remove('cur');
    current = i;
    const el = els.get(i);
    if (el) {
      el.classList.add('cur');
      if (scroll) el.scrollIntoView({block: 'nearest', inline: 'nearest'});
    }
    $('sel-ref').textContent = f.ref;
    $('sel-name').textContent = f.name;
    $('sel-id').textContent = f.id;

    const facts = $('facts');
    clear(facts);
    for (const r of factRows(f)) {
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

    for (const c of chart.querySelectorAll('.frame.cite')) c.classList.remove('cite');
    for (const b of exp.querySelectorAll('button.cite')) {
      const j = byId.get(b.dataset.node);
      const ce = els.get(j);
      if (ce && j !== i) ce.classList.add('cite');
    }

    const path = $('path');
    clear(path);
    const chain = [];
    for (let g = f; g && g.parent; g = frames[byId.get(g.parent)]) chain.push(g);
    chain.reverse();
    for (const g of chain) {
      const li = make('li');
      const b = make('button');
      b.type = 'button';
      b.dataset.node = g.id;
      b.appendChild(make('span', 'ref', g.ref));
      b.appendChild(make('span', 'pn', g.name));
      b.appendChild(make('span', 'c', g.share));
      li.appendChild(b);
      path.appendChild(li);
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

  let toastTimer = 0;
  const toast = (msg) => {
    const t = $('toast');
    t.textContent = msg;
    t.classList.add('on');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove('on'), 2200);
  };
  const fallback = (text) => {
    const box = $('copybox');
    box.value = text;
    box.hidden = false;
    box.focus();
    box.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch (e) {
      ok = false;
    }
    toast(ok ? 'Copied the prompt' : 'Press Ctrl+C to copy the selected prompt');
  };
  const copy = (text) => {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(() => toast('Copied the prompt'), () => fallback(text));
    } else {
      fallback(text);
    }
  };

  chart.addEventListener('click', (ev) => {
    const el = ev.target.closest('.frame');
    if (el) select(Number(el.dataset.i), false);
  });
  chart.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Enter' && ev.key !== ' ') return;
    const el = ev.target.closest('.frame');
    if (!el) return;
    ev.preventDefault();
    select(Number(el.dataset.i), false);
  });
  document.addEventListener('click', (ev) => {
    const chip = ev.target.closest('[data-prompt]');
    if (chip) {
      copy(chip.dataset.prompt);
      return;
    }
    const ref = ev.target.closest('[data-node]');
    if (ref && byId.has(ref.dataset.node)) select(byId.get(ref.dataset.node), true);
  });

  if (byId.has(model.selected)) select(byId.get(model.selected), false);
})();
