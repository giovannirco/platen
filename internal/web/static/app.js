// Platen web interface. No framework, no build step: this file is what runs.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

// h builds DOM nodes: h('div', {class: 'row'}, 'text', h('b', {}, 'more')).
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'style') for (const [p, val] of Object.entries(v)) el.style.setProperty(p, val);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) if (c != null && c !== false) el.append(c.nodeType ? c : document.createTextNode(c));
  return el;
}

const SVG = 'http://www.w3.org/2000/svg';
function icon(path) {
  const s = document.createElementNS(SVG, 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('aria-hidden', 'true');
  const p = document.createElementNS(SVG, 'path');
  p.setAttribute('d', path);
  s.append(p);
  return s;
}
const ICONS = {
  print: 'M7 9V4h10v5M7 17H5a2 2 0 0 1-2-2v-4a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2v4a2 2 0 0 1-2 2h-2M7 14h10v6H7z',
  scan: 'M4 8V6a2 2 0 0 1 2-2h2M16 4h2a2 2 0 0 1 2 2v2M20 16v2a2 2 0 0 1-2 2h-2M8 20H6a2 2 0 0 1-2-2v-2M4 12h16',
  doc: 'M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8zM14 3v5h5M9 13h6M9 17h4',
};

// ---- API ---------------------------------------------------------------------

class ApiError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `HTTP ${status}`);
    this.status = status;
    this.code = body?.error?.code;
    this.details = body?.error?.details;
  }
}

async function api(method, path, body) {
  const opts = { method, headers: { 'X-Platen-Client': 'web' }, credentials: 'same-origin' };
  if (body instanceof FormData) opts.body = body;
  else if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch('/api/v1' + path, opts);
  let data = null;
  try { data = await res.json(); } catch { /* no body */ }
  if (res.status === 401) {
    await login();
    return api(method, path, body);
  }
  if (!res.ok) throw new ApiError(res.status, data);
  return data;
}

function login() {
  const dlg = $('#dlg-login');
  return new Promise((resolve) => {
    const form = $('#login-form');
    const submit = async (ev) => {
      ev.preventDefault();
      const res = await fetch('/api/v1/login', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: $('#login-token').value }),
      });
      if (res.ok) {
        form.removeEventListener('submit', submit);
        dlg.close();
        resolve();
      } else {
        const err = $('#login-error');
        err.textContent = 'That token is not valid.';
        err.hidden = false;
      }
    };
    form.addEventListener('submit', submit);
    if (!dlg.open) dlg.showModal();
  });
}

// ---- small helpers -----------------------------------------------------------

function toast(message, { bad = false, link } = {}) {
  const t = h('div', { class: 'toast' + (bad ? ' bad' : '') }, message);
  if (link) t.append(' ', h('a', { href: link.href, target: '_blank', rel: 'noopener' }, link.text));
  $('#toasts').append(t);
  setTimeout(() => t.remove(), bad ? 8000 : 4500);
}

async function busy(button, fn) {
  if (button.classList.contains('busy')) return;
  button.classList.add('busy');
  try { return await fn(); }
  catch (err) { if (err) toast(err.message, { bad: true }); }
  finally { button.classList.remove('busy'); }
}

function ago(iso) {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 45) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
}

function bytes(n) {
  if (n < 1024 * 1024) return `${Math.max(1, Math.round(n / 1024))} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}

const plural = (n, one, many = one + 's') => `${n} ${n === 1 ? one : many}`;

function seg(el, onChange) {
  const attr = el.getAttribute('role') === 'tablist' ? 'aria-selected' : 'aria-pressed';
  el.addEventListener('click', (ev) => {
    const b = ev.target.closest('button');
    if (!b || b.disabled) return;
    for (const x of $$('button', el)) x.setAttribute(attr, x === b ? 'true' : 'false');
    onChange?.(b.dataset.v ?? b.dataset.source);
  });
  return () => $(`button[${attr}="true"]`, el)?.dataset.v;
}

function confirmDialog(title, text, okLabel) {
  const dlg = $('#dlg-confirm');
  $('#confirm-title').textContent = title;
  $('#confirm-text').textContent = text;
  $('#confirm-ok').textContent = okLabel;
  dlg.returnValue = 'cancel';
  dlg.showModal();
  return new Promise((resolve) => dlg.addEventListener('close', () => resolve(dlg.returnValue === 'ok'), { once: true }));
}

// ---- state -------------------------------------------------------------------

const state = {
  info: null, printers: [], scanners: [], jobs: [], scans: [],
  meta: null,            // Paperless tags, correspondents, types
  print: { source: 'file', file: null, paperlessId: 0, copies: 1, check: null },
  scan: null,            // the scan being worked on
  page: null,            // id of the page shown on the stage
};

const stateClass = { idle: 'ok', processing: 'busy', stopped: 'bad', completed: 'ok', pending: 'busy', held: 'warn', canceled: 'warn', aborted: 'bad', unknown: '' };
const stateLabel = { idle: 'Ready', processing: 'Printing', stopped: 'Stopped', completed: 'Done', pending: 'Waiting', held: 'On hold', canceled: 'Cancelled', aborted: 'Failed', unknown: 'Unknown' };

// ---- home --------------------------------------------------------------------

function tankColor(m) {
  if (m.color) return m.color;
  const n = m.name.toLowerCase();
  if (n.includes('cyan')) return '#00a8e8';
  if (n.includes('magenta')) return '#e6007e';
  if (n.includes('yellow')) return '#f5c400';
  return '#1b1a17';
}

function printerCard(p) {
  const card = h('article', { class: 'card device' });
  const pillClass = p.online ? (stateClass[p.state] ?? '') : 'bad';
  const reasons = (p.state_reasons || []).map((r) => r.replace(/-(warning|report|error)$/, '').replaceAll('-', ' '));
  card.append(
    h('div', { class: 'device-head' },
      h('div', {}, h('div', { class: 'device-name' }, p.name), h('div', { class: 'device-model' }, p.online ? (p.make_and_model || 'Printer') : 'Not reachable')),
      h('span', { class: `pill ${pillClass}` }, p.online ? (stateLabel[p.state] ?? p.state) : 'Offline')),
  );
  if (!p.online) {
    card.append(h('p', { class: 'note' }, p.error || 'The printer did not answer.'));
    return card;
  }
  if (p.markers?.length) {
    card.append(h('div', { class: 'tanks', role: 'group', 'aria-label': 'Ink levels' }, p.markers.map((m) => {
      const known = m.level >= 0;
      const low = known && m.level <= Math.max(m.low, 10);
      return h('div', { class: 'tank' + (low ? ' low' : ''), title: `${m.name}: ${known ? m.level + '%' : 'unknown'}` },
        h('div', { class: 'tank-glass' }, h('div', { class: 'tank-liquid', style: { '--level': known ? m.level : 0, '--tank': tankColor(m) } })),
        h('span', { class: 'tank-pct' }, known ? `${m.level}%` : '?'),
        h('span', { class: 'tank-name' }, m.name.replace(/\(.*\)/, '').trim()));
    })));
  }
  const meta = h('div', { class: 'device-meta' });
  if (p.media_labels?.length) meta.append(h('span', {}, 'Paper ', h('b', {}, [...new Set(p.media_labels)].slice(0, 2).join(', '))));
  meta.append(h('span', {}, 'Queue ', h('b', {}, String(p.queued_jobs))));
  if ((p.sides || []).length > 1) meta.append(h('span', {}, h('b', {}, 'Two-sided')));
  if (reasons.length) meta.append(h('span', {}, h('b', {}, reasons.join(', '))));
  card.append(meta, h('div', { class: 'device-actions' },
    h('a', { class: 'btn primary', href: '#/print' }, 'Print'),
    h('button', { class: 'btn ghost', type: 'button', title: 'Make the printer flash, to see which one it is',
      onclick: (ev) => busy(ev.currentTarget, async () => { await api('POST', `/printers/${p.id}/identify`); toast(`${p.name} is flashing.`); }) }, 'Find it')));
  return card;
}

function scannerCard(s) {
  const card = h('article', { class: 'card device' });
  const busyNow = s.state === 'Processing';
  card.append(h('div', { class: 'device-head' },
    h('div', {}, h('div', { class: 'device-name' }, s.name), h('div', { class: 'device-model' }, s.online ? (s.make_and_model || 'Scanner') : 'Not reachable')),
    h('span', { class: `pill ${s.online ? (busyNow ? 'busy' : 'ok') : 'bad'}` }, s.online ? (busyNow ? 'Scanning' : 'Ready') : 'Offline')));
  if (!s.online) {
    card.append(h('p', { class: 'note' }, s.error || 'The scanner did not answer.'));
    return card;
  }
  const meta = h('div', { class: 'device-meta' });
  if (s.max_width_mm) meta.append(h('span', {}, 'Glass ', h('b', {}, `${Math.round(s.max_width_mm)} × ${Math.round(s.max_height_mm)} mm`)));
  if (s.resolutions?.length) meta.append(h('span', {}, 'Up to ', h('b', {}, `${Math.max(...s.resolutions)} dpi`)));
  if (s.sources?.includes('feeder')) meta.append(h('span', {}, h('b', {}, 'Document feeder')));
  card.append(meta, h('div', { class: 'device-actions' }, h('a', { class: 'btn primary', href: '#/scan' }, 'Scan')));
  return card;
}

function paperlessCard() {
  const card = h('article', { class: 'card device' });
  const on = state.info?.paperless;
  card.append(h('div', { class: 'device-head' },
    h('div', {}, h('div', { class: 'device-name' }, 'Paperless-ngx'), h('div', { class: 'device-model' }, on ? 'Scans can be filed with one tap' : 'Not connected')),
    h('span', { class: `pill ${on ? 'ok' : ''}` }, on ? 'Connected' : 'Off')));
  if (on) {
    const meta = h('div', { class: 'device-meta' });
    if (state.meta) meta.append(h('span', {}, h('b', {}, String(state.meta.tags.length)), ' tags'), h('span', {}, h('b', {}, String(state.meta.correspondents.length)), ' correspondents'));
    card.append(meta, h('div', { class: 'device-actions' }, h('a', { class: 'btn', href: state.info.paperless_url, target: '_blank', rel: 'noopener' }, 'Open Paperless')));
  } else {
    card.append(h('p', { class: 'note' }, 'Add a paperless section to the configuration to file scans automatically.'));
  }
  return card;
}

function renderHome() {
  const grid = $('#device-grid');
  grid.replaceChildren(...state.printers.map(printerCard), ...state.scanners.map(scannerCard), paperlessCard());
  if (!state.printers.length && !state.scanners.length) {
    grid.prepend(h('div', { class: 'empty' }, 'No printer or scanner is configured yet. Add them to platen.yaml and restart.'));
  }
  const recent = [
    ...state.jobs.slice(0, 4).map((j) => ({ at: j.created_at, row: jobRow(j) })),
    ...state.scans.slice(0, 4).map((s) => ({ at: s.created_at, row: scanRow(s) })),
  ].sort((a, b) => new Date(b.at) - new Date(a.at)).slice(0, 5).map((x) => x.row);
  $('#recent-list').replaceChildren(...(recent.length ? recent : [h('div', { class: 'empty' }, 'Nothing yet. Print or scan something.')]));
}

// ---- activity ----------------------------------------------------------------

function jobRow(j) {
  const active = ['pending', 'processing', 'held', 'stopped'].includes(j.state);
  const meta = [plural(j.pages * j.copies, 'page'), plural(j.sheets, 'sheet'), j.via, ago(j.created_at)].filter(Boolean).join(' · ');
  return h('div', { class: 'row' },
    h('div', { class: 'row-icon' }, icon(ICONS.print)),
    h('div', { style: { 'min-width': '0' } }, h('div', { class: 'row-title' }, j.title), h('div', { class: 'row-meta' }, meta)),
    h('div', { class: 'row-end' },
      h('span', { class: `pill ${stateClass[j.state] ?? ''}` }, stateLabel[j.state] ?? j.state),
      active && h('button', { class: 'link-btn', type: 'button', onclick: (ev) => busy(ev.currentTarget, async () => { await api('DELETE', `/jobs/${j.id}`); await refreshJobs(); }) }, 'Cancel')));
}

function scanRow(s) {
  const first = s.pages[0];
  const filed = s.paperless;
  const meta = [plural(s.pages.length, 'page'), `${s.dpi} dpi`, s.via, ago(s.created_at)].filter(Boolean).join(' · ');
  return h('div', { class: 'row' },
    h('div', { class: 'row-icon' }, first ? h('img', { src: `/api/v1/scans/${s.id}/pages/${first.id}/image?size=480`, alt: '', loading: 'lazy' }) : icon(ICONS.scan)),
    h('div', { style: { 'min-width': '0' } }, h('div', { class: 'row-title' }, s.title || 'Untitled scan'), h('div', { class: 'row-meta' }, meta)),
    h('div', { class: 'row-end' },
      filed?.url ? h('a', { class: 'link-btn', href: filed.url, target: '_blank', rel: 'noopener' }, 'Paperless')
        : filed ? h('span', { class: `pill ${filed.status === 'failure' ? 'bad' : 'busy'}` }, filed.status === 'failure' ? 'Not filed' : 'Filing') : null,
      h('a', { class: 'link-btn', href: `#/scan/${s.id}` }, 'Open'),
      h('a', { class: 'link-btn', href: `/api/v1/scans/${s.id}/document` }, 'PDF')));
}

function renderActivity() {
  $('#job-list').replaceChildren(...(state.jobs.length ? state.jobs.map(jobRow) : [h('div', { class: 'empty' }, 'No print jobs yet.')]));
  $('#scan-list').replaceChildren(...(state.scans.length ? state.scans.map(scanRow) : [h('div', { class: 'empty' }, 'No scans yet.')]));
}

// ---- print -------------------------------------------------------------------

const getDuplex = seg($('#opt-duplex'), () => checkPrint());
const getColor = seg($('#opt-color'), () => checkPrint());
const getQuality = seg($('#opt-quality'), () => checkPrint());

function currentPrinter() {
  const id = $('#opt-printer').value;
  return state.printers.find((p) => p.id === id) || state.printers.find((p) => p.default) || state.printers[0];
}

function mediaLabel(name) {
  const parts = name.split('_');
  if (parts.length < 3) return name;
  const known = { a4: 'A4', a5: 'A5', a6: 'A6', a3: 'A3', letter: 'Letter', legal: 'Legal', b5: 'B5', 'index-4x6': 'Photo 10×15 (4×6 in)', '5x7': 'Photo 13×18 (5×7 in)' };
  return known[parts[1]] || `${parts[1]} (${parts[2]})`;
}

function renderPrintOptions() {
  const sel = $('#opt-printer');
  const keep = sel.value;
  sel.replaceChildren(...state.printers.map((p) => h('option', { value: p.id, selected: keep ? p.id === keep : p.default }, p.name + (p.online ? '' : ' (offline)'))));
  $('#opt-printer-wrap').hidden = state.printers.length < 2;
  const p = currentPrinter();
  const duplex = (p?.sides || []).some((s) => s.startsWith('two-sided'));
  $('#opt-duplex-wrap').hidden = !duplex;
  const media = $('#opt-media');
  const keepMedia = media.value;
  const names = p?.media || [];
  const first = ['iso_a4_210x297mm', 'na_letter_8.5x11in', 'na_legal_8.5x14in', 'iso_a5_148x210mm', 'na_index-4x6_4x6in', 'na_5x7_5x7in'].filter((n) => names.includes(n));
  const rest = names.filter((n) => !first.includes(n) && !n.includes('borderless'));
  media.replaceChildren(h('option', { value: '' }, p?.media_default ? `Loaded (${mediaLabel(p.media_default)})` : 'What the printer has loaded'),
    ...[...first, ...rest].map((n) => h('option', { value: n, selected: n === keepMedia }, mediaLabel(n))));
  $('#tab-paperless').hidden = !state.info?.paperless;
}

function printSource() {
  const src = state.print.source;
  if (src === 'file') return state.print.file ? { file: state.print.file } : null;
  if (src === 'url') { const v = $('#print-url').value.trim(); return v ? { url: v } : null; }
  if (src === 'text') { const v = $('#print-text').value; return v.trim() ? { text: v } : null; }
  if (src === 'paperless') return state.print.paperlessId ? { paperless_id: state.print.paperlessId } : null;
  return null;
}

function printBody(extra = {}) {
  const src = printSource();
  if (!src) return null;
  const opts = {
    printer: $('#opt-printer').value || '', copies: state.print.copies, duplex: $('#opt-duplex-wrap').hidden ? 'off' : getDuplex(),
    color: getColor(), quality: getQuality(), media: $('#opt-media').value, pages: $('#opt-pages').value.trim(), ...extra,
  };
  if (src.file) {
    const form = new FormData();
    form.append('file', src.file);
    for (const [k, v] of Object.entries(opts)) form.append(k, String(v));
    return form;
  }
  return { ...opts, source: src };
}

let checkTimer;
function checkPrint() {
  clearTimeout(checkTimer);
  checkTimer = setTimeout(async () => {
    const summary = $('#print-summary');
    const body = printBody({ dry_run: true });
    $('#btn-print').disabled = !body;
    summary.classList.remove('bad');
    if (!body) { summary.textContent = 'Choose something to print.'; return; }
    summary.textContent = 'Checking the document…';
    try {
      const r = await api('POST', '/print', body);
      state.print.check = r;
      const p = r.pages * r.copies;
      summary.replaceChildren(h('b', {}, plural(p, 'page')), ' on ', h('b', {}, plural(r.sheets, 'sheet')), ` of ${mediaLabel(r.media)}`,
        r.document_pages !== r.pages ? ` (of ${r.document_pages} in the document)` : '',
        r.format === 'image/pwg-raster' ? '. Platen converts it for this printer.' : '.');
    } catch (err) {
      $('#btn-print').disabled = true;
      summary.classList.add('bad');
      summary.textContent = err.message;
    }
  }, 350);
}

function setFile(file) {
  state.print.file = file;
  const drop = $('#drop');
  drop.classList.toggle('has-file', !!file);
  $('#drop-title').textContent = file ? file.name : 'Drop a file here';
  $('#drop-hint').textContent = file ? `${bytes(file.size)} · tap to choose another` : 'or tap to choose. You can also paste a picture.';
  checkPrint();
}

async function doPrint(button) {
  await busy(button, async () => {
    let body = printBody();
    if (!body) return;
    let res;
    try {
      res = await api('POST', '/print', body);
    } catch (err) {
      if (err.code !== 'confirmation_required') throw err;
      const d = err.details;
      const ok = await confirmDialog('Use this much paper?', `This job takes ${plural(d.sheets, 'sheet')} of paper (${plural(d.pages, 'page')}, ${plural(d.copies, 'copy', 'copies')}).`, `Print ${plural(d.sheets, 'sheet')}`);
      if (!ok) return;
      body = printBody({ confirm: true });
      res = await api('POST', '/print', body);
    }
    toast(res.message);
    await refreshJobs();
  });
}

let plTimer;
function searchPaperless() {
  clearTimeout(plTimer);
  plTimer = setTimeout(async () => {
    const box = $('#pl-results');
    try {
      const r = await api('GET', '/paperless/documents?limit=8&query=' + encodeURIComponent($('#pl-query').value));
      box.replaceChildren(...(r.documents.length ? r.documents.map((d) => h('div', {
        class: 'row', role: 'option', tabindex: '0', 'aria-selected': String(d.id === state.print.paperlessId),
        onclick: () => { state.print.paperlessId = d.id; searchPaperless(); checkPrint(); },
        onkeydown: (ev) => { if (ev.key === 'Enter' || ev.key === ' ') ev.currentTarget.click(); },
      }, h('div', { class: 'row-icon' }, icon(ICONS.doc)),
        h('div', { style: { 'min-width': '0' } }, h('div', { class: 'row-title' }, d.title), h('div', { class: 'row-meta' }, [d.created, d.page_count && plural(d.page_count, 'page')].filter(Boolean).join(' · '))),
        h('div', {}))) : [h('div', { class: 'empty' }, 'Nothing found.')]));
    } catch (err) {
      box.replaceChildren(h('div', { class: 'empty' }, err.message));
    }
  }, 250);
}

function initPrint() {
  seg($('#print-source'), (source) => {
    state.print.source = source;
    for (const pane of $$('.source')) pane.hidden = pane.dataset.pane !== source;
    if (source === 'paperless') searchPaperless();
    checkPrint();
  });
  $('#file').addEventListener('change', (ev) => setFile(ev.target.files[0] || null));
  const drop = $('#drop');
  for (const type of ['dragenter', 'dragover']) drop.addEventListener(type, (ev) => { ev.preventDefault(); drop.classList.add('over'); });
  for (const type of ['dragleave', 'drop']) drop.addEventListener(type, (ev) => { ev.preventDefault(); drop.classList.remove('over'); });
  drop.addEventListener('drop', (ev) => { if (ev.dataTransfer.files[0]) setFile(ev.dataTransfer.files[0]); });
  document.addEventListener('paste', (ev) => {
    if (location.hash !== '#/print') return;
    const file = [...(ev.clipboardData?.files || [])][0];
    if (file) setFile(file);
  });
  for (const id of ['print-url', 'print-text', 'opt-pages']) $('#' + id).addEventListener('input', checkPrint);
  for (const id of ['opt-printer', 'opt-media']) $('#' + id).addEventListener('change', () => { if (id === 'opt-printer') renderPrintOptions(); checkPrint(); });
  $('#pl-query').addEventListener('input', searchPaperless);
  const copies = (d) => {
    const max = state.info?.limits.max_copies || 10;
    state.print.copies = Math.min(max, Math.max(1, state.print.copies + d));
    $('#copies').textContent = state.print.copies;
    checkPrint();
  };
  $('#copies-minus').addEventListener('click', () => copies(-1));
  $('#copies-plus').addEventListener('click', () => copies(1));
  $('#btn-print').addEventListener('click', (ev) => doPrint(ev.currentTarget));
}

// ---- scan --------------------------------------------------------------------

const getScanColor = seg($('#scan-color'));
const getScanDpi = seg($('#scan-dpi'));

function showPage(pageId) {
  state.page = pageId;
  const img = $('#stage-img');
  const sc = state.scan;
  const has = sc && pageId;
  img.hidden = !has;
  $('#stage-empty').hidden = !!has;
  const src = has ? `/api/v1/scans/${sc.id}/pages/${pageId}/image?size=1400` : '';
  if (has && img.getAttribute('src') !== src) img.src = src;
  for (const t of $$('.thumb')) t.setAttribute('aria-current', String(t.dataset.page === pageId));
}

function renderScan() {
  const sc = state.scan;
  const strip = $('#strip');
  const pages = sc?.pages || [];
  strip.replaceChildren(...pages.map((p, i) => h('div', { class: 'thumb', 'data-page': p.id, 'aria-current': String(p.id === state.page), onclick: () => showPage(p.id) },
    h('img', { src: `/api/v1/scans/${sc.id}/pages/${p.id}/image?size=480`, alt: `Page ${i + 1}` }),
    h('span', { class: 'thumb-n' }, String(i + 1)),
    h('div', { class: 'thumb-tools' },
      h('button', { type: 'button', title: 'Move earlier', disabled: i === 0, onclick: (ev) => { ev.stopPropagation(); movePage(p.id, i - 1); } }, '‹'),
      h('button', { type: 'button', class: 'del', title: 'Remove this page', onclick: (ev) => { ev.stopPropagation(); removePage(p.id); } }, '×'),
      h('button', { type: 'button', title: 'Move later', disabled: i === pages.length - 1, onclick: (ev) => { ev.stopPropagation(); movePage(p.id, i + 1); } }, '›')))));
  strip.hidden = pages.length === 0;
  showPage(pages.some((p) => p.id === state.page) ? state.page : pages.at(-1)?.id || null);
  $('#btn-scan').textContent = pages.length ? 'Add a page' : 'Scan';
  $('#scan-after').hidden = pages.length === 0;
  $('#btn-download').href = sc ? `/api/v1/scans/${sc.id}/document` : '#';
  $('#btn-file').hidden = !state.info?.paperless;
  $('#btn-copy').hidden = state.printers.length === 0;
  for (const el of [...$$('#scan-color button'), ...$$('#scan-dpi button'), $('#scan-paper')]) el.disabled = pages.length > 0;
  const note = $('#scan-note');
  const filed = sc?.paperless;
  note.hidden = !filed;
  if (filed) {
    note.replaceChildren(filed.status === 'success' ? 'Filed in Paperless. ' : filed.status === 'failure' ? `Paperless refused it: ${filed.message} ` : 'Paperless is importing it… ',
      filed.url ? h('a', { href: filed.url, target: '_blank', rel: 'noopener' }, 'Open the document') : '');
  }
}

async function doScan(button) {
  await busy(button, async () => {
    $('#beam').hidden = false;
    try {
      if (!state.scan) {
        state.scan = await api('POST', '/scans', {
          scanner: $('#scan-scanner').value || '', color: getScanColor(), resolution: Number(getScanDpi()), paper: $('#scan-paper').value, title: $('#scan-title').value.trim(),
        });
      } else {
        state.scan = await api('POST', `/scans/${state.scan.id}/pages`);
      }
      state.page = state.scan.pages.at(-1)?.id;
      history.replaceState(null, '', `#/scan/${state.scan.id}`);
    } finally {
      $('#beam').hidden = true;
    }
    renderScan();
    refreshScans();
  });
}

async function removePage(pageId) {
  try {
    state.scan = await api('DELETE', `/scans/${state.scan.id}/pages/${pageId}`);
    renderScan();
  } catch (err) { toast(err.message, { bad: true }); }
}

async function movePage(pageId, to) {
  try {
    state.scan = await api('POST', `/scans/${state.scan.id}/pages/${pageId}/move`, { to });
    renderScan();
  } catch (err) { toast(err.message, { bad: true }); }
}

async function saveTitle() {
  const sc = state.scan;
  const title = $('#scan-title').value.trim();
  if (sc && title !== (sc.title || '')) state.scan = await api('PATCH', `/scans/${sc.id}`, { title });
}

async function fileScan(button) {
  const dlg = $('#dlg-file');
  if (!state.meta) state.meta = await api('GET', '/paperless').catch(() => null);
  const meta = state.meta || { tags: [], correspondents: [], document_types: [] };
  $('#file-title').value = $('#scan-title').value.trim();
  const fill = (id, list) => $(id).replaceChildren(...list.map((x) => h('option', { value: x.name })));
  fill('#list-tags', meta.tags); fill('#list-correspondents', meta.correspondents); fill('#list-types', meta.document_types);
  $('#tag-suggest').replaceChildren(...meta.tags.filter((t) => !t.name.startsWith('paperless-gpt')).slice(0, 12).map((t) => h('button', {
    type: 'button', class: 'chip', onclick: () => {
      const input = $('#file-tags');
      const have = input.value.split(',').map((s) => s.trim()).filter(Boolean);
      if (!have.includes(t.name)) input.value = [...have, t.name].join(', ');
    },
  }, t.name)));
  dlg.returnValue = 'cancel';
  dlg.showModal();
  const ok = await new Promise((resolve) => dlg.addEventListener('close', () => resolve(dlg.returnValue === 'ok'), { once: true }));
  if (!ok) return;
  await busy(button, async () => {
    await saveTitle();
    state.scan = await api('POST', `/scans/${state.scan.id}/paperless`, {
      title: $('#file-title').value.trim(), tags: $('#file-tags').value.split(',').map((s) => s.trim()).filter(Boolean),
      correspondent: $('#file-correspondent').value.trim(), document_type: $('#file-type').value.trim(), created: $('#file-date').value, wait: true,
    });
    const f = state.scan.paperless;
    if (f?.status === 'success') toast('Filed in Paperless.', { link: f.url && { href: f.url, text: 'Open' } });
    else toast(f?.message || 'Paperless did not confirm the import yet.', { bad: f?.status === 'failure' });
    state.meta = null;
    renderScan();
    refreshScans();
  });
}

async function copyScan(button) {
  await busy(button, async () => {
    const gray = state.scan.color === 'gray';
    let body = { color: gray ? 'monochrome' : 'auto' };
    let res;
    try { res = await api('POST', `/scans/${state.scan.id}/print`, body); }
    catch (err) {
      if (err.code !== 'confirmation_required') throw err;
      const ok = await confirmDialog('Use this much paper?', `The copy takes ${plural(err.details.sheets, 'sheet')} of paper.`, 'Print');
      if (!ok) return;
      res = await api('POST', `/scans/${state.scan.id}/print`, { ...body, confirm: true });
    }
    toast(res.message);
    refreshJobs();
  });
}

function resetScan() {
  state.scan = null; state.page = null;
  $('#scan-title').value = '';
  history.replaceState(null, '', '#/scan');
  renderScan();
}

function renderScanners() {
  const sel = $('#scan-scanner');
  sel.replaceChildren(...state.scanners.map((s) => h('option', { value: s.id, selected: s.default }, s.name)));
  $('#scan-scanner-wrap').hidden = state.scanners.length < 2;
  const s = state.scanners.find((x) => x.default) || state.scanners[0];
  for (const b of $$('#scan-dpi button')) b.hidden = !!s?.resolutions?.length && !s.resolutions.includes(Number(b.dataset.v));
  $('#btn-scan').disabled = !s?.online;
}

function initScan() {
  $('#btn-scan').addEventListener('click', (ev) => doScan(ev.currentTarget));
  $('#btn-file').addEventListener('click', (ev) => fileScan(ev.currentTarget).catch((err) => toast(err.message, { bad: true })));
  $('#btn-copy').addEventListener('click', (ev) => copyScan(ev.currentTarget));
  $('#btn-newscan').addEventListener('click', resetScan);
  $('#scan-title').addEventListener('change', () => saveTitle().catch((err) => toast(err.message, { bad: true })));
  $('#btn-download').addEventListener('click', () => { saveTitle().catch(() => {}); });
}

// ---- data --------------------------------------------------------------------

async function refreshDevices() {
  const [p, s] = await Promise.all([api('GET', '/printers'), api('GET', '/scanners')]);
  state.printers = p.printers; state.scanners = s.scanners;
  renderHome(); renderPrintOptions(); renderScanners();
}
async function refreshJobs() { state.jobs = (await api('GET', '/jobs?limit=30')).jobs; renderHome(); renderActivity(); }
async function refreshScans() { state.scans = (await api('GET', '/scans?limit=30')).scans || []; renderHome(); renderActivity(); }

function listen() {
  const es = new EventSource('/api/v1/events');
  let jobTimer, scanTimer;
  es.addEventListener('job', () => { clearTimeout(jobTimer); jobTimer = setTimeout(() => { refreshJobs(); refreshDevices(); }, 200); });
  const onScan = (ev) => {
    const e = JSON.parse(ev.data);
    if (state.scan && e.id === state.scan.id && e.data) { state.scan = e.data; renderScan(); }
    clearTimeout(scanTimer); scanTimer = setTimeout(refreshScans, 200);
  };
  es.addEventListener('scan', onScan);
  es.addEventListener('scan.deleted', () => refreshScans());
}

// ---- routing -----------------------------------------------------------------

async function route() {
  const [, view = 'home', arg] = location.hash.split('/');
  const name = ['print', 'scan', 'activity'].includes(view) ? view : 'home';
  for (const s of $$('.view')) s.hidden = s.id !== `view-${name}`;
  for (const a of $$('[data-view]')) {
    if (a.dataset.view === name) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
  document.title = name === 'home' ? 'Platen' : `${name[0].toUpperCase()}${name.slice(1)} · Platen`;
  if (name === 'scan' && arg && arg !== state.scan?.id) {
    try {
      state.scan = await api('GET', `/scans/${arg}`);
      state.page = state.scan.pages.at(-1)?.id || null;
      $('#scan-title').value = state.scan.title || '';
    } catch { state.scan = null; }
  }
  if (name === 'scan') renderScan();
  if (name === 'print') checkPrint();
  window.scrollTo(0, 0);
}

function initChrome() {
  const root = document.documentElement;
  const saved = localStorage.getItem('platen-theme');
  if (saved) root.dataset.theme = saved;
  $('#btn-theme').addEventListener('click', () => {
    const dark = root.dataset.theme ? root.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
    root.dataset.theme = dark ? 'light' : 'dark';
    localStorage.setItem('platen-theme', root.dataset.theme);
  });
  $('#btn-connect').addEventListener('click', () => {
    const i = state.info;
    const auth = i.auth ? ' \\\n  --header "Authorization: Bearer <token>"' : '';
    $('#mcp-url').textContent = i.mcp_url;
    $('#mcp-claude').textContent = `claude mcp add --transport http platen ${i.mcp_url}${auth}`;
    $('#api-curl').textContent = `curl ${i.auth ? '-H "Authorization: Bearer <token>" ' : ''}-F file=@document.pdf -F copies=1 ${i.base_url}/api/v1/print`;
    $('#dlg-connect').showModal();
  });
}

async function main() {
  initChrome(); initPrint(); initScan();
  window.addEventListener('hashchange', route);
  try {
    state.info = await api('GET', '/info');
    await Promise.all([refreshDevices(), refreshJobs(), refreshScans()]);
    if (state.info.paperless) api('GET', '/paperless').then((m) => { state.meta = m; renderHome(); }).catch(() => {});
  } catch (err) {
    toast(err.message, { bad: true });
  }
  await route();
  listen();
  setInterval(() => { if (!document.hidden) refreshDevices().catch(() => {}); }, 15000);
}

main();
