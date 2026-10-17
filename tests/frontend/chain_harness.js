// Стенд «собрать цепочку»: applyPreset() из редактора переносит сценарий в
// state.doc. Исполняется НАСТОЯЩАЯ функция из web/static/editor/app.js против
// замоканного DOM — не копия логики.
//
//     node tests/frontend/chain_harness.js [путь-к-репозиторию]
//
// Что здесь закрывается. applyPreset — второй путь в state.doc (первый —
// openFile). Он молча переносил не все поля сценария: network и secrets в
// документ не попадали. Проверено на живом GUI: «собрать цепочку» из
// llm_same_provider.yaml давал файл вообще без `secrets:` — валидатор писал
// про это предупреждением, файл сохранялся, и пайплайн падал уже на запуске
// с secrets_missing. Сетевая политика при этом превращалась в запрет
// (пустое поле = deny), и набор сценариев, чьи плагины заявляют сеть, из
// сборщика выпадал целиком.
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const REPO = process.argv[2] || process.cwd();
const SRC = path.join(REPO, 'web/static/editor/app.js');

const RECT = { left: 0, top: 0, right: 2600, bottom: 1700, width: 2600, height: 1700 };
let uid = 0;
const POS = {};

function box(left, top, width, height) {
  return { left, top, right: left + width, bottom: top + height, width, height };
}

class El {
  constructor(tag) {
    this.tagName = (tag || 'div').toUpperCase();
    this.children = [];
    this.parent = null;
    this._h = {};
    this._rect = box(0, 0, 800, 40);
    this._id = ++uid;
    this.dataset = {};
    this.style = {};
    this._s = new Set();
    this.classList = {
      _s: this._s,
      add(...c) { c.forEach(x => this._s.add(x)); },
      remove(...c) { c.forEach(x => this._s.delete(x)); },
      toggle(c, on) { on === undefined ? (this._s.has(c) ? this._s.delete(c) : this._s.add(c)) : (on ? this._s.add(c) : this._s.delete(c)); },
      contains(c) { return this._s.has(c); },
    };
  }
  get className() { return [...this._s].join(' '); }
  set className(v) {
    this._s = new Set(String(v).split(/\s+/).filter(Boolean));
    if (this.classList) this.classList._s = this._s;
  }
  get textContent() { return this._t || ''; }
  set textContent(v) { this._t = String(v); this.children = []; }
  set innerHTML(v) { this._html = String(v); this.children = []; this._parsed = null; }
  get innerHTML() { return this._html || ''; }
  appendChild(c) { c.parent = this; this.children.push(c); this._html = null; return c; }
  remove() {
    if (this.parent) this.parent.children = this.parent.children.filter(x => x !== this);
    this.parent = null;
  }
  setAttribute(k, v) { this[k] = v; }
  getAttribute(k) { return this[k]; }
  removeAttribute(k) { delete this[k]; }
  querySelectorAll(sel) {
    const out = [];
    const kids = el => (el._kids ? el._kids() : el.children);
    const walk = el => { for (const c of kids(el)) { if (matchSel(c, sel)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  addEventListener(t, f) { (this._h[t] = this._h[t] || []).push(f); }
  removeEventListener(t, f) { if (this._h[t]) this._h[t] = this._h[t].filter(x => x !== f); }
  getBoundingClientRect() { return this._rect; }
  closest(sel) { return walkUp(this, el => matchSel(el, sel)); }
  fire(type, ev = {}) {
    const base = { type, stopPropagation() {}, preventDefault() {}, target: this, ...ev };
    for (const f of this._h[type] || []) f(base);
  }
  _kids() {
    if (this.children.length) return this.children;
    if (this._parsed) return this._parsed;
    const stack = [this];
    const out = [];
    const re = /<(\/?)([\w-]+)((?:[^>"']|"[^"]*"|'[^']*')*?)(\/?)>|([^<]+)/g;
    let m;
    while ((m = re.exec(this._html || ''))) {
      if (m[5] !== undefined && m[5].trim()) continue;
      if (m[1] === '/') { if (stack.length > 1) stack.pop(); continue; }
      const el = new El(m[2]);
      el.parent = stack[stack.length - 1];
      applyAttrs(el, m[3] || '');
      out.push(el);
      if (!m[4]) stack.push(el);
    }
    const pos = POS[this.dataset.id] || null;
    const px = pos ? pos[0] : 0, py = pos ? pos[1] : 0;
    let row = 0, chip = 0;
    for (const el of out) {
      const c = el.className;
      let b;
      if (c.includes('nh')) b = box(0, 0, 300, 24);
      else if (c.includes('inrow')) { b = box(2, 50 + row * 22, 296, 22); row++; }
      else if (c.includes('outchip')) { b = box(2, 130 + chip * 20, 80, 18); chip++; }
      else if (c.includes('src')) b = box(180, 50 + (row ? row - 1 : 0) * 22, 116, 20);
      else b = box(2, 130 + chip * 20, 296, 20);
      el._rect = box(b.left + px, b.top + py, b.width, b.height);
    }
    if (pos) this._rect = box(px, py, 300, 210);
    this._parsed = out;
    return out;
  }
}
function applyAttrs(el, s) {
  const cls = /class="([^"]*)"/.exec(s);
  if (cls) el.className = cls[1];
  const ds = /data-([\w-]+)="([^"]*)"/.exec(s);
  if (ds) el.dataset[ds[1]] = ds[2];
}
function walkUp(el, pred) { for (let e = el; e; e = e.parent) if (pred(e)) return e; return null; }
function matchSimple(el, sel) {
  const idm = /^#([\w-]+)$/.exec(sel);
  if (idm) return el.id === idm[1];
  const m = /^([\w-]+)?((?:\.[\w-]+|\[[^\]]+\])*)$/.exec(sel);
  if (!m) return false;
  if (m[1] && el.tagName !== m[1].toUpperCase()) return false;
  for (const tk of (m[2].match(/\.[\w-]+|\[[^\]]+\]/g) || [])) {
    if (tk.startsWith('.')) { if (!el.classList.contains(tk.slice(1))) return false; }
    else {
      const a = /^\[([\w-]+)(?:="([^"]*)")?\]$/.exec(tk);
      if (!a) return false;
      const key = a[1].replace(/^data-/, '').replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      if (el.dataset[key] === undefined) return false;
      if (a[2] !== undefined && el.dataset[key] !== a[2]) return false;
    }
  }
  return true;
}
function matchSel(el, sel) {
  for (const union of sel.split(',').map(x => x.trim()).filter(Boolean)) {
    const parts = union.split(/\s+/).filter(Boolean);
    if (!parts.length) continue;
    if (!matchSimple(el, parts[parts.length - 1])) continue;
    let node = el.parent, i = parts.length - 2, hit = true;
    while (i >= 0) {
      let found = false;
      while (node) { if (matchSimple(node, parts[i])) { found = true; node = node.parent; break; } node = node.parent; }
      if (!found) { hit = false; break; }
      i--;
    }
    if (hit) return true;
  }
  return false;
}

// ── DOM: неизвестный #id создаётся на лету. Иначе renderAll() упирается в
// null на элементе, который в живом интерфейсе есть, и стенд падает не по
// существу.
const byId = {};
for (const id of ['canvas', 'edges', 'props', 'palette', 'ctxmenu', 'notes', 'note-layer',
  'file-name', 'file-open', 'file-list', 'btn-save', 'btn-run', 'btn-yaml', 'close-yaml',
  'yaml-view', 'btn-undo', 'btn-redo', 'canvas-hint', 'errs', 'toast', 'palette-hint',
  'steps-count', 'instrip', 'inlayer', 'app', 'val-badge', 'val-err', 'p-file', 'p-name',
  'p-steps', 'p-input', 'p-foreach', 'p-net', 'p-sec', 'p-gates', 'layer-inputs',
  'layer-pipeline', 'btn-save-as']) {
  const e = new El(id.startsWith('btn') || id.includes('file') ? 'button' : 'div');
  e.id = id;
  e._rect = id === 'canvas' ? RECT : box(0, 0, 800, 40);
  byId[id] = e;
}
const doc = new El('body');
doc.getElementById = id => byId[id] || null;
doc.createElement = t => new El(t);
doc.createElementNS = (ns, t) => new El(t);
doc.addEventListener = () => {};
doc.removeEventListener = () => {};
doc.elementFromPoint = () => null;
doc.querySelector = s => {
  const m = /^#([\w-]+)$/.exec(s);
  if (m) {
    if (!byId[m[1]]) { byId[m[1]] = new El('div'); byId[m[1]].id = m[1]; }
    return byId[m[1]];
  }
  return byId.canvas.querySelector(s);
};
doc.querySelectorAll = s => {
  const m = /^#([\w-]+)$/.exec(s);
  return m ? (byId[m[1]] ? [byId[m[1]]] : []) : byId.canvas.querySelectorAll(s);
};

const PLUGINS = [
  { id: 'plugins/official/llm_gemini', input: { prompt: {}, system: {} }, output: { text: {} },
    permissions: { secrets: ['GEMINI_API_KEY'], network: [{ host: '*', port: 443 }] } },
  { id: 'core/human_gate', input: {}, output: {} },
  { id: 'plugins/community/word_freq', input: { text: {} }, output: { words: {}, lines: {} } },
];
const ctx = vm.createContext({
  window: {
    wedraSession: async () => true,
    location: { href: 'http://x/editor/', search: '', origin: 'http://x' },
    addEventListener: () => {}, removeEventListener: () => {},
    innerWidth: 1600, innerHeight: 900,
  },
  document: doc, console, setTimeout: () => 0, clearTimeout: () => {},
  fetch: async () => ({
    ok: true, status: 200,
    headers: { get: () => 'application/json' },
    json: async () => ({}), text: async () => '',
  }),
  location: { href: 'http://x/editor/', search: '', origin: 'http://x' },
  confirm: () => true, alert: () => {}, wedraSession: async () => true,
  Date, Math, JSON, Object, Array, String, Number, Boolean, RegExp, Error,
});
vm.runInContext(fs.readFileSync(SRC, 'utf8'), ctx, { filename: 'app.js' });
const ed = vm.runInContext('({state, applyPreset, renderAll, setLayer})', ctx);

let pass = 0, fail = 0;
function ok(name, cond, extra) {
  if (cond) { pass++; console.log('  OK   ' + name); }
  else { fail++; console.log('  FAIL ' + name + (extra ? '  -> ' + extra : '')); }
}

function freshDoc() {
  ed.state.doc = {
    name: 'new_pipeline', file: 'new_pipeline.yaml', format_version: '', input: [],
    steps: [], secrets: [], network: '', gates: '',
    foreach: '', foreach_item: '', item_type: '', item_format: '',
  };
  ed.state.unsupported = [];
  ed.state.plugins = PLUGINS;
  ed.state.doc.input = [{ name: 'topic', default: 'арбузы', typed: false, type: '', has_default: true }];
}

// Сценарий из реестра: сеть разрешена, объявлен ключ, цикла нет.
const LLM = {
  name: 'llm_same_provider', network: 'allow', secrets: ['GEMINI_API_KEY'], gates: 'human_only',
  unsupported: [],
  steps: [
    { id: 'draft', plugin: 'plugins/official/llm_gemini', pos: [0, 0], on_error: 'retry', bind: { system: 'input.draft_system' }, retry: { attempts: 3, delay: '2s', backoff: 'exponential' }, timeout: '45s' },
    { id: 'review', plugin: 'core/human_gate', pos: [0, 0], on_error: 'stop', form: [{ field: 'steps.draft.text', editable: true, type: 'string' }], actions: ['accept', 'reject'], on_reject: 'stop' },
  ],
};

async function main() {
  console.log('сборщик цепочки: applyPreset должен переносить сценарий целиком');

  // --- 1. сценарий с сетью и ключом ---
  freshDoc();
  await ed.applyPreset('llm_same_provider.yaml', 'topic', { file: 'llm_same_provider.yaml', field: 'prompt', doc: JSON.parse(JSON.stringify(LLM)) });
  ok('шаги сценария перенесены', ed.state.doc.steps.length === 2, String(ed.state.doc.steps.length));
  ok('вход привязан к первому шагу', ed.state.doc.steps[0].bind.prompt === 'input.topic',
    JSON.stringify(ed.state.doc.steps[0].bind));
  ok('network сценария перенесён', ed.state.doc.network === 'allow', JSON.stringify(ed.state.doc.network));
  ok('secrets сценария перенесены', JSON.stringify(ed.state.doc.secrets) === '["GEMINI_API_KEY"]',
    JSON.stringify(ed.state.doc.secrets));
  ok('gates сценария перенесены', ed.state.doc.gates === 'human_only', JSON.stringify(ed.state.doc.gates));
  // перенос должен быть виден человеку, а не только в состоянии
  const noteAfterLLM = byId.note.textContent;
  ok('подпись на холсте называет перенесённую сеть', /сеть сценария: allow/.test(noteAfterLLM), JSON.stringify(noteAfterLLM));
  ok('подпись на холсте называет перенесённый ключ', /GEMINI_API_KEY/.test(noteAfterLLM), JSON.stringify(noteAfterLLM));

  // массив приходит из разбора документа — он не должен быть тем же самым
  // объектом: правка чипов в UI не имеет права дописать ключ в сценарий
  const before = JSON.stringify(ed.state.doc.secrets);
  ed.state.doc.secrets.push('ПОСТОРONНИЙ');
  ok('массив secrets — копия, исходный сценарий не затронут',
    JSON.stringify(LLM.secrets) === '["GEMINI_API_KEY"]', JSON.stringify(LLM.secrets));
  ed.state.doc.secrets = JSON.parse(before);

  // --- 2. сценарий без сети и без ключей: значения обнуляются, а не наследуются
  // от прежнего документа. Иначе плагин, которому сеть не нужна, получил бы
  // разрешение от чужого сценария.
  freshDoc();
  ed.state.doc.network = 'allow';
  ed.state.doc.secrets = ['STALE_KEY'];
  await ed.applyPreset('plain.yaml', 'topic', {
    file: 'plain.yaml', field: 'text', doc: {
      name: 'plain', network: '', secrets: [], gates: '', unsupported: [],
      steps: [{ id: 'freq', plugin: 'plugins/community/word_freq', pos: [0, 0], on_error: 'stop', bind: { text: 'input.topic' } }],
    },
  });
  ok('сеть прежнего документа не унаследована', ed.state.doc.network === '', JSON.stringify(ed.state.doc.network));
  ok('ключи прежнего документа не унаследованы', ed.state.doc.secrets.length === 0,
    JSON.stringify(ed.state.doc.secrets));

  // --- 3. цикл сценария (после v0.28) — регрессия, чтобы не сломать снова ---
  freshDoc();
  await ed.applyPreset('csv_foreach.yaml', 'topic', {
    file: 'csv_foreach.yaml', field: 'text', doc: {
      name: 'csv_foreach', network: '', secrets: [], gates: '', unsupported: [],
      foreach: 'input.rows', foreach_item: 'row', item_type: 'object', item_format: '',
      steps: [{ id: 'load', plugin: 'plugins/community/word_freq', pos: [0, 0], on_error: 'stop', bind: { text: 'input.row.name' } }],
    },
  });
  ok('foreach сценария перенесён', ed.state.doc.foreach === 'input.rows', JSON.stringify(ed.state.doc.foreach));
  ok('foreach_item сценария перенесён', ed.state.doc.foreach_item === 'row', JSON.stringify(ed.state.doc.foreach_item));
  ok('item_type сценария перенесён', ed.state.doc.item_type === 'object', JSON.stringify(ed.state.doc.item_type));
  ok('item_format сценария перенесён', ed.state.doc.item_format === '', JSON.stringify(ed.state.doc.item_format));

  // --- 4. поле, которым редактор не управляет, обязано запретить сохранение ---
  // save() отказывается писать файл при непустом state.unsupported. Сборщик
  // строит документ из разобранного YAML: не перенеся пометку, он получил бы
  // файл без этого поля — ровно та потеря, ради которой пометка существует.
  freshDoc();
  await ed.applyPreset('exotic.yaml', 'topic', {
    file: 'exotic.yaml', field: 'text', doc: {
      name: 'exotic', network: '', secrets: [], gates: '',
      unsupported: ['pipeline.exotic_mode'],
      steps: [{ id: 'freq', plugin: 'plugins/community/word_freq', pos: [0, 0], on_error: 'stop', bind: { text: 'input.topic' } }],
    },
  });
  ok('неуправляемое поле сценария доехало до state.unsupported',
    Array.isArray(ed.state.unsupported) && ed.state.unsupported.includes('pipeline.exotic_mode'),
    JSON.stringify(ed.state.unsupported));
  ok('повторная сборка не плодит дубли пометок',
    (await (async () => {
      await ed.applyPreset('exotic.yaml', 'topic', {
        file: 'exotic.yaml', field: 'text', doc: {
          name: 'exotic', network: '', secrets: [], gates: '',
          unsupported: ['pipeline.exotic_mode'],
          steps: [{ id: 'freq', plugin: 'plugins/community/word_freq', pos: [0, 0], on_error: 'stop', bind: { text: 'input.topic' } }],
        },
      });
      return ed.state.unsupported.filter(x => x === 'pipeline.exotic_mode').length;
    })()) === 1, JSON.stringify(ed.state.unsupported));

  // --- 5. слой после сборки — пайплайн, а не входы; перенос виден человеку ---
  ok('сборка оставила слой «пайплайн»', ed.state.layer === 'pipeline', ed.state.layer);

  console.log('\n' + (fail ? `провалено ${fail}, ` : '') + `пройдено ${pass}`);
  console.log(fail ? '\nСТЕНД НЕ ЗЕЛЕНЫЙ' : '\nвсе проверки пройдены');
  process.exit(fail ? 1 : 0);
}

main().catch(e => { console.error('СТЕНД УПАЛ:', e && e.stack || e); process.exit(1); });