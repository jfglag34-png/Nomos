// v0.25 — редактор пайплайнов: палитра → холст (сетка 20px), bind-связи,
// undo/redo, валидация и сериализация через ядро (Go), сохранение PUT.
// v0.27: when — условие шага (path/op/value, 10 операторов ядра).
// v0.28: foreach/parallel_group/after_foreach на шаге + foreach-батч
// пайплайна (foreach/foreach_item/item_type/item_format).
// v0.29: retry (on_error: retry + retry{attempts, delay, backoff}).
// v0.30: input schema (type/required/default/format/description), step loops,
// round-trip всех полей цикла и работающий Save As по имени файла.
// v0.5: secrets (pipeline.secrets — env-ключи плагинов) в UI (чипы в
// блоке «Пайплайн» + подсказка в шаге, какой ключ просит плагин).
// v0.6: network (pipeline.network — политика allow/deny) в UI: чекбокс
// «deny» + подсказка в шаге, какую сеть просит плагин (и не запрещено ли).
// v0.8a: перетаскивание палитра→холст на mouse-событиях (ghost) + клик =
// добавить на свободное место. Нативный HTML5 DnD не работает в sandboxed
// iframe (превью) — mouse-события работают везде.
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"'`]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;','`':'&#96;'}[c]));
const inputText = value => value === null || Array.isArray(value) || (typeof value === 'object')
  ? JSON.stringify(value, null, 2) : (value === undefined ? '' : String(value));
const parseInputText = (text, current, expectedType = '') => {
  if (expectedType === 'array' || (!expectedType && Array.isArray(current))) {
    const value = JSON.parse(text);
    if (!Array.isArray(value)) throw new Error('ожидается массив JSON');
    return value;
  }
  if (expectedType === 'object' || (!expectedType && current !== null && typeof current === 'object')) {
    const value = JSON.parse(text);
    if (value === null || Array.isArray(value) || typeof value !== 'object') throw new Error('ожидается объект JSON');
    return value;
  }
  if (expectedType === 'number' || (!expectedType && typeof current === 'number')) {
    const value = Number(text);
    if (text.trim() === '' || !Number.isFinite(value)) throw new Error('ожидается число');
    return value;
  }
  if (expectedType === 'boolean' || (!expectedType && typeof current === 'boolean')) {
    if (text !== 'true' && text !== 'false') throw new Error('ожидается true или false');
    return text === 'true';
  }
  if (expectedType === 'string') return text;
  if (current === null) return JSON.parse(text);
  return text;
};
const INPUT_TYPES = ['string', 'number', 'boolean', 'array', 'object'];
const defaultForInputType = type => ({string: '', number: 0, boolean: false, array: [], object: {}}[type] ?? '');
const valueMatchesInputType = (value, type) => {
  if (type === 'array') return Array.isArray(value);
  if (type === 'object') return value !== null && typeof value === 'object' && !Array.isArray(value);
  if (type === 'number') return typeof value === 'number' && Number.isFinite(value);
  return typeof value === type;
};
const inputValueEditor = (input, idx) => {
  const value = input.default;
  const type = input.typed ? input.type : '';
  const jsonValue = value === null || Array.isArray(value) || typeof value === 'object';
  if (type === 'array' || type === 'object' || (!type && jsonValue)) {
    return `<textarea data-indef="${idx}" rows="3" spellcheck="false">${esc(inputText(value))}</textarea>`;
  }
  if (type === 'boolean' || (!type && typeof value === 'boolean')) {
    const valid = typeof value === 'boolean';
    return `<select data-indef="${idx}">${valid ? '' : '<option value="" selected disabled>исправь default</option>'}<option value="true"${value === true ? ' selected' : ''}>true</option><option value="false"${value === false ? ' selected' : ''}>false</option></select>`;
  }
  const number = type === 'number' || (!type && typeof value === 'number');
  const display = type === 'string' && typeof value !== 'string' && value != null ? JSON.stringify(value) : inputText(value);
  return `<input data-indef="${idx}" type="${number ? 'number' : 'text'}"${number ? ' step="any"' : ''} value="${esc(display)}" placeholder="значение по умолчанию" spellcheck="false"/>`;
};
const inputEditor = (input, idx) => {
  const typeOpts = INPUT_TYPES.map(v => `<option value="${v}"${input.type === v ? ' selected' : ''}>${v}</option>`).join('');
  const required = input.required === true ? 'true' : input.required === false ? 'false' : '';
  const mismatch = input.typed && input.has_default && !valueMatchesInputType(input.default, input.type);
  const value = input.has_default ? `<div class="input-value">${inputValueEditor(input, idx)}</div>` : '';
  return `<div class="input-editor">
    <div class="input-row">
      <input data-iname="${idx}" value="${esc(input.name)}" placeholder="имя" spellcheck="false"/>
      <select data-ikind="${idx}" title="формат входа"><option value="value"${input.typed ? '' : ' selected'}>значение</option><option value="schema"${input.typed ? ' selected' : ''}>схема</option></select>
      <button data-indel="${idx}" title="удалить вход">×</button>
    </div>
    ${input.typed ? `<div class="input-schema">
      <div class="input-meta-grid">
        <label>тип<select data-itype="${idx}">${typeOpts}</select></label>
        <label>обязательность<select data-irequired="${idx}">
          <option value=""${required === '' ? ' selected' : ''}>не задана</option>
          <option value="true"${required === 'true' ? ' selected' : ''}>обязательный</option>
          <option value="false"${required === 'false' ? ' selected' : ''}>необязательный</option>
        </select></label>
      </div>
      <div class="input-meta-grid">
        <label>format<input data-iformat="${idx}" value="${esc(input.format || '')}" placeholder="email, url…" spellcheck="false"/></label>
        <label>описание<input data-idescription="${idx}" value="${esc(input.description || '')}" placeholder="для чего поле" spellcheck="false"/></label>
      </div>
      <label class="input-default-toggle"><input type="checkbox" data-ihasdefault="${idx}"${input.has_default ? ' checked' : ''}/> значение по умолчанию</label>
      ${value}
      ${mismatch ? `<div class="hint" style="color:var(--err)">default не соответствует type: ${esc(input.type)} — исправь значение или тип.</div>` : ''}
    </div>` : value}
  </div>`;
};
const GRID = 20;
const snap = v => Math.round(v / GRID) * GRID;

let state = {
  plugins: [],
  doc: { name: 'new_pipeline', file: 'new_pipeline.yaml', format_version: '', input: [], steps: [],
    secrets: [], network: '', gates: '',
    foreach: '', foreach_item: '', item_type: '', item_format: '' },
  unsupported: [],
  sel: null,
  // Связь хранится ГОТОВЫМ значением bind, а не парой {step, field}: источником
  // может быть не только выход шага (steps.a.lines), но и вход пайплайна
  // (input.photo). С жёсткой формой steps.X.Y артефакт было бы не к чему
  // привязать, и пришлось бы городить отдельный путь.
  link: null, // {value, label, step, field}
  // v0.37: протяжка — {step, field|null, x1, y1, x, y}. field=null означает
  // «зажат узел целиком»: выход выбирается сам, если он один.
  // v0.38: + value/label — для протяжки от карточки артефакта (input.<имя>).
  linkDrag: null,
  // Пояснения на холсте. Живут ОТДЕЛЬНО от state.doc: панель — это подсказка
  // редактора, а не часть пайплайна. В state.doc она попала бы в
  // /api/serialize/pipeline и сломала бы сериализацию — backend трогать нельзя.
  notes: [],
  noteSig: '',        // подпись состояния, под которое собрана авто-панель
  noteOff: '',        // состояние, панель которого человек закрыл руками
  // Сеть, объявленная сценарием и НЕ перенесённая сборщиком цепочки: значит
  // ровно одно — «allow». Живёт рядом с notes, а не в state.doc, потому что
  // это не свойство пайплайна, а напоминание редактора: пока человек не
  // выдал сеть кликом, цепочка без сети, и молчать об этом нельзя.
  netPending: '',
  // Слой редактора: 'inputs' — только входные значения, 'pipeline' — только
  // шаги и связи. Первый экран — входы: с них начинается работа с пайплайном.
  layer: 'inputs',
  palQuery: '',
  undo: [],
  redo: [],
  yaml: '',
  valErrs: [],
  valWarns: [],
  validating: false,
};

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) {
    // H4: 401 — нет сессии человека (cookie выдаётся обменом одноразового кода)
    if (r.status === 401) { sessionOverlay(); throw new Error('нет сессии: нужен одноразовый код из терминала wedra'); }
    throw new Error(typeof body === 'string' ? body : JSON.stringify(body));
  }
  return body;
}
async function apiRaw(path, opts = {}) {
  const r = await fetch(path, opts);
  const text = await r.text();
  if (!r.ok) {
    if (r.status === 401) { sessionOverlay(); throw new Error('нет сессии: нужен одноразовый код из терминала wedra'); }
    throw new Error(text);
  }
  return text;
}

// ── H4: сессия человека ──────────────────────────────────────────────────
// Всё под /api/* (кроме /api/health) требует cookie. Cookie выдаётся обменом
// ОДНОРАЗОВОГО кода, напечатанного в терминале, где запущен wedra. Пока cookie
// нет, редактор показывает поле ввода кода вместо пустого холста: тот же путь,
// что и у `wedra gui --open` (там код едет в ссылке ?c= и ставит cookie сразу).
function sessionOverlay() {
  if (document.getElementById('wedra-session-box')) return;
  const box = document.createElement('div');
  box.id = 'wedra-session-box';
  // стили — в editor/index.html (#wedra-session-box): разметка не тащит цвета в JS
  box.innerHTML =
    '<div class="sbox">' +
    '<h2>Вход в Nomos</h2>' +
    '<p>Код входа одноразовый и напечатан в терминале, где запущен wedra. Он нужен один раз: ' +
    'обменяется на cookie, и редактор перезагрузится сам.</p>' +
    '<input id="wedra-code" placeholder="XXXX-XXXX-XXXX" autocomplete="off"/>' +
    '<div id="wedra-code-err" class="serr"></div>' +
    '<button id="wedra-code-go">Войти</button>' +
    '</div>';
  document.body.appendChild(box);
  const input = box.querySelector('#wedra-code');
  const err = box.querySelector('#wedra-code-err');
  const submit = async () => {
    const code = input.value.trim();
    if (!code) return;
    err.textContent = '';
    const r = await fetch('/api/session', {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({code}),
    });
    if (r.ok) { location.reload(); return; }
    const d = await r.json().catch(() => ({}));
    err.textContent = d.error || 'код не подошёл';
    input.select();
  };
  box.querySelector('#wedra-code-go').onclick = submit;
  input.onkeydown = e => { if (e.key === 'Enter') submit(); };
  input.focus();
}

// wedraSession — можно ли работать. Сессия выключена (--no-session) или уже
// есть — сразу true; иначе показываем поле кода и ждём перезагрузки.
async function wedraSession() {
  let st = null;
  try { st = await (await fetch('/api/session')).json(); } catch (e) { return true; }
  if (!st || !st.required || st.authenticated) return true;
  sessionOverlay();
  return false;
}

// ── manifest-хелперы ─────────────────────────────────────────────────────
// Сопоставление шага пайплайна с плагином из /api/plugins.
//
// Разделители путей приводятся к одному виду: шаг в YAML записан как
// `plugins/community/text_analyzer`, а API на Windows отдаёт `dir` с
// обратными слэшами (`plugins\community\text_analyzer`). Без нормализации
// сравнение не сходилось НИГДЕ — узлы оставались без входов и выходов, списки
// bind были пустыми, то есть редактор на Windows был слепым.
const slash = s => String(s || '').replace(/\\/g, '/');

function pluginInfo(id) {
  if (id === 'core/human_gate') return { id, input: {}, output: {}, description: 'человек в петле' };
  const want = slash(id);
  const tail = want.slice(want.lastIndexOf('/') + 1);
  return state.plugins.find(p => p.id === id || p.id === tail || slash(p.dir).endsWith('/' + want) || slash(p.dir).endsWith('/' + tail)) || null;
}
function inFields(p) {
  const info = pluginInfo(p);
  return info ? Object.keys(info.input || {}) : [];
}
function outFields(p) {
  const info = pluginInfo(p);
  return info ? Object.keys(info.output || {}) : [];
}
// Совместимость выхода источника с полем приёмника: тип должен совпасть, а если
// оба объявили format — совпасть и он. Манифесты типы объявляют (abuseipdb:
// ip: string/ip, days: number), и API их отдаёт, поэтому совпадение проверяется
// на клиенте ДО создания связи, а не после — на валидации ядра.
function outFits(outName, outSpec, inName, inSpec, fromInput) {
  const ot = outSpec && outSpec.type, it = inSpec && inSpec.type;
  if (it && ot && it !== ot) return false;
  const of = outSpec && outSpec.format, inf = inSpec && inSpec.format;
  if (fromInput) {
    // Введённое человеком значение — сам человек и есть источник формата.
    // Проверять format у набранного руками адреса бессмысленно: из-за этого
    // пропадали все пресеты, чей первый шаг ждёт email. Единственное, что
    // оставляем жёстким: файл не подменяется текстом и наоборот.
    return (inf === 'file_ref') === (of === 'file_ref');
  }
  if (inf && of && inf !== of) return false;
  // поле жёстко просит ip — текстовый выход LLM с format=text не подойдёт
  if (inf && !of && inf !== 'text') return false;
  return true;
}

// Выходы источника, годящиеся в это поле, в порядке предпочтения: сначала тем
// же именем (ip→ip), потом формат, потом остальные совместимые.
function compatibleOuts(srcStep, inName, inSpec) {
  const srcInfo = pluginInfo(srcStep.plugin) || {};
  const outs = srcInfo.output || {};
  const names = Object.keys(outs);
  const ok = names.filter(n => outFits(n, outs[n], inName, inSpec));
  ok.sort((a, b) => (a === inName ? -1 : 0) - (b === inName ? -1 : 0));
  return ok;
}
// ── входные узлы ─────────────────────────────────────────────────────────
// Слой ввода слева: значение, которое человек отдаёт пайплайну. Это не шаг и не
// плагин, а запись pipeline.input — плагины уже умеют брать значения из
// input.*, поэтому ядро менять не пришлось. Позиция не хранится: схема input
// это map, и положить туда pos некуда, поэтому раскладка считается по порядку.
const IN_Y0 = 56, IN_STEP = 88;

function inputSpec(i) {
  return { type: i.typed ? (i.type || 'string') : 'string', format: i.format || '' };
}

function isFileInput(i) {
  return i.format === 'file_ref'
    || /\.(jpg|jpeg|png|gif|webp|bmp|tiff?|heic|pdf|csv|json|txt|md|docx?|xlsx?)$/i.test(String(i.default || ''));
}

function freeInputName() {
  let n = state.doc.input.length + 1;
  while (state.doc.input.some(i => i.name === 'field' + n)) n++;
  return 'field' + n;
}

function addInput(kind) {
  pushUndo();
  // typed: false — узел-вход это ЗНАЧЕНИЕ, а не схема. При typed: true сервер
  // пишет в pipeline.input дескриптор {type, default, format}, и тогда
  // input.nick резолвится в объект: ядро отвечает «тип string несовместим с
  // выходом input.nick (object)». Примеры в examples/ так и сделаны —
  // input.dir_before: "testdata/before", голое значение.
  const want = kind === 'file' ? 'file' : (kind === 'number' ? 'count' : 'nick');
  const name = state.doc.input.some(i => i.name === want) ? freeInputName() : want;
  const idx = state.doc.input.length;
  state.doc.input.push(kind === 'file'
    ? { name, default: '', typed: false, type: 'string', format: 'file_ref', has_default: true }
    : kind === 'number'
      ? { name, default: 0, typed: false, type: 'number', has_default: true }
      : { name, default: '', typed: false, type: 'string', has_default: true });
  renderAll();
  note('вход «' + name + '» добавлен — тяни его точку в поле шага');
  // Сразу ставим фокус в поле значения: иначе после «Текст или ник» надо ещё
  // раз кликнуть по узлу, и первый шаг «ввести ник» выглядел как «куда жать».
  const nodes = document.querySelectorAll('#inlayer .inode');
  const f = nodes[idx] && nodes[idx].querySelector('.ival');
  if (f) { f.focus(); f.select && f.select(); }
}

// Значение пишется прямо в узел. Файл сначала заливается на сервер: плагину
// нужен абсолютный путь (PROTOCOL §1 — относительный разрешился бы в
// каталоге плагина, и файл он не нашёл бы).
async function setInputValue(idx, raw) {
  const inp = state.doc.input[idx];
  if (!inp) return;
  if (inp.format === 'file_ref') {
    if (!raw) { pushUndo(); inp.default = ''; inp.has_default = false; renderAll(); return; }
    const fd = new FormData();
    fd.append('file', raw);
    try {
      const a = await api('/api/assets', { method: 'POST', body: fd });
      pushUndo();
      inp.default = a.path;
      inp.has_default = true;
      renderAll();
      note('файл загружен: ' + a.name);
    } catch (e) {
      note('файл не загрузился: ' + (e.message || e), true);
    }
    return;
  }
  pushUndo();
  if (inp.type === 'number') {
    const n = Number(String(raw).replace(',', '.'));
    inp.default = Number.isFinite(n) ? n : 0;
  } else if (inp.type === 'boolean') {
    inp.default = raw === true || raw === 'true';
  } else {
    inp.default = String(raw);
  }
  inp.has_default = true;
  scheduleValidate();
}

function pickFile(idx) {
  const el = document.createElement('input');
  el.type = 'file';
  el.onchange = () => { if (el.files && el.files[0]) setInputValue(idx, el.files[0]); };
  el.click();
}

// «Собрать цепочку» для входа. Раньше на этом месте было меню, которое ничего
// не открывало: слоя входа не существовало, а цепочка не выбиралась. Теперь
// показываются готовые сценарии реестра, и подходящий по типу входа можно
// поставить одним действием — шаги добавятся, а вход свяжется с первым полем.
async function buildChainFor(idx) {
  const inp = state.doc.input[idx];
  if (!inp) return;
  const m = $('#ctxmenu');
  if (!m) return;
  let list = [];
  try {
    const r = await api('/api/presets');
    list = (r.presets || []).filter(p => p.installed);
  } catch (e) {
    note('реестр сценариев недоступен: ' + (e.message || e), true);
    return;
  }
  if (!list.length) { note('в реестре нет сценариев', true); return; }
  m.innerHTML = '<div class="cap">считаю сценарии…</div>';
  m.style.display = 'block';
  const r0 = m.getBoundingClientRect();
  m.style.left = '120px';
  m.style.top = '120px';

  // Для каждого сценария смотрим, в какое поле ПЕРВОГО шага попадёт значение.
  // Показывать надо «сценарий → поле», а не просто сценарий: подходящих по
  // ТИПУ полей много, а по смыслу — одно. Без этого вход молча уезжал, скажем,
  // в delimiter csv-сценария: тип строковый, и никакой ошибки.
  const spec = inputSpec(inp);
  const rows = await Promise.all(list.map(async p => {
    try {
      const yamlText = await apiRaw('/api/pipelines/' + encodeURIComponent(p.file));
      const parsed = await api('/api/parse/pipeline', {
        method: 'POST', headers: { 'Content-Type': 'application/yaml' }, body: yamlText,
      });
      const doc = parsed.doc || parsed;
      const st = (doc.steps || [])[0];
      if (!st) return null;
      const ins = (pluginInfo(st.plugin) || {}).input || {};
      const keys = Object.keys(ins);
      const fit = keys.filter(f => outFits(inp.name, spec, f, ins[f], true));
      if (!fit.length) return null;
      // приоритет: поле названо как вход, потом поле берёт значение из input.*,
      // потом первое совместимое
      const fromName = f => String(ins[f].from || '');
      fit.sort((a, b) => {
        const sa = (a === inp.name ? 2 : 0) + (/^input\./.test(fromName(a)) ? 1 : 0);
        const sb = (b === inp.name ? 2 : 0) + (/^input\./.test(fromName(b)) ? 1 : 0);
        return sb - sa;
      });
      return { file: p.file, step: st.id, field: fit[0], doc, exact: fit[0] === inp.name };
    } catch (e) { return null; }
  }));
  const good = rows.filter(Boolean).sort((a, b) => (b.exact - a.exact));
  if (!good.length) {
    m.innerHTML = '<div class="cap">ни один сценарий не принимает этот вход</div>';
    return;
  }
  m.innerHTML = '<div class="cap">сценарий и поле, куда попадёт значение</div>'
    + good.map((g, i) => `<button data-row="${i}">${esc(g.file.replace(/\.ya?ml$/i, ''))}`
      + ` <span style="color:var(--faint)">→ ${esc(g.step)}.${esc(g.field)}</span></button>`).join('');
  m.querySelectorAll('[data-row]').forEach(b => {
    b.onclick = () => {
      m.style.display = 'none';
      const g = good[+b.dataset.row];
      applyPreset(g.file, inp.name, g);
    };
  });
}

async function applyPreset(file, inputName, pre) {
  try {
    let doc = pre && pre.doc;
    if (!doc) {
      const yamlText = await apiRaw('/api/pipelines/' + encodeURIComponent(file));
      const parsed = await api('/api/parse/pipeline', {
        method: 'POST', headers: { 'Content-Type': 'application/yaml' }, body: yamlText,
      });
      doc = parsed.doc || parsed;
    }
    if (!doc.steps || !doc.steps.length) { note('в сценарии нет шагов', true); return; }
    pushUndo();
    // разводим шаги по холсту, чтобы они не налезали друг на друга
    doc.steps.forEach((st, i) => {
      st.pos = [120 + (i % 3) * 300, 90 + Math.floor(i / 3) * 220];
      if (!st.id) st.id = st.plugin.split(/[\\/]/).pop().replace(/[^\w]/g, '_') + '_' + (i + 1);
    });
    state.doc.steps = doc.steps;
    // Управляющий поток сценария ОБЯЗАТЕЛЕН, а не украшение: в csv_foreach
    // foreach_item = row, и поля внутри цикла пишутся как input.row.name.
    // Сборщик цепочки их терял, и на холсте появлялись стрелки в никуда, а
    // валидатор писал «в input пайплайна нет поля row»: переменная цикла не
    // вход, а имя элемента foreach.
    if (doc.gates) state.doc.gates = doc.gates;
    state.doc.foreach = doc.foreach || '';
    state.doc.foreach_item = doc.foreach_item || '';
    state.doc.item_type = doc.item_type || '';
    state.doc.item_format = doc.item_format || '';
    // ── network и secrets: переносятся РАЗНО, и это не опечатка ────────────
    //
    // network из сценария НЕ переносится. Сеть — это доступ, а не свойство
    // цепочки, и выдавать её должен человек осознанным действием: тот, кто
    // нажал «собрать цепочку», скорее всего не читал YAML внутри пресета, а
    // `network: allow` там стоит в третьей строке и выглядит как часть описания.
    // Перенос дал бы доступ в сеть одним кликом по сборщику — ровно то, что
    // запрещает политика проекта («сеть выдаёт человек, а не файл»).
    // Собранная цепочка обязана быть без сети: пустое поле = deny (см.
    // EffectiveNetwork в internal/pipeline/model.go), и выдать сеть может
    // только клик по «разрешить сеть» или выбор network в свойствах. Поле
    // обнуляется безусловно, а не только когда сценарий что-то объявил: allow,
    // выданный вручную прежнему набору шагов, к новой цепочке отношения не
    // имеет, а сохранить его молча значило бы выдать сеть без спроса.
    // Знание «сеть не перенесена, её надо выдать» НЕ теряется вместе со
    // значением: оно висит в state.netPending и печатается на холсте.
    const presetNet = String(doc.network || '');
    state.doc.network = '';
    state.netPending = presetNet === 'allow' ? presetNet : '';
    //
    // secrets, наоборот, переносятся: ключ — это ОБЪЯВЛЕНИЕ, какие переменные
    // окружения читает цепочка, а не выдача доступа. Сам ключ остаётся в
    // окружении, и раннер требует наличия КАЖДОГО объявленного ключа. Потеря
    // объявления была беззвучной (сверка «плагин просит ключ ↔ pipeline» живёт
    // в предупреждениях, файл сохранялся), и пайплайн падал уже на запуске с
    // secrets_missing — то есть терялись данные о цепочке. Лишний ключ, как и
    // недостающий, роняет ран, поэтому переносится значение как есть,
    // включая пустое. Уравнивать эти два поля нельзя: одно — доступ, другое —
    // описание. Перенос secrets копией: правка чипов в UI не должна дописывать
    // ключ в исходный сценарий.
    state.doc.secrets = (doc.secrets || []).slice();
    // Поле, которым редактор не управляет, обязано ДОЕХАТЬ до state.unsupported:
    // иначе сборщик строит документ без него, сериализация его отбрасывает, и
    // save() проходит — потеря получается ровно той, ради которой страж стоит.
    for (const f of (doc.unsupported || [])) {
      if (!Array.isArray(state.unsupported)) state.unsupported = [];
      if (!state.unsupported.includes(f)) state.unsupported.push(f);
    }
    // ссылки input.X: сперва пробуем обслужить их ОДНИМ введённым значением,
    // и только несовместимые (путь к файлу, csv) получают собственный вход.
    // Раньше на каждую ссылку заводился новый вход — цепочка из трёх шагов
    // требовала три ввода вместо одного, и введённый ник оставался неиспользованным.
    const loopVar = String(doc.foreach_item || '');
    const wanted = [];
    for (const st of doc.steps) {
      const ins = (pluginInfo(st.plugin) || {}).input || {};
      for (const [field, src] of Object.entries(st.bind || {})) {
        const m = String(src || '').match(/^input\.([\w-]+)/);
        if (m) wanted.push({ name: m[1], field, step: st, spec: ins[field] || null });
      }
    }
    const userInput = state.doc.input.find(i => i.name === inputName);
    const userSpec = userInput ? inputSpec(userInput) : null;
    const have = new Set(state.doc.input.map(i => i.name));
    let merged = 0, added = 0;
    for (const w of wanted) {
      if (w.name === loopVar) continue;              // переменная цикла, не вход
      if (have.has(w.name)) continue;                // вход уже объявлен
      // совместимо ли с тем, что человек уже ввёл
      if (userSpec && w.spec && outFits(w.name, userSpec, w.field, w.spec, true)) {
        w.step.bind[w.field] = 'input.' + inputName;
        merged++;
        continue;
      }
      // несовместимо (нужен путь к файлу, csv, число) — свой вход по образцу поля
      state.doc.input.push({
        name: w.name,
        default: '',
        typed: false,
        type: (w.spec && w.spec.type) || 'string',
        ...(w.spec && w.spec.format ? { format: w.spec.format } : {}),
        has_default: true,
      });
      have.add(w.name);
      added++;
    }
    const first = doc.steps[0];
    const key = pre ? pre.field : Object.keys((pluginInfo(first.plugin) || {}).input || {})[0];
    if (key) {
      first.bind = first.bind || {};
      first.bind[key] = 'input.' + inputName;
    }
    setLayer('pipeline');
    renderAll();
    const missing = added;
    note('сценарий ' + file + ': вход → ' + first.id + '.' + key
      + (merged ? '; наш вход «' + inputName + '» получил ещё полей: ' + merged : '')
      + (missing ? '; заведён отдельных входов: ' + missing : '')
      + (loopVar ? '; цикл по элементу «' + loopVar + '»' : '')
      + (presetNet === 'allow'
        ? '; сеть сценария (allow) НЕ перенесена — сеть выдаёт человек, нажми «разрешить сеть»'
        : presetNet ? '; сеть сценария: ' + presetNet + ' — так и оставлено (сеть не выдаётся)' : '')
      + (state.doc.secrets.length ? '; ключи: ' + state.doc.secrets.join(', ') : ''));
  } catch (e) {
    note('сценарий не собран: ' + (e.message || e), true);
  }
}

// Разводит шаги, которые лежат друг на друге. Часть примеров в examples/
// задаёт всем шагам одну позицию, и они приезжали стопкой в угол: подписи
// наезжали друг на друга, верхний шаг закрывал остальные. Раскладка
// применяется только когда накладка действительно есть — иначе не трогаем
// то, что человек расставил руками.
function spreadSteps() {
  const steps = state.doc.steps || [];
  if (steps.length < 2) return false;
  const seen = new Map();
  let clash = false;
  for (const s of steps) {
    const key = (s.pos || []).join(',');
    if (seen.has(key)) { clash = true; break; }
    seen.set(key, true);
  }
  if (!clash) return false;
  steps.forEach((s, i) => {
    s.pos = [120 + (i % 3) * 300, 90 + Math.floor(i / 3) * 230];
  });
  return true;
}

function openCtxMenu(x, y) {
  const m = $('#ctxmenu');
  if (!m) return;
  m.innerHTML = '<div class="cap">вход</div>'
    + '<button data-kind="text">Текст или ник</button>'
    + '<button data-kind="number">Число</button>'
    + '<button data-kind="file">Фото или файл</button>'
    + '<div class="sep"></div>'
    + '<div class="cap">шаг</div>'
    + '<button data-open="palette">Выбрать плагин слева…</button>';
  m.style.display = 'block';
  const r = m.getBoundingClientRect();
  m.style.left = Math.min(x, window.innerWidth - r.width - 8) + 'px';
  m.style.top = Math.min(y, window.innerHeight - r.height - 8) + 'px';
  m.querySelectorAll('[data-kind]').forEach(b => {
    b.onclick = () => { m.style.display = 'none'; addInput(b.dataset.kind); };
  });
  const op = m.querySelector('[data-open]');
  if (op) op.onclick = () => {
    m.style.display = 'none';
    const p = $('#palette .plug');
    if (p) { p.scrollIntoView({ block: 'center' }); p.focus(); }
    note('плагины — слева, перетащи на холст');
  };
}

// Тянем от точки входа к полю шага. Проверка типов та же, что и у шаг→шаг:
// вход — это источник, у него объявлены тип и формат.
function dropInputLink(inp, ev) {
  const d = state.linkDrag;
  state.linkDrag = null;
  if (!d || !inp) return;
  drawGhost();
  const el = document.elementFromPoint(ev.clientX, ev.clientY);
  const row = el && el.closest ? el.closest('.inrow[data-field]') : null;
  const node = row && row.closest('.node');
  if (!row || !node || !node.dataset || !node.dataset.id) {
    note('отпусти на поле входа шага', true);
    return;
  }
  const st = stepById(node.dataset.id);
  if (!st) return;
  const info = pluginInfo(st.plugin) || {};
  const spec = (info.input || {})[row.dataset.field] || {};
  const src = inputSpec(inp);
  if (!outFits(inp.name, src, row.dataset.field, spec, true)) {
    note('вход «' + inp.name + '» (' + (src.format || src.type) + ') не подходит в поле '
      + row.dataset.field + ' (' + (spec.format || spec.type || '?') + ')', true);
    return;
  }
  pushUndo();
  st.bind = st.bind || {};
  st.bind[row.dataset.field] = 'input.' + inp.name;
  state.sel = st.id;
  renderAll();
}

function renderInputNodes() {
  const host = $('#inlayer');
  if (!host) return;
  host.innerHTML = state.doc.input.map((inp, i) => {
    const spec = inputSpec(inp);
    const file = isFileInput(inp);
    const val = inp.default == null ? '' : String(inp.default);
    return `<div class="inode" data-inp="${i}" data-role="input" style="top:${IN_Y0 + i * IN_STEP}px">
      <div class="ihead"><span class="dot"></span><b>${esc(inp.name)}</b>
        <span class="itype">${esc(inp.format || spec.type)}</span></div>
      ${file
        ? `<div class="ifile"><button data-pick="${i}">выбрать</button>
             <span class="fp" title="${esc(val)}">${esc(val ? val.split(/[\\/]/).pop() : 'файл не выбран')}</span></div>`
        : `<input class="ival" data-ival="${i}" value="${esc(val)}" spellcheck="false"
             placeholder="${inp.type === 'number' ? '0' : 'значение'}"${inp.type === 'number' ? ' inputmode="decimal"' : ''}/>`}
      <div class="iacts">
        <button data-build="${i}">собрать цепочку</button>
        <button data-topipeline="${i}">в пайплайн →</button>
      </div>
    </div>`;
  }).join('');
  host.querySelectorAll('[data-ival]').forEach(el => {
    el.onchange = () => setInputValue(+el.dataset.ival, el.value);
    el.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); el.blur(); } };
  });
  host.querySelectorAll('[data-pick]').forEach(b => {
    b.onclick = () => pickFile(+b.dataset.pick);
  });
  // Слои раздельны, поэтому вход НЕ тянется к шагу: связь через экран была бы
  // ровно тем смешением, ради которого слои и разделены. Вход подключается
  // списком источника в правой панели, а отсюда — кнопками.
  host.querySelectorAll('[data-build]').forEach(b => {
    b.onclick = () => buildChainFor(+b.dataset.build);
  });
  host.querySelectorAll('[data-topipeline]').forEach(b => {
    b.onclick = () => setLayer('pipeline');
  });
}

// Полоса входов на слое пайплайна. Значения здесь не редактируются — это
// только источники для протяжки: перетащил на поле шага, и появилась связь
// input.имя. Само значение правится на слое входов.
function renderInputStrip() {
  const host = $('#instrip');
  if (!host) return;
  if (!state.doc.input.length) { host.innerHTML = ''; return; }
  host.innerHTML = '<div class="icap">входы</div>'
    + state.doc.input.map((inp, i) => {
      const used = state.doc.steps.some(s => Object.values(s.bind || {})
        .some(v => String(v || '').match(new RegExp('^input\\.' + inp.name + '(\\.|$)'))));
      return `<div class="isrc${used ? ' set' : ''}" data-isrc="${i}" title="${esc(inp.name)} — тяни к полю шага">`
        + `<span class="dot"></span><span class="n">${esc(inp.name)}</span></div>`;
    }).join('');
  host.querySelectorAll('.isrc').forEach(el => {
    el.addEventListener('mousedown', e => {
      const inp = state.doc.input[+el.dataset.isrc];
      if (!inp || e.button !== 0) return;
      e.preventDefault();
      state.linkDrag = { input: inp.name, step: null, field: null, label: 'input.' + inp.name };
      state.linkDrag.x = e.clientX; state.linkDrag.y = e.clientY;
      drawGhost();
      const move = ev => {
        state.linkDrag.x = ev.clientX; state.linkDrag.y = ev.clientY;
        drawGhost();
        const t = document.elementFromPoint(ev.clientX, ev.clientY);
        const hit = t && t.closest ? t.closest('.inrow[data-field]') : null;
        const prev = $('#canvas').querySelector('.hovered');
        if (prev) prev.classList.remove('hovered');
        if (hit) hit.classList.add('hovered');
      };
      const up = ev => {
        document.removeEventListener('mousemove', move);
        document.removeEventListener('mouseup', up);
        dropInputLink(inp, ev);
      };
      document.addEventListener('mousemove', move);
      document.addEventListener('mouseup', up);
    });
  });
}

// Подходящий уже существующий вход. Зачем это: клик по ошибке «в input
// пайплайна нет поля username» раньше ЗАВОДИЛ новый вход username, даже когда
// человек уже ввёл ник в `nick`. Дальше шаг брал пустой input.username, а
// введённое значение оставалось неиспользованным. Теперь поле подключается к
// тому входу, который уже есть и подходит по типу.
function findCompatibleInput(spec) {
  if (!spec) return -1;
  const t = spec.type || 'string';
  const f = spec.format || '';
  return state.doc.input.findIndex(i => {
    const s = inputSpec(i);
    if (s.type !== t) return false;
    if (f && s.format && f !== s.format) return false;
    // текст подходит тексту; формат вроде ip ждёт ip
    if (f && !s.format && f !== 'text') return false;
    return true;
  });
}

// Ошибка «в input пайплайна нет поля X» кликабельна: клик либо подключает поле
// к уже введённому значению, либо создаёт вход X, мигает им и ставит фокус.
function wireIssueClicks() {
  const ve = $('#val-err');
  if (!ve) return;
  ve.querySelectorAll('[data-issue]').forEach(b => {
    b.onclick = () => gotoInput(b.dataset.issue, b.dataset.istep, b.dataset.iport);
  });
  ve.querySelectorAll('[data-net]').forEach(b => {
    b.onclick = () => {
      pushUndo();
      state.doc.network = b.dataset.net;
      // Человек выдал сеть сам — напоминание «сеть сценария не перенесена»
      // больше не нужно и панель уходит.
      state.netPending = '';
      renderAll();
      note('сеть разрешена для пайплайна: network: allow');
    };
  });
}

function gotoInput(name, stepId, port) {
  if (!name) return;
  // 1) поле уже объявлено плагином — ищем совместимый вход и подключаем его
  if (stepId && port) {
    const st = stepById(stepId);
    const spec = st && ((pluginInfo(st.plugin) || {}).input || {})[port];
    const idx = findCompatibleInput(spec || { type: 'string' });
    if (st && idx >= 0) {
      pushUndo();
      st.bind = st.bind || {};
      st.bind[port] = 'input.' + state.doc.input[idx].name;
      setLayer('pipeline');
      renderAll();
      const el = document.querySelector('.node[data-id="' + stepId + '"] .inrow[data-field="' + port + '"]');
      if (el) { el.classList.add('flash'); setTimeout(() => el.classList.remove('flash'), 2600); }
      note('поле ' + stepId + '.' + port + ' взяло уже введённое «'
        + state.doc.input[idx].name + '» — новый вход не понадобился');
      return;
    }
  }
  // 2) подходящего входа нет — создаём и ведём к нему
  let idx = state.doc.input.findIndex(i => i.name === name);
  if (idx < 0) {
    pushUndo();
    state.doc.input.push({ name, default: '', typed: false, type: 'string', has_default: true });
    renderInputNodes();
    scheduleValidate();
    idx = state.doc.input.length - 1;
  }
  setLayer('inputs');
  const nodes = document.querySelectorAll('#inlayer .inode');
  const el = nodes[idx];
  if (el) {
    el.classList.add('flash');
    setTimeout(() => el.classList.remove('flash'), 2600);
    const f = el.querySelector('.ival');
    if (f) f.focus();
  }
  note('вход «' + name + '» — введи значение');
}

function stepById(id) {
  return state.doc.steps.find(s => s.id === id) || null;
}
function nodeById(id) {
  return document.querySelector(`.node[data-id="${id}"]`);
}
// editKey — правило материализации гейта (как в ядре): basename, при
// коллизии — steps.X.field → «X_field»
function gateOutKeys(gateStep) {
  const bnCount = {};
  for (const f of gateStep.form || []) {
    const bn = f.field.split('.').pop();
    bnCount[bn] = (bnCount[bn] || 0) + 1;
  }
  return (gateStep.form || []).map(f => {
    const bn = f.field.split('.').pop();
    if (bnCount[bn] > 1) {
      const parts = f.field.split('.');
      if (parts.length >= 3 && parts[0] === 'steps') return parts[1] + '_' + bn;
      return f.field.replace(/\./g, '_');
    }
    return bn;
  });
}
// источники для bind: input.* + steps.<id>.<out> (у гейта — из его формы)
function sourceOptions(excludeId) {
  const opts = [];
  for (const i of state.doc.input) opts.push('input.' + i.name);
  for (const st of state.doc.steps) {
    if (st.id === excludeId) continue;
    const outs = st.plugin === 'core/human_gate' ? gateOutKeys(st) : outFields(st.plugin);
    for (const o of outs) opts.push('steps.' + st.id + '.' + o);
  }
  return opts;
}

// ── undo/redo ─────────────────────────────────────────────────────────────
// Снимок — это документ ПЛЮС напоминание о невыданной сети сценария: без
// второго undo откатывал бы цепочку к прежнему виду, а панель продолжала бы
// говорить «сеть сценария НЕ перенесена» — враньё вместо предупреждения.
function undoSnap() {
  return JSON.stringify({ doc: state.doc, netPending: state.netPending || '' });
}
function undoUnsnap(s) {
  const o = JSON.parse(s);
  state.doc = o.doc;
  state.netPending = o.netPending || '';
}
function pushUndo() {
  state.undo.push(undoSnap());
  if (state.undo.length > 100) state.undo.shift();
  state.redo = [];
}
function undo() {
  if (!state.undo.length) return;
  state.redo.push(undoSnap());
  undoUnsnap(state.undo.pop());
  state.sel = null;
  // снимок не знает ни про вооружение, ни про протяжку, ни про кандидатов:
  // после undo/redo источник связи мог исчезнуть или сменить выход.
  state.link = null;
  state.linkDrag = null;
  const fileInput = $('#file-name');
  if (fileInput) fileInput.value = state.doc.file || '';
  const fileSelect = $('#file-open');
  if (fileSelect) fileSelect.value = state.doc.file || '';
  renderAll();
}
function redo() {
  if (!state.redo.length) return;
  state.undo.push(undoSnap());
  undoUnsnap(state.redo.pop());
  state.sel = null;
  state.link = null;
  const fileInput = $('#file-name');
  if (fileInput) fileInput.value = state.doc.file || '';
  const fileSelect = $('#file-open');
  if (fileSelect) fileSelect.value = state.doc.file || '';
  renderAll();
}

// ── модель → DOM ──────────────────────────────────────────────────────────
function nextStepId() {
  let n = state.doc.steps.length + 1;
  let id = 's' + n;
  while (state.doc.steps.some(s => s.id === id)) id = 's' + (++n);
  return id;
}

function addStep(plugin, x, y) {
  pushUndo();
  const st = {
    id: nextStepId(), plugin, pos: [snap(x), snap(y)],
    on_error: 'stop', timeout: '', bind: {}, when: null,
    foreach: '', foreach_item: '', after_foreach: false, parallel_group: '',
    loop: '', loop_condition: '', max_iterations: 0, retry: null,
  };
  if (plugin === 'core/human_gate') {
    st.form = []; st.actions = ['accept', 'reject']; st.on_reject = 'stop'; st.approval = '';
  }
  state.doc.steps.push(st);
  state.sel = st.id;
  renderAll();
  return st; // v0.38: подсказкам цепочки нужен созданный шаг, чтобы связать его
}

// Категория плагина — по тому, с чем он работает. Порядок проверок важен:
// фото и файлы проверяются первыми, иначе «всё, где есть file_ref» ушло бы в
// «Текст» (у exifread поле file — это строка).
const PLUG_CATS = [
  ['Фото и изображения', p => /(exif|photo|image|фото|изображен|картин)/i.test(p._hay)],
  ['Файлы и документы', p => /(file_ref|\.pdf|документ|файл(?!ы))/i.test(p._hay)],
  ['Таблицы и CSV', p => /(csv|таблиц|excel|xlsx)/i.test(p._hay)],
  ['Сеть, домены, IP', p => /(domain|\bip\b|ipv|dns|mx\b|whois|url|http|сайт|домен)/i.test(p._hay)],
  ['LLM и текст', p => /(llm|gpt|openai|anthropic|gemini|текст|промпт)/i.test(p._hay)],
  ['Числа и списки', p => /^\s*$/.test(p._numOnly ? 'x' : '') || false],
];

function plugCat(p) {
  const ins = p.input || {};
  const keys = Object.keys(ins);
  const fmts = keys.map(k => (ins[k].format || '') + ' ' + (ins[k].type || '')).join(' ');
  p._hay = ((p.id || '') + ' ' + (p.description || '') + ' ' + fmts).toLowerCase();
  p._cat = keys.length === 0 ? 'Без входа'
    : keys.every(k => (ins[k].type === 'number')) ? 'Числа'
      : keys.every(k => (ins[k].type === 'array' || ins[k].type === 'object')) ? 'Списки и структуры'
        : null;
  if (p._cat) return p._cat;
  if (/(file_ref)/i.test(fmts)) return 'Фото и изображения';
  if (keys.some(k => ins[k].type === 'array')) return 'Списки и структуры';
  if (keys.some(k => ins[k].type === 'number')) return 'Числа';
  return 'Текст';
}

const CAT_ORDER = ['Текст', 'Фото и изображения', 'Файлы и документы', 'Таблицы и CSV',
  'Сеть, домены, IP', 'LLM и текст', 'Числа', 'Списки и структуры', 'Без входа'];

function renderPalette() {
  const el = $('#plugin-list');
  const q = (state.palQuery || '').trim().toLowerCase();
  const hit = state.plugins.filter(p => {
    if (!q) return true;
    return ((p.id || '') + ' ' + (p.description || '')).toLowerCase().includes(q);
  }).map(p => Object.assign({}, p));
  const groups = new Map();
  for (const p of hit) {
    const c = plugCat(p);
    (groups.get(c) || groups.set(c, []).get(c)).push(p);
  }
  const cats = [...groups.keys()].sort((a, b) => {
    const ia = CAT_ORDER.indexOf(a), ib = CAT_ORDER.indexOf(b);
    return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
  });
  if (!hit.length) { el.innerHTML = '<div class="empty">ничего не найдено</div>'; bindPalette(); return; }
  el.innerHTML = cats.map(c => `<div class="pcat">${esc(c)} <span>${groups.get(c).length}</span></div>`
    + groups.get(c).map(p => {
      const ins = Object.keys(p.input || {}).length, outs = Object.keys(p.output || {}).length;
      return `<div class="plug" data-plugin="${esc(p.id)}">
        <b>${esc(p.id)}</b>
        <small>${esc(p.description || '')}</small>
        <div class="io">in: ${ins} · out: ${outs}</div>
      </div>`;
    }).join('')).join('');
  bindPalette();
}

function bindPalette() {
  // v0.8a: mouse-based drag (нативный DnD мёртв в sandboxed iframe)
  //
  // Область сбора — вся палитра (#palette), а не только #plugin-list. Карточка
  // core/human_gate лежит в разметке РЯДОМ с #plugin-list (index.html), и
  // раньше подписывался только #plugin-list — то есть гейт нельзя было ни
  // перетащить, ни добавить кликом, хотя выглядел он как остальные.
  document.querySelectorAll('#palette .plug').forEach(el2 => {
    el2.addEventListener('mousedown', e => startPaletteDrag(e, el2.dataset.plugin));
  });
}

// v0.8a: перетаскивание плагина на холст через mouse-события:
// мousedown → ghost следует за курсором → mouseup над холстом = добавить;
// клик без движения = добавить на свободное место. Работает в sandboxed
// iframe (превью), WebView2 и обычном браузере.
function startPaletteDrag(e, pluginId) {
  if (e.button !== 0) return;
  e.preventDefault(); // запрет выделения текста при волочении
  // Плагины живут на слое пайплайна: перетащили из палитры — значит человек
  // уже строит цепочку, и переключать слой молча не надо. Переключаем явно.
  if (state.layer !== 'pipeline') setLayer('pipeline');
  const startX = e.clientX, startY = e.clientY;
  const wrap = $('#canvas-wrap'), canvas = $('#canvas');
  let moved = false;
  const ghost = document.createElement('div');
  ghost.className = 'plug-ghost';
  ghost.textContent = pluginId;
  document.body.appendChild(ghost);
  const placeGhost = ev => { ghost.style.left = (ev.clientX + 12) + 'px'; ghost.style.top = (ev.clientY + 10) + 'px'; };
  placeGhost(e);
  const overCanvas = ev => {
    const r = wrap.getBoundingClientRect();
    return ev.clientX >= r.left && ev.clientX <= r.right && ev.clientY >= r.top && ev.clientY <= r.bottom;
  };
  const mm = ev => {
    if (Math.abs(ev.clientX - startX) + Math.abs(ev.clientY - startY) > 4) moved = true;
    placeGhost(ev);
    wrap.classList.toggle('drop-over', moved && overCanvas(ev));
  };
  const mu = ev => {
    document.removeEventListener('mousemove', mm);
    document.removeEventListener('mouseup', mu);
    ghost.remove();
    wrap.classList.remove('drop-over');
    if (overCanvas(ev)) {
      const cRect = canvas.getBoundingClientRect();
      addStep(pluginId, ev.clientX - cRect.left - 115, ev.clientY - cRect.top - 20);
    } else if (!moved) {
      const i = state.doc.steps.length;
      addStep(pluginId, 40 + (i % 3) * 270, 40 + Math.floor(i / 3) * 140);
    }
  };
  document.addEventListener('mousemove', mm);
  document.addEventListener('mouseup', mu);
}

// Роль шага. Порядок важен: гейт — это всегда человек, даже если у него
// включили when; условие важнее перебора, потому что when меняет СМЫСЛ шага,
// а foreach — только количество повторов. Роли холодные: тёплое отдано
// семантике (has-err), и красный «перебор» читался бы как сбой.
const ROLE_LABEL = {
  input: 'вход', step: 'шаг', gate: 'гейт',
  when: 'условие', foreach: 'перебор', parallel: 'параллельно',
};
function stepRole(st) {
  if (st.plugin === 'core/human_gate') return 'gate';
  if (st.parallel_group) return 'parallel';
  if (st.when) return 'when';
  if (st.foreach || st.after_foreach || st.loop) return 'foreach';
  return 'step';
}

function renderNodes() {
  const canvas = $('#canvas');
  canvas.querySelectorAll('.node').forEach(n => n.remove());
  // Подсказку показываем только на пустом холсте: дальше она перекрывала бы
  // узлы и мешала работать.
  const hint = $('#canvas-hint');
  if (hint) hint.classList.toggle('hidden', state.doc.steps.length > 0);
  const link = state.link;
  for (const st of state.doc.steps) {
    const gate = st.plugin === 'core/human_gate';
    const info = pluginInfo(st.plugin);
    const role = stepRole(st);
    const node = document.createElement('div');
    node.className = 'node' + (gate ? ' gate' : '') + (state.sel === st.id ? ' selected' : '');
    node.dataset.role = role;
    node.style.left = st.pos[0] + 'px';
    node.style.top = st.pos[1] + 'px';
    node.dataset.id = st.id;
    const ins = gate
      ? '<div class="inrow"><span>form</span><span class="src set">см. справа</span></div>'
      : (inFields(st.plugin).map(f => {
          const src = (st.bind || {})[f] || '';
          // Пока оружие «связать» заряжено, поля-цели подсвечиваются, а поля
          // ЭТОГО шага — нет: связать шаг с самим собой бессмысленно.
          const isTarget = link && link.step !== st.id;
          const cls = isTarget ? ' linkable' : '';
          return `<div class="inrow${cls}" data-field="${esc(f)}" title="${isTarget ? 'принять связь в это поле' : ''}"><span>${esc(f)}</span><span class="src ${src ? 'set' : ''}">${esc(src || '—')}</span></div>`;
        }).join('') || '<div class="inrow"><span>входов нет</span></div>');
    const outs = gate
      ? '<span class="outchip" title="выходы = поля формы">поля формы</span>'
      : (outFields(st.plugin).map(o => {
          const armed = link && link.step === st.id && link.field === o;
          return `<span class="outchip${armed ? ' armed' : ''}" data-out="${esc(o)}" title="зажми и веди к другому шагу — или щелчок, чтобы вооружить связь">${esc(o)}</span>`;
        }).join('') || '<span class="outchip">нет</span>');
    node.innerHTML = `
      <div class="nh"><span class="rwrap"><span class="rdot"></span><span class="nid">${esc(st.id)}</span><span class="rlabel">${esc(ROLE_LABEL[role] || '')}</span></span><span class="nplug">${gate ? 'human_gate' : esc((info && (info.id === st.plugin ? st.plugin.split('/').pop() : st.plugin)) || st.plugin)}</span></div>
      <div class="body">${ins}<div style="margin-top:6px">${outs}</div></div>
      <div class="foot"><span>on_err: ${esc(st.on_error || 'stop')}</span>${st.when ? `<span title="when: ${esc(st.when.path || '')}">⚖ when:${esc(st.when.op || '')}</span>` : ''}${st.foreach ? '<span title="foreach: ' + esc(st.foreach) + '">⤨ foreach</span>' : ''}${st.parallel_group ? `<span title="parallel_group">∥ ${esc(st.parallel_group)}</span>` : ''}${st.after_foreach ? `<span title="after_foreach">⤓ post</span>` : ''}${st.loop ? `<span title="цикл шага">⟳ loop</span>` : ''}${st.timeout ? `<span>${esc(st.timeout)}</span>` : ''}</div>`;
    canvas.appendChild(node);
    node.addEventListener('mousedown', e => startNodeDrag(e, st));
    node.addEventListener('click', e => { e.stopPropagation(); state.sel = st.id; renderAll(); });
    // Связывание: щелчок по выходу вооружает связь, щелчок по полю входа
    // другого шага — принимает её. Всё остальное (перетаскивание, выделение)
    // остаётся как было.
    node.querySelectorAll('.outchip[data-out]').forEach(chip => {
      // Зажали на выходе — поехали протяжка. Отпустили тут же, без движения:
      // сработает click ниже и связь просто вооружится вторым щелчком.
      chip.addEventListener('mousedown', e => startLinkDrag(st, chip.dataset.out, e));
      chip.addEventListener('click', e => {
        e.stopPropagation();
        armLink(st, chip.dataset.out);
      });
    });
    // Зажали в теле узла — тянем связь (источник выберется сам, если он один).
    // Шапка и нижняя полоса — перенос узла, там связь не начинается.
    node.addEventListener('mousedown', e => {
      if (e.target.closest('.nh') || e.target.closest('.foot')) return;
      if (e.target.closest('.inrow[data-field]') || e.target.closest('.outchip[data-out]')) return;
      startLinkDrag(st, null, e);
    });
    if (link) {
      node.querySelectorAll('.inrow[data-field]').forEach(row => {
        if (st.id === link.step) return;
        row.addEventListener('click', e => {
          e.stopPropagation();
          applyLink(st, link, row.dataset.field);
        });
      });
    }
  }
  renderLinkBar();
  renderEdges();
}

// armLink — вооружить связь из (шаг, поле). Повторный щелчок по тому же выходу
// и щелчок по другому выходу переключают источник; повторный по тому же —
// отмена. Связь со своим шагом невозможна, поэтому клик по выходу шага,
// который уже вооружён, просто переставляет источник.
function armLink(st, field) {
  const cur = state.link;
  state.link = (cur && cur.step === st.id && cur.field === field)
    ? null
    : { value: `steps.${st.id}.${field}`, label: `${st.id}.${field}`, step: st.id, field };
  renderAll();
}

function cancelLink() {
  if (!state.link && !state.linkDrag) return;
  state.link = null;
  state.linkDrag = null;
  renderAll();
}

// ─── v0.37: протяжка, как в канвасе Obsidian ────────────────────────────────
//
// Зажал на плагине (или на конкретном выходе) → ведёшь резиновую линию →
// отпустил на другом шаге. Имени поля нигде вводить не надо.
//
// Формат пайплайна не умеет «просто связать два узла»: в `bind` обязано быть
// имя поля-приёмника, поэтому либо источник, либо приёмник приходится выбрать.
// Отсюда правило: тянем с выхода — попадаем в поле (точная связь); тянем с узла
// целиком — выход подбирается по типам (первый совместимый в первое свободное
// поле), а если совместимого нет, связь не появляется и причина называется
// вслух. Ни ввода, ни угадывания. Список пар-кандидатов панелью диктовал
// прежний экран; сейчас выход выбирается автоматически по типу выхода.

// Канвас-координаты из клиентских: узлы лежат внутри #canvas со скроллом.
function canvasXY(ev) {
  const cRect = $('#canvas').getBoundingClientRect();
  return { x: ev.clientX - cRect.left, y: ev.clientY - cRect.top };
}

function startLinkDrag(st, field, ev) {
  ev.preventDefault();
  ev.stopPropagation();
  const node = nodeById(st.id);
  const host = field
    ? node.querySelector(`.outchip[data-out="${field}"]`)
    : node;
  const r = host.getBoundingClientRect();
  const cRect = $('#canvas').getBoundingClientRect();
  const p = canvasXY(ev);
  state.linkDrag = {
    step: st.id,
    field: field || null,
    x1: (field ? r.right : r.left) - cRect.left,
    y1: r.top + r.height / 2 - cRect.top,
    x: p.x,
    y: p.y,
  };
  renderAll();
  const move = e => {
    const q = canvasXY(e);
    state.linkDrag.x = q.x;
    state.linkDrag.y = q.y;
    drawGhost();
    // Подсветка цели: без неё тянуть приходится на ощупь. Подсвечиваем и
    // само поле под курсором, и узел-цель — дроп идёт по узлу, а не по полю.
    const el = document.elementFromPoint(e.clientX, e.clientY);
    const hit = el && el.closest ? el.closest('.inrow[data-field],.node[data-id]') : null;
    const prev = $('#canvas').querySelector('.hovered');
    if (prev) prev.classList.remove('hovered');
    const prevNode = $('#canvas').querySelector('.drop-target');
    if (prevNode) prevNode.classList.remove('drop-target');
    const tgt = hit && hit.closest('.node');
    if (tgt && tgt !== nodeById(st.id)) {
      tgt.classList.add('drop-target');
      if (hit !== tgt) hit.classList.add('hovered');
    }
  };
  const up = e => {
    document.removeEventListener('mousemove', move);
    document.removeEventListener('mouseup', up);
    const h = $('#canvas').querySelector('.hovered');
    if (h) h.classList.remove('hovered');
    dropLink(e);
  };
  document.addEventListener('mousemove', move);
  document.addEventListener('mouseup', up);
}

// Резиновая линия. Обновляем только свой <path>, а не весь svg: mousemove
// идёт на каждый кадр, перерисовывать все рёбра там незачем.
function drawGhost() {
  const d = state.linkDrag;
  const svg = document.getElementById('edges');
  if (!svg) return;
  let g = svg.querySelector('#link-ghost');
  if (!d) { if (g) g.remove(); return; }
  if (!g) {
    g = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    g.id = 'link-ghost';
    svg.appendChild(g);
  }
  const dx = Math.max(34, Math.abs(d.x - d.x1) * 0.45);
  g.setAttribute('d',
    `M ${d.x1} ${d.y1} C ${d.x1 + dx} ${d.y1}, ${d.x - dx} ${d.y}, ${d.x} ${d.y}`);
  g.setAttribute('class', 'ghost');
}

// Что под курсором в момент отпускания. Прямое наведение на строку входа даёт
// самую точную связь, поэтому проверяем её раньше узла.
function dropLink(ev) {
  const d = state.linkDrag;
  state.linkDrag = null;
  if (!d) return;
  const el = document.elementFromPoint(ev.clientX, ev.clientY);
  const node = el && el.closest ? el.closest('.node') : null;
  // Мимо узла: не перерисовываем. Иначе пересозданный чит/строка потеряет
  // событие click, и двукличковая привязка перестанет работать.
  if (!node || !node.dataset || !node.dataset.id) { drawGhost(); return; }
  const toStep = node.dataset.id;
  if (toStep === d.step) { drawGhost(); return; } // в самого себя — нельзя
  const st = stepById(toStep);
  if (!st) { drawGhost(); return; }


  // Тянем с узла целиком: выход один либо тот самый, с которого тянули за чип.
  const srcStep = stepById(d.step);
  const row = el.closest ? el.closest('.inrow[data-field]') : null;
  const tgtInfo = pluginInfo(st.plugin) || {};
  const ins = tgtInfo.input || {};

  // Поле входа: если курсор стоял на конкретном поле — оно и есть цель. Иначе
  // берём то, в которое ещё не входили; при равенстве — первое годное.
  const candidates = (row && row.closest('.node') === node)
    ? [row.dataset.field]
    : Object.keys(ins).filter(f => !st.bind || !st.bind[f]);
  const pool = candidates.length ? candidates : Object.keys(ins);

  // Явно выбранный выход (тянули за чип) — проверяем и его: лучше отказать, чем
  // связать несовместимое.
  let picked = null, why = '';
  if (d.field) {
    const srcInfo = pluginInfo(srcStep.plugin) || {};
    const srcOut = (srcInfo.output || {})[d.field] || {};
    for (const f of pool) {
      if (outFits(d.field, srcOut, f, ins[f])) { picked = [f, d.field]; break; }
    }
    if (!picked) why = `${d.step}.${d.field} не подходит ни в одно поле: разные типы или форматы`;
  } else {
    for (const f of pool) {
      const ok = compatibleOuts(srcStep, f, ins[f]);
      if (ok.length) { picked = [f, ok[0]]; break; }
    }
    if (!picked) {
      const aim = (row && row.closest('.node') === node) ? ' в поле ' + row.dataset.field : '';
      why = `у шага ${d.step} нет выхода, годящегося${aim} в ${st.id}`;
    }
  }

  if (picked) {
    applyLink(st, { value: `steps.${srcStep.id}.${picked[1]}`, label: `${srcStep.id}.${picked[1]}` }, picked[0]);
  } else {
    // Ничего не связали и сказали почему. Молчаливый отказ выглядел бы как
    // зависшая протяжка, а связь «на всякий случай» — как готовая, но неверная.
    cancelLink();
    if (why) note(why, true);
  }
}

function applyLink(st, link, field) {
  pushUndo();
  st.bind = st.bind || {};
  st.bind[field] = link.value;
  state.link = null;
  state.linkDrag = null;
  state.sel = st.id;
  renderAll();
}

// Панели подтверждения больше нет: связь делается по наведению, а что именно
// тянется — показывает подсветка полей на узлах. Функция оставлена вызовом,
// чтобы renderAll не переименовывать.
function renderLinkBar() {}

// Маркеры-стрелки едут вместе с рёбрами: `svg.innerHTML = s` ниже сносит
// содержимое #edges целиком, поэтому <defs>, объявленные в HTML, исчезали бы
// после первого же рендера.
const EDGE_DEFS =
  '<defs>' +
  '<marker id="edge-arr" viewBox="0 0 9 9" refX="8" refY="4.5" markerWidth="6.5" markerHeight="6.5" orient="auto">' +
  '<path d="M0.5,0.5 L8.5,4.5 L0.5,8.5 z" fill="rgba(233,229,221,.55)"/></marker>' +
  '<marker id="edge-arr-hi" viewBox="0 0 9 9" refX="8" refY="4.5" markerWidth="6.5" markerHeight="6.5" orient="auto">' +
  '<path d="M0.5,0.5 L8.5,4.5 L0.5,8.5 z" fill="#9b8cb8"/></marker>' +
  '</defs>';

// edgesTouch — есть ли у шага хоть одно ребро: входящее (его bind ссылается на
// другой шаг или на input.*) или исходящее (на него ссылается чужой bind).
// Если рёбер нет, при перетаскивании пересчитывать нечего.
function edgesTouch(id) {
  const self = state.doc.steps.find(s => s.id === id);
  if (self && Object.values(self.bind || {}).some(v => v)) return true;
  const tag = `steps.${id}.`;
  return state.doc.steps.some(s =>
    Object.values(s.bind || {}).some(v => String(v || '').startsWith(tag)));
}

function roleColorOf(nodeEl) {
  if (!nodeEl || typeof getComputedStyle !== 'function') return '#8fa3b8';
  const v = getComputedStyle(nodeEl).getPropertyValue('--role');
  return String(v || '').trim() || '#8fa3b8';
}

function renderEdges() {
  const svg = $('#edges');
  const cRect = $('#canvas').getBoundingClientRect();
  const portsSvg = $('#edge-ports');
  const path = (x1, y1, x2, y2) => {
    const mx = (x1 + x2) / 2;
    return `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`;
  };
  // Порт — точка на краю узла; отвод — короткая линия от порта в поле.
  // Обе части считаются здесь, из живых getBoundingClientRect, поэтому при
  // перетаскивании узла порт едет вместе с ним: renderEdges зовётся и в
  // процессе перетаскивания (scheduleEdges), а не только на отпускании.
  const dot = (x, y, c) => `<circle cx="${x}" cy="${y}" r="4" fill="${c}"`
    + ` stroke="#12161a" stroke-width="1.5"/>`;
  const lead = (ax, ay, bx, by, c) => `<path d="M${ax},${ay} L${bx},${by}" fill="none"`
    + ` stroke="${c}" stroke-width="1.3" opacity=".5"/>`;
  let s = '';
  let stubs = ''; // подписанные начала связей input.* — они с другого слоя
  let ports = ''; // порты и отводы: слой #edge-ports, он поверх узлов
  for (const st of state.doc.steps) {
    for (const [field, src] of Object.entries(st.bind || {})) {
      if (!src) continue;
      const m = src.match(/^steps\.([^.]+)\.([\w\-.]+)$/);
      const dstEl = document.querySelector(`.node[data-id="${st.id}"] .inrow[data-field="${field}"]`);
      if (!dstEl) continue;
      const dRect = dstEl.getBoundingClientRect();
      // Точка входа — левый край шага, а не строка поля внутри него: узел
      // непрозрачный и нарисован поверх рёбер, поэтому конец линии (и стрелка)
      // в поле входа целиком уходили под корпус, и связь была видна только
      // обрывком в зазоре между шагами. По y остаётся центр поля входа — по
      // нему и видно, в какое именно поле пришла связь.
      const dstNode = dstEl.closest('.node');
      const nRect = dstNode ? dstNode.getBoundingClientRect() : dRect;
      const x2 = nRect.left - cRect.left - 6, y2 = dRect.top - cRect.top + dRect.height / 2;
      // ПОРТ назначения: точка ролевого цвета на левом краю узла ровно на
      // высоте поля + отвод в само поле — видно, в какое именно поле пришла
      // связь. Отвод рисуется поверх узлов (слой #edge-ports), поэтому не
      // тонет в корпусе; сама стрелка остаётся снаружи, у порта.
      if (portsSvg && dstNode) {
        const dc = roleColorOf(dstNode);
        ports += dot(x2, y2, dc);
        ports += lead(x2, y2, nRect.left - cRect.left + 8, y2, dc);
      }
      let x1 = 0, y1 = 0, ok = true;
      if (m) {
        const chip = document.querySelector(`.node[data-id="${m[1]}"] .outchip[data-out="${m[2]}"]`);
        if (!chip) { ok = false; }
        else {
          const sRect = chip.getBoundingClientRect();
          x1 = sRect.right - cRect.left + 4; y1 = sRect.top - cRect.top + sRect.height / 2;
          // ПОРТ источника: точка ролевого цвета сразу за чипом выхода.
          // Чип подписан именем, но без точки начала всех линий читались
          // одинаково — отличала только форма стрелки на конце.
          if (portsSvg) ports += dot(x1, y1, roleColorOf(chip.closest('.node')));
        }
      } else {
        // input.* — вход живёт на другом слое. Начало линии помечаем точкой:
        // имя входа и так написано в полосе входов слева, и подпись у начала
        // была вторым тем же именем рядом — дважды одно и то же на экране.
        ok = false;
        x1 = 96; y1 = y2;
        // маркер входа — холодный: тёплое в палитре только за семантикой
        stubs += `<circle cx="10" cy="${y2}" r="3" fill="#5f97c4"/>`;
      }
      s += `<path class="edge${ok ? '' : ' hi'}" d="${path(x1, y1, x2, y2)}"`
        + ` marker-end="url(#${ok ? 'edge-arr' : 'edge-arr-hi'})"/>`;
    }
  }
  svg.innerHTML = EDGE_DEFS + stubs + s;
  if (portsSvg) portsSvg.innerHTML = ports;
  // Резиновая линия протяжки живёт в этом же svg, а innerHTML его снёс.
  // Восстанавливаем, если протяжка ещё идёт.
  drawGhost();
}

// ── панель свойств ────────────────────────────────────────────────────────
const FORM_TYPES = ['', 'string', 'number', 'boolean', 'array', 'object'];
const FORM_FORMATS = ['', 'text', 'email', 'url', 'ip', 'file_ref'];
const WHEN_OPS = [
  ['', '— без условия —'], ['truthy', 'truthy — значение истинно'],
  ['exists', 'exists — путь существует'], ['missing', 'missing — пути нет'],
  ['eq', 'eq — равно'], ['neq', 'neq — не равно'],
  ['gt', 'gt — больше (число)'], ['gte', 'gte — больше или равно (число)'],
  ['lt', 'lt — меньше (число)'], ['lte', 'lte — меньше или равно (число)'],
  ['contains', 'contains — содержит (строка/массив)'],
];
const NO_VALUE_OPS = ['truthy', 'exists', 'missing'];
function whenBlock(st) {
  const w = st.when;
  const opOpts = WHEN_OPS.map(([v, t]) => `<option value="${v}"${(w ? w.op : '') === v ? ' selected' : ''}>${t}</option>`).join('');
  if (!w) return `<div class="pblock"><label>when — условие (шаг идёт, только если истинно; иначе skipped)</label>
    <select data-whenop>${opOpts}</select></div>`;
  const srcs = sourceOptions(st.id);
  let pathOpts = '<option value="">— путь —</option>' +
    srcs.map(o => `<option value="${esc(o)}"${w.path === o ? ' selected' : ''}>${esc(o)}</option>`).join('');
  if (w.path && !srcs.includes(w.path)) {
    pathOpts += `<option value="${esc(w.path)}" selected>${esc(w.path)} (вручную)</option>`;
  }
  const needVal = !NO_VALUE_OPS.includes(w.op);
  return `
  <div class="pblock"><label>when — условие (шаг идёт, только если истинно; иначе skipped)</label>
    <select data-whenop>${opOpts}</select></div>
  <div class="prow"><div class="pblock" style="flex:2"><label>when.path (input.* / steps.X.out)</label>
    <select data-whenpath>${pathOpts}</select></div>
  ${needVal ? `<div class="pblock" style="flex:1"><label>when.value${w.op.startsWith('g') || w.op.startsWith('l') ? ' (число)' : ''}</label>
    <input data-whenv value="${esc(w.value ?? '')}" placeholder="10 / text" spellcheck="false"/></div>` : ''}</div>`;
}

// v0.28: управляющий поток шага (foreach/parallel_group/after_foreach).
// human_gate: foreach и parallel_group запрещены ядром — вместо полей подсказка.
function flowBlock(st) {
  const gate = st.plugin === 'core/human_gate';
  if (gate) {
    return '<div class="hint">foreach / parallel_group: не применяются к human_gate (гейт сериализует терминал) — ядро это ошибками валидации.</div>';
  }
  const srcs = sourceOptions(st.id);
  let fOpts = '<option value="">— без foreach —</option>' +
    srcs.map(o => `<option value="${esc(o)}"${st.foreach === o ? ' selected' : ''}>${esc(o)}</option>`).join('');
  if (st.foreach && !srcs.includes(st.foreach)) {
    fOpts += `<option value="${esc(st.foreach)}" selected>${esc(st.foreach)} (вручную)</option>`;
  }
  return `
  <div class="pblock"><label>foreach — массив: шаг по каждому элементу (input.&lt;foreach_item&gt; перезаписывается)</label>
    <select data-sforeach>${fOpts}</select></div>
  <div class="prow"><div class="pblock" style="flex:1"><label>foreach_item (по умолчанию item)</label>
    <input data-sfitem value="${esc(st.foreach_item || '')}" placeholder="item" spellcheck="false"/></div>
  <div class="pblock" style="flex:1"><label>parallel_group — смежные шаги с одним именем — параллельно</label>
    <input data-spgrp value="${esc(st.parallel_group || '')}" placeholder="analyze" spellcheck="false"/></div></div>
  <div class="pblock"><label><input type="checkbox" data-safter ${st.after_foreach ? 'checked' : ''}/> after_foreach — один раз после всего pipeline-foreach (агрегаты steps.X_all)</label></div>`;
}

function loopBlock(st) {
  const on = !!st.loop;
  if (!on) {
    return `<div class="pblock"><label class="loop-toggle"><input type="checkbox" data-sloop-toggle/> цикл шага — повторять до условия</label></div>`;
  }
  return `
  <div class="pblock"><label class="loop-toggle"><input type="checkbox" data-sloop-toggle checked/> цикл шага — повторять до условия</label>
    <div class="hint">Шаг запускается повторно, пока условие истинно. Пустое условие использует steps.&lt;id&gt;.continue; пустой лимит — 100 итераций.</div>
  </div>
  <div class="prow">
    <div class="pblock" style="flex:2"><label>loop_condition — путь к boolean</label>
      <input data-sloop-condition value="${esc(st.loop_condition || '')}" placeholder="steps.${esc(st.id)}.continue" spellcheck="false"/></div>
    <div class="pblock" style="flex:1"><label>max_iterations (1–100; пусто = 100)</label>
      <input data-sloop-max type="number" min="1" max="100" value="${esc(st.max_iterations || '')}" placeholder="100" spellcheck="false"/></div>
  </div>`;
}

function pipelineFlowBlock() {
  const d = state.doc;
  const srcs = sourceOptions(null);
  let fOpts = '<option value="">— без батча —</option>' +
    srcs.map(o => `<option value="${esc(o)}"${d.foreach === o ? ' selected' : ''}>${esc(o)}</option>`).join('');
  if (d.foreach && !srcs.includes(d.foreach)) {
    fOpts += `<option value="${esc(d.foreach)}" selected>${esc(d.foreach)} (вручную)</option>`;
  }
  return `
  <div class="pblock"><label>foreach — весь пайплайн по каждому элементу массива</label>
    <select data-pforeach>${fOpts}</select></div>
  <div class="prow"><div class="pblock" style="flex:1"><label>foreach_item (item-переменная, по умолчанию item)</label>
    <input data-pfitem value="${esc(d.foreach_item || '')}" placeholder="row" spellcheck="false"/></div>
  <div class="pblock" style="flex:1"><label>item_type (object / string / number)</label>
    <input data-ptype value="${esc(d.item_type || '')}" placeholder="object" spellcheck="false"/></div>
  <div class="pblock" style="flex:1"><label>item_format</label>
    <input data-pformat value="${esc(d.item_format || '')}" placeholder="(пусто)" spellcheck="false"/></div></div>
  <div class="hint">Шаги с after_foreach выполняются один раз после всех элементов
  (агрегаты steps.X_all). См. examples/csv_foreach_summary.yaml.</div>`;
}

// v0.29: retry — повтор таймаутов и retryable-ошибок; исчерпан = stop.
function retryBlock(st) {
  const r = st.retry;
  const on = r || st.on_error === 'retry';
  return `
  <div class="pblock"><label><input type="checkbox" data-sretry ${on ? 'checked' : ''}/> retry — повторять таймауты и retryable-ошибки (исчерпан = stop)</label></div>
  ${on ? `<div class="prow"><div class="pblock" style="flex:1"><label>attempts (≥1)</label>
    <input data-srattempts type="number" min="1" value="${esc(r ? r.attempts : 3)}" spellcheck="false"/></div>
  <div class="pblock" style="flex:1"><label>delay (2s, 500ms, 1m…)</label>
    <input data-srdelay value="${esc(r ? (r.delay || '') : '')}" placeholder="2s" spellcheck="false"/></div>
  <div class="pblock" style="flex:1"><label>backoff</label>
    <select data-srbackoff><option value="fixed"${(r && r.backoff === 'fixed') || (!r) ? ' selected' : ''}>fixed</option><option value="exponential"${r && r.backoff === 'exponential' ? ' selected' : ''}>exponential</option></select></div></div>` : ''}`;
}

// v0.6: подсказка в шаге — какую сеть просит плагин (манифест) и не
// запрещена ли она политикой пайплайна. Пустое поле — это тоже deny
// (EffectiveNetwork), и после сборки цепочки из сценария с allow оно
// пустое всегда: называть такой шаг «аудит — журнал» значило бы показывать
// человеку, будто сеть выдана. Формулировки повторяют networkDenyBecause.
function pluginNetworkHint(st) {
  const info = pluginInfo(st.plugin);
  const net = (info && info.permissions && info.permissions.network) || [];
  if (!net.length) return '';
  const hosts = net.map(n => (n.any_host ? '*' : (n.host + ':' + n.port))).join(', ');
  if (state.doc.network === 'allow')
    return `<div class="hint">сеть плагина: ${esc(hosts)} (declare-now, аудит — журнал)</div>`;
  const why = state.doc.network === 'deny'
    ? 'пайплайн запрещает сеть (network: deny, ошибка валидации)'
    : 'сеть не объявлена: пустое поле = deny, ошибка валидации. Выдайте её '
      + 'кнопкой «разрешить сеть» или выбором network → allow в свойствах пайплайна';
  return `<div class="hint">сеть плагина: <span style="color:var(--err,#e5534b)">${esc(hosts)} — ${esc(why)}</span></div>`;
}

// v0.5: подсказка в шаге — какие env-ключи просит плагин (манифест) и
// объявлены ли они в pipeline secrets.
function pluginSecretsHint(st) {
  const info = pluginInfo(st.plugin);
  const needs = (info && info.permissions && info.permissions.secrets) || [];
  if (!needs.length) return '';
  const have = new Set(state.doc.secrets || []);
  const missing = needs.filter(k => !have.has(k));
  const txt = missing.length
    ? `<span style="color:var(--err,#e5534b)">нужны ключи ${needs.map(esc).join(', ')} — не все объявлены в secrets пайплайна (блок «Пайплайн»)</span>`
    : `просит ключи ${needs.map(esc).join(', ')} — объявлены в secrets пайплайна ✓`;
  return `<div class="hint">secrets плагина: ${txt}</div>`;
}

// v0.5: secrets — чипы env-ключей + добавление. Ключ читается ядром из env
// и передаётся плагину; кросс-чек с permissions.secrets — валидатор (warnings).
function secretsBlock() {
  const keys = state.doc.secrets || [];
  const chips = keys.map((k, i) =>
    `<span class="chip">${esc(k)}<button data-sdel="${i}" title="убрать">×</button></span>`).join(' ');
  return `<div class="chips">${chips || '<span class="hint">нет ключей</span>'}
    <input data-skey placeholder="ENV_KEY" spellcheck="false" style="width:160px"/>
    <button data-sadd>+ ключ</button></div>
    <div class="hint">Ключи читаются из окружения при запуске (secrets: [KEY] в YAML).</div>`;
}

function renderProps() {
  const el = $('#props');
  let html = '';
  if (state.unsupported.length) {
    html += `<div id="banner" style="display:block">Пайплайн содержит поля, которые редактор не умеет сохранять:
      <b>${state.unsupported.map(esc).join(', ')}</b>. Сохранение отключено, чтобы не потерять данные; открой файл в YAML-режиме.</div>`;
  }
  const st = state.doc.steps.find(s => s.id === state.sel);
  if (st) {
    const gate = st.plugin === 'core/human_gate';
    const ins = gate ? '' : inFields(st.plugin).map(f => {
      const opts = ['<option value="">— не привязано —</option>']
        .concat(sourceOptions(st.id).map(o => `<option value="${esc(o)}"${st.bind[f] === o ? ' selected' : ''}>${esc(o)}</option>`)).join('');
      // текущее значение, которого нет в списке (например, ручной путь)
      if (st.bind[f] && !sourceOptions(st.id).includes(st.bind[f]) && !('input.' + st.bind[f])) {
        opts += `<option value="${esc(st.bind[f])}" selected>${esc(st.bind[f])} (вручную)</option>`;
      }
      return `<div class="pblock"><label>bind: ${esc(f)}</label><select data-bind="${esc(f)}">${opts}</select></div>`;
    }).join('');
    const gateBlock = gate ? `
      <div class="pblock"><label>form — построчно: «путь» или «e:путь» (editable)</label>
        <textarea data-gform rows="4" spellcheck="false">${esc((st.form || []).map(f => (f.editable ? 'e:' : '') + f.field).join('\n'))}</textarea></div>
      ${(st.form || []).map((f, idx) => {
        const type = f.type || '', format = f.format || '';
        const typeOpts = FORM_TYPES.map(v => `<option value="${v}"${v === type ? ' selected' : ''}>${v || '— type —'}</option>`).join('') +
          (!FORM_TYPES.includes(type) ? `<option value="${esc(type)}" selected>${esc(type)} (вручную)</option>` : '');
        const formatOpts = FORM_FORMATS.map(v => `<option value="${v}"${v === format ? ' selected' : ''}>${v || '— format —'}</option>`).join('') +
          (!FORM_FORMATS.includes(format) ? `<option value="${esc(format)}" selected>${esc(format)} (вручную)</option>` : '');
        return `<div class="prow">
          <div class="pblock"><label>form[${idx + 1}] type — ${esc(f.field)}</label><select data-gtype="${idx}">${typeOpts}</select></div>
          <div class="pblock"><label>format</label><select data-gformat="${idx}">${formatOpts}</select></div>
        </div>`;
      }).join('')}
      <div class="pblock"><label>actions — по одному на строку</label>
        <textarea data-gactions rows="3" spellcheck="false">${esc((st.actions || []).join('\n'))}</textarea></div>
      <div class="pblock"><label>on_reject</label>
        <select data-gonreject>
          <option value="stop"${(st.on_reject || 'stop') === 'stop' ? ' selected' : ''}>stop (ран остановлен)</option>
          <option value=""${!st.on_reject ? ' selected' : ''}>continue (идём дальше)</option>
        </select></div>
      <div class="pblock"><label>approval</label>
        <select data-gapproval>
          <option value=""${!st.approval ? ' selected' : ''}>по умолчанию (any)</option>
          <option value="any"${st.approval === 'any' ? ' selected' : ''}>any</option>
          <option value="human"${st.approval === 'human' ? ' selected' : ''}>human — только человек</option>
        </select></div>` : '';
    html += `
      <h3 style="display:flex;align-items:center;justify-content:space-between">Шаг<button data-select-pipeline>‹ настройки пайплайна</button></h3>
      <div class="prow">
        <div class="pblock"><label>id</label><input data-sid value="${esc(st.id)}" spellcheck="false"/></div>
        <div class="pblock"><label>on_error</label>
          <select data-serr><option value="stop"${st.on_error === 'stop' ? ' selected' : ''}>stop</option><option value="skip"${st.on_error === 'skip' ? ' selected' : ''}>skip</option><option value="retry"${st.on_error === 'retry' ? ' selected' : ''}>retry (блок ниже)</option></select></div>
      </div>
      <div class="pblock"><label>плагин</label><input value="${esc(st.plugin)}" readonly style="color:var(--dim)"/></div>
      ${pluginSecretsHint(st)}
      ${pluginNetworkHint(st)}
      <div class="pblock"><label>timeout (пусто = 60s): 10s, 1m30s, …</label><input data-stimeout value="${esc(st.timeout || '')}" placeholder="60s" spellcheck="false"/></div>
      ${whenBlock(st)}
      ${flowBlock(st)}
      ${loopBlock(st)}
      ${retryBlock(st)}
      ${ins}
      ${gateBlock}
      <button class="danger" data-del>удалить шаг</button>
      <div class="hint">Двигай шаг за заголовок. Связи — в «bind» (список источников) — рёбра перерисуются сами.</div>`;
  } else {
    const rows = state.doc.input.map(inputEditor).join('');
    html += `
      <h3>Пайплайн (выбери шаг для правки шага)</h3>
      <div class="pblock"><label>имя</label><input data-pname value="${esc(state.doc.name)}" spellcheck="false"/></div>
      <div class="pblock"><label>gates — политика гейтов</label>
        <select data-pgates>
          <option value=""${!state.doc.gates ? ' selected' : ''}>по шагам (approval)</option>
          <option value="any"${state.doc.gates === 'any' ? ' selected' : ''}>any</option>
          <option value="human_only"${state.doc.gates === 'human_only' ? ' selected' : ''}>human_only — все гейты только человеком</option>
        </select></div>
      <div class="pblock"><label>входы input.* — значения или schema</label>${rows || '<div class="hint">нет входов</div>'}
        <button data-inadd>+ вход</button></div>
      <div class="pblock"><label>secrets — env-ключи для плагинов (v0.5)</label>
        ${secretsBlock()}</div>
<div class="pblock"><label>network — сетевая политика (v0.6)</label>
        <select data-nnet>
          <option value=""${!state.doc.network ? ' selected' : ''}>по умолчанию — сеть запрещена</option>
          <option value="allow"${state.doc.network === 'allow' ? ' selected' : ''}>разрешить — allow</option>
          <option value="deny"${state.doc.network === 'deny' ? ' selected' : ''}>запретить — deny</option>
        </select>
        <div class="hint">Плагин, заявивший сеть в манифесте, требует <b>allow</b>: пустое поле и
        deny для него — ошибка валидации. Разрешение даётся плагину, а не всему пайплайну:
        список хостов остаётся в его манифесте, а решение пишется в журнал.</div></div>
      <h3>Батч (pipeline foreach)</h3>
      ${pipelineFlowBlock()}
      <div class="hint">Шагов: ${state.doc.steps.length}. Перетащи плагин слева на холст (или кликни по нему — узел появится на свободном месте).
      Обычные входы — фактические значения input.*; «схема» — декларация type, required, format, description и default.</div>`;
  }
  el.innerHTML = html;
  wireProps(st);
}

function wireProps(st) {
  const el = $('#props');
  const selectPipeline = el.querySelector('[data-select-pipeline]');
  if (selectPipeline) selectPipeline.onclick = () => { state.sel = null; renderAll(); };
  el.querySelectorAll('[data-bind]').forEach(sel => {
    sel.onchange = () => {
      pushUndo();
      if (st.bind) {
        if (sel.value) st.bind[sel.dataset.bind] = sel.value;
        else delete st.bind[sel.dataset.bind];
      }
      renderAll();
    };
  });
  const sid = el.querySelector('[data-sid]');
  if (sid) sid.onchange = () => {
    const old = st.id, v = sid.value.trim();
    if (!v || v === old) { sid.value = old; return; }
    if (state.doc.steps.some(s => s.id === v)) { sid.value = old; alert('id уже занят'); return; }
    pushUndo();
    // переименовываем и все ссылки в чужих bind
    for (const s of state.doc.steps) for (const [k, src] of Object.entries(s.bind || {})) {
      if (src && src.startsWith('steps.' + old + '.')) s.bind[k] = src.replace('steps.' + old + '.', 'steps.' + v + '.');
    }
    st.id = v; state.sel = v;
    renderAll();
  };
  const serr = el.querySelector('[data-serr]');
  if (serr) serr.onchange = () => {
    pushUndo();
    st.on_error = serr.value;
    // v0.29: on_error=retry без блока — заполняем дефолты (3/1s/fixed)
    if (serr.value === 'retry' && !st.retry) st.retry = { attempts: 3, delay: '1s', backoff: 'fixed' };
    renderAll();
  };
  const sret = el.querySelector('[data-sretry]');
  if (sret) sret.onchange = () => {
    pushUndo();
    if (sret.checked) {
      st.retry = st.retry || { attempts: 3, delay: '1s', backoff: 'fixed' };
    } else {
      st.retry = null;
      if (st.on_error === 'retry') st.on_error = 'stop';
    }
    renderAll();
  };
  const sra = el.querySelector('[data-srattempts]');
  if (sra) sra.onchange = () => { pushUndo(); if (st.retry) st.retry.attempts = Math.max(1, parseInt(sra.value, 10) || 1); renderAll(); };
  const srd = el.querySelector('[data-srdelay]');
  if (srd) srd.onchange = () => { pushUndo(); if (st.retry) st.retry.delay = srd.value.trim(); renderAll(); };
  const srb = el.querySelector('[data-srbackoff]');
  if (srb) srb.onchange = () => { pushUndo(); if (st.retry) st.retry.backoff = srb.value; renderAll(); };
  const sto = el.querySelector('[data-stimeout]');
  if (sto) sto.onchange = () => { pushUndo(); st.timeout = sto.value.trim(); renderAll(); };
  const wop = el.querySelector('[data-whenop]');
  if (wop) wop.onchange = () => {
    pushUndo();
    const op = wop.value;
    if (!op) { st.when = null; }
    else {
      const cur = st.when || {};
      st.when = {
        path: cur.path || '', op,
        value: NO_VALUE_OPS.includes(op) ? undefined : (cur.value !== undefined ? cur.value : ''),
      };
    }
    renderAll();
  };
  const wpath = el.querySelector('[data-whenpath]');
  if (wpath) wpath.onchange = () => { pushUndo(); if (st.when) st.when.path = wpath.value; renderAll(); };
  const wval = el.querySelector('[data-whenv]');
  if (wval) wval.onchange = () => { pushUndo(); if (st.when) st.when.value = wval.value; renderAll(); };
  const sf = el.querySelector('[data-sforeach]');
  if (sf) sf.onchange = () => { pushUndo(); st.foreach = sf.value; renderAll(); };
  const sfi = el.querySelector('[data-sfitem]');
  if (sfi) sfi.onchange = () => { pushUndo(); st.foreach_item = sfi.value.trim(); renderAll(); };
  const spg = el.querySelector('[data-spgrp]');
  if (spg) spg.onchange = () => { pushUndo(); st.parallel_group = spg.value.trim(); renderAll(); };
  const sa = el.querySelector('[data-safter]');
  if (sa) sa.onchange = () => { pushUndo(); st.after_foreach = sa.checked; renderAll(); };
  const loopToggle = el.querySelector('[data-sloop-toggle]');
  if (loopToggle) loopToggle.onchange = () => {
    pushUndo();
    if (loopToggle.checked) st.loop = st.loop || 'repeat';
    else { st.loop = ''; st.loop_condition = ''; st.max_iterations = 0; }
    renderAll();
  };
  const loopCondition = el.querySelector('[data-sloop-condition]');
  if (loopCondition) loopCondition.onchange = () => { pushUndo(); st.loop_condition = loopCondition.value.trim(); renderAll(); };
  const loopMax = el.querySelector('[data-sloop-max]');
  if (loopMax) loopMax.onchange = () => {
    const raw = loopMax.value.trim();
    const value = raw === '' ? 0 : Number(raw);
    if (!Number.isInteger(value) || (raw !== '' && value < 1) || value > 100) {
      alert('max_iterations должен быть от 1 до 100; пустое поле использует 100.');
      renderAll();
      return;
    }
    pushUndo(); st.max_iterations = value; renderAll();
  };
  const gform = el.querySelector('[data-gform]');
  if (gform) gform.onchange = () => {
    const previous = st.form || [];
    const lines = gform.value.split('\n').map(l => l.trim()).filter(Boolean);
    const used = new Set();
    pushUndo();
    st.form = lines.map((l, idx) => {
      const editable = l.startsWith('e:');
      const field = editable ? l.slice(2) : l;
      let oldIdx = previous.findIndex((f, i) => !used.has(i) && !!f.editable === editable && f.field === field);
      if (oldIdx < 0 && lines.length === previous.length && !used.has(idx)) oldIdx = idx;
      if (oldIdx >= 0) used.add(oldIdx);
      const old = previous[oldIdx] || {};
      return { field, editable, type: old.type || '', format: old.format || '' };
    });
    renderAll();
  };
  el.querySelectorAll('[data-gtype]').forEach(sel => {
    sel.onchange = () => {
      const field = (st.form || [])[+sel.dataset.gtype];
      if (!field) return;
      pushUndo();
      field.type = sel.value;
      renderAll();
    };
  });
  el.querySelectorAll('[data-gformat]').forEach(sel => {
    sel.onchange = () => {
      const field = (st.form || [])[+sel.dataset.gformat];
      if (!field) return;
      pushUndo();
      field.format = sel.value;
      renderAll();
    };
  });
  const gact = el.querySelector('[data-gactions]');
  if (gact) gact.onchange = () => {
    pushUndo();
    st.actions = gact.value.split('\n').map(l => l.trim()).filter(Boolean);
    renderAll();
  };
  const gonr = el.querySelector('[data-gonreject]');
  if (gonr) gonr.onchange = () => { pushUndo(); st.on_reject = gonr.value; renderAll(); };
  const gapproval = el.querySelector('[data-gapproval]');
  if (gapproval) gapproval.onchange = () => { pushUndo(); st.approval = gapproval.value; renderAll(); };
  const del = el.querySelector('[data-del]');
  if (del) del.onclick = () => {
    pushUndo();
    state.doc.steps = state.doc.steps.filter(s => s.id !== st.id);
    // чужие bind на удалённый — очищаем
    for (const s of state.doc.steps) for (const [k, src] of Object.entries(s.bind || {})) {
      if (src && src.startsWith('steps.' + st.id + '.')) delete s.bind[k];
    }
    state.sel = null;
    renderAll();
  };
  const pn = el.querySelector('[data-pname]');
  if (pn) pn.onchange = () => { pushUndo(); state.doc.name = pn.value.trim() || state.doc.name; renderAll(); };
  const pgates = el.querySelector('[data-pgates]');
  if (pgates) pgates.onchange = () => { pushUndo(); state.doc.gates = pgates.value; renderAll(); };
  el.querySelectorAll('[data-iname]').forEach(control => control.onchange = () => {
    const idx = +control.dataset.iname;
    const input = state.doc.input[idx];
    const name = control.value.trim();
    if (!name || state.doc.input.some((other, i) => i !== idx && other.name === name)) {
      alert(!name ? 'Имя входа не может быть пустым.' : 'Такое имя входа уже есть.');
      renderAll();
      return;
    }
    pushUndo(); input.name = name; renderAll();
  });
  el.querySelectorAll('[data-ikind]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.ikind];
    if (control.value === 'schema') {
      pushUndo();
      input.typed = true;
      input.type = input.type || (Array.isArray(input.default) ? 'array' : input.default === null ? 'string' : typeof input.default === 'object' ? 'object' : typeof input.default);
      if (!INPUT_TYPES.includes(input.type)) input.type = 'string';
      input.required = input.required ?? null;
      input.format = input.format || '';
      input.description = input.description || '';
      input.has_default = input.has_default !== false;
      if (!input.has_default || !valueMatchesInputType(input.default, input.type)) input.default = defaultForInputType(input.type);
      renderAll();
      return;
    }
    if (input.typed && (input.required !== undefined && input.required !== null || input.format || input.description || !input.has_default) &&
        !confirm('Переключить на обычное значение? Параметры type/required/format/description будут удалены.')) {
      renderAll();
      return;
    }
    pushUndo();
    input.typed = false;
    input.default = input.has_default ? input.default : '';
    input.has_default = true;
    delete input.type; delete input.required; delete input.format; delete input.description;
    renderAll();
  });
  el.querySelectorAll('[data-itype]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.itype];
    pushUndo();
    input.type = control.value;
    if (!input.has_default || !valueMatchesInputType(input.default, input.type)) input.default = defaultForInputType(input.type);
    renderAll();
  });
  el.querySelectorAll('[data-irequired]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.irequired];
    pushUndo();
    if (control.value === '') delete input.required;
    else input.required = control.value === 'true';
    renderAll();
  });
  el.querySelectorAll('[data-iformat]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.iformat];
    pushUndo(); input.format = control.value.trim(); renderAll();
  });
  el.querySelectorAll('[data-idescription]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.idescription];
    pushUndo(); input.description = control.value.trim(); renderAll();
  });
  el.querySelectorAll('[data-ihasdefault]').forEach(control => control.onchange = () => {
    const input = state.doc.input[+control.dataset.ihasdefault];
    pushUndo();
    input.has_default = control.checked;
    if (control.checked && input.default === undefined) input.default = defaultForInputType(input.type || 'string');
    renderAll();
  });
  el.querySelectorAll('[data-indef]').forEach(control => {
    control.onchange = () => {
      const input = state.doc.input[control.dataset.indef];
      try {
        const value = parseInputText(control.value, input.default, input.typed ? input.type : '');
        if (input.typed && !valueMatchesInputType(value, input.type)) throw new Error('значение не соответствует type: ' + input.type);
        pushUndo();
        input.default = value;
        renderAll();
      } catch (e) {
        alert('Некорректное значение input.' + input.name + ': ' + e.message);
        renderAll();
      }
    };
  });
  el.querySelectorAll('[data-indel]').forEach(b => b.onclick = () => { pushUndo(); state.doc.input.splice(+b.dataset.indel, 1); renderAll(); });
  const pf = el.querySelector('[data-pforeach]');
  if (pf) pf.onchange = () => { pushUndo(); state.doc.foreach = pf.value; renderAll(); };
  const pfi = el.querySelector('[data-pfitem]');
  if (pfi) pfi.onchange = () => { pushUndo(); state.doc.foreach_item = pfi.value.trim(); renderAll(); };
  const pt = el.querySelector('[data-ptype]');
  if (pt) pt.onchange = () => { pushUndo(); state.doc.item_type = pt.value.trim(); renderAll(); };
  const pfmt = el.querySelector('[data-pformat]');
  if (pfmt) pfmt.onchange = () => { pushUndo(); state.doc.item_format = pfmt.value.trim(); renderAll(); };
  const inadd = el.querySelector('[data-inadd]');
  if (inadd) inadd.onclick = () => {
    let n = state.doc.input.length + 1;
    while (state.doc.input.some(input => input.name === 'field' + n)) n++;
    pushUndo();
    state.doc.input.push({ name: 'field' + n, default: '', typed: false, has_default: true });
    renderAll();
  };
  // v0.5: secrets — добавление/удаление env-ключей
  const skey = el.querySelector('[data-skey]');
  const sadd = el.querySelector('[data-sadd]');
  const addSecret = () => {
    const v = (skey && skey.value || '').trim();
    if (!v) return;
    if ((state.doc.secrets || []).includes(v)) { renderAll(); return; }
    pushUndo();
    state.doc.secrets = state.doc.secrets || [];
    state.doc.secrets.push(v);
    renderAll();
  };
  if (sadd) sadd.onclick = addSecret;
  if (skey) skey.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); addSecret(); } };
  el.querySelectorAll('[data-sdel]').forEach(b => b.onclick = () => {
    pushUndo();
    state.doc.secrets.splice(+b.dataset.sdel, 1);
    renderAll();
  });
  // v0.6: network — политика allow/deny/пусто(запрет). Любой выбор человека —
  // в том числе явный запрет — снимает напоминание о невыданной сети сценария.
  const nnet = el.querySelector('[data-nnet]');
  if (nnet) nnet.onchange = () => {
    pushUndo();
    state.doc.network = nnet.value;
    state.netPending = '';
    renderAll();
  };
}

// ── слои: входы и пайплайн ────────────────────────────────────────────────
// Два разных экрана, а не две колонки на одном холсте. На слое входа есть
// только точки со значениями; на слое пайплайна — только шаги и связи. Вход
// подключается к шагу НЕ протяжкой через экран, а списком источника в правой
// панели: так слои остаются раздельными и связь всё равно видна в подписи поля.
function setLayer(l) {
  state.layer = l === 'inputs' ? 'inputs' : 'pipeline';
  const app = $('#app');
  if (app) app.dataset.layer = state.layer;
  const bIn = $('#layer-inputs'), bPipe = $('#layer-pipeline');
  if (bIn) bIn.classList.toggle('active', state.layer === 'inputs');
  if (bPipe) bPipe.classList.toggle('active', state.layer === 'pipeline');
  const inp = $('#inlayer');
  if (inp) inp.style.display = state.layer === 'inputs' ? '' : 'none';
  const strip = $('#instrip');
  if (strip) strip.style.display = state.layer === 'pipeline' ? '' : 'none';
  renderAll();
}

function renderAll() {
  const onInputs = state.layer === 'inputs';
  // Слои обязаны быть видно раздельно, а не «примерно». Переключение слоя не
  // очищало старые узлы: на слое вход продолжал лежать шаг, и два слоя
  // выглядели смешанными — ровно то, от чего мы уходили.
  const canvas = $('#canvas');
  if (canvas) {
    canvas.querySelectorAll('.node').forEach(n => n.remove());
    const svg = $('#edges');
    if (svg) svg.innerHTML = '';
  }
  if (!onInputs) {
    renderNodes();
    renderInputStrip();
    renderEdges();
    syncNote();
  }
  if (onInputs) renderInputNodes();
  renderProps();
  // Подсказка — только для по-настоящему пустого слоя. Раньше она стояла
  // всегда и налезала ровно на колонку входов.
  const hint = $('#canvas-hint');
  if (hint) {
    const empty = onInputs
      ? !state.doc.input.length
      : !state.doc.steps.length;
    hint.classList.toggle('hidden', !empty);
    hint.innerHTML = onInputs
      ? '<b>Слой входов.</b> Здесь только то, что пайплайн получает: текст или ник,'
        + ' число, фото или файл. Правой кнопкой по холсту — то же самое.'
        + '<br><span style="opacity:.75">Дальше переключись на «Пайплайн» и свяжи вход'
        + ' с шагом: в правой панели у поля выбери источник input.*</span>'
      : '<b>Слой пайплайна.</b> Здесь шаги и связи. Перетащи плагин слева'
        + ' на холст, протяни связь от выхода шага к полю другого.'
        + '<br><span style="opacity:.75">Значения, которые пайплайн получает, задаются'
        + ' на слое «Входы»; подключить их можно у любого поля в правой панели.</span>';
  }
  // Стрелки связей живут в отдельном SVG и не перерисуются сами: без этого
  // вызова после загрузки, связывания или любого ререндера связей не видно,
  // хотя данные в bind уже есть.
  renderEdges();
  scheduleValidate();
}

// ── пояснения на холсте ───────────────────────────────────────────────────
// Текст берётся из реальных данных проекта: ошибки валидации ядра, состав
// текущей цепочки, объявленный foreach. Ничего не выдумывается — панель либо
// повторяет то, что ядро уже сказало, либо перечисляет то, что реально лежит
// в state.doc. Автопанель одна: она отвечает на «что делать сейчас».
function noteSigNow() {
  const errs = (state.valErrs || []).length;
  return [errs, state.doc.steps.length, state.doc.foreach || '',
    state.doc.network || '', (state.doc.gates || ''), state.netPending || ''].join('|');
}

// Сценарий объявил сеть, а сборщик цепочки её не перенёс: молчать об этом
// нельзя — человек должен понимать, что цепочка собрана без сети и что
// выдать сеть его решение, одним кликом. Строка нужна в двух ветках панели
// (ошибки и сводка), иначе при E_NETWORK_DENIED объяснение ядра «поле network
// не задано» осталось бы без причины.
function netPendingRow() {
  return '<span class="row"><span class="k">сеть сценария:</span> <code>'
    + esc(state.netPending) + '</code> НЕ перенесена — сборщик цепочки сеть не '
    + 'выдаёт. Выдайте её сами: «разрешить сеть» в панели ошибок слева или '
    + 'network → allow в свойствах пайплайна.</span>';
}

function autoNote() {
  const errs = state.valErrs || [];
  if (errs.length) {
    return {
      kind: 'err', title: 'Ошибки валидации',
      // Не перенесённая сценарием сеть проговаривается и на панели ошибок:
      // иначе при E_NETWORK_DENIED человек прочтёт «поле network не задано» и
      // не поймёт, что причина — ровно что сеть не выдана, и кому её выдать.
      body: '<span class="row"><span class="k">ядро вернуло:</span> <code>'
        + esc(errs[0]) + '</code></span>'
        + (state.netPending ? netPendingRow() : '')
        + (errs.length > 1
          ? `<span class="row"><span class="k">ещё ${errs.length - 1} — см. панель «ошибки» слева.</span></span>`
          : '')
        + '<span class="row">Панель можно убрать крестиком — ошибки всё равно видны на самом узле.</span>',
    };
  }
  const steps = state.doc.steps || [];
  if (!steps.length) {
    return {
      kind: 'hint', title: 'Слой пайплайна',
      body: '<span class="row">Шагов пока нет. Перетащи плагин из палитры слева'
        + ' на холст и протяни связь от выхода одного шага к полю другого.</span>',
    };
  }
  const gates = steps.filter(s => s.plugin === 'core/human_gate').length;
  const rows = ['<span class="row"><span class="k">шагов:</span> <code>' + steps.length + '</code>'
    + (gates ? ' <span class="k">· гейтов:</span> <code>' + gates + '</code>' : '') + '</span>'];
  if (state.doc.foreach) {
    rows.push('<span class="row"><span class="k">перебор:</span> <code>'
      + esc(state.doc.foreach) + '</code> — шаг выполняется на каждый элемент.</span>');
  }
  if (state.doc.network) {
    rows.push('<span class="row"><span class="k">сеть:</span> <code>'
      + esc(state.doc.network) + '</code></span>');
  }
  // Сценарий объявил сеть, а сборщик её не перенёс. Молчаливый deny здесь
  // недопустим: человек должен понимать, что цепочка собрана без сети и что
  // выдать её — его решение, одним кликом. Панель висит до выдачи или до
  // явного запрета, а не до следующей валидации.
  if (state.netPending) {
    rows.push(netPendingRow());
  }
  const unbound = steps.filter(s => s.plugin !== 'core/human_gate'
    && inFields(s.plugin).some(f => !(s.bind || {})[f])).length;
  if (unbound) {
    rows.push('<span class="row"><span class="k">без входа:</span> <code>'
      + unbound + '</code> — протяни связь к полю или выбери источник справа.</span>');
  }
  return { kind: 'info', title: 'Эта цепочка', body: rows.join('') };
}

function syncNote() {
  const host = $('#notes');
  if (!host) return;
  const sig = noteSigNow();
  if (sig !== state.noteSig) {
    state.noteSig = sig;
    if (state.noteOff !== sig) state.notes = [{ id: 'ctx', pos: [700, 420], ...autoNote() }];
  }
  renderNotes();
}

function renderNotes() {
  const host = $('#notes');
  if (!host) return;
  host.innerHTML = '';
  for (const n of state.notes) {
    const el = document.createElement('div');
    el.className = 'note' + (n.kind === 'err' ? ' err' : '');
    el.style.left = n.pos[0] + 'px';
    el.style.top = n.pos[1] + 'px';
    el.dataset.note = n.id;
    el.innerHTML = `<div class="nh"><b>${esc(n.title)}</b>`
      + `<button class="nx" data-nx="${esc(n.id)}" title="убрать пояснение">×</button></div>`
      + `<div class="nb">${n.body}</div>`;
    el.querySelector('.nh').addEventListener('mousedown', e => startNoteDrag(e, n, el));
    el.querySelector('.nx').addEventListener('click', e => {
      e.stopPropagation();
      if (n.id === 'ctx') state.noteOff = state.noteSig;
      state.notes = state.notes.filter(x => x.id !== n.id);
      renderNotes();
    });
    host.appendChild(el);
  }
}

function startNoteDrag(e, n, el) {
  if (e.button !== 0) return;
  e.preventDefault();
  e.stopPropagation();
  const sx = e.clientX, sy = e.clientY;
  const ox = n.pos[0], oy = n.pos[1];
  let moved = false;
  const mm = ev => {
    const dx = ev.clientX - sx, dy = ev.clientY - sy;
    if (Math.abs(dx) + Math.abs(dy) > 3) moved = true;
    el.style.left = (ox + dx) + 'px';
    el.style.top = (oy + dy) + 'px';
  };
  const mu = ev => {
    document.removeEventListener('mousemove', mm);
    document.removeEventListener('mouseup', mu);
    if (moved) {
      n.pos = [Math.max(0, snap(ox + ev.clientX - sx)), Math.max(0, snap(oy + ev.clientY - sy))];
      renderNotes();
    }
  };
  document.addEventListener('mousemove', mm);
  document.addEventListener('mouseup', mu);
}

// ── канвас: drop из палитры, drag узлов ───────────────────────────────────
function initCanvas() {
  const wrap = $('#canvas-wrap'), canvas = $('#canvas');
  // v0.8a: добавление из палитры — через mouse-события (startPaletteDrag);
  // нативный dragover/drop не работает в sandboxed iframe (превью)
  canvas.addEventListener('mousedown', e => {
    if (e.target === canvas || e.target.id === 'edges') { state.sel = null; renderAll(); }
  });
  // Правая кнопка на холсте — точка входа без поиска глазами: «с чего начать»
  // стоит под курсором, а не в панели свойств. Те же три пункта, что и в палитре,
  // потому что второй способ добавления одного и того же учить не за чем.
  canvas.addEventListener('contextmenu', e => {
    e.preventDefault();
    openCtxMenu(e.clientX, e.clientY);
  });
  document.addEventListener('mousedown', e => {
    const m = $('#ctxmenu');
    if (m && !m.contains(e.target)) m.style.display = 'none';
  });

  $('#input-add').querySelectorAll('[data-add-in]').forEach(b => {
    b.onclick = () => addInput(b.dataset.addIn);
  });
  $('#layer-inputs').onclick = () => setLayer('inputs');
  $('#layer-pipeline').onclick = () => setLayer('pipeline');
  const ps = $('#pal-search');
  if (ps) ps.oninput = () => { state.palQuery = ps.value; renderPalette(); };
  window.addEventListener('keydown', e => {
    const tag = (e.target.tagName || '').toLowerCase();
    const typing = tag === 'input' || tag === 'textarea' || tag === 'select';
    // Esc снимает связывание раньше всего: оно вооружено мышью, иначе
    // отменить его можно только повторным щелчком по тому же выходу.
    if (e.key === 'Escape' && (state.link || state.linkDrag)) { e.preventDefault(); cancelLink(); return; }
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'z' && !e.shiftKey) { e.preventDefault(); undo(); return; }
    if ((e.ctrlKey || e.metaKey) && (e.key.toLowerCase() === 'y' || (e.key.toLowerCase() === 'z' && e.shiftKey))) { e.preventDefault(); redo(); return; }
    if ((e.key === 'Delete' || e.key === 'Backspace') && state.sel && !typing) {
      const st = state.doc.steps.find(s => s.id === state.sel);
      if (st) { e.preventDefault();
        pushUndo();
        state.doc.steps = state.doc.steps.filter(s => s.id !== st.id);
        for (const s of state.doc.steps) for (const [k, src] of Object.entries(s.bind || {})) {
          if (src && src.startsWith('steps.' + st.id + '.')) delete s.bind[k];
        }
        state.sel = null;
        if (state.link && state.link.step === st.id) state.link = null;
        if (state.linkDrag && state.linkDrag.step === st.id) state.linkDrag = null;
        renderAll();
      }
    }
  });
}

function startNodeDrag(e, st) {
  if (e.button !== 0) return;
  // Зоны захвата разведены: поля входа и выходы соединяют, всё остальное
  // (шапка и нижняя полоса узла) переносит. Раньше переносила только шапка, а
  // её полоса в три пикселя — за узел держаться неудобно, и выглядело так,
  // будто узлы не двигаются вовсе.
  if (e.target.closest('.outchip[data-out],.inrow[data-field]')) return;
  if (!e.target.closest('.nh,.foot')) return;
  e.preventDefault();
  const startX = e.clientX, startY = e.clientY;
  const ox = st.pos[0], oy = st.pos[1];
  const node = e.target.closest('.node');
  let moved = false;
  // Рёбра цепляются к узлу живьём. Раньше при протяжке менялись только
  // left/top самого узла, а renderEdges() вызывался лишь на отпускании:
  // линия всё время перетаскивания стояла на старом месте и «отрывалась» от
  // шага. Рисуем не чаще кадра — mousemove приходит чаще, чем браузер рисует.
  const hasEdges = edgesTouch(st.id);
  let frame = 0;
  const scheduleEdges = () => {
    if (frame) return;
    frame = typeof requestAnimationFrame === 'function'
      ? requestAnimationFrame(() => { frame = 0; renderEdges(); })
      : setTimeout(() => { frame = 0; renderEdges(); }, 16);
  };
  const mm = ev => {
    const dx = ev.clientX - startX, dy = ev.clientY - startY;
    if (Math.abs(dx) + Math.abs(dy) > 3) moved = true;
    node.style.left = (ox + dx) + 'px';
    node.style.top = (oy + dy) + 'px';
    if (hasEdges) scheduleEdges();
  };
  const mu = ev => {
    document.removeEventListener('mousemove', mm);
    document.removeEventListener('mouseup', mu);
    if (moved) {
      pushUndo();
      const dx = ev.clientX - startX, dy = ev.clientY - startY;
      st.pos = [Math.max(0, snap(ox + dx)), Math.max(0, snap(oy + dy))];
      renderAll();
    }
  };
  document.addEventListener('mousemove', mm);
  document.addEventListener('mouseup', mu);
}

// Помечает на холсте поля, которые ядро назвало ошибочными. Без этого связь
// выглядит готовой: в узле стоит «steps.s1.model» рядом с полем number, и
// понять, что это бессмыслица, можно только пройдя к валидатору.
function paintIssues() {
  const bad = {};
  for (const i of (state.valIssues || [])) {
    if (!i.step) continue;
    (bad[i.step] = bad[i.step] || new Set()).add(i.port || '');
  }
  document.querySelectorAll('.node').forEach(n => {
    const set = bad[n.dataset.id];
    n.classList.toggle('has-err', !!set);
    n.querySelectorAll('.inrow[data-field]').forEach(r => {
      r.classList.toggle('bad', !!(set && set.has(r.dataset.field)));
    });
  });
}

// Единственное место, где редактор говорит с человекой. Раньше его не было:
// отказ связываться и подробности ошибок валидации просто некуда было вывести,
// поэтому связь «на всякий случай» оставалась единственным исходом дропа.
let noteTimer = null;
function note(msg, bad) {
  const el = $('#note');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('bad', !!bad);
  el.hidden = !msg;
  clearTimeout(noteTimer);
  if (msg) {
    const ms = bad ? 6000 : 2600;
    noteTimer = setTimeout(() => { el.hidden = true; }, ms);
  }
}

// ── валидация / сериализация / сохранение ─────────────────────────────────
let valTimer = null;
function scheduleValidate() {
  state.validating = true;
  $('#val-badge').textContent = '…';
  $('#val-badge').className = 'badge run';
  clearTimeout(valTimer);
  valTimer = setTimeout(doValidate, 500);
}
async function doValidate() {
  try {
    const res = await api('/api/serialize/pipeline', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(state.doc),
    });
    state.yaml = res.yaml || '';
    state.valErrs = res.errors || [];
    state.valWarns = res.warnings || [];
    // issues приходят адресными (code/step/port) — по ним плохое поле
    // помечается прямо на холсте, а не ищется глазами в тексте
    state.valIssues = (res.issues || []).filter(i => i.severity === 'error');
    const b = $('#val-badge');
    if (state.valErrs.length) { b.textContent = 'ошибки: ' + state.valErrs.length; b.className = 'badge err'; }
    else { b.textContent = 'валиден' + (state.valWarns.length ? ' · ' + state.valWarns.length + ' warn' : ''); b.className = 'badge ok'; }
    const ve = $('#val-err');
    if (ve) {
      // Строки ошибок — кнопки: клик по «нет поля X» ведёт на слой входов к
      // самому X. Тексты берём из issues (там есть step и port), а не
      // складываем строки: к строке нельзя привязать действие.
      const miss = [];
      const netIssue = [];
      for (const i of (state.valIssues || [])) {
        const m = String(i.message || '').match(/нет поля ([\w-]+)/);
        miss.push(m ? m[1] : '');
        netIssue.push(i.code === 'E_NETWORK_DENIED' ? '1' : '');
      }
      ve.innerHTML = (state.valErrs || []).map((t, i) => miss[i]
        ? `<button data-issue="${esc(miss[i])}" data-istep="${esc((state.valIssues[i] || {}).step || '')}"`
          + ` data-iport="${esc((state.valIssues[i] || {}).port || '')}">${esc(t)}`
          + `<div class="go">→ ввести «${esc(miss[i])}»</div></button>`
        : netIssue[i]
          ? '<button data-net="allow">'+esc(t)+'<div class="go">→ разрешить сеть (network: allow)</div></button>'
          : `<button data-issue="">${esc(t)}</button>`).join('');
      ve.hidden = !state.valErrs.length;
      wireIssueClicks();
    }
    paintIssues();
    if (state.layer !== 'inputs') syncNote();
  } catch (e) {
    const b = $('#val-badge');
    b.textContent = 'ошибка'; b.className = 'badge err';
    state.valErrs = [String(e.message || e)];
  } finally {
    state.validating = false;
  }
}

function normalizePipelineFilename(raw) {
  let file = String(raw || '').trim();
  if (!file || file.includes('/') || file.includes('\\') || /[\x00-\x1f<>:"|?*]/.test(file) || file === '.' || file === '..') return '';
  file = file.replace(/\.ya?ml$/i, '');
  if (!file || file === '.' || file === '..') return '';
  return file + '.yaml';
}

function rebaseHistoryFilename(file) {
  for (const history of [state.undo, state.redo]) {
    for (let i = 0; i < history.length; i++) {
      const snapshot = JSON.parse(history[i]);
      if (Object.prototype.hasOwnProperty.call(snapshot, 'file')) {
        snapshot.file = file;
        history[i] = JSON.stringify(snapshot);
      }
    }
  }
}

// save() возвращает true только когда файл действительно записан на диск.
// Кнопке запуска это нужно, чтобы не стартовать ран по несохранённому YAML:
// иначе человек правит связи, жмёт «Запустить» и получает старый результат.
async function save() {
  const file = normalizePipelineFilename($('#file-name').value || state.doc.file);
  const status = $('#save-status');
  if (!file) {
    status.textContent = 'некорректное имя файла'; status.className = 'badge err';
    $('#file-name').focus();
    return false;
  }
  $('#file-name').value = file;
  try {
    await doValidate();
  } catch {}
  if (state.validating) return;
  if (state.unsupported.length) { alert('Нельзя сохранить: редактор не управляет полями: ' + state.unsupported.join(', ')); return false; }
  if (state.valErrs.length) { alert('Сначала исправь ошибки:\n' + state.valErrs.join('\n')); return false; }
  const st = $('#save-status');
  try {
    await apiRaw('/api/pipelines/' + encodeURIComponent(file), { method: 'PUT', body: state.yaml });
    state.doc.file = file;
    rebaseHistoryFilename(file);
    st.textContent = 'сохранено: ' + file; st.className = 'badge ok';
    await loadFileList();
    return true;
  } catch (e) {
    st.textContent = 'ошибка'; st.className = 'badge err';
    alert('Сохранение: ' + e.message);
    return false;
  }
}

// runFromEditor — сохранить и запустить, затем уйти в консоль на этот ран.
//
// Сначала save(): запускать несохранённый YAML — это получить результат старой
// версии файла и потом гадать, почему правки не подействовали. Гейт оставляем
// человеку (yes: false): редактор — место подготовки, решение всё равно за
// человеком; автоматический прогон живёт в консоли.
async function runFromEditor() {
  const status = $('#save-status');
  const btn = $('#btn-run');
  if (!(await save())) return;
  const file = state.doc.file;
  btn.disabled = true;
  status.textContent = 'запуск…'; status.className = 'badge run';
  try {
    const res = await api('/api/run', { method: 'POST', body: JSON.stringify({ file, yes: false }) });
    if (res && res.run) { location.href = '/?run=' + encodeURIComponent(res.run); return; }
    status.textContent = 'ран без id'; status.className = 'badge err';
  } catch (e) {
    status.textContent = 'запуск не удался'; status.className = 'badge err';
    alert('Запуск: ' + e.message);
  } finally {
    btn.disabled = false;
  }
}

// ── открытие / список файлов ──────────────────────────────────────────────
async function loadFileList() {
  try {
    const list = await api('/api/pipelines');
    const sel = $('#file-open');
    const cur = state.doc.file;
    sel.innerHTML = '<option value="">— открыть… —</option>' +
      list.filter(p => !p.error).map(p => `<option value="${esc(p.file)}"${p.file === cur ? ' selected' : ''}>${esc(p.name || p.file)}</option>`).join('');
  } catch (e) { console.error(e); }
}

async function openFile(file) {
  try {
    const yamlText = await apiRaw('/api/pipelines/' + encodeURIComponent(file));
    const doc = await api('/api/parse/pipeline', { method: 'POST', body: yamlText });
    pushUndo();
    state.doc = {
      name: doc.name, file, format_version: doc.format_version || '',
      input: (doc.input || []).map(i => ({
        name: i.name,
        default: i.has_default ? i.default : undefined,
        typed: !!i.typed,
        type: i.type || '',
        required: i.required,
        format: i.format || '',
        description: i.description || '',
        has_default: !!i.has_default,
      })),
      steps: (doc.steps || []).map((s, si) => ({
        id: s.id, plugin: s.plugin,
        // позиция из YAML, а при её отсутствии — по индексу, а не Math.random:
        // случайная раскладка менялась при каждом открытии одного файла
        pos: Array.isArray(s.pos) && s.pos.length === 2
          ? [s.pos[0], s.pos[1]]
          : [120 + (si % 3) * 300, 90 + Math.floor(si / 3) * 230],
        on_error: s.on_error || 'stop', timeout: s.timeout || '',
        bind: s.bind || {},
        form: (s.form || []).map(f => ({ field: f.field, editable: !!f.editable, type: f.type || '', format: f.format || '' })),
        actions: s.actions || [], on_reject: s.on_reject || '', approval: s.approval || '',
        when: s.when || null,
        foreach: s.foreach || '', foreach_item: s.foreach_item || '',
        after_foreach: !!s.after_foreach, parallel_group: s.parallel_group || '',
        loop: s.loop || '', loop_condition: s.loop_condition || '',
        max_iterations: Number(s.max_iterations) || 0,
        retry: s.retry || null,
      })),
      foreach: doc.foreach || '', foreach_item: doc.foreach_item || '',
      item_type: doc.item_type || '', item_format: doc.item_format || '',
      secrets: doc.secrets || [], network: doc.network || '', gates: doc.gates || '',
    };
    state.unsupported = doc.unsupported || [];
    state.sel = null;
    // Открытый файл приносит свою сеть как есть (round-trip, инвариант
    // editor.go), а напоминание о невыданной сети чужого сценария здесь ни к
    // чему: цепочку никто не собирал.
    state.netPending = '';
    $('#file-name').value = file;
    // Селект «открыть существующий» тоже должен показать открытый файл:
    // loadFileList() отмечает в нём текущий документ, а на обычном пути
    // (консоль → редактор) это всегда new_pipeline.yaml, и без этой строки
    // в шапке имя файла и имя в списке расходились.
    const fsel = $('#file-open');
    if (fsel && [...fsel.options].some(o => o.value === file)) fsel.value = file;
    // несколько шагов на одной позиции — из-за этого они приезжали стопкой
    if (spreadSteps()) note('шаги стояли друг на друге — развёл их по сетке');
    renderAll();
  } catch (e) {
    alert('Не удалось открыть: ' + e.message);
  }
}

// ── YAML-просмотр ─────────────────────────────────────────────────────────
async function showYaml() {
  try {
    const res = await api('/api/serialize/pipeline', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(state.doc),
    });
    state.yaml = res.yaml || '';
    $('#yaml-pre').textContent = state.yaml +
      (res.errors && res.errors.length ? '\n\nошибки валидации:\n' + res.errors.join('\n') : '');
    $('#yaml-view').style.display = 'block';
  } catch (e) {
    $('#yaml-pre').textContent = 'ошибка сериализации: ' + e.message;
    $('#yaml-view').style.display = 'block';
  }
}
window.closeYaml = () => { $('#yaml-view').style.display = 'none'; };

// ── init ──────────────────────────────────────────────────────────────────
async function init() {
  // H4: без сессии /api/* закрыт, поэтому сначала вход, потом редактор
  if (!await wedraSession()) return;
  try { state.plugins = await api('/api/plugins'); } catch (e) { console.error(e); }
  renderPalette();
  initCanvas();
  await loadFileList();
  $('#file-name').value = state.doc.file;
  $('#file-name').onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); save(); } };
  $('#file-open').onchange = e => { if (e.target.value) openFile(e.target.value); };
  $('#btn-save').onclick = save;
  $('#btn-run').onclick = runFromEditor;
  $('#btn-yaml').onclick = showYaml;
  $('#close-yaml').onclick = () => { $('#yaml-view').style.display = 'none'; };
  $('#btn-undo').onclick = undo;
  $('#btn-redo').onclick = redo;
  // v0.38: режим артефактов передаёт имя собранного пайплайна через
  // sessionStorage — параметра ?open= у редактора нет, а заводить ради
  // одного перехода второй способ открытия незачем.
  let openNext = '';
  try { openNext = sessionStorage.getItem('wedra.open') || ''; } catch (e) { /* приватный режим */ }
  if (openNext) {
    try { sessionStorage.removeItem('wedra.open'); } catch (e) { /* пусто */ }
    // loadFileList() не возвращает признака успеха, а openFile() сам
    // разбирается с ошибкой. Поэтому просто открываем.
    await openFile(openNext);
    return;
  }
  renderAll();
}
init();
