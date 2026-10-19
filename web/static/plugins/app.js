// Редактор плагинов — отдельная страница /plugins/.
//
// Зачем своя страница, а не вкладка в артефактах или в консоли: здесь
// правится ИСПОЛНЯЕМЫЙ код. Последствия правки (плагин перестаёт быть
// доверенным, тесты зовут Python и держат кнопки) не должны смешиваться с
// загрузкой файлов, поэтому у режима свой экран, как у артефактов и редактора.
//
// Три правила, из которых следует всё остальное:
//
//  1. ПРАВКА ПЕРЕСТАНЕТ БЫТЬ ДОВЕРЕННОЙ. Доверие ядро считает по хэшу
//     содержимого (plugin.DecideTrust), поэтому любая запись меняет хэш и
//     плагин выпадает из allow-list. Молчать об этом после «сохранено» —
//     значит человек узнаёт о блокировке из лога рана. Поэтому после PUT
//     страница говорит об этом явно и обновляет метку доверия.
//  2. СНАЧАЛА ПРОВЕРИТЬ, ПОТОМ ПИСАТЬ. «Сохранить и проверить» — это
//     validate, и только при нуле ошибок PUT. Ошибки показываются списком с
//     кодами, а не одной строкой.
//  3. НИЧЕГО НЕ ТЕРЯТЬ МОЛЧА. Несохранённая правка живёт в черновике
//     (st.drafts) и переживает переключение плагинов; о её наличии говорят
//     точки — на вкладке файла и в строке списка. Удаления здесь нет, и
//     confirm() на странице тоже нет: терять правку по чужому «да» нельзя,
//     а молча — тем более.
//
// Без внешних зависимостей (офлайн: всё встроено в бинаррь через go:embed).

const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"'`]/g,
  c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;', '`': '&#96;' }[c]));

// Четыре файла плагина. Порядок — как вкладки: манифест, код, тесты,
// зависимости. Имена файлов здесь и в контракте API совпадают.
const FILES = ['plugin.yaml', 'main.py', 'plugin.test.yaml', 'requirements.lock'];

// Имя плагина: строчная латиница, дальше цифры и подчёркивания. Та же форма,
// что требует ядро, — проверка ДО запроса, чтобы «создать» не превращалось в
// 400 с непонятным текстом.
const ID_RE = /^[a-z][a-z0-9_]*$/;

let st = {
  plugins: [],          // /api/plugins — список для рельсы и выбора
  pipelines: null,      // только ради числа в рельсе; null = не считали
  current: null,        // выбранный id
  drafts: {},           // id -> {files, dirty, dir, missing, trusted, sha, report, loaded}
  trust: null,          // ответ /api/trust: где живёт конфиг доверия оператора
  no_isolator: false,   // изолятора на этой платформе нет (Windows/macOS)
  tab: 'plugin.yaml',   // открытый файл
  q: '',                // поиск в списке
  busy: false,          // идёт запрос к серверу — кнопки заблокированы
  loading: false,       // читаются файлы плагина
};

// ── сессия человека ────────────────────────────────────────────────────────
// Всё под /api/* (кроме /api/health и /api/session) требует cookie. Cookie
// выдаётся обменом ОДНОРАЗОВОГО кода из терминала, где запущен wedra. Пока
// cookie нет, показываем поле ввода кода вместо пустого редактора — тот же
// путь, что в консоли (web/static/app.js) и в редакторе (editor/app.js).
function sessionOverlay() {
  if (document.getElementById('wedra-session-box')) return;
  const box = document.createElement('div');
  box.id = 'wedra-session-box';
  // стили — в index.html (#wedra-session-box): разметка не тащит цвета в JS
  box.innerHTML =
    '<div class="sbox">' +
    '<h2>Вход в Nomos</h2>' +
    '<p>Код входа одноразовый и напечатан в терминале, где запущен wedra. Он нужен один раз: ' +
    'обменяется на cookie, и страница перезагрузится сама.</p>' +
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
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ code }),
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
  let s = null;
  try { s = await (await fetch('/api/session')).json(); } catch (e) { return true; }
  if (!s || !s.required || s.authenticated) return true;
  sessionOverlay();
  return false;
}

// ── API ────────────────────────────────────────────────────────────────────
// Ошибка несёт код ответа и тело: PUT на 400 отдаёт {ok:false, errors, warnings}
// и это НЕ исключение «что-то сломалось», а список причин, который надо
// показать. Поэтому тело сохраняем, а не превращаем в строку.
function apiFail(msg, status, body) {
  const e = new Error(msg);
  e.status = status;
  e.body = body;
  return e;
}

function messageOf(body, status) {
  if (body && Array.isArray(body.errors) && body.errors.length) {
    return body.errors.map(i => (i && i.code ? i.code + ': ' : '') + String((i && (i.message || i.detail)) || '')).join('\n');
  }
  if (body && body.error) return String(body.error);
  if (typeof body === 'string' && body.trim()) return body.trim();
  return 'HTTP ' + status;
}

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const ct = r.headers.get('content-type') || '';
  let body = null;
  if (ct.includes('json')) body = await r.json().catch(() => null);
  else body = await r.text();
  if (r.status === 401) {
    sessionOverlay();
    throw apiFail('нет сессии: нужен одноразовый код из терминала wedra', 401, body);
  }
  if (!r.ok) throw apiFail(messageOf(body, r.status), r.status, body);
  return body;
}

function json(path, method, payload) {
  return api(path, {
    method: method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });
}

const ep = (id, tail) => '/api/plugins/' + encodeURIComponent(id) + tail;

// ── статус и блокировка кнопок ─────────────────────────────────────────────
function status(text, kind) {
  const s = $('#status');
  s.textContent = text || '';
  s.className = kind || '';
}

// Пока идёт запрос, ни одна команда не доступна: два «Сохранить» подряд писали
// бы два раза, а тест во время записи гонял бы полузаписанный плагин.
function setBusy(on) {
  st.busy = !!on;
  syncButtons();
}

function syncButtons() {
  const has = !!draft();
  $('#btn-validate').disabled = !has || st.busy;
  $('#btn-save').disabled = !has || st.busy;
  $('#btn-test').disabled = !has || st.busy;
  $('#np-create').disabled = st.busy;
}

// ── черновики ──────────────────────────────────────────────────────────────
// Черновик на плагин: прочитанные файлы, какие из них правлены, что сервер
// сказал о доверии и какие файлы отсутствуют. Всё, что показывает страница,
// берётся отсюда, поэтому переключение плагинов ничего не теряет.
function draft() {
  return st.current ? st.drafts[st.current] || null : null;
}

function newDraft(id) {
  return {
    id: id,
    files: {},        // имя файла → текст
    dirty: {},        // имя файла → true, если текст меняли
    dir: '',
    missing: [],      // каких файлов нет на диске (ответ GET /source)
    trusted: false,
    sha: '',
    // Причина отказа в запуске: на Windows/macOS она говорит, что изолятора
    // нет, и это меняет подсказку — «запустится в песочнице» там было бы враньём.
    blocked_reason: '',
    report: null,     // последний отчёт: валидация, запись или тесты
    loaded: false,
  };
}

function draftOf(id) {
  if (!st.drafts[id]) st.drafts[id] = newDraft(id);
  return st.drafts[id];
}

const isDirty = d => !!d && Object.keys(d.dirty).length > 0;

// Последний открытый плагин помнится между сессиями — тем же способом, что
// последний пайплайн в консоли (см. web/static/app.js: LAST_PIPELINE_KEY):
// localStorage с версией ключа, иначе смена формата сделала бы старое
// значение нечитаемым молча. Значение проверяется по списку плагинов —
// сохранённый id мог исчезнуть с диска.
const LAST_PLUGIN_KEY = 'wedra.lastPlugin.v1';

function rememberPlugin(id) {
  if (!id) return;
  try { localStorage.setItem(LAST_PLUGIN_KEY, id); } catch (e) { /* приватный режим */ }
}

function lastPlugin() {
  try { return localStorage.getItem(LAST_PLUGIN_KEY) || ''; } catch (e) { return ''; }
}

// Содержимое для запроса — ВСЕ четыре файла. Валидатор должен видеть плагин
// целиком, а не только изменённый кусок: правило про обязательное поле в
// манифесте иначе не сработает.
function filesPayload(d) {
  const out = {};
  FILES.forEach(f => { out[f] = d.files[f] || ''; });
  return out;
}

// ── список плагинов ────────────────────────────────────────────────────────
function renderList() {
  const box = $('#plist');
  const q = st.q.trim().toLowerCase();
  const all = st.plugins.filter(p =>
    !q || String(p.id || '').toLowerCase().indexOf(q) >= 0
      || String(p.description || '').toLowerCase().indexOf(q) >= 0);

  // Счётчики в рельсе — тот же приём, что в артефактах: элемент может не
  // найтись, и тогда страница падать не должна из-за числа.
  const set = (sel, n) => { const el = $(sel); if (el) el.textContent = n == null ? '' : String(n); };
  set('#rail-plug-count', st.plugins.length || '');
  set('#rail-pip-count', st.pipelines == null ? '' : st.pipelines.length);

  if (!st.plugins.length) {
    box.innerHTML = '<div class="empty">Список плагинов пуст — в каталоге plugins/ нет '
      + 'ни одного каталога с plugin.yaml.</div>';
    return;
  }
  if (!all.length) {
    box.innerHTML = '<div class="empty">Ничего не нашлось. Все плагины — в поле поиска выше.</div>';
    return;
  }

  box.innerHTML = all.map(p => {
    const d = st.drafts[p.id];
    const ext = p.trusted === true
      ? '<span class="tag ok">доверен</span>'
      : '<span class="tag ext" title="не доверен: вне allow-list, запуск только в изоляторе">внешний</span>';
    return '<button class="plug' + (p.id === st.current ? ' on' : '') + '" data-id="' + esc(p.id) + '"'
      + ' title="' + esc(p.description || '') + '">'
      + '<span class="nm">' + esc(p.id) + '</span>'
      + (isDirty(d) ? '<span class="dirty" title="есть несохранённая правка"></span>' : '')
      + ext
      + '<span class="why">v' + esc(p.version || '?') + '</span>'
      + '</button>';
  }).join('');

  box.querySelectorAll('.plug').forEach(b => {
    b.addEventListener('click', () => select(b.dataset.id));
  });
}

// ── шапка правки: путь, доверие, хэш, отсутствующие файлы ──────────────────
function renderMeta() {
  const d = draft();
  const path = $('#m-path'), trust = $('#m-trust'), sha = $('#m-sha'), miss = $('#m-miss');
  if (!d || !d.loaded) {
    // Плагин выбран, но файлы не прочитаны (или их нет). Разные случаи —
    // разные слова: «не выбран» здесь было бы враньём.
    path.textContent = st.loading
      ? 'читаю файлы плагина…'
      : (d ? d.id + ' — файлы не прочитаны' : 'плагин не выбран');
    trust.textContent = '—';
    trust.className = 'tag';
    sha.textContent = '';
    miss.hidden = true;
    return;
  }
  path.textContent = d.dir || d.id;
  trust.textContent = d.trusted ? 'доверен' : 'внешний код';
  trust.className = 'tag ' + (d.trusted ? 'ok' : 'ext');
  trust.title = d.trusted
    ? 'хэш содержимого совпадает с allow-list'
    : 'не доверен: хэш содержимого не совпадает с allow-list, запуск только в изоляторе';
  sha.textContent = d.sha ? 'sha256 ' + String(d.sha).replace(/^sha256:/, '').slice(0, 12) : '';
  if (d.missing && d.missing.length) {
    miss.hidden = false;
    miss.textContent = 'нет файла: ' + d.missing.join(', ');
    miss.title = 'Сервер отдал эти файлы как отсутствующие. Пока файла нет, правка сюда его создаст.';
  } else {
    miss.hidden = true;
  }
  renderTrustHint(d);
}

// renderTrustHint — что делать с плагином, которому не доверено.
//
// Формулировка зависит от изолятора, и это не украшение: на Linux такой плагин
// просто запустится в bwrap, и доверять его не нужно. На Windows и macOS
// изолятора в проекте нет, поэтому там единственный способ запустить СВОЙ код —
// внести хэш в конфиг оператора. Без этой подсказки человек видит «внешний код»
// и упирается в стену, не понимая, что стена проходит по правильной причине.
function renderTrustHint(d) {
  const box = $('#trust-hint');
  if (!box) return;
  if (!d || !d.loaded || d.trusted || !d.sha) {
    box.hidden = true;
    box.innerHTML = '';
    return;
  }
  box.hidden = false;
  const entry = d.id + '@' + d.sha;
  const iso = d.blocked_reason || '';
  const noIso = /windows|macos|нет\b/i.test(iso) || st.no_isolator;
  const where = st.trust && st.trust.config_path
    ? 'Файл: <code>' + esc(st.trust.config_path) + '</code>'
    : 'Файл доверия не выбран: задайте <code>--trust-config=&lt;путь&gt;</code> или <code>WEDRA_TRUST_CONFIG</code>. Рабочий каталог не читается намеренно.';
  box.innerHTML =
    '<h4>Плагину не доверено</h4>' +
    (noIso
      ? '<p>На этой платформе изолятора нет: недоверенный код не запустится вообще. ' +
        'Если это <b>ваш</b> код — внесите его хэш в конфиг доверия оператора, и он получит ' +
        'права пользователя. Чужой код так запускать нельзя.</p>'
      : '<p>Запуск возможен в изоляции (Linux, bwrap) — доверять его не нужно. ' +
        'Чтобы доверен был и он, внесите хэш в конфиг оператора.</p>') +
    '<pre class="clip">' + esc('  - "' + entry + '"') + '</pre>' +
    '<div class="hint">' + where + '</div>';
}

// ── вкладки и поля ─────────────────────────────────────────────────────────
function renderTabs() {
  const d = draft();
  $('#ptabs').querySelectorAll('button').forEach(b => {
    const f = b.dataset.f;
    b.classList.toggle('active', f === st.tab);
    // Точка — несохранённая правка в этом файле.
    b.querySelector('.dt').hidden = !(d && d.dirty[f]);
  });
}

function renderEditors() {
  const d = draft();
  FILES.forEach(f => {
    const ta = $('#f-' + f);
    const val = d && d.loaded ? (d.files[f] || '') : '';
    // Значение ставим только когда оно разошлось: перерисовка не должна
    // съедать курсор и выделение посреди правки.
    if (ta.value !== val) ta.value = val;
    ta.hidden = f !== st.tab;
    ta.disabled = !d || !d.loaded;
  });
}

function renderReport() {
  const box = $('#report');
  const d = draft();
  const rep = d && d.report;
  if (!rep) { box.hidden = true; box.innerHTML = ''; return; }

  const errs = Array.isArray(rep.errors) ? rep.errors : [];
  const warns = Array.isArray(rep.warnings) ? rep.warnings : [];
  const cases = Array.isArray(rep.cases) ? rep.cases : [];
  let html = '';

  if (rep.title) html += '<div class="rep-sec' + (rep.kind === 'err' ? ' err' : rep.kind === 'warn' ? ' warn' : '') + '">'
    + esc(rep.title) + '</div>';

  if (errs.length) {
    html += '<div class="rep-sec err">ошибки (' + errs.length + ')</div>' + errs.map(i => issueHTML(i, 'err')).join('');
  }
  if (warns.length) {
    html += '<div class="rep-sec warn">предупреждения (' + warns.length + ')</div>' + warns.map(i => issueHTML(i, 'warn')).join('');
  }
  if (cases.length) {
    html += '<div class="rep-sec' + (rep.failed ? ' err' : '') + '">тесты: прошло '
      + esc(String(rep.passed)) + ', упало ' + esc(String(rep.failed)) + '</div>';
    html += cases.map(c => {
      const ok = c && c.ok === true;
      return '<div class="tcase' + (ok ? '' : ' bad') + '">'
        + '<span class="mark">' + (ok ? '✓' : '✗') + '</span>'
        + '<span class="nm">' + esc((c && c.name) || 'без имени') + '</span>'
        + '<span class="dtl">' + esc((c && c.detail) || '') + '</span>'
        + '</div>';
    }).join('');
  }
  // Итоговая строка: «ошибок нет» при пустом отчёте и потеря доверия после
  // записи. Показывается всегда, когда она есть, — иначе важное осталось бы
  // только в строке статуса, которую следующий клик затёр бы.
  if (rep.note) {
    html += '<div class="rep-sec' + (rep.kind === 'warn' ? ' warn' : rep.kind === 'err' ? ' err' : '') + '">'
      + esc(rep.note) + '</div>';
  }
  if (!html) { box.hidden = true; box.innerHTML = ''; return; }
  box.innerHTML = html;
  box.hidden = false;
}

// Строка отчёта: код + текст. Полосу слева задаёт РАЗДЕЛ, в котором строка
// лежит (ошибки или предупреждения): в контракте у того и другого {code,
// message} без признака серьёзности, и красить всё по умолчанию красным
// значило бы выдавать предупреждение за ошибку. Место (line/file/field)
// показываем, только если сервер его прислал, — придумывать «строку 12» из
// воздуха нельзя.
function issueHTML(i, kind) {
  const it = i && typeof i === 'object' ? i : { message: String(i) };
  const cls = kind || (it.severity === 'warning' || it.severity === 'warn' ? 'warn' : 'err');
  const where = it.line != null ? 'строка ' + esc(String(it.line))
    : (it.file || it.field) ? esc(String(it.file || it.field)) : '';
  return '<div class="issue ' + cls + '">'
    + '<span class="code">' + esc(it.code || '—') + '</span>'
    + '<span class="msg">' + esc(it.message || it.detail || '') + '</span>'
    + (where ? '<span class="where">' + where + '</span>' : '')
    + '</div>';
}

function render() {
  renderList();
  renderMeta();
  renderTabs();
  renderEditors();
  renderReport();
  syncButtons();
}

// ── загрузка ───────────────────────────────────────────────────────────────
async function loadPlugins() {
  const p = await api('/api/plugins');
  // Проверка Array.isArray не формальность: если сервер отдаст объект (ошибка,
  // прокси, не тот Content-Type), сообщение падало бы не о том, что случилось.
  st.plugins = Array.isArray(p) ? p : [];
  if (!Array.isArray(p)) status('сервер вернул не список плагинов', 'err');
}

async function select(id) {
  if (st.busy) return;
  st.current = id;
  rememberPlugin(id);
  const d = draftOf(id);
  render();
  if (d.loaded) return;   // черновик уже в памяти — второй раз за диск не ходим

  setBusy(true);
  st.loading = true;
  renderMeta();
  try {
    const s = await api(ep(id, '/source'));
    // Ответ — источник правды. Поля, которых сервер не прислал, остаются
    // пустыми, а не выдумываются: неизвестный хэш честнее отсутствующего.
    d.dir = s.dir || '';
    d.trusted = s.trusted === true;
    d.sha = s.content_sha256 || '';
    d.blocked_reason = s.blocked_reason || '';
    d.missing = Array.isArray(s.missing) ? s.missing : [];
    FILES.forEach(f => { d.files[f] = typeof s.files[f] === 'string' ? s.files[f] : ''; });
    d.dirty = {};
    d.loaded = true;
    d.report = null;
    status(`загружены файлы ${id}` + (d.missing.length ? ` (нет: ${d.missing.join(', ')})` : ''), 'ok');
  } catch (e) {
    if (e.status === 404) {
      d.loaded = false;
      d.report = null;
      status(`у плагина ${id} нет файлов на диске (404). Создай его заново или выбери другой.`, 'err');
    } else {
      status(`не читаются файлы ${id}: ${e.message}`, 'err');
    }
  } finally {
    st.loading = false;
    setBusy(false);
    render();
  }
}

// Обновить запись списка после записи: доверие и хэш у плагина в /api/plugins
// уже другие, и список обязан показывать то же, что шапка правки.
function syncListTrust(id, trusted, sha) {
  const p = st.plugins.find(x => x.id === id);
  if (!p) return;
  p.trusted = trusted === true;
  if (sha) p.content_sha256 = sha;
}

// ── команды ────────────────────────────────────────────────────────────────
// Проверка без записи: POST /api/plugins/<id>/validate с текущим текстом.
// Возвращает true, когда ошибок нет, — этим же пользуется save().
async function validate() {
  const d = draft();
  if (!d || !d.loaded) { status('сначала выбери плагин', 'err'); return false; }
  setBusy(true);
  try {
    const v = await json(ep(d.id, '/validate'), 'POST', { files: filesPayload(d) });
    const errs = Array.isArray(v.errors) ? v.errors : [];
    const warns = Array.isArray(v.warnings) ? v.warnings : [];
    d.report = {
      // Заголовок жёлтый, только когда есть что показать жёлтым: «ошибок нет»
      // при пустых предупреждениях — обычный результат, а не тревога.
      kind: errs.length ? 'err' : (warns.length ? 'warn' : 'ok'),
      title: 'проверка ' + d.id + (errs.length ? ' — ошибок ' + errs.length : ' — ошибок нет'),
      errors: errs,
      warnings: warns,
      note: warns.length ? '' : 'ошибок и предупреждений нет',
    };
    // ok в ответе — это не «всё хорошо», а «ошибок нет»; предупреждения при
    // этом остаются, и показывать их нужно.
    status(errs.length
      ? `ошибок ${errs.length}, предупреждений ${warns.length} — ничего не записано`
      : `ошибок нет, предупреждений ${warns.length}`, errs.length ? 'err' : 'ok');
    return errs.length === 0;
  } catch (e) {
    const errs = e.body && Array.isArray(e.body.errors) ? e.body.errors : [];
    const warns = e.body && Array.isArray(e.body.warnings) ? e.body.warnings : [];
    d.report = {
      kind: 'err',
      title: 'проверка ' + d.id + ' не прошла (HTTP ' + (e.status || '?') + ')',
      errors: errs.length ? errs : [{ code: 'E_HTTP_' + (e.status || '?'), message: e.message }],
      warnings: warns,
    };
    status('проверка не прошла: ' + e.message, 'err');
    return false;
  } finally {
    setBusy(false);
    render();
  }
}

// Запись: сначала validate, и только при нуле ошибок PUT. Именно в этом порядке
// — иначе «Сохранить и проверить» проверял бы уже записанное.
async function save() {
  const d = draft();
  if (!d || !d.loaded) { status('сначала выбери плагин', 'err'); return; }

  const changed = Object.keys(d.dirty).filter(f => d.dirty[f]);
  if (!changed.length) {
    status('правок нет — сохранять нечего', 'warn');
    return;
  }

  setBusy(true);
  status('проверяю перед записью…', 'busy');
  let ok = false;
  let v = null;
  try {
    v = await json(ep(d.id, '/validate'), 'POST', { files: filesPayload(d) });
    ok = !(Array.isArray(v.errors) && v.errors.length);
  } catch (e) {
    setBusy(false);
    const errs = e.body && Array.isArray(e.body.errors) ? e.body.errors : [];
    d.report = {
      kind: 'err',
      title: 'проверка ' + d.id + ' не прошла (HTTP ' + (e.status || '?') + ') — запись отменена',
      errors: errs.length ? errs : [{ code: 'E_HTTP_' + (e.status || '?'), message: e.message }],
      warnings: e.body && Array.isArray(e.body.warnings) ? e.body.warnings : [],
    };
    status('проверка не прошла, ничего не записано: ' + e.message, 'err');
    render();
    return;
  }

  if (!ok) {
    setBusy(false);
    d.report = {
      kind: 'err',
      title: 'проверка ' + d.id + ' — ошибок ' + v.errors.length + ', запись отменена',
      errors: v.errors,
      warnings: Array.isArray(v.warnings) ? v.warnings : [],
    };
    status('ошибки в проверке, ничего не записано: ' + v.errors.length, 'err');
    render();
    return;
  }

  // Пишем только изменённые файлы. Отсутствующие на диске (missing) в пустой
  // виде не отправляем: создавать пустой requirements.lock никому не нужно.
  const payload = {};
  changed.forEach(f => {
    if ((d.missing || []).indexOf(f) >= 0 && !(d.files[f] || '').trim()) return;
    payload[f] = d.files[f] || '';
  });

  if (!Object.keys(payload).length) {
    setBusy(false);
    d.dirty = {};
    status('правки совпадали с отсутствующими файлами — записывать нечего', 'warn');
    render();
    return;
  }

  status('записываю…', 'busy');
  try {
    const res = await json(ep(d.id, '/source'), 'PUT', { files: payload });
    const written = Array.isArray(res.written) ? res.written : Object.keys(payload);
    d.dirty = {};
    d.trusted = res.trusted === true;
    d.sha = res.content_sha256 || d.sha;
    d.blocked_reason = res.blocked_reason || '';
    // Файл, которого не было, теперь есть — снимаем отметку «нет файла».
    d.missing = (d.missing || []).filter(f => written.indexOf(f) < 0);
    syncListTrust(d.id, d.trusted, d.sha);

    const warns = Array.isArray(res.warnings) ? res.warnings : [];
    d.report = {
      // Запись без потери доверия — обычный успех; жёлтым она становится
      // ровно тогда, когда доверие потеряно (об этом же строкой ниже).
      kind: (!d.trusted || warns.length) ? 'warn' : 'ok',
      title: 'записано: ' + (written.join(', ') || 'ничего'),
      errors: [],
      warnings: warns,
      note: d.trusted ? '' : 'плагин больше не доверенный — содержимое изменилось, '
        + 'а доверять можно только по хэшу из allow-list',
    };

    // ГЛАВНОЕ, что обязан сказать этот экран: после правки плагин не доверен.
    // Доверие ядро считает по хэшу содержимого, allow-list хэш не меняет —
    // то есть «сохранено» без этой оговорки было бы враньём.
    const trustNote = d.trusted
      ? ''
      : ' Плагин перестал быть доверенным: изменился хэш содержимого, а доверять можно '
        + 'только по хэшу из allow-list. Запуск теперь только в изоляторе и только с '
        + '--allow-untrusted-plugins (причина и условие — в консоли).';
    status('сохранено: ' + (written.join(', ') || 'ничего') + trustNote, d.trusted ? 'ok' : 'warn');
  } catch (e) {
    const errs = e.body && Array.isArray(e.body.errors) ? e.body.errors : [];
    const warns = e.body && Array.isArray(e.body.warnings) ? e.body.warnings : [];
    d.report = {
      kind: 'err',
      title: 'запись не прошла (HTTP ' + (e.status || '?') + ')',
      errors: errs.length ? errs : [{ code: 'E_HTTP_' + (e.status || '?'), message: e.message }],
      warnings: warns,
    };
    status('не записано: ' + e.message, 'err');
  } finally {
    setBusy(false);
    render();
  }
}

// Тесты: POST /api/plugins/<id>/test. Зовёт Python — МЕДЛЕННЫЙ вызов, поэтому
// кнопки заблокированы, а статус прямо говорит, что происходит. Предупреждение
// про несохранённую правку обязательно: контракт теста принимает пустое тело и
// гоняет то, что лежит на диске.
async function runTests() {
  const d = draft();
  if (!d || !d.loaded) { status('сначала выбери плагин', 'err'); return; }
  const warnDirty = isDirty(d);
  setBusy(true);
  status('идёт… запускается Python, это обычно несколько секунд', 'busy');
  try {
    const t = await json(ep(d.id, '/test'), 'POST', {});
    const cases = Array.isArray(t.cases) ? t.cases : [];
    const passed = typeof t.passed === 'number' ? t.passed : cases.filter(c => c && c.ok).length;
    const failed = typeof t.failed === 'number' ? t.failed : cases.length - passed;
    d.report = {
      kind: failed ? 'err' : 'ok',
      title: 'тесты ' + d.id + ': прошло ' + passed + ', упало ' + failed,
      errors: [],
      warnings: warnDirty ? [{
        code: 'W_UNSAVED_NOT_TESTED',
        message: 'в редакторе есть несохранённая правка — тесты гоняют файлы, '
          + 'которые лежат на диске, то есть ДО правки. Сохрани и прогони снова.',
      }] : [],
      cases: cases,
      passed: passed,
      failed: failed,
    };
    status(failed
      ? `тесты: прошло ${passed}, упало ${failed}`
      : `тесты: прошло ${passed}, упало 0`, failed ? 'err' : 'ok');
  } catch (e) {
    const cases = e.body && Array.isArray(e.body.cases) ? e.body.cases : [];
    d.report = {
      kind: 'err',
      title: 'тесты ' + d.id + ' не прошли (HTTP ' + (e.status || '?') + ')',
      errors: [{ code: 'E_HTTP_' + (e.status || '?'), message: e.message }],
      warnings: [],
      cases: cases,
      passed: 0,
      failed: cases.length,
    };
    status('тесты не прошли: ' + e.message, 'err');
  } finally {
    setBusy(false);
    render();
  }
}

// ── создание плагина ───────────────────────────────────────────────────────
async function createPlugin(e) {
  if (e) e.preventDefault();
  const errBox = $('#np-err');
  errBox.textContent = '';
  const id = $('#np-id').value.trim();

  // Проверка ДО запроса. Форма отправляет id в путь плагина и в allow-list
  // позже, поэтому кривое имя — это не «сервер разберётся», а мусор на диске.
  if (!id) { errBox.textContent = 'нужно id плагина'; $('#np-id').focus(); return; }
  if (!ID_RE.test(id)) {
    errBox.textContent = 'id должен быть строчной латиницей: первый символ — буква, '
      + 'дальше буквы, цифры и подчёркивание. Например: my_plugin.';
    $('#np-id').focus();
    return;
  }
  if (st.plugins.some(p => p.id === id)) {
    errBox.textContent = 'плагин ' + id + ' уже есть — открой его в списке';
    return;
  }

  setBusy(true);
  try {
    const res = await json('/api/plugins', 'POST', {
      id: id,
      template: $('#np-template').value || 'skeleton',
      author: $('#np-author').value.trim(),
      description: $('#np-desc').value.trim(),
    });
    // 200 без ok:false — плагин создан, но сказать «готово» можно лишь зная id.
    if (res && res.ok === false) {
      errBox.textContent = 'не создан: ' + messageOf(res, 400);
      return;
    }
    const dir = (res && res.dir) || '';
    $('#np-id').value = '';
    $('#np-author').value = '';
    $('#np-desc').value = '';
    // Блокировку снимаем ДО выбора плагина: select() отказывается работать под
    // ней, и новый плагин просто не открылся бы — тихий провал после «создано».
    setBusy(false);
    await loadPlugins();
    await select((res && res.id) || id);
    status('создан ' + id + (dir ? ' → ' + dir : ''), 'ok');
  } catch (e) {
    const errs = e.body && Array.isArray(e.body.errors) ? e.body.errors : [];
    errBox.textContent = errs.length
      ? errs.map(i => (i.code ? i.code + ': ' : '') + (i.message || '')).join('\n')
      : 'не создан: ' + e.message;
  } finally {
    setBusy(false);
    render();
  }
}

// ── события ────────────────────────────────────────────────────────────────
$('#q').addEventListener('input', e => { st.q = e.target.value; renderList(); });
$('#btn-validate').addEventListener('click', () => validate());
$('#btn-save').addEventListener('click', save);
$('#btn-test').addEventListener('click', runTests);
$('#np-create').addEventListener('click', createPlugin);

// Enter в поле id создаёт плагин — форма из одной кнопки, и человек жмёт
// Enter по привычке, а не кликает мышью.
['#np-id', '#np-author', '#np-desc'].forEach(sel => {
  $(sel).addEventListener('keydown', e => { if (e.key === 'Enter') createPlugin(e); });
});

// Вкладки файлов. Каждое поле живёт в DOM постоянно (иначе переключение
// съедало бы курсор), скрываются только неактивные.
$('#ptabs').addEventListener('click', e => {
  const b = e.target.closest('button[data-f]');
  if (!b) return;
  st.tab = b.dataset.f;
  renderTabs();
  renderEditors();
  const ta = $('#f-' + st.tab);
  if (ta) ta.focus();
});

// Правка помечает файл и вкладку. Черновик сохраняется в памяти страницы,
// поэтому переключение плагинов не теряет его, — но и на диск он не попал.
FILES.forEach(f => {
  $('#f-' + f).addEventListener('input', e => {
    const d = draft();
    if (!d) return;
    d.files[f] = e.target.value;
    d.dirty[f] = true;
    d.report = null;   // отчёт относился к прежнему тексту — он больше не про эту правку
    renderList();
    renderTabs();
    renderReport();
  });
});

// ── старт ──────────────────────────────────────────────────────────────────
(async function init() {
  if (!await wedraSession()) return;
  // Сведения о доверии нужны подсказке «как запустить свой плагин». Это НЕ
  // обязательный шаг: консоль обязана открыться и без него, поэтому ошибка
  // молча превращается в нейтральный текст, а не в пустую страницу.
  try {
    st.trust = await api('/api/trust');
  } catch (e) {
    st.trust = {error: e.message};
  }
  try {
    await loadPlugins();
  } catch (e) {
    status('не загрузился список плагинов: ' + e.message, 'err');
  }
  // Пайплайны — только ради числа в рельсе. Отдельным try: неудача здесь не
  // должна превращаться в сообщение об ошибке поверх рабочего экрана.
  try {
    const ps = await api('/api/pipelines');
    st.pipelines = Array.isArray(ps) ? ps.filter(p => !p.error) : [];
  } catch (e) { st.pipelines = null; }

  // Открытый плагин переживает перезагрузку страницы: без этого F5 терял бы
  // выбранный объект. Сохранённый id проверяется по списку — плагина могло не
  // стать на диске.
  const want = lastPlugin();
  if (want && st.plugins.some(p => p.id === want)) {
    await select(want);
  } else if (st.plugins.length) {
    await select(st.plugins[0].id);
  } else {
    render();
  }
})();
