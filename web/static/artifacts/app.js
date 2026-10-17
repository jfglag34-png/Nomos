// v0.38 — режим артефактов: отдельная страница, а не строчка в редакторе.
//
// Своя работа: загрузить файл, выбрать первый плагин, дальше цепочка
// достраивается по типам выходов. На выходе — готовый пайплайн, который
// открывается в редакторе. В редакторе про артефакты не знает ничего:
// смешивать два разных инструмента в один экран оказалось неудачно.
//
// Почему первый плагин выбирается руками. В манифестах нет полей mime и
// расширений — объявлять, что плагин ест картинку, нечем. Угадывать по
// именам полей и комментариям означало бы предлагать цепочки, которые не
// работают. Поэтому здесь есть ТОЧНЫЙ признак — input с format: file_ref
// (его объявили 5 плагинов) и честный список остальных с поиском. Автоподбор
// по типу файла — это секция accepts в манифесте, отдельная работа.

const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"'`]/g,
  c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;', '`': '&#96;' }[c]));

let st = {
  plugins: [],
  assets: [],
  pipelines: null,   // список нужен только ради счётчика в рельсе; null = не считали
  file: null,   // выбранный загруженный артефакт
  chain: [],    // [{plugin, dir, binds:[{field, from}], missing:[field]}]
  built: null,  // имя сохранённого пайплайна
  q: '',
};

// ── API ────────────────────────────────────────────────────────────────────
async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) {
    if (r.status === 401) return null; // нет сессии — покажем экран входа
    throw new Error(typeof body === 'string' ? body : JSON.stringify(body));
  }
  return body;
}

function status(text, kind) {
  const s = $('#status');
  s.textContent = text || '';
  s.className = kind || '';
}

// ── вход ───────────────────────────────────────────────────────────────────
async function sessionGate() {
  const ok = await (async () => {
    try {
      const r = await fetch('/api/session');
      const j = await r.json();
      return j && j.authenticated !== false;
    } catch (e) { return false; }
  })();
  if (!ok) {
    status('Нужен вход в GUI: открой http://127.0.0.1:8765/ и введи одноразовый код из терминала.', 'err');
  }
  return ok;
}

// ── имя входа пайплайна ────────────────────────────────────────────────────
function assetKey(name) {
  // Сервер уже транслитерирует имя в ASCII (assets.go), здесь только
  // регистр и мусор. Если вдруг придёт нечитаемое имя — добавляем хвост,
  // чтобы разные файлы не получили один вход.
  let s = String(name || '').replace(/\.[^.]*$/, '').toLowerCase();
  s = s.replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '');
  if (!s) return 'artifact' + shortHash(String(name || ''));
  if (/^[0-9]/.test(s)) s = 'a' + s;
  return s;
}
function shortHash(str) {
  let h = 2166136261;
  for (let i = 0; i < str.length; i++) { h ^= str.charCodeAt(i); h = Math.imul(h, 16777619); }
  return (h >>> 0).toString(16).padStart(8, '0').slice(0, 6);
}

// ── загрузка ───────────────────────────────────────────────────────────────
async function upload(file) {
  if (!file) return;
  status(`загружаю ${file.name}…`, 'busy');
  try {
    const fd = new FormData();
    fd.append('file', file, file.name);
    const a = await api('/api/assets', { method: 'POST', body: fd });
    if (!a) return;
    st.assets = [a].concat(st.assets.filter(x => x.name !== a.name));
    st.file = a;
    st.chain = [];
    st.built = null;
    status(`загружено: ${a.name} → input.${assetKey(a.name)}`, 'ok');
  } catch (e) {
    status('не загрузилось: ' + e.message, 'err');
  }
  render();
}

// ── плагины ────────────────────────────────────────────────────────────────
function infoOf(p) {
  return { id: p.id, dir: p.dir, desc: p.description || '', input: p.input || {}, output: p.output || {} };
}
function portType(d) { return d && typeof d === 'object' && typeof d.type === 'string' ? d.type : ''; }
function portFormat(d) { return d && typeof d === 'object' && typeof d.format === 'string' ? d.format : ''; }


// Объявил ли плагин вход файлом. Ровно то, что он написал в манифесте,
// без догадок: format: file_ref.
function fileInputs(p) {
  return Object.keys(p.input || {}).filter(f => portFormat(p.input[f]) === 'file_ref');
}
function fileInputsLoose(p) {
  // Для первого шага: поле с признаком файла — file_ref. Всё остальное, что
  // выглядит как путь (тип string с format text), НЕ считаем файловым входом:
  // exiftool и exifread объявили format: text с комментарием «путь к файлу»,
  // но это комментарий, а не контракт, и доверять ему — значит угадывать.
  return fileInputs(p);
}

// Кто принимает выходы последнего шага.
function suggest(last) {
  if (!last) return [];
  const outs = Object.keys(last.output || {}).map(f => ({ field: f, type: portType(last.output[f]) }));
  if (!outs.length) return [];
  const out = [];
  for (const p of st.plugins) {
    if (p.id === last.id || p.dir === last.dir) continue;
    if (p.id === 'core/human_gate') continue;
    for (const f of Object.keys(p.input || {})) {
      if (!isDataPort(f)) continue; // wall_timeout — лимит рана, не данные
      const want = portType(p.input[f]);
      for (const o of outs) {
        let score = 0;
        if (want && o.type && want === o.type) score = 2;
        else if (!want) score = 1;
        if (!score) continue;
        out.push({ p, field: f, score, outField: o.field,
          inType: want || '?', outType: o.type || '?' });
      }
    }
  }
  out.sort((a, b) => b.score - a.score
    || a.p.id.localeCompare(b.p.id) || a.field.localeCompare(b.field));
  const seen = new Set();
  // БЕЗ усечения: при 99 плагинах подходящих по типу десятки, и обрезка до
  // двенадцати прятала нужный плагин за алфавитом (report_formatter
  // выпадал за «alldns…»). Отсечение — дело отображения, и оно обязано
  // показывать, сколько всего найдено, иначе список врёт.
  return out.filter(x => {
    const k = x.p.id + '|' + x.field;
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });
}

// ── рабочие порты: не данные ───────────────────────────────────────────────
//
// wall_timeout/timeout — это лимит времени РАНА, а не данные пайплайна. Но
// объявлены они обычными входами (у 64 плагинов из 99), поэтому подбор по
// типам предлагал связать с ними любой числовой выход: список подсказок
// наполовину состоял из wall_timeout, а в цепочку попадал timeout вместо
// данных. Проверено по манифесту: «wall_timeout (опц., общий лимит рана,
// 300)» — abuseipdb, десятки других.
//
// Это не угадывание, а знание о формате: канал, который тратит время на весь
// ран, не может брать значение из выхода соседнего шага.
const OPS_PORTS = new Set(['wall_timeout', 'timeout']);

function isDataPort(field) {
  return !OPS_PORTS.has(field);
}

// ── планирование связей шага ───────────────────────────────────────────────
//
// Ядро требует, чтобы были связаны ВСЕ обязательные порты шага: не связал —
// валидация падает («порт results: в input пайплайна нет поля results»).
// Поэтому шаг связывается не одним полем, а всеми, чем можем:
//
//   - во вход, который объявил format: file_ref, идёт файл (первый шаг);
//   - остальные получают выход предыдущего шага, если типы совпали;
//   - необязательные (optional: true), которым не нашлось, пропускаем;
//   - обязательные, которым не нашлось, попадают в missing: пайплайн ещё не
//     запустится, и человек должен это видеть, а не узнать на ранe.
//
// Именно это и объясняло, почему цепочка из трёх шагов не проходила
// валидацию: report_formatter просит два входа, а связывали один.
// outputsOf — выходы плагина в виде [{field, type}].
function outputsOf(p) {
  const out = [];
  for (const f of Object.keys((p && p.output) || {})) out.push({ field: f, type: portType(p.output[f]) });
  return out;
}

// prevOutputs — выходы последнего шага цепочки. Передаются ЯВНО:.planStep не
// должен читать st.chain, иначе он зависит от того, что цепочка уже
// достроена «по очереди», и перестаёт работать при любом другом вызове.
function prevOutputs() {
  const last = st.chain[st.chain.length - 1];
  if (!last) return [];
  const p = st.plugins.find(x => x.id === last.plugin || x.dir === last.dir);
  return p ? outputsOf(p) : [];
}

function planStep(p, isFirst) {
  const prev = isFirst ? [] : prevOutputs();
  const binds = [];
  const missing = [];
  const ambiguous = [];
  const used = new Set();

  // Файл получают ЛЮБЫЕ шаги с файловым портом, а не только первый.
  // pipeline.input — общий вход пайплайна, и читать один файл могут
  // несколько шагов подряд: exiftool вытащит метаданные, потом бинарный разбор того же
  // файла. Раньше файл доставался только шагу №1, и второй шаг с файловым
  // портом получал «не хватает: file» — хотя файл на диске лежит.
  const fileField = fileInputsLoose(p)[0] || null;
  if (fileField) binds.push({ field: fileField, from: null });

  for (const f of Object.keys(p.input || {})) {
    if (f === fileField || !isDataPort(f)) continue; // лимит рана не берётся из выхода соседа
    const d = p.input[f] || {};
    const want = portType(d);
    // одноимённый выход — детерминированно и осмысленно
    const byName = prev.find(o => !used.has(o.field) && o.field.toLowerCase() === f.toLowerCase());
    if (byName) { used.add(byName.field); binds.push({ field: f, from: byName.field }); continue; }
    const compat = prev.filter(o => !used.has(o.field) && (!want || !o.type || want === o.type));
    if (compat.length === 1) { used.add(compat[0].field); binds.push({ field: f, from: compat[0].field }); continue; }
    // Несколько равных кандидатов — не угадываем: молчаливый выбор даёт
    // валидный, но бессмысленный пайплайн (csv_loader отдаёт и rows, и
    // headers, оба array).
    if (compat.length > 1) { ambiguous.push({ field: f, choices: compat.map(o => o.field) }); continue; }
    if (!d.optional) missing.push(f);
  }
  return { binds, missing, ambiguous };
}

// ── сборка пайплайна ───────────────────────────────────────────────────────
// yamlStr — значение в двойных кавычках. Экранирование обязательно: путь
// приходит с диска, и кавычка или обратный слэш в имени каталога сделали бы
// YAML нечитаемым (сервер отвечал «parse: YAML» без указания места).
// Имя САМОГО файла сервер уже очищает до [A-Za-z0-9._-], но каталог
// выбирает человек.
function yamlStr(v) {
  const s = String(v == null ? '' : v);
  return '"' + s.split('\\').join('\\\\').split('"').join('\\"') + '"';
}

function buildYAML() {
  if (!st.file || !st.chain.length) return '';
  const key = assetKey(st.file.name);
  const lines = [];
  lines.push('# собрано в режиме артефактов');
  lines.push('format_version: "0.2"');
  lines.push('');
  lines.push('pipeline:');
  lines.push('  name: artifact_chain');
  lines.push('  input:');
  lines.push(`    ${key}: ${yamlStr(st.file.path)}`);
  lines.push('');
  lines.push('  steps:');
  st.chain.forEach((c, i) => {
    lines.push(`    - id: s${i + 1}`);
    lines.push(`      plugin: ${c.dir || c.plugin}`);
    // pos нужен, чтобы шаги в редакторе встали в ряд, а не друг на друга.
    // Без него редактор расставляет их случайно (и они накладываются — я это
    // увидел на скриншоте: два узла в одной точке, «on_err: stop» нарисован
    // дважды). Схема принимает pos и ядро его читает.
    lines.push(`      pos: [${40 + i * 340}, 60]`);
    const binds = (c.binds || []).map(b =>
      `${b.field}: ${b.from === null ? 'input.' + key : 'steps.s' + i + '.' + b.from}`);
    if (binds.length) lines.push(`      bind: { ${binds.join(', ')} }`);
    lines.push('      on_error: stop');
  });
  lines.push('');
  return lines.join('\n');
}

async function build() {
  const yaml = buildYAML();
  if (!yaml) { status('Нужен файл и хотя бы один шаг.', 'err'); return; }
  status('проверяю и сохраняю…', 'busy');
  try {
    // /api/validate/pipeline ждёт СЫРОЙ yaml телом, а не {yaml: ...}:
    // handleValidatePipeline делает LoadPipelineFileFromBytes(r.Body).
    const v = await api('/api/validate/pipeline', {
      method: 'POST', headers: { 'Content-Type': 'application/yaml' }, body: yaml,
    });
    // Раньше здесь стояло `v.errors || v.issues || []`, и это была ловушка:
    // эндпоинт отдаёт errors, warnings И issues (все замечания), причём errors
    // равен null, когда ошибок нет. Фолбэк на issues подхватывал
    // ПРЕДУПРЕЖДЕНИЯ и объявлял их ошибками: пайплайн, который ядро приняло
    // (ok=true), помечался как негодный, а сообщение печатало
    // «[object Object]» — ошибки приходят объектами.
    const errs = Array.isArray(v && v.errors) ? v.errors : [];
    const warns = Array.isArray(v && v.warnings) ? v.warnings : [];
    const asText = list => list.map(x => (x && typeof x === 'object') ? (x.message || x.msg || JSON.stringify(x)) : String(x));
    if (errs.length) {
      $('#out').hidden = false;
      $('#out').textContent = yaml;
      status('ядро нашло ошибки (показал yaml): ' + asText(errs).join('; '), 'err');
      return;
    }
    const name = 'artifact_' + Date.now().toString(36) + '.yaml';
    await api('/api/pipelines/' + encodeURIComponent(name), {
      method: 'PUT', headers: { 'Content-Type': 'application/yaml' }, body: yaml,
    });
    st.built = name;
    $('#out').hidden = false;
    $('#out').textContent = yaml;
    // Предупреждения ядра полезны (например «файл не найден»), но это не
    // повод отказывать в сборке — показываем отдельной строкой.
    status('собрано и сохранено: ' + name
      + (warns.length ? ' · предупреждений: ' + asText(warns).length : ''), 'ok');
  } catch (e) {
    status('не собралось: ' + e.message, 'err');
  }
  render();
}

// ── отрисовка ──────────────────────────────────────────────────────────────
function renderAssets() {
  const box = $('#alist');
  if (!st.assets.length) { box.innerHTML = ''; return; }
  box.innerHTML = st.assets.map(a => {
    const on = st.file && st.file.name === a.name;
    return `<div class="a${on ? ' on' : ''}" data-a="${esc(a.name)}" role="button" tabindex="0">` +
      `<span class="nm">${esc(a.name)}</span>` +
      `<span class="sz">${Math.max(1, Math.round(a.size / 1024))} КиБ</span>` +
      `<span class="in">input.${esc(assetKey(a.name))}</span></div>`;
  }).join('');
  box.querySelectorAll('.a').forEach(el => {
    const pick = () => {
      st.file = st.assets.find(x => x.name === el.dataset.a) || null;
      st.chain = [];
      st.built = null;
      render();
    };
    el.addEventListener('click', pick);
    el.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(); } });
  });
}

function renderChain() {
  const box = $('#chain');
  if (!st.chain.length) {
    box.innerHTML = '<div class="empty">Пока пусто. Выбери файл и добавь первый шаг — или '
      + 'воспользуйся подсказками ниже.</div>';
    return;
  }
  const parts = st.chain.map((c, i) => {
    const binds = (c.binds || []).map(b => `${b.field} ← `
      + (b.from === null ? 'input.' + assetKey(st.file.name) : 'steps.s' + i + '.' + b.from)).join(', ');
    const miss = (c.missing || []).length
      ? `<span class="miss">не хватает: ${esc(c.missing.join(', '))}</span>` : '';
    const pl = st.plugins.find(x => x.id === c.plugin || x.dir === c.dir);
    const ext = pl && pl.trusted !== true
      ? `<span class="extw" title="плагин не в allow-list доверия; без изолятора он не запустится">внешний код</span>` : '';
    const amb = (c.ambiguous || []).map(a =>
      `<span class="amb">${esc(a.field)} ← выбери: ` +
      a.choices.map((ch, k) => `<button class="pick" data-step="${i}" data-pick="${esc(a.field)}"` +
        ` data-choice="${k}">${esc(ch)}</button>`).join('') + `</span>`).join('');
    return `<div class="cstep">` +
      `<span class="idx">${i + 1}</span>` +
      `<span class="nm">${esc(String(c.plugin).split('/').pop())}</span>` +
      `<span class="bind">${esc(binds || '—')}</span>` + ext + amb + miss +
      `<button class="rm" data-i="${i}" title="убрать шаг">×</button></div>`;
  });
  box.innerHTML = parts.join('<div class="carrow"></div>');
  box.querySelectorAll('.rm').forEach(b => {
    b.addEventListener('click', () => {
      st.chain.splice(+b.dataset.i, 1);
      st.built = null;
      render();
    });
  });
  // Неоднозначный вход: человек выбирает, какой выход туда пойдёт.
  box.querySelectorAll('[data-pick]').forEach(b => {
    b.addEventListener('click', () => {
      const c = st.chain[+b.dataset.step];
      const amb = (c.ambiguous || []).find(x => x.field === b.dataset.pick);
      if (!amb) return;
      c.binds = (c.binds || []).filter(x => x.field !== amb.field);
      c.binds.push({ field: amb.field, from: amb.choices[+b.dataset.choice] });
      c.ambiguous = (c.ambiguous || []).filter(x => x.field !== amb.field);
      st.built = null;
      render();
    });
  });
}

function renderPicker() {
  const box = $('#pluglist');
  // В цепочке лежит ЗАПИСЬ шага ({plugin, dir, binds, missing}), а suggest()
  // ждёт полный объект плагина с input/output. Сюда раньше передавалась
  // запись, и подсказки молча выходили пустыми — то есть в живом интерфейсе
  // их не показывало НИКОГДА, а вместо них всплывал список всех 99 плагинов.
  // Стенд этого не ловил: он кормил suggest() настоящим плагином.
  const entry = st.chain.length ? st.chain[st.chain.length - 1] : null;
  const lastP = entry ? st.plugins.find(x => x.id === entry.plugin || x.dir === entry.dir) : null;
  let sug = lastP ? suggest(infoOf(lastP)) : [];
  const q = String(st.q || '').trim().toLowerCase();

  // Поиск сужает и подсказки тоже: при 99 плагинах список длинный.
  if (q) {
    sug = sug.filter(x => x.p.id.toLowerCase().indexOf(q) >= 0
      || String(x.p.description || '').toLowerCase().indexOf(q) >= 0);
  }
  // Список всех плагинов показываем ВСЕГДА, а не только когда подсказок нет:
  // подсказки строятся по типам выходов, а нужен, скажем, второй
  // читающий файл (exiftool после csv_loader) — в подсказках его не будет
  // никогда, и человек упирался в стенку.
  // Подсказки — отдельный блок, идущий ПЕРЕД списком всех плагинов.
  const sugHtml = (() => {
    if (!sug.length) return '';
    const SHOW = 25;
    const more = sug.length - SHOW;
    return `<div class="empty">Подсказки по типам выходов <b>${esc(lastP.id)}</b>`
      + ` — клик добавит шаг и свяжет. Найдено ${sug.length}`
      + (more > 0 ? `, показаны первые ${SHOW}; остальные — в поиске сверху` : '') + `:</div>` +
      sug.slice(0, SHOW).map(x => `<button class="plug" data-p="${esc(x.p.id)}" data-d="${esc(x.p.dir)}"`
        + ` data-f="${esc(x.field)}" data-o="${esc(x.outField)}" title="${esc(x.p.description || '')}">`
        + `<span class="nm">${esc(x.p.id)}</span>`
        + `<span class="tag ${x.score === 2 ? 'exact' : 'any'}">${x.score === 2 ? 'тип' : 'любой'}</span>`
        + `<span class="why">${esc(x.outField)} → ${esc(x.field)} · ${esc(x.outType)} → ${esc(x.inType)}</span></button>`
      ).join('');
  })();

  {
    const all = st.plugins
      .filter(p => p.id !== 'core/human_gate')
      .filter(p => !q || p.id.toLowerCase().indexOf(q) >= 0 || (p.description || '').toLowerCase().indexOf(q) >= 0);
    const withFile = all.filter(p => fileInputsLoose(p).length > 0);
    const rest = all.filter(p => !fileInputsLoose(p).length);
    const card = (p, tag) => {
      const flds = fileInputsLoose(p);
      // Внешний код помечаем прямо в списке. /api/plugins отдаёт trusted, и
      // без метки человек узнаёт о блокировке только из лога рана: «плагин
      // exiftool — внешний код … песочница недоступна». На Windows изолятора
      // нет, так что недоверенный плагин не запустится вообще.
      const ext = p.trusted === true ? '' : '<span class="tag ext" title="не доверен: без изолятора не запустится">внешний</span>';
      return `<button class="plug" data-p="${esc(p.id)}" data-d="${esc(p.dir)}" title="${esc(p.description || '')}">` +
        `<span class="nm">${esc(p.id)}</span>` + ext +
        (tag ? `<span class="tag ${tag.k}">${esc(tag.t)}</span>` : '') +
        `<span class="why">in:${Object.keys(p.input || {}).length} out:${Object.keys(p.output || {}).length}` +
        (flds.length ? ' · файл:' + esc(flds.join(',')) : '') + `</span></button>`;
    };
    box.innerHTML = sugHtml + (withFile.length
? `<div class="empty">Объявили вход файлом (format: file_ref)${st.chain.length ? ' — такой шаг тоже возьмёт твой файл' : ' — можно взять первым'}:</div>`
        + withFile.map(p => card(p, { k: 'file', t: 'файл' })).join('')
      : '') +
      `<div class="empty">${q ? 'Ничего не нашлось. Все плагины:' : 'Все плагины:'}</div>` +
      rest.map(p => card(p, null)).join('');
  }

  box.querySelectorAll('.plug').forEach(b => {
    b.addEventListener('click', () => {
      if (!st.file) { status('Сначала загрузи файл — без него не на что опереться.', 'err'); return; }
      const p = st.plugins.find(x => x.id === b.dataset.p);
      if (!p) return;
      const plan = planStep(p, st.chain.length === 0);
      st.chain.push({ plugin: p.id, dir: p.dir, binds: plan.binds, missing: plan.missing, ambiguous: plan.ambiguous });
      st.built = null;
      if (st.chain.length === 1) {
        status(plan.missing.length
          ? `первый шаг ${p.id}: файла хватило не на все порты — не хватает ${plan.missing.join(', ')}`
          : `первый шаг: ${p.id} ← input.${assetKey(st.file.name)}`,
          plan.missing.length ? 'err' : 'ok');
      } else if (plan.missing.length) {
        status(`${p.id}: не хватает ${plan.missing.join(', ')} — пайплайн пока не запустится`, 'err');
      } else {
        status(`${p.id}: все порты связаны`, 'ok');
      }
      render();
    });
  });
}

function render() {
  renderRailCounts();
  renderAssets();
  renderChain();
  renderPicker();
  $('#to-editor').disabled = !st.built;
}

// Счётчики в рельсе. Рельса — навигация всего приложения, и пустые числа у
// разделов читались бы как «пусто», а не как «не считали». Пайплайны —
// единственный список, которого эта страница не грузит; он нужен только ради
// числа, и ошибка здесь не должна мешать работе режима.
function renderRailCounts() {
  const set = (sel, n) => { const el = $(sel); if (el) el.textContent = n == null ? '' : String(n); };
  set('#rail-plug-count', st.plugins.length);
  set('#rail-pip-count', st.pipelines === null ? '' : st.pipelines.length);
}

// ── события ────────────────────────────────────────────────────────────────
$('#drop').addEventListener('click', () => $('#file').click());
$('#file').addEventListener('change', e => {
  const f = e.target.files && e.target.files[0];
  e.target.value = '';
  upload(f);
});
const drop = $('#drop');
drop.addEventListener('dragover', e => {
  if (!e.dataTransfer) return;
  e.preventDefault();
  e.dataTransfer.dropEffect = 'copy';
  drop.classList.add('over');
});
drop.addEventListener('dragleave', () => drop.classList.remove('over'));
drop.addEventListener('drop', e => {
  if (!e.dataTransfer) return;
  e.preventDefault();
  drop.classList.remove('over');
  const files = Array.from(e.dataTransfer.files || []);
  if (files.length === 1) upload(files[0]);
  else if (files.length > 1) status(`беру только первый из ${files.length} файлов`, 'err'), upload(files[0]);
});
$('#q').addEventListener('input', e => { st.q = e.target.value; renderPicker(); });
$('#build').addEventListener('click', build);
$('#to-editor').addEventListener('click', () => {
  if (!st.built) return;
  // Редактор открывает файл из своего выпадающего списка; параметра ?open= у
  // него нет. Поэтому передаём имя через хранилище сессии и НЕ обещаем
  // «откроется сразу» — кнопка ведёт в редактор, где файл уже выбран.
  try { sessionStorage.setItem('wedra.open', st.built); } catch (e) { /* приватный режим */ }
  location.href = '/editor/';
});
$('#clear').addEventListener('click', () => {
  st.chain = []; st.file = null; st.built = null;
  $('#out').hidden = true;
  status('');
  render();
});

// ── старт ──────────────────────────────────────────────────────────────────
(async function init() {
  if (!await sessionGate()) return;
  try {
    // Проверка Array.isArray не формальность: если сервер отдаст объект
    // (ошибка, прокси, не тот Content-Type), следующая строка упала бы с
    // «st.plugins.filter is not a function» — сообщение ни о чём не говорит.
    const p = await api('/api/plugins');
    const a = await api('/api/assets');
    st.plugins = Array.isArray(p) ? p : [];
    st.assets = Array.isArray(a) ? a : [];
    if (!Array.isArray(p)) status('сервер вернул не список плагинов — подсказки будут пустыми', 'err');
    if (st.assets.length) st.file = st.assets[0];
  } catch (e) {
    status('не загрузились плагины/артефакты: ' + e.message, 'err');
  }
  // Пайплайны — только ради числа в рельсе. Отдельным try: неудача здесь не
  // должна превращаться в сообщение об ошибке поверх рабочего экрана.
  try {
    const ps = await api('/api/pipelines');
    st.pipelines = Array.isArray(ps) ? ps.filter(p => !p.error) : [];
  } catch (e) { st.pipelines = null; }
  render();
})();