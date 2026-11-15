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

  const chainFromModel = (i) => {
    const chain = [];
    for (let g = frames[i]; g && g.parent; g = frames[byId.get(g.parent)]) chain.push(g);
    return chain.reverse();
  };

  let panel;
  const select = (i, scroll) => {
    const f = frames[i];
    if (!f) return;
    panel.show(f, chainFromModel(i));
    const el = els.get(i);
    if (el && scroll) el.scrollIntoView({block: 'nearest', inline: 'nearest'});
  };
  panel = perfAgentPanel(model, {
    frameById: (id) => frames[byId.get(id)],
    elementFor: (id) => els.get(byId.get(id)),
    onSelect: (id) => {
      if (byId.has(id)) select(byId.get(id), true);
    },
  });

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
    if (ref && !ref.closest('#exp, #path') && byId.has(ref.dataset.node)) select(byId.get(ref.dataset.node), true);
  });

  if (byId.has(model.selected)) select(byId.get(model.selected), false);
})();
