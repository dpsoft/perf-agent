// The live page. It reads the state the agent drives (the event stream, and
// GET state when the stream is down), fetches the shown publication's model
// and rejected history, and draws them with the shared panel. Every request
// is a relative GET. The only received strings that become part of a request
// path are publication and node IDs, and only when they have their fixed
// shapes. Every figure comes from the server: this script formats no number.
(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);

  const transport = {
    getJSON: async (path) => {
      const r = await fetch(path, {cache: 'no-store'});
      if (!r.ok) throw new Error('request refused: ' + path);
      return r.json();
    },
    // subscribe opens the event stream. onState gets each state; onTrouble
    // gets true when the browser gave the stream up for good (a refusal,
    // such as too many open tabs) and false while it reconnects; onOpen is
    // called when the stream is open.
    subscribe: (onState, onTrouble, onOpen) => {
      const es = new EventSource('events');
      es.addEventListener('state', (ev) => {
        let st;
        try {
          st = JSON.parse(ev.data);
        } catch (e) {
          return;
        }
        onState(st);
      });
      es.addEventListener('open', () => onOpen());
      es.addEventListener('error', () => onTrouble(es.readyState === EventSource.CLOSED));
      return es;
    },
  };

  const GONE = 'perf-agent-mcp is not running; this is the last state it sent. Ask your agent for a new page URL, or download a static copy while it runs.';
  const PAUSED = 'Live updates are paused because too many tabs have this page open. The page checks for changes every few seconds.';

  // A seq is a decimal string. newer compares two by length, then
  // lexically, so "10" is newer than "9" and no seq is ever a number.
  const digits = /^[0-9]+$/;
  const canonical = (s) => s.replace(/^0+(?=[0-9])/, '');
  const newer = (a, b) => {
    if (typeof a !== 'string' || !digits.test(a)) return false;
    if (typeof b !== 'string' || !digits.test(b)) return true;
    const x = canonical(a);
    const y = canonical(b);
    return x.length !== y.length ? x.length > y.length : x > y;
  };

  const pubShape = /^p[0-9]{1,18}$/;
  const nodeShape = /^[0-9A-Za-z_-]{1,64}(\/[0-9A-Za-z_-]{1,64})?$/;
  const isPub = (id) => typeof id === 'string' && pubShape.test(id);
  const isNode = (id) => typeof id === 'string' && nodeShape.test(id);
  const pubPath = (id, rest) => 'pub/' + id + '/' + rest;
  const nodePath = (id, route, node) => pubPath(id, route + '/' + encodeURIComponent(node));

  const make = (tag, cls, text) => {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  };
  const clear = (e) => {
    while (e.firstChild) e.removeChild(e.firstChild);
  };

  // acceptedSeq is the highest seq accepted so far. It is set before any of
  // a state's work starts, so an older state that arrives while a newer one
  // is still loading is refused. renderedSeq is the seq whose work finished.
  let acceptedSeq = '';
  let renderedSeq = '';
  // retrySeq is an accepted seq whose work failed: the same state may be
  // accepted once more. failures counts its failed attempts, and failGen
  // is the generation when the last one failed.
  let retrySeq = '';
  let failures = 0;
  let failGen = -1;
  // gen moves on every accepted agent state and every local navigation. An
  // async chain commits to the DOM only if gen is still the one it started
  // with.
  let gen = 0;
  // agent is the last accepted state; applied the last one whose work
  // finished. While they differ, an agent chain is pending or was cut
  // short, possibly after committing part of its work.
  let agent = null;
  let applied = null;
  // loadingGen is the generation of an agent chain that is loading a
  // publication switch; -1 when none is.
  let loadingGen = -1;
  // view is what the DOM shows: the publication, its whole model, the model
  // drawn (the whole one or a focus), its history, the focused node and the
  // selected node, with the drawn model's frames and elements by node ID.
  let view = null;

  // A publication is an explained view or a diff; their models differ in
  // shape, and a diff's frames are turned into the panel's frames.
  const isDiff = (m) => !!m && m.kind === 'diff';
  const selectedOf = (m) => (isDiff(m) ? m.selected : m.model.selected);
  // diffPanelFrame is a diff frame as the panel shows one: its facts are
  // the model's fact rows read from the frame's fields, and its share in a
  // path is its change in share. The rows' titles are set once shown.
  const diffPanelFrame = (f, rows) => Object.assign({}, f, {
    share: f.delta_share,
    facts: (rows || []).map((r) => ({v: f[r.f] || ''})),
  });

  let panel = null;
  const panelModel = {
    get fact_rows() {
      if (!view) return [];
      return isDiff(view.shown) ? view.shown.fact_rows : view.shown.model.fact_rows;
    },
    get prompts() {
      return view && !isDiff(view.shown) ? view.shown.model.prompts : [];
    },
  };

  // --- Drawing a publication ---

  // titleSegments writes a title's segments: text, and the numbers no claim
  // checked, marked as the static page marks them.
  const titleSegments = (parent, segs) => {
    for (const s of segs || []) {
      if (s.kind === 'unchecked') {
        const m = make('mark', 'unchecked', s.text);
        m.title = s.title || '';
        parent.appendChild(m);
      } else {
        parent.appendChild(document.createTextNode(s.text || ''));
      }
    }
  };

  const renderPubs = () => {
    const nav = $('pubs');
    clear(nav);
    for (const p of (agent && agent.publications) || []) {
      if (!isPub(p.id)) continue;
      const b = make('button');
      b.type = 'button';
      b.dataset.pub = p.id;
      if (view && view.pub === p.id) b.setAttribute('aria-current', 'true');
      b.appendChild(make('span', 'ref', p.id));
      const t = make('span');
      titleSegments(t, p.title_segs);
      b.appendChild(t);
      b.appendChild(make('span', 'vw', p.view || ''));
      nav.appendChild(b);
    }
  };

  const renderPills = () => {
    const pills = $('pills');
    clear(pills);
    if (!view) return;
    if (isDiff(view.whole)) {
      pills.appendChild(make('span', 'pill ok', view.whole.pill));
      return;
    }
    pills.appendChild(make('span', 'pill ai', '✦ Explained by an agent via MCP'));
    const ok = make('span', 'pill ok', view.whole.checked_pill);
    ok.title = 'Each structured claim was compared with the engine at its rounding, and each frame reference resolved';
    pills.appendChild(ok);
    pills.appendChild(make('span', 'pill', 'prose is not verified'));
    const h = view.hist;
    if (!h) return;
    for (const text of [h.rejected_pill, h.limit_pill, h.history_pill]) {
      if (text) pills.appendChild(make('span', 'pill bad', text));
    }
  };

  const rejectedRow = (row) => {
    const li = make('li');
    li.appendChild(make('span', 'ref', row.attempt));
    li.appendChild(document.createTextNode(' '));
    if (row.evidence) {
      li.appendChild(make('span', 'muted', '(' + row.evidence + ')'));
      li.appendChild(document.createTextNode(' '));
    }
    li.appendChild(document.createTextNode(row.label));
    if (row.kind === 'claim') {
      if (row.metric) {
        li.appendChild(document.createTextNode(' ('));
        li.appendChild(make('code', '', row.metric));
        li.appendChild(document.createTextNode(' of ' + (row.node || '') + ')'));
      }
      li.appendChild(document.createTextNode(':'));
      if (row.agent) {
        li.appendChild(document.createTextNode(' the agent wrote '));
        li.appendChild(make('s', '', row.agent));
      }
      if (row.engine) {
        li.appendChild(document.createTextNode((row.agent ? ',' : '') + ' perf-agent has '));
        li.appendChild(make('b', '', row.engine));
      }
      li.appendChild(make('div', 'why', (row.code || '') + ': ' + (row.reason || '')));
    } else if (row.kind !== 'more') {
      li.appendChild(document.createTextNode(': ' + (row.reason || '')));
    }
    return li;
  };

  const renderRejected = () => {
    const box = $('rejected');
    clear(box);
    const h = view && view.hist;
    if (!h || (!(h.rows && h.rows.length) && !h.rejected_note)) return;
    // The history follows the server's record of the profile and view, so
    // it lists attempts made after this publication as well as before it.
    const sec = make('section', 'sec');
    const attempt = view.whole.attempt ? ' (attempt ' + view.whole.attempt + ')' : '';
    sec.appendChild(make('h5', '', 'Rejected attempts on this profile and view'));
    sec.appendChild(make('p', 'muted', 'perf-agent refused these attempts on this profile and view, before or after this publication' + attempt +
      ". A claim row strikes through the agent's figure beside the engine's; reference and limit rows say what was wrong."));
    if (h.rejected_note) sec.appendChild(make('p', 'muted', h.rejected_note));
    const ul = make('ul', 'rej');
    for (const row of h.rows || []) ul.appendChild(rejectedRow(row));
    sec.appendChild(ul);
    box.appendChild(sec);
  };

  // applyHistory shows h if it is the shown publication's and newer than
  // the history shown. Histories are not tied to a generation: a newer one
  // is right whatever the page has selected since.
  const applyHistory = (h) => {
    if (!view || !h || h.publication !== view.pub) return;
    if (view.hist && !newer(h.version, view.hist.version)) return;
    view.hist = h;
    renderPills();
    renderRejected();
  };

  const renderLegend = (m) => {
    const legend = $('legend');
    clear(legend);
    if (isDiff(m)) {
      for (const l of m.legend || []) {
        const c = make('span', 'c');
        const sw = make('i', 'sw dsw');
        sw.dataset.key = l.key;
        c.appendChild(sw);
        c.appendChild(document.createTextNode(l.label));
        legend.appendChild(c);
      }
      return;
    }
    for (const l of m.legend || []) {
      const c = make('span', 'c');
      const sw = make('i', 'sw');
      sw.dataset.domain = l.key;
      c.appendChild(sw);
      c.appendChild(document.createTextNode(l.label));
      legend.appendChild(c);
    }
    legend.appendChild(make('span', 'c key', '✦ path with an explanation'));
  };

  const renderNotes = (m) => {
    const notes = $('notes');
    clear(notes);
    notes.setAttribute('aria-label', isDiff(m) ? 'Comparison notes' : 'Sampling notes');
    let items = m.notes || [];
    if (isDiff(m)) {
      items = [].concat(m.warnings || [], m.notes || [],
        (m.base_notes || []).map((n) => 'base: ' + n), (m.new_notes || []).map((n) => 'new: ' + n),
        m.empty_note ? [m.empty_note] : []);
    }
    notes.hidden = !items.length;
    if (notes.hidden) return;
    notes.appendChild(make('b', '', isDiff(m) ? 'Comparison notes' : 'Sampling notes'));
    const ul = make('ul');
    for (const n of items) ul.appendChild(make('li', '', n));
    notes.appendChild(ul);
  };

  const renderQuality = (m) => {
    const q = $('quality');
    clear(q);
    for (const r of m.quality || []) {
      q.appendChild(make('span', 'k', r.k));
      const v = make('span', 'v', r.v);
      v.title = r.t;
      q.appendChild(v);
      q.appendChild(make('span', 'st st-' + r.s, r.s));
      q.appendChild(make('span', 'p', r.n));
    }
  };

  const renderFoot = (m) => {
    const foot = $('foot');
    clear(foot);
    if (isDiff(m)) {
      for (const [side, name, handle, total] of [['Base', m.base_name, m.base_handle, m.base_total], ['New', m.new_name, m.new_handle, m.new_total]]) {
        const s = make('span', '', side + ' ' + name + ': ' + total);
        s.title = handle;
        foot.appendChild(s);
      }
      foot.appendChild(make('span', '', 'View ' + m.view));
      return;
    }
    const total = make('span', '', 'Total ' + m.total);
    total.title = m.total_title;
    foot.appendChild(total);
    foot.appendChild(make('span', '', 'View ' + m.view));
    if (m.profile_name) foot.appendChild(make('span', '', 'Profile ' + m.profile_name));
    if (m.digest) {
      const d = make('span', '', 'Digest ');
      d.appendChild(make('code', '', m.digest));
      foot.appendChild(d);
    }
    if (m.attempt) foot.appendChild(make('span', '', 'Attempt ' + m.attempt));
  };

  const updateButtons = () => {
    $('focus-btn').disabled = !view || !view.selected || view.focus === view.selected;
    $('whole-btn').disabled = !view || !view.focus;
  };

  // drawModel draws m, the publication's whole model or a focus of it:
  // the frames, the axis, what was left out and the explained rows. The
  // frames' geometry is the model's strings with their units; the static
  // page's "cur" class is dropped, because this page owns the selection.
  // A diff's frames take their colour from the model's hue and tint.
  const drawModel = (m) => {
    const diff = isDiff(m);
    view.shown = m;
    view.list = diff ? m.frames.map((f) => diffPanelFrame(f, m.fact_rows)) : m.model.frames;
    view.frames = new Map();
    view.els = new Map();
    for (const f of view.list) view.frames.set(f.id, f);

    const chart = $('chart');
    clear(chart);
    chart.classList.toggle('diff', diff);
    chart.style.height = m.height + 'px';
    for (const d of m.drawn) {
      const f = view.list[d.i];
      if (!f) continue;
      const el = make('div', 'frame');
      for (const c of (d.cls || '').split(' ')) {
        if (c && c !== 'cur') el.classList.add(c);
      }
      el.style.left = d.left + '%';
      el.style.width = d.width + '%';
      el.style.top = d.top + 'px';
      el.dataset.i = d.i;
      if (diff) {
        el.style.setProperty('--h', d.hue);
        el.style.setProperty('--t', d.tint);
      } else {
        el.dataset.domain = d.domain;
        if (d.res) el.dataset.resolution = d.res;
      }
      el.tabIndex = 0;
      el.setAttribute('role', 'button');
      el.title = d.tip;
      const label = make('span', 'fl');
      if (d.star) label.appendChild(make('span', 'star', '✦'));
      label.appendChild(document.createTextNode(d.marker ? d.marker + ' ' + f.name : f.name));
      el.appendChild(label);
      chart.appendChild(el);
      view.els.set(f.id, el);
    }

    const axis = $('axis');
    clear(axis);
    axis.appendChild(make('span', '', m.view + ' · ' + m.axis));
    if (diff) {
      axis.appendChild(make('span', '', 'base ' + m.base_total + ' · new ' + m.new_total));
    } else {
      const total = make('span', '', 'total ' + m.total);
      total.title = m.total_title;
      axis.appendChild(total);
    }

    const left = $('left-out');
    clear(left);
    for (const note of [m.narrow_note, m.limit_note, m.size_note]) {
      if (note) left.appendChild(make('p', 'left-out', note));
    }

    const rows = $('rows');
    clear(rows);
    $('rows-sec').hidden = diff;
    if (diff) {
      updateButtons();
      return;
    }
    for (const r of m.rows || []) {
      const b = make('button', 'exrow');
      b.type = 'button';
      b.dataset.node = r.node;
      b.appendChild(make('span', 'star', '✦'));
      b.appendChild(make('span', 'ref', r.ref));
      b.appendChild(make('code', '', r.name));
      b.appendChild(make('span', 'w', r.share + ' · ' + r.cum));
      rows.appendChild(b);
    }
    if (!rows.firstChild) rows.appendChild(make('p', 'muted', 'The agent explained no path.'));
    updateButtons();
  };

  const clearPanel = () => {
    for (const id of ['sel-ref', 'sel-name', 'sel-id']) $(id).textContent = '';
    for (const id of ['facts', 'exp', 'path', 'chips']) clear($(id));
    $('noexp').hidden = true;
  };

  // commitPublication shows publication id with its model and history,
  // which were fetched together and are shown together.
  const commitPublication = (id, model, hist) => {
    view = {pub: id, whole: model, shown: null, hist: null, focus: '', selected: '', list: [], frames: new Map(), els: new Map()};
    document.title = model.doc_title || 'perf-agent live page';
    const title = $('title');
    clear(title);
    titleSegments(title, model.title_segs);
    $('view').textContent = model.view;
    $('waiting').hidden = true;
    // A diff too large for a static copy says so instead of offering one.
    const exp = $('export');
    const exportable = !isDiff(model) || (model.export && model.export.ok);
    if (exportable) {
      exp.href = pubPath(id, 'export');
      exp.download = id + '.html';
      exp.textContent = 'Download a static copy';
      exp.title = '';
      exp.removeAttribute('aria-disabled');
    } else {
      exp.removeAttribute('href');
      exp.removeAttribute('download');
      exp.textContent = 'No static copy';
      exp.title = (model.export && model.export.reason) || '';
      exp.setAttribute('aria-disabled', 'true');
    }
    exp.hidden = false;
    $('quality-sec').hidden = isDiff(model);
    renderLegend(model);
    renderNotes(model);
    renderQuality(model);
    renderFoot(model);
    clearPanel();
    drawModel(model);
    applyHistory(hist);
    renderPills();
    renderRejected();
    renderPubs();
  };

  // --- Selecting a node ---

  // refOf names a node as the drawn model does, or by its node ID when the
  // model does not hold it.
  const refOf = (id) => {
    const f = view && view.frames.get(id);
    return f ? f.ref : id;
  };
  const relabelSegs = (segs) => (segs || []).map((s) => (s.kind === 'node' ? Object.assign({}, s, {text: refOf(s.node)}) : s));
  // relabel names a fetched frame, its node references and its path from
  // the drawn model: a frame response carries no refs of its own.
  const relabel = (frame) => {
    const f = Object.assign({}, frame, {ref: refOf(frame.id)});
    if (frame.explanation) {
      f.explanation = frame.explanation.map((e) => ({
        prose: relabelSegs(e.prose),
        hypotheses: (e.hypotheses || []).map(relabelSegs),
        next: relabelSegs(e.next),
      }));
    }
    return f;
  };
  const chainOf = (id) => {
    const chain = [];
    for (let g = view.frames.get(id); g && g.parent; g = view.frames.get(g.parent)) chain.push(g);
    return chain.reverse();
  };

  const showFrame = (f, path, scroll) => {
    panel.show(f, path);
    // A diff has no explanations: its figures are the engine's, and no
    // agent claim is made about them. A row with exact digits shows them
    // on hover.
    if (isDiff(view.whole)) {
      $('noexp').hidden = true;
      const vals = $('facts').querySelectorAll('.v');
      (view.whole.fact_rows || []).forEach((r, i) => {
        if (r.t && vals[i]) vals[i].title = f[r.t] || '';
      });
    }
    view.selected = f.id;
    const el = view.els.get(f.id);
    if (el && scroll) el.scrollIntoView({block: 'nearest', inline: 'nearest'});
    updateButtons();
    return f;
  };

  // selectIn selects node id in the drawn model, fetching its frame when
  // the model does not hold it. It resolves to the frame shown, or null when
  // the page moved on first.
  const selectIn = async (id, g, scroll) => {
    if (g !== gen || !view) return null;
    if (view.frames.has(id)) return showFrame(view.frames.get(id), chainOf(id), scroll);
    const v = view;
    const r = await transport.getJSON(nodePath(v.pub, 'frame', id));
    if (g !== gen || view !== v || !r || !r.frame || r.frame.id !== id) return null;
    const diff = isDiff(v.whole);
    const frame = diff ? diffPanelFrame(r.frame, v.whole.fact_rows) : r.frame;
    const path = (r.path || []).map((p) => Object.assign({}, p, {ref: refOf(p.id)}, diff ? {share: p.delta_share} : {}));
    return showFrame(relabel(frame), path, scroll);
  };

  // focusOn draws the subtree under node id. It resolves to false when the
  // page moved on first.
  const focusOn = async (id, g) => {
    const v = view;
    const m = await transport.getJSON(nodePath(v.pub, 'focus', id));
    if (g !== gen || view !== v || !m || m.focus !== id) return false;
    v.focus = id;
    drawModel(m);
    return true;
  };

  const loadPub = async (id, g) => {
    const [model, hist] = await Promise.all([transport.getJSON(pubPath(id, 'model')), transport.getJSON(pubPath(id, 'history'))]);
    if (g !== gen) return false;
    if (!model || (model.kind !== 'explained' && model.kind !== 'diff')) throw new Error('this page cannot show a publication of this kind');
    commitPublication(id, model, hist);
    return true;
  };

  const refreshHistory = async (id) => {
    applyHistory(await transport.getJSON(pubPath(id, 'history')));
  };

  let toastTimer = 0;
  const toast = (msg) => {
    const t = $('toast');
    t.textContent = msg;
    t.classList.add('on');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => t.classList.remove('on'), 2200);
  };

  // --- The user's own navigation: local until the next agent state ---

  const quietly = (p) => p.catch(() => {});
  // While the agent's publication switch loads, the frames on screen are
  // about to go: navigating them would only cancel the switch.
  const agentLoading = () => loadingGen === gen;

  const localSelect = (id, scroll) => {
    if (!view || !isNode(id) || agentLoading()) return;
    const g = ++gen;
    quietly(selectIn(id, g, scroll));
  };

  const focusHere = () => {
    if (!view || !view.selected || agentLoading()) return;
    const id = view.selected;
    const g = ++gen;
    quietly(focusOn(id, g).then((ok) => ok && selectIn(id, g, false)));
  };

  const wholeView = () => {
    if (!view || !view.focus || agentLoading()) return;
    const g = ++gen;
    const id = view.selected;
    view.focus = '';
    drawModel(view.whole);
    if (id) quietly(selectIn(id, g, true));
  };

  const openPub = (id) => {
    if (!isPub(id)) return;
    const g = ++gen;
    quietly(loadPub(id, g).then((ok) => {
      const sel = ok && selectedOf(view.whole);
      return sel && isNode(sel) ? selectIn(sel, g, true) : null;
    }));
  };

  // --- Agent states ---

  // rendered records the seq whose work finished, for display only.
  const rendered = (seq) => {
    renderedSeq = seq;
    document.body.dataset.seq = renderedSeq;
  };

  const entryOf = (st, id) => ((st && st.publications) || []).find((p) => p.id === id);

  const sameTarget = (a, b) => !!a && !!b && a.current === b.current && a.selected === b.selected && a.focus === b.focus;

  // noteState is what #load-note says, apart from the failure count:
  // '' (nothing), 'trying' (a state's retries are live) or 'partial' (the
  // page stopped retrying the agent's last state).
  let noteState = '';
  const noteText = {
    '': '',
    trying: 'perf-agent could not load this publication; the page keeps trying.',
    partial: "Your agent's last page could not be fully loaded.",
  };
  const loadNote = (state) => {
    noteState = state;
    const n = $('load-note');
    n.textContent = noteText[state];
    n.hidden = !state;
  };

  // settled records st as applied: its work is done, so nothing is being
  // retried.
  const settled = (st) => {
    applied = st;
    rendered(st.seq);
    retrySeq = '';
    failures = 0;
    loadNote('');
  };

  // stopRetrying ends the retries of a state the page moved away from. If
  // the page says it keeps trying, it now says the agent's page was not
  // fully loaded.
  const stopRetrying = () => {
    if (noteState === 'trying') loadNote('partial');
    retrySeq = '';
    failures = 0;
  };

  // applyState refreshes the shown publication's history when st moves
  // it, and, unless st is quiet, steers the page to st's publication,
  // focus and selection. Each step compares st with what is on screen
  // (view), not with an earlier state, so a chain cut short after drawing
  // part of its work is undone or finished by the next state.
  const applyState = async (st, g, quiet, prev) => {
    renderPubs();
    if (view) {
      const e = entryOf(st, view.pub);
      if (e && (!view.hist || newer(e.history, view.hist.version))) quietly(refreshHistory(view.pub));
    }
    if (quiet || !isPub(st.current)) {
      settled(st);
      return;
    }
    if (!view || view.pub !== st.current) {
      loadingGen = g;
      try {
        if (!(await loadPub(st.current, g))) return;
      } finally {
        if (loadingGen === g) loadingGen = -1;
      }
    }
    if (g !== gen) return;
    const focus = isNode(st.focus) ? st.focus : '';
    if (focus) {
      if (view.focus !== focus && !(await focusOn(focus, g))) return;
    } else if (view.focus) {
      view.focus = '';
      drawModel(view.whole);
    }
    if (g !== gen) return;
    const sel = isNode(st.selected) ? st.selected : selectedOf(view.whole);
    let f = null;
    if (isNode(sel)) {
      f = await selectIn(sel, g, true);
      if (!f) return;
    }
    if (g !== gen) return;
    if (f && prev && !sameTarget(prev, st)) toast('Your agent selected ' + f.ref + ' ' + f.name);
    settled(st);
  };

  // Retries of a state whose work failed back off. After quickRetries
  // failures the page says it cannot load the publication, and a retry
  // keeps the generation: it runs only while the user has not navigated
  // since the last failure, and otherwise the retries stop.
  const retryDelays = [1000, 2000, 5000, 10000];
  const quickRetries = 3;

  // onState accepts st if its seq is newer than any accepted so far, and
  // records that before any of its work starts. A quiet state (no agent
  // chain pending or cut short, and the same publication, selection and
  // focus as the last accepted) only refreshes history and keeps the
  // generation, so the user's own fetches in flight still land.
  const onState = (st) => {
    if (!st || typeof st !== 'object') return;
    const again = retrySeq !== '' && st.seq === retrySeq && st.seq === acceptedSeq;
    if (!again && !newer(st.seq, acceptedSeq)) return;
    if (again && failures >= quickRetries && gen !== failGen) {
      stopRetrying();
      return;
    }
    acceptedSeq = st.seq;
    retrySeq = '';
    if (!again) {
      // A newer state supersedes the one being retried: its retries end
      // here, and so does the note promising them.
      failures = 0;
      if (noteState === 'trying') loadNote('');
    }
    const prev = agent;
    const quiet = !again && applied !== null && applied === agent && sameTarget(st, agent);
    const g = quiet || failures >= quickRetries ? gen : ++gen;
    agent = st;
    applyState(st, g, quiet, again ? st : prev).then(() => {
      // Cut short by a newer state or by the user's own navigation.
      if (applied !== st && agent === st) stopRetrying();
    }, () => {
      if (g !== gen || agent !== st) {
        if (agent === st) stopRetrying();
        return;
      }
      retrySeq = st.seq;
      failures++;
      failGen = gen;
      if (failures >= quickRetries) loadNote('trying');
      setTimeout(checkState, retryDelays[Math.min(failures, retryDelays.length) - 1]);
    });
  };

  // --- The connection ---

  const banner = (kind, text) => {
    const b = $('banner');
    b.className = kind;
    b.textContent = text;
    b.hidden = !text;
  };

  let stream = null;
  // epoch moves on every connection event: the stream opening, failing or
  // delivering a state. A check of GET state changes the banner only if no
  // connection event happened while it waited, so an old answer never
  // overrides what the connection has shown since.
  let epoch = 0;
  // pollRun names the one polling loop allowed to run; 0 when none is.
  let pollRun = 0;
  let pollRuns = 0;
  let pollStep = 0;
  let polls = 0;
  const pollDelays = [1000, 2000, 5000];

  // checkState reads GET state and resolves to 'ok', 'failed' (and the
  // banner says the server is gone) or 'stale'.
  const checkState = async () => {
    const e = epoch;
    try {
      onState(await transport.getJSON('state'));
      return e === epoch ? 'ok' : 'stale';
    } catch (err) {
      if (e !== epoch) return 'stale';
      banner('gone', GONE);
      return 'failed';
    }
  };

  // poll reads the state while the stream is refused, slower while
  // nothing changes, and tries the stream again now and then. A loop whose
  // run is no longer pollRun stops, before and after each read.
  const poll = async (run) => {
    if (run !== pollRun) return;
    const before = acceptedSeq;
    const r = await checkState();
    if (run !== pollRun) return;
    if (r === 'ok') banner('quiet', PAUSED);
    pollStep = acceptedSeq !== before ? 0 : Math.min(pollStep + 1, pollDelays.length - 1);
    polls++;
    if (polls % 3 === 0 && stream && stream.readyState === EventSource.CLOSED) connect();
    if (run === pollRun) setTimeout(() => poll(run), pollDelays[pollStep]);
  };

  const startPolling = () => {
    if (pollRun) return;
    pollRun = ++pollRuns;
    pollStep = 0;
    const run = pollRun;
    setTimeout(() => poll(run), pollDelays[0]);
  };

  const onOpen = () => {
    epoch++;
    pollRun = 0;
    banner('', '');
  };

  const onTrouble = async (gaveUp) => {
    epoch++;
    const r = await checkState();
    if (r === 'stale') return;
    if (gaveUp) {
      if (r === 'ok') banner('quiet', PAUSED);
      startPolling();
    } else if (r === 'ok' && !pollRun) {
      banner('', '');
    }
  };

  const onStreamState = (st) => {
    epoch++;
    onState(st);
  };

  const connect = () => {
    stream = transport.subscribe(onStreamState, onTrouble, onOpen);
  };

  // --- Wiring ---

  panel = perfAgentPanel(panelModel, {
    frameById: (id) => (view ? view.frames.get(id) : undefined),
    elementFor: (id) => (view && view.els.get(id)) || null,
    onSelect: (id) => localSelect(id, true),
  });

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

  const frameOf = (el) => view && view.list && view.list[el.dataset.i];
  const chart = $('chart');
  chart.addEventListener('click', (ev) => {
    const el = ev.target.closest('.frame');
    const f = el && frameOf(el);
    if (f) localSelect(f.id, false);
  });
  chart.addEventListener('keydown', (ev) => {
    if (ev.key !== 'Enter' && ev.key !== ' ') return;
    const el = ev.target.closest('.frame');
    const f = el && frameOf(el);
    if (!f) return;
    ev.preventDefault();
    localSelect(f.id, false);
  });
  $('focus-btn').addEventListener('click', focusHere);
  $('whole-btn').addEventListener('click', wholeView);
  document.addEventListener('click', (ev) => {
    const chip = ev.target.closest('[data-prompt]');
    if (chip) {
      copy(chip.dataset.prompt);
      return;
    }
    const pub = ev.target.closest('[data-pub]');
    if (pub) {
      openPub(pub.dataset.pub);
      return;
    }
    const ref = ev.target.closest('[data-node]');
    if (ref && !ref.closest('#exp, #path, #chart')) localSelect(ref.dataset.node, true);
  });

  (async () => {
    await checkState();
    connect();
  })();
})();
