// v0.22 — консоль WEDRA: раны, live-терминал, контекст, DAG.
// Без внешних зависимостей (офлайн: всё встроено).
const $ = s => document.querySelector(s);
const esc = s => String(s ?? '').replace(/[&<>"'`]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;','`':'&#96;'}[c]));

// errText — текст ошибки из журнала. В journal.jsonl error приходит ОБЪЕКТОМ
// {code, message, retryable}, и печать его напрямую давала «[object object]».
// Показываем сообщение, а при его отсутствии — код.
function errText(e) {
  if (e == null) return '';
  if (typeof e === 'string') return e;
  if (typeof e === 'object') return e.message || e.err_msg || e.code || JSON.stringify(e);
  return String(e);
}

let state = {
  tab: 'menu',
  runs: [],
  currentRun: null,
  journal: { events: [], since: 0, total: 0 },
  autoScroll: true,
  pipelines: [],
  currentPipe: null,
  presets: [],
  plugFilter: 'all', // фильтр каталога плагинов по реальному признаку trusted
  timers: {},
};

async function api(path, opts = {}) {
  const r = await fetch(path, opts);
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json() : await r.text();
  if (!r.ok) {
    // H4: 401 — нет сессии человека. Cookie ставится обменом одноразового кода
    // из терминала wedra; показываем поле ввода, а не текст в консоли.
    if (r.status === 401) { sessionOverlay(); throw new Error('нет сессии: нужен одноразовый код из терминала wedra'); }
    // 400 {issues} — валидация до запуска: показываем коды, а не сырой JSON
    if (body && Array.isArray(body.issues)) {
      const errs = body.issues.filter(i => i.severity === 'error');
      throw new Error(errs.map(i => i.code + ': ' + i.message).join('\n') || JSON.stringify(body));
    }
    throw new Error(typeof body === 'string' ? body : (body.error || JSON.stringify(body)));
  }
  return body;
}

// ── H4: сессия человека ──────────────────────────────────────────────────
// Всё под /api/* (кроме /api/health) требует cookie. Cookie выдаётся обменом
// ОДНОРАЗОВОГО кода, напечатанного в терминале, где запущен wedra. Пока cookie
// нет, страница показывает поле ввода кода вместо пустых списков: тот же путь,
// что и у `wedra gui --open` (там код едет в ссылке ?c= и ставит cookie сразу).
function sessionOverlay() {
  if (document.getElementById('wedra-session-box')) return;
  const box = document.createElement('div');
  box.id = 'wedra-session-box';
  // стили — в index.html (#wedra-session-box): разметка не тащит цвета в JS
  box.innerHTML =
    '<div class="sbox">' +
    '<h2>Вход в WEDRA</h2>' +
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

// ── каркас: разделы слева, режимы объекта, вход через последний пайплайн ────
// Референс — редактор n8n. Главного экрана там нет: открываешь — сразу
// объект. Поэтому:
//
//   * разделы приложения живут в тонкой рельсе слева (#tab-pipelines,
//     #tab-plugins, /artifacts/);
//   * прогоны и артефакты относятся к конкретному пайплайну, поэтому они
//     не разделы, а РЕЖИМЫ над объектом;
//   * точкой входа служит редактор последнего открытого пайплайна, а витрина
//     (#menu) остаётся вторичной и показывается, только когда выбирать нечего.
//
// Нижняя зона рельсы (Помощь, Что нового) — как Templates/Variables/Help в
// референсе: вторичное, не конкурирует с разделами.

// Последний открытый пайплайн: помним между сессиями. Ключ с версией схемы,
// иначе смена формата сделала бы старое значение нечитаемым молча.
const LAST_PIPELINE_KEY = 'wedra.lastPipeline.v1';

function rememberPipeline(file) {
  if (!file) return;
  try { localStorage.setItem(LAST_PIPELINE_KEY, file); } catch (e) { /* приватный режим */ }
}

function lastPipeline() {
  try { return localStorage.getItem(LAST_PIPELINE_KEY) || ''; } catch (e) { return ''; }
}

function parseScheduleFromYaml(yaml) {
  if (!yaml || yaml.indexOf('schedule:') === -1) return null;
  // минимальный парсер: выдираем блок schedule из YAML (от schedule: до steps:)
  const m = yaml.match(/schedule:\s*\n([\s\S]*?)\n\s*steps:/);
  if (!m) return null;
  const block = m[1];
  const get = (k) => { const r = block.match(new RegExp(k + ':\\s*\"?([^\"\\n]+)\"?')); return r ? r[1].trim() : ''; };
  const cron = get('cron');
  if (!cron) return null;
  return {
    cron: cron.replace(/^\"|\"$/g, ''),
    timezone: get('timezone') || 'Europe/Minsk',
    enabled: block.indexOf('enabled: false') === -1,
    overlap: get('overlap') || 'skip',
    catchUp: block.indexOf('catch_up: true') !== -1,
    humanGate: get('human_gate') || 'autoApprove',
    alerts: {
      on_waiting: get('on_waiting') || '',
      webhook: get('webhook') || ''
    }
  };
}

function buildYamlWithSchedule(yaml, sched) {
  if (!sched) {
    // убрать блок schedule целиком
    return yaml.replace(/\n\s*schedule:\s*\n(?:[ \t]+[^\n]*\n)*/g, '\n');
  }
  const block = `  schedule:\n    cron: "${sched.cron}"\n    timezone: ${sched.timezone}\n    enabled: ${sched.enabled}\n    overlap: ${sched.overlap}\n    catch_up: ${sched.catchUp}\n    human_gate: ${sched.humanGate}\n    alerts:\n      on_waiting: ${sched.alerts.on_waiting || 'telegram_notifier'}\n      webhook: "${sched.alerts.webhook || ''}"\n`;
  if (yaml.indexOf('schedule:') !== -1) {
    return yaml.replace(/\n\s*schedule:\s*\n(?:[ \t]+[^\n]*\n)*/g, '\n' + block);
  }
  // вставить перед steps:
  return yaml.replace(/\n(\s*)steps:/, '\n' + block + '$1steps:');
}

function wireScheduleCard(file, yaml, sched) {
  const status = document.getElementById('sched-status');
  const cronEl = document.getElementById('sched-cron');
  const tzEl = document.getElementById('sched-tz');
  const overlapEl = document.getElementById('sched-overlap');
  const catchupEl = document.getElementById('sched-catchup');
  const nextEl = document.getElementById('sched-next');
  const nextEl2 = document.getElementById('sched-next2');
  const alertsEl = document.getElementById('sched-alerts');
  if (!cronEl) return;
  const updateNext = () => {
    // грубая человекочитаемая подсказка как в cron.go Human()
    const c = cronEl.value.trim();
    const human = { '*/5 * * * *':'каждые 5м', '0 * * * *':'каждый час', '0 9 * * *':'каждый день 09:00', '0 9 * * 1':'пн 09:00' }[c] || c;
    if (nextEl) nextEl.textContent = c ? human : '';
    if (nextEl2) nextEl2.textContent = c ? human : '—';
  };
  updateNext();
  cronEl.addEventListener('input', updateNext);
  document.querySelectorAll('[data-cron]').forEach(b => b.addEventListener('click', () => { cronEl.value = b.dataset.cron; updateNext(); }));
  document.querySelectorAll('input[name=\"sched-gate\"]').forEach(r => r.addEventListener('change', () => {
    if (alertsEl) alertsEl.style.display = document.querySelector('input[name=\"sched-gate\"]:checked').value === 'wait' ? 'block' : 'none';
  }));
  const btnSolo = document.getElementById('sched-mode-solo');
  const btnCron = document.getElementById('sched-mode-cron');
  const panel = document.getElementById('sched-panel');
  const soloHint = document.getElementById('sched-solo-hint');
  if (btnSolo) btnSolo.addEventListener('click', () => {
    panel.style.display = 'none'; soloHint.style.display='block';
    btnSolo.style.background='var(--acc)'; btnSolo.style.color='var(--sunken)'; btnCron.style.background='transparent'; btnCron.style.color='var(--bone)';
    saveSchedule(file, yaml, null);
  });
  if (btnCron) btnCron.addEventListener('click', () => {
    panel.style.display = 'block'; soloHint.style.display='none';
    btnCron.style.background='var(--acc)'; btnCron.style.color='var(--sunken)'; btnSolo.style.background='transparent'; btnSolo.style.color='var(--bone)';
    const s = collectScheduleFromUI();
    if (!s.cron) s.cron='0 9 * * *';
    saveSchedule(file, yaml, s);
  });
  const saveBtn = document.getElementById('sched-save');
  if (saveBtn) saveBtn.addEventListener('click', () => {
    const s = collectScheduleFromUI();
    saveSchedule(file, yaml, s);
  });
}

function collectScheduleFromUI() {
  const cron = (document.getElementById('sched-cron')||{}).value || '0 9 * * *';
  const timezone = (document.getElementById('sched-tz')||{}).value || 'Europe/Minsk';
  const overlap = (document.getElementById('sched-overlap')||{}).value || 'skip';
  const catchUp = !!(document.getElementById('sched-catchup')||{}).checked;
  const hg = document.querySelector('input[name=\"sched-gate\"]:checked');
  const humanGate = hg ? hg.value : 'autoApprove';
  const on_waiting = (document.getElementById('sched-tg')||{}).value || 'telegram_notifier';
  const webhook = (document.getElementById('sched-hook')||{}).value || '';
  return { cron, timezone, enabled: true, overlap, catchUp, humanGate, alerts: { on_waiting, webhook } };
}

async function saveSchedule(file, yaml, sched) {
  const st = document.getElementById('sched-status');
  if (st) st.textContent = 'сохранение…';
  const newYaml = buildYamlWithSchedule(yaml, sched);
  try {
    const r = await fetch('/api/pipelines/' + encodeURIComponent(file), { method: 'PUT', headers: {'Content-Type': 'text/yaml'}, body: newYaml });
    const body = await r.text();
    let j; try { j = JSON.parse(body); } catch {}
    if (!r.ok) {
      const msg = j && j.issues ? j.issues.map(i=>i.code+': '+i.message).join('\\n') : body;
      throw new Error(msg);
    }
    if (st) { st.textContent = '✓ сохранено'; st.style.color='var(--ok)'; }
    setTimeout(() => { if(st) st.textContent=''; }, 1500);
    // переоткрыть карточку с новым yaml
    setTimeout(() => openPipeline(file), 600);
  } catch(e) {
    if (st) { st.textContent = e.message; st.style.color='var(--err)'; }
  }
}

// enterLastPipeline — увести в редактор последнего пайплайна.
//
// Переход идёт через sessionStorage 'wedra.open': это тот же способ, которым
// режим артефактов открывает собранный пайплайн в редакторе (см.
// editor/app.js), то есть второй способ открытия не появляется.
//
// Витрина показывается в двух случаях: пайплайнов нет вовсе, либо сохранённый
// файл исчез с диска. Во втором случае ссылка на него убирается, иначе вход
// вёл бы в пустоту.
function enterLastPipeline() {
  const file = lastPipeline();
  if (file && state.pipelines.some(p => p.file === file)) {
    try { sessionStorage.setItem('wedra.open', file); } catch (e) { /* приватный режим */ }
    location.href = '/editor/';
    return;
  }
  if (file) {
    rememberPipeline('');
    try { sessionStorage.removeItem(LAST_PIPELINE_KEY); } catch (e) { /* пусто */ }
  }
  setTab('menu');
  renderMenu();
}

// Помнить пайплайн, когда человек открыл его в списке: это и есть «последний
// открытый», из которого вход строит редактор.
// Помощь и «Что нового». Раньше здесь стоял alert: он притворялся, что
// содержимое есть, а его не было — хуже, чем сказать прямо. Теперь это
// настоящие панели.
//
// Список команд НЕ выдуман: он переписан из printHelp() в internal/cli/root.go
// (верхнеуровневые команды и подкоманды pipeline/plugin/runs). При изменении
// CLI этот список надо синхронизировать — копия, автопоиска здесь быть не может.
const CLI_COMMANDS = [
  ['Сценарии', [
    'wedra pipeline run <file.yaml> [--yes] [--resume=<run_id>]',
    'wedra pipeline install <name|file.yaml|url>',
    'wedra pipeline validate <file.yaml> [--json]',
    'wedra pipeline plan <file.yaml>',
    'wedra pipeline lint <file.yaml>',
  ]],
  ['Плагины', [
    'wedra plugin install <name>[@version]',
    'wedra plugin validate <dir>',
    'wedra plugin test <dir> [--conformance]',
    'wedra plugin create <dir>',
    'wedra plugin inspect <dir>',
    'wedra plugin search <query>',
    'wedra plugin list',
  ]],
  ['Прогоны и консоль', [
    'wedra runs list [var/runs]',
    'wedra runs show <run_id> [var/runs]',
    'wedra runs resume <run_id> <pipeline.yaml>',
    'wedra gui [--listen 127.0.0.1:8765] [--open]',
    'wedra approve <run_id> <step_id>',
  ]],
  ['Диагностика', [
    'wedra doctor [--json] [--plugins=<dir>]',
    'wedra check [--list|--fast|--census]',
    'wedra demo [--runs-dir=<dir>]',
    'wedra mcp --plugins=<dir>',
  ]],
];
const CLI_DOCS = [
  'protocol/v0.2/PROTOCOL.md', 'protocol/v0.2/ERRORS.md',
  'docs/quickstart.md', 'docs/architecture.md', 'docs/mcp.md',
];

function wireSheet(id, fill) {
  const el = $(id);
  if (!el) return;
  const close = () => { el.hidden = true; };
  fill();
  el.hidden = false; // открыть: без этой строки панель наполнялась и оставалась скрытой
  const x = $(id + '-close');
  if (x) x.onclick = close;
  el.addEventListener('click', e => { if (e.target === el) close(); });
  document.addEventListener('keydown', e => { if (e.key === 'Escape' && !el.hidden) close(); });
}

function wireRail() {
  const on = (id, fn) => { const el = $(id); if (el) el.onclick = fn; };
  on('#tab-pipelines', () => setTab('pipelines'));
  on('#tab-plugins', () => { setTab('plugins'); loadPluginsTab(); });
  on('#mode-runs', () => setTab('runs'));
  // Редактор и артефакты живут на своих страницах; из консоли в редактор —
  // с текущим пайплайном, иначе просто на холст.
  on('#mode-editor', () => {
    const file = state.currentPipe || lastPipeline();
    if (file) {
      try { sessionStorage.setItem('wedra.open', file); } catch (e) { /* приватный */ }
    }
    location.href = '/editor/';
  });
  on('#rail-new', () => {
    setTab('pipelines');
    try { sessionStorage.setItem('wedra.open', ''); } catch (e) { /* приватный */ }
    location.href = '/editor/';
  });
  // Помощь и «Что нового» — настоящие панели, см. CLI_COMMANDS выше.
  on('#rail-help', () => wireSheet('#helpbox', () => {
    const bd = $('#helpbox-bd');
    if (!bd) return;
    bd.innerHTML = CLI_COMMANDS.map(([grp, cmds]) =>
      `<div class="hb-g"><b>${esc(grp)}</b>`
      + cmds.map(c => `<code class="hb-cmd">${esc(c)}</code>`).join('')
      + '</div>').join('')
      + '<div class="hb-g"><b>Документация в репозитории</b>'
      + CLI_DOCS.map(p => `<span class="hb-path">${esc(p)}</span>`).join('')
      + '<p class="pf-hint">Полный список с флагами — <code>wedra --help</code> '
      + 'в терминале.</p></div>';
  }));
  on('#rail-whatsnew', () => wireSheet('#newsbox', () => {
    const bd = $('#newsbox-bd');
    if (!bd) return;
    // Версия — из /api/health, не выдуманная: та же, что в углу шапки.
    bd.innerHTML = `<div class="hb-ver">установлено: v${esc(state.version || '—')}</div>`
      + '<p class="pf-hint" style="margin-top:0">Что изменилось в этой и прошлых '
      + 'версиях — в файле <code>CHANGELOG.md</code> в корне репозитория.</p>'
      + '<div class="hb-g" style="margin-top:14px"><b>Документы проекта</b>'
      + CLI_DOCS.map(p => `<span class="hb-path">${esc(p)}</span>`).join('')
      + '</div>';
  }));
  wirePrefs();
}

// ── панель настроек ───────────────────────────────────────────────────────
// Живёт в prefs.js (общий модуль для трёх страниц). Здесь только обвязка:
// наполнение кнопок из списков пресетов и перерисовка «pressed».
function wirePrefs() {
  if (!window.WPrefs) return;
  const on = (id, fn) => { const el = $(id); if (el) el.onclick = fn; };
  const panel = $('#prefs');
  if (!panel) return;
  const P = window.WPrefs;

  const mkOpt = (val, label, swatch) =>
    `<button class="pf-opt" type="button" data-v="${esc(val)}" aria-pressed="false">`
    + (swatch ? `<span class="pf-swatch" style="background:${esc(swatch)}"></span>` : '')
    + `<span>${esc(label)}</span></button>`;

  const dens = $('#pf-density'), acc = $('#pf-accent'), rail = $('#pf-rail');
  if (dens) dens.innerHTML = Object.keys(P.DENSITY).map(k => mkOpt(k, P.DENSITY[k].label)).join('');
  if (acc) {
    acc.innerHTML = Object.keys(P.ACCENT).map(k => mkOpt(k, P.ACCENT[k].label, P.ACCENT[k].acc)).join('');
  }
  if (rail) rail.innerHTML = Object.keys(P.RAIL).map(k => mkOpt(k, P.RAIL[k].label)).join('');

  const groups = [[dens, 'density'], [acc, 'accent'], [rail, 'rail']];
  for (const [host, key] of groups) {
    if (!host) continue;
    host.addEventListener('click', e => {
      const b = e.target.closest('.pf-opt');
      if (!b) return;
      P.set({ [key]: b.dataset.v });
    });
  }

  const cr = $('#pf-roles'), cl = $('#pf-role-labels');
  if (cr) cr.addEventListener('change', () => P.set({ roles: cr.checked }));
  if (cl) cl.addEventListener('change', () => P.set({ roleLabels: cl.checked }));

  const reset = $('#pf-reset');
  if (reset) reset.addEventListener('click', () => P.reset());

  const open = () => { panel.hidden = false; refresh(); };
  const close = () => { panel.hidden = true; };
  const refresh = () => {
    const p = P.get();
    for (const [host, key] of groups) {
      if (!host) continue;
      host.querySelectorAll('.pf-opt').forEach(b => {
        b.setAttribute('aria-pressed', b.dataset.v === p[key] ? 'true' : 'false');
      });
    }
    if (cr) cr.checked = p.roles;
    if (cl) cl.checked = p.roleLabels;
  };

  on('#rail-prefs', open);
  on('#prefs-close', close);
  panel.addEventListener('click', e => { if (e.target === panel) close(); });
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && !panel.hidden) close();
  });
  P.onChange = refresh;
  // Панель настроек живёт здесь, а не дублируется на каждой странице: сами
  // настройки — общее состояние (prefs.js, localStorage), и рельса режима
  // артефактов ведёт на неё адресом /?prefs. Без этой строки пункт «Настройки»
  // на другой странице был бы ссылкой в никуда.
  if (new URLSearchParams(location.search).has('prefs')) open();
}

// setModeHighlight — какая вкладка РЕЖИМА подсвечена. Разделы (рельса) и
// режимы (шапка) — разные шкалы, и подсвечивать надо обе согласованно.
function setModeHighlight() {
  // Режимы относятся к объекту-пайплайну. На «Плагинах» объекта нет, и
  // подсвечивать там «Редактор» значило бы врать: ни один режим не открыт.
  const objTab = state.tab === 'runs' || state.tab === 'pipelines' || state.tab === 'menu';
  const mode = state.tab === 'runs' ? 'runs' : (objTab ? 'editor' : '');
  const me = $('#mode-editor'), mr = $('#mode-runs');
  if (me) me.classList.toggle('active', mode === 'editor');
  if (mr) mr.classList.toggle('active', mode === 'runs');
  const obj = $('#obj-name');
  if (obj) {
    obj.textContent = state.currentPipe
      ? state.currentPipe
      : (state.tab === 'runs' ? 'прогоны' : '');
  }
}

function setRailCounts(pipelines, plugins) {
  const pip = $('#rail-pip-count'), plug = $('#rail-plug-count');
  if (pip && pipelines) pip.textContent = pipelines.length || '';
  if (plug && plugins) plug.textContent = plugins.length || '';
}

// ── header ───────────────────────────────────────────────────────────────
async function init() {
  // H4: без сессии /api/* закрыт, поэтому сначала вход, потом интерфейс
  if (!await wedraSession()) return;
  try {
    const h = await api('/api/health');
    $('#ver').textContent = 'v' + h.version;
    state.version = h.version;
  } catch { $('#ver').textContent = 'оффлайн'; state.version = ''; }
  wireRail();
  setRailCounts([], []);
  $('#run-btn').onclick = startRun;
  // v0.35: меню считает пайплайны («из N запустить»), поэтому список должен
  // прийти до отрисовки, а не гонкой с ним.
  await loadPipelines();
  tickRuns();
  state.timers.runs = setInterval(tickRuns, 2500);
  // v0.35: по умолчанию — главное меню, а не список ранов: первый вопрос
  // новичка «с чего начать», и ответ на него должен быть на экране.
  const runParam = new URLSearchParams(location.search).get('run');
  // Разделы консоли — вкладки одной страницы, поэтому из редактора и режима
  // артефактов на них приходится приходить адресом: /#runs открывает «Раны».
  // Прямой заход адресом работает как раньше: /#runs открывает «Прогоны».
  // Разве что витрина переехала из точки входа в запасной экран.
  const hash = (location.hash || '').replace('#', '');
  if (runParam) { setTab('runs'); openRunDetail(runParam, true); }
  else if (['runs', 'pipelines', 'plugins', 'menu'].includes(hash)) {
    setTab(hash);
    if (hash === 'menu') renderMenu();
    else if (hash === 'plugins') loadPluginsTab();
  } else if (!state.pipelines.length) {
    // Пайплайнов нет — витрина единственное, что можно показать.
    setTab('menu'); renderMenu();
  } else {
    enterLastPipeline();
    return;  // уходим на /editor/, сценарий ниже не нужен
  }
}

// rail — кнопки разделов в левой рельсе. Раньше здесь жил ещё #tab-menu
// (витрина), но витрина стала запасным экраном и в рельсе ей не место, поэтому
// переключение идёт по наличию элемента, а не по предположению.
function markSection(el, on) {
  if (el) el.classList.toggle('active', on);
}

// Пустой правый экран списка пайплайнов («Выбери пайплайн») — самое заметное
// расхождение с референсом: там справа сразу объект. Решено НЕ отдельным
// экраном и не переходом в редактор, а превью: превью уже собирается из
// существующих API (/api/pipelines/<file> + /api/plan/pipeline — имя, формат,
// число шагов, состав плагинов, гейты, ошибки и предупреждения валидации, плюс
// DAG). Ничего нового выдумывать не пришлось, и выбор остаётся неразрушающим:
// клик по строке НЕ уводит из консоли. Поэтому при входе на вкладку
// подставляется превью — последнего открытого, иначе первого в списке.
function ensurePipelinePreview() {
  if (state.currentPipe && state.pipelines.some(p => p.file === state.currentPipe)) return;
  if (!state.pipelines.length) {
    // Списка нет — выбирать нечего, и «выбери пайплайн» было бы враньём.
    const d = $('#pdetail');
    if (d) {
      d.innerHTML = '<div class="empty">Пайплайнов пока нет. Создай первый'
        + ' в редакторе или положи файл в каталог сценариев.</div>';
    }
    return;
  }
  const want = lastPipeline();
  const file = state.pipelines.some(p => p.file === want) ? want : state.pipelines[0].file;
  openPipeline(file);
}

function setTab(t) {
  state.tab = t;
  markSection($('#tab-runs'), t === 'runs');
  markSection($('#tab-pipelines'), t === 'pipelines');
  markSection($('#tab-plugins'), t === 'plugins');
  setModeHighlight();
  $('#menu').style.display = t === 'menu' ? '' : 'none';
  $('#runs-aside').style.display = t === 'runs' ? '' : 'none';
  $('#pip-aside').style.display = t === 'pipelines' ? '' : 'none';
  $('#plug-aside').style.display = t === 'plugins' ? '' : 'none';
  $('#detail').style.display = t === 'runs' ? '' : 'none';
  $('#pdetail').style.display = t === 'pipelines' ? '' : 'none';
  // Правый экран списка не должен оставаться пустым: справа сразу превью.
  if (t === 'pipelines') ensurePipelinePreview();
  $('#plugdetail').style.display = t === 'plugins' ? '' : 'none';
  // Группы в шапке переключаются вместе с разделом: на «Плагинах» объекта
  // нет, и режимы объекта («Редактор») там были бы враньём.
  $('#modes-obj').hidden = t === 'plugins';
  $('#modes-plug').hidden = t !== 'plugins';
}

// ── главная: рабочий стол, а не витрина (v0.35) ───────────────────────────
// Собирается из уже существующих /api/* — своего состояния у меню нет.
//
// Структура и её причины (иначе вернётся «слайд презентации»):
//
//	заголовок малого кегля  — не landing page, а рабочий экран;
//	одно главное действие    — раньше вес был одинаковый у всех карточек;
//	недавние прогоны         — вместо витринных метрик; единственное, что
//	                          показывает состояние работы пользователя;
//	список задач по группам  — сначала то, что человек делает, потом
//	                          технические примеры; раньше они были смешаны;
//	строка, а не плитка      — плотность как в списке файлов.
//
// Мотив цепочки (узел + связь) живёт в заголовке секций, в главном действии и
// в пустом состоянии: продукт буквально про граф шагов, и без этого знака
// экран неотличим от любого шаблона.

// chainSVG — фирменный знак: узел и проходящая связь. Один раз здесь, дальше
// только переиспользование, иначе мотив расползётся по разной геометрии.
function chainSVG(accent) {
  return `<span class="chain${accent ? ' acc' : ''}" aria-hidden="true"><svg viewBox="0 0 22 11" width="22" height="11">
    <path class="cc" d="M0 5.5h7.5M14.5 5.5H22"/>
    <circle class="cn" cx="11" cy="5.5" r="3.5"/>
  </svg></span>`;
}

// технические примеры: демонстрации механики, а не задачи человека.
// Список задан здесь, а не выводится по эвристике «в описании есть foreach»:
// состав реестра меняется, и тогда демки молча переедут в задачи.
const TECHNICAL_PRESETS = new Set([
  'when_demo', 'foreach_step_demo', 'parallel_demo',
  'csv_foreach', 'csv_foreach_summary', 'llm_text_chain',
]);

async function renderMenu() {
  const [presets, runs, plugins] = await Promise.all([
    api('/api/presets').catch(() => ({available: false, reason: 'нет связи с ядром', presets: []})),
    api('/api/runs').catch(() => []),
    api('/api/plugins').catch(() => []),
  ]);
  state.presets = presets.presets || [];
  const recent = (runs || []).slice(0, 3);

  // Одно главное действие. Второстепенные — строками ниже, без крупных кнопок.
  const primary = `
    <button class="mprimary" id="m-new-run">
      ${chainSVG(true)}
      <span>
        <span class="mp-k">Начать новый запуск</span>
        <span class="mp-d">${state.pipelines.length
          ? `Выбрать пайплайн из ${state.pipelines.length} и запустить.`
          : 'Выбрать пайплайн и запустить.'} Гейт можно оставить человеку или принять автоматически.</span>
      </span>
      <span class="mp-go">к списку →</span>
    </button>`;

  // Недавние прогоны — единственный блок про состояние работы. Раньше здесь
  // стояла полоса метрик (99/99 доверенных, 12 пресетов), которая занимала
  // место и не помогала начать.
  const runsBlock = recent.length
    ? recent.map(r => `
      <div class="rrow" data-run-open="${esc(r.id)}">
        <span class="rt">${esc(r.status || '?')}</span>
        <span class="rp">${esc(r.pipeline || r.id)}</span>
        <span class="rm">${esc(r.id)}</span>
      </div>`).join('')
    : `<div class="mempty">${chainSVG(false)}<span>Ранов пока нет. Начни новый запуск — здесь появится таймлайн, контекст и журнал каждого шага.</span></div>`;

  // Список задач. Сначала то, что человек делает, потом — отдельно —
  // технические примеры: раньше они стояли вперемешку, и это была половина
  // претензии «шаблонно».
  const nameOf = p => (p.file || p.name || '').replace(/\.ya?ml$/i, '');
  // МАССИВ строк, а не склеенная строка: ниже он фильтруется по группам, и
  // обращение rows[i] к строке молча отдало бы символ вместо разметки.
  const rows = state.presets.map(p => `
    <div class="trow">
      ${chainSVG(false)}
      <span>
        <span class="tk" data-preset-open="${esc(p.file)}">${esc(nameOf(p))}</span>
        <span class="td">${esc(p.description || 'без описания')}</span>
      </span>
      <span class="tacts">
        <button class="mini" data-preset-open="${esc(p.file)}">открыть</button>
        <button class="mini go" data-preset-run="${esc(p.file)}"
          ${p.installed ? '' : 'disabled title="файла нет в examples/"'}>запустить</button>
      </span>
    </div>`);

  const isTech = p => TECHNICAL_PRESETS.has(nameOf(p));
  const human = state.presets.filter(p => !isTech(p));
  const tech = state.presets.filter(isTech);
  const block = list => (list.length
    ? list.map(p => rows[state.presets.indexOf(p)]).join('')
    : `<div class="mempty">${chainSVG(false)}<span>пусто</span></div>`);

  const presetsBody = presets.available
    ? (state.presets.length ? '' : '<div class="mempty">В реестре нет пресетов</div>')
    : `<div class="mempty">${chainSVG(false)}<span>Реестр недоступен (${esc(presets.reason || 'причина неизвестна')}) — пресеты показывать не из чего.</span></div>`;

  $('#menu').innerHTML = `
    <div class="wrap">
      <div class="mhead">
        ${chainSVG(false)}
        <h2>Запустить цепочку</h2>
        <span class="mh-sub">Строка ниже — готовая цепочка шагов с человеком в гейте.</span>
      </div>
      ${primary}

      <div class="mgroup">
        <h3 class="gh">Недавние прогоны <span class="gc">${recent.length ? recent.length : ''}</span></h3>
        ${runsBlock}
      </div>

      <div class="mgroup">
        <h3 class="gh">Рабочие задачи</h3>
        <p class="gn">Почта, телефоны, IBAN, дубликаты — то, что человек делает руками.</p>
        ${presets.available ? block(human) : ''}
      </div>

      <div class="mgroup">
        <h3 class="gh">Технические примеры</h3>
        <p class="gn">Показывают механику — условия, циклы, параллельность, секреты.
          Это демонстрации возможностей, а не рабочие задачи.</p>
        ${presetsBody ? '' : block(tech)}
      </div>

      <button class="sectbtn" id="goto-plugins">
        <span>Каталог плагинов</span><span class="cnt">${plugins.length}</span>
        <span class="tsub">${(plugins || []).filter(p => p.trusted).length} доверенных · поиск, манифест, установка зависимостей</span><span class="chev">→</span>
      </button>
      <div class="note">Плагины — исполняемый код. Перед установкой проверяй источник и объявленные
        в манифесте права: <span style="font-family:var(--mono)">network</span>,
        <span style="font-family:var(--mono)">filesystem</span>,
        <span style="font-family:var(--mono)">secrets</span>.</div>
    </div>`;

  const newRun = $('#m-new-run');
  if (newRun) newRun.onclick = () => setTab('pipelines');
  $('#menu').querySelectorAll('[data-run-open]').forEach(el => {
    el.onclick = () => { setTab('runs'); openRunDetail(el.dataset.runOpen, true); };
  });
  $('#menu').querySelectorAll('[data-preset-run]').forEach(b => {
    b.onclick = () => runPreset(b.dataset.presetRun);
  });
  $('#menu').querySelectorAll('[data-preset-open]').forEach(el => {
    el.onclick = () => { setTab('pipelines'); openPipeline(el.dataset.presetOpen); };
  });
  const gotoPl = $('#goto-plugins');
  if (gotoPl) gotoPl.onclick = () => { setTab('plugins'); loadPluginsTab(); };
}

// Запуск пресета из меню: тот же путь, что у кнопки «Запустить» на вкладке
// ранов, только без прыжка по вкладкам — human_gate всё равно нужен на экране.
async function runPreset(file) {
  setTab('runs');
  const sel = $('#run-select');
  if (sel && ![...sel.options].some(o => o.value === file)) {
    // файла нет рядом — честно говорим, вместо того чтобы молча ничего не
    // сделать: пресет из реестра ставится через `wedra pipeline install`.
    $('#run-status').textContent = `файла ${file} нет в каталоге пайплайнов — поставь: wedra pipeline install <имя>`;
    return;
  }
  if (sel) sel.value = file;
  $('#run-gate').checked = true;
  await startRun();
}

// ── раны ─────────────────────────────────────────────────────────────────
async function tickRuns() {
  let runs;
  try { runs = await api('/api/runs'); } catch { return; }
  state.runs = runs;
  // новый ран после запуска из браузера → открыть его деталку
  if (state.timers.pendingNew) {
    const fresh = runs.find(r => state.runIdsBeforeStart.indexOf(r.id) < 0);
    if (fresh) {
      state.timers.pendingNew = false;
      openRunDetail(fresh.id, true);
      const rs = $('#run-status');
      if (rs) rs.textContent = 'идёт — смотри таймлайн справа';
    }
  }
  renderRuns();
  const btn = $('#run-btn');
  const anyRunning = runs.some(r => r.status === 'running');
  if (!anyRunning && btn && btn.disabled) {
    btn.disabled = false;
    const rs = $('#run-status');
    if (rs && /идёт|старт/.test(rs.textContent)) rs.textContent = '';
  }
}

function renderRuns() {
  const el = $('#runs-list');
  if (!state.runs.length) { el.innerHTML = '<div class="empty">ранов пока нет</div>'; return; }
  el.innerHTML = state.runs.slice(0, 60).map(r => {
    const st = r.status === 'ok' ? 'ok' : r.status === 'running' ? 'run' : r.status === 'aborted' ? 'err' : r.status === 'interrupted' ? 'skip' : r.status === 'failed' ? 'err' : 'skip';
    const label = r.status === 'running' ? 'идёт…' : r.status;
    const t = (r.last || r.started || '').replace('T', ' ').replace('Z', '');
    return `<div class="run-item ${state.currentRun === r.id ? 'active' : ''}" data-run-id="${esc(r.id)}">
      <div class="top"><span class="pipeline">${esc(r.pipeline || '?')}</span><span class="badge ${st}">${esc(label)}</span></div>
      <div class="meta">${esc(r.id)} · ${esc(r.steps)} ш. · ${esc(t)}</div>
    </div>`;
  }).join('');
  el.querySelectorAll('[data-run-id]').forEach(item => item.addEventListener('click', () => openRunDetail(item.dataset.runId, true)));
  $('#runs-count').textContent = `(${state.runs.length})`;
}

// ── запуск из браузера ───────────────────────────────────────────────────
async function loadPipelines() {
  try {
    state.pipelines = await api('/api/pipelines');
    const sel = $('#run-select');
    sel.innerHTML = state.pipelines
      .filter(p => !p.error)
      .map(p => `<option value="${esc(p.file)}">${esc(p.name || p.file)} — ${esc(p.steps)} ш.${p.foreach ? ' · foreach' : ''}</option>`)
      .join('');
    renderPipList();
  } catch (e) { console.error(e); }
}

function renderPipList() {
  const el = $('#pip-list');
  el.innerHTML = state.pipelines.map(p =>
    `<div class="pitem ${state.currentPipe === p.file ? 'active' : ''}" data-pipeline-file="${esc(p.file)}">
      ${esc(p.name || p.file)} <small>${esc(p.steps)} ш.${p.foreach ? ' · foreach ' + esc(p.foreach) : ''}${p.error ? ' · error' : ''}</small>
    </div>`).join('');
  el.querySelectorAll('[data-pipeline-file]').forEach(item => item.addEventListener('click', () => openPipeline(item.dataset.pipelineFile)));
  $('#pip-count').textContent = `(${state.pipelines.length})`;
  setRailCounts(state.pipelines, null);
}

// ── вкладка «Плагины»: каталог витрины поверх установленного ──
//
// Два разных источника, и это не дублирование:
//   /api/plugins — что лежит на диске, с правами и решением о доверии;
//   /api/store   — что объявлено в реестре, с готовностью из doctor.
// Сливаем их по имени, и ни один не подменяет другой: плагин может стоять
// вручную и отсутствовать в реестре, а может быть в реестре и не стоять вовсе.
async function loadPluginsTab() {
  try {
    state.pluginsFull = await api('/api/plugins');
  } catch (e) {
    $('#plug-tab-list').innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
    return;
  }
  state.installedByID = {};
  for (const p of state.pluginsFull) state.installedByID[p.id] = p;

  // Витрина по умолчанию НЕ меряет готовность: doctor опрашивает каждый плагин
  // интерпретатором и ходит в индекс PyPI, это десятки секунд. Открытие
  // вкладки не должно их стоить, поэтому каталог приходит лёгким, а замер —
  // отдельной кнопкой (см. openPluginDetail).
  try {
    state.store = await api('/api/store');
  } catch (e) {
    state.store = {plugins: [], available: false, reason: e.message, status_measured: false};
  }
  renderPluginsTabList('');
  const s = $('#plug-tab-search');
  if (s) s.oninput = () => renderPluginsTabList(s.value);
  // Фильтр держится в состоянии, а не в замыкании: переживает повторный
  // вход на вкладку и перерисовку поиска.
  for (const [id, val] of [
    ['#plug-filter-all', 'all'], ['#plug-filter-ok', 'ok'],
    ['#plug-filter-no', 'no'], ['#plug-filter-store', 'store'],
  ]) {
    const b = $(id);
    if (b) b.onclick = () => { state.plugFilter = val; renderPluginsTabList(s ? s.value : ''); };
  }
  // Замер — осознанное действие, а не побочный эффект открытия вкладки: он
  // опрашивает каждый плагин интерпретатором и ходит в индекс PyPI.
  const hb = $('#plug-health');
  if (hb) {
    hb.onclick = async () => {
      hb.disabled = true;
      hb.textContent = 'проверяю…';
      try {
        state.store = await api('/api/store?status=1');
      } catch (e) {
        state.store = {plugins: [], available: false, reason: e.message, status_measured: true};
      } finally {
        hb.disabled = false;
        hb.textContent = 'Проверить готовность';
        renderPluginsTabList($('#plug-tab-search') ? $('#plug-tab-search').value : '');
      }
    };
  }
  $('#plug-tab-count').textContent = `(${storeRows().length})`;
  setRailCounts(state.pipelines, state.pluginsFull);
}

// storeRows — объединённый список для вкладки. Ключ слияния — имя плагина;
// при конфликте приоритет у /api/plugins, потому что он описывает то, что
// РЕАЛЬНО лежит на диске, а не то, что объявлено.
function storeRows() {
  const rows = [];
  const seen = new Set();
  const cat = (state.store && state.store.plugins) || [];
  for (const s of cat) {
    seen.add(s.name);
    const inst = state.installedByID ? state.installedByID[s.name] : null;
    rows.push(Object.assign({}, inst || {}, s, {name: s.name, in_registry: true}));
  }
  for (const p of state.pluginsFull) {
    if (seen.has(p.id)) continue;
    rows.push(Object.assign({}, p, {name: p.id, in_registry: false, installed: true}));
  }
  rows.sort((a, b) => String(a.name).localeCompare(String(b.name)));
  return rows;
}

function renderPluginsTabList(q) {
  const needle = (q || '').trim().toLowerCase();
  let hit = storeRows().filter(p =>
    (p.name || '').toLowerCase().includes(needle) ||
    (p.description || '').toLowerCase().includes(needle));
  // Фильтр по реальному признаку, а не по придуманной категории: в allow-list
  // плагин пройдёт trust-гейт реестра, вне — нет. Счётчик показывает, сколько
  // таких на самом деле.
  const f = state.plugFilter || 'all';
  if (f === 'ok') hit = hit.filter(p => p.installed && p.trusted);
  else if (f === 'no') hit = hit.filter(p => p.installed && !p.trusted);
  else if (f === 'store') hit = hit.filter(p => !p.installed);
  const c = $('#plug-filter-count');
  if (c) {
    const total = storeRows().length;
    const installed = storeRows().filter(p => p.installed).length;
    const ok = storeRows().filter(p => p.installed && p.trusted).length;
    c.textContent = `${total} всего · ${installed} установлено · ${ok} в allow-list`;
  }
  for (const [id, val] of [
    ['#plug-filter-all', 'all'], ['#plug-filter-ok', 'ok'],
    ['#plug-filter-no', 'no'], ['#plug-filter-store', 'store'],
  ]) {
    const b = $(id);
    if (b) b.classList.toggle('active', f === val);
  }
  $('#plug-tab-list').innerHTML = hit.map(p => {
    // Неустановленный плагин рисуется иначе: у него нет ни доверия, ни прав,
    // и показывать ему точку как у установленного — врать о состоянии.
    if (!p.installed) {
      return `<div class="pitem" data-plugin-id="${esc(p.name)}"><span class="tdot"></span> ${esc(p.name)}`
        + ` <small>${esc(p.version || '')} · не установлен</small></div>`;
    }
    const dot = p.trusted ? '<span class="tdot ok"></span>' : '<span class="tdot"></span>';
    return `<div class="pitem" data-plugin-id="${esc(p.name)}">${dot} ${esc(p.name)}`
      + ` <small>${esc(p.version || '')}${p.trusted ? '' : ' · вне allow-list'}</small></div>`;
  }).join('') || '<div class="empty">Ничего не найдено</div>';
  $('#plug-tab-list').querySelectorAll('[data-plugin-id]').forEach(el =>
    el.addEventListener('click', () => openPluginDetail(el.dataset.pluginId)));
}

async function openPluginDetail(id) {
  document.querySelectorAll('#plug-tab-list .pitem').forEach(el =>
    el.classList.toggle('active', el.dataset.pluginId === id));
  $('#plugdetail').innerHTML = '<div class="empty">загрузка…</div>';

  // Неустановленный плагин из витрины: у него нет ни прав, ни доверия, ни
  // каталога, поэтому /api/plugins/<id> вернёт 404, а карточка из него
  // получилась бы пустой. Рисуем карточку каталога с установкой.
  const row = storeRows().find(p => p.name === id);
  if (row && !row.installed) {
    $('#plugdetail').innerHTML = renderStoreCard(row);
    const b = $('#store-install');
    if (b) b.onclick = () => installStorePlugin(id, b);
    return;
  }

  let d;
  try {
    d = await api('/api/plugins/' + encodeURIComponent(id));
  } catch (e) {
    $('#plugdetail').innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
    return;
  }
  const reqs = (((d.runtime || {}).requires) || []);
  // Права приходят из /api/plugins/{id} полем permissions, а в нём — структура
  // pipeline.Permissions: у неё только yaml-теги, поэтому encoding/json отдаёт
  // ИМЕНА ПОЛЕЙ ГО (Network/Filesystem/Secrets/AnyHost), а не нижний регистр.
  // Чтение d.permissions.network молча давало пустоту, и строка «Права» писала
  // «сеть: —» даже для плагина, который в манифесте заявил сеть. Берём оба
  // регистра: так карточка переживёт и json-теги в Go, и их отсутствие.
  const perms = d.permissions || {};
  const net = perms.Network || perms.network || [];
  const netLine = net.map(n => {
    const any = n.AnyHost || n.any_host;
    const h = n.Host != null ? n.Host : n.host;
    const p = n.Port != null ? n.Port : n.port;
    // any_host печатаем как «*:порт», а не словом: так же эту заявку плагина
    // показывает редактор в предупреждении валидатора («шаг syntax: плагин
    // заявил сеть: *:53»), и читатель карточки видит то же самое.
    return (any ? (h || '*') : h) + (p != null && p !== '' ? ':' + p : '');
  });
  const fsLine = perms.Filesystem || perms.filesystem || '';
  const secLine = perms.Secrets || perms.secrets || [];
  const trustLine = d.trusted
    ? '<span class="badge ok">доверен</span>'
    : '<span class="badge err">вне allow-list</span>'
      + (d.blocked_reason ? `<div class="hint">${esc(d.blocked_reason)}</div>` : '');
  const depsBlock = reqs.length
    ? `<div class="dbox"><div class="dlist">${reqs.map(esc).join('<br>')}</div>
       <div class="dsub">точные пины из манифеста — ставится только это</div>
       <button class="mini" id="deps-install">установить</button></div>
       <div class="hint" id="deps-status"></div>
       <pre class="jnl" id="deps-out" style="display:none;max-height:220px"></pre>`
    : '<div class="hint">Зависимостей не объявлено (runtime.requires пуст) — ставить нечего.</div>';
  $('#plugdetail').innerHTML = `
    <div class="dhead"><h2 class="mono">${esc(d.id)}</h2>${trustLine}</div>
    <div class="sub mono dim">${esc(d.version || '')} · ${esc(d.author || '')}</div>
    <p style="line-height:1.6">${esc(d.description || 'без описания')}</p>
    <h4>Права</h4>
    <div class="hint">сеть: ${net.length ? esc(netLine.join(', ')) : '—'};
      файлы: ${esc(fsLine || '—')};
      секреты: ${esc(secLine.join(', ') || '—')}</div>
    <h4 style="margin-top:16px">Зависимости Python</h4>
    ${depsBlock}`;
  const btn = $('#deps-install');
  if (btn) btn.onclick = () => installPluginDeps(d.id, reqs, btn);

  // Готовность — только если замер был запрошен явно (?status=1). Без этого
  // молчание о неизвестном нельзя показывать как «всё готово»: витрина
  // отвечает на вопрос «что стоит», а не «что запустится».
  // status_measured живёт на уровне ответа, а не элемента: замер один на весь
  // каталог, и читать его из строки плагина было бы подменой смысла.
  const measured = state.store && state.store.status_measured;
  const health = measured && row && row.status ? row : null;
  if (health) {
    const badge = health.status === 'ready'
      ? '<span class="badge ok">готов</span>'
      : '<span class="badge err">' + esc(health.status || 'не готов') + '</span>';
    const hint = health.install_hint
      ? `<div class="hint">чего не хватает: ${esc(health.install_hint)}</div>`
      : '';
    $('#plugdetail').insertAdjacentHTML('beforeend',
      `<h4 style="margin-top:16px">Готовность на этой машине</h4>${badge}${hint}`);
  }
}

// renderStoreCard — карточка плагина, которого ещё нет на диске.
//
// Честность здесь в том, что не показано ничего, чего нет: ни «готов», ни
// «вне allow-list» — решать о доверии нечего, пока нет содержимого. Соблазн
// нарисовать серую точку и тишину был бы удобен, но это ровно та ложь, от
// которой защищает doctor: отсутствие информации неотличимо от успеха.
function renderStoreCard(p) {
  return `
    <div class="dhead"><h2 class="mono">${esc(p.name)}</h2>
      <span class="badge">не установлен</span></div>
    <div class="sub mono dim">${esc(p.version || '')}</div>
    <p style="line-height:1.6">${esc(p.description || 'без описания')}</p>
    <div class="hint">источник: ${esc(p.source || '—')}</div>
    <div class="hint">Готовность неизвестна: каталога нет на диске, и что плагину
      нужно доустановить, видно только после установки и замера.</div>
    <button class="primary" id="store-install">Установить</button>
    <div class="hint" id="store-status"></div>
    <pre class="jnl" id="store-out" style="display:none;max-height:220px"></pre>`;
}

async function installStorePlugin(name, btn) {
  if (!confirm(`Установить плагин ${name} из реестра?`)) return;
  const st = $('#store-status'), out = $('#store-out');
  btn.disabled = true;
  btn.textContent = 'ставится…';
  try {
    const r = await api('/api/store/install', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({name}),
    });
    if (st) st.textContent = r.ok ? 'Готово.' : ('Ошибка: ' + (r.error || ''));
    if (out && r.log) { out.style.display = ''; out.textContent = r.log.slice(-3000); }
    // Перечитываем вкладку: после установки плагин должен переехать из
    // «не установлен» в список стоящих, а решение о доверии появляется вместе
    // с содержимым на диске.
    if (r.ok) { await loadPluginsTab(); openPluginDetail(name); }
  } catch (e) {
    if (st) st.textContent = 'Ошибка: ' + e.message;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Установить';
  }
}

async function installPluginDeps(id, reqs, btn) {
  const cmd = `python -m pip install ${reqs.join(' ')}`;
  if (!confirm(`Установить зависимости плагина ${id}?\n\n${cmd}\n\nСтавятся только точные пины из манифеста.`)) return;
  btn.disabled = true;
  btn.textContent = 'ставится…';
  const st = $('#deps-status'), out = $('#deps-out');
  try {
    const r = await api('/api/plugins-install-deps?id=' + encodeURIComponent(id), {method: 'POST'});
    if (st) st.textContent = r.ok ? 'Готово.' : ('Ошибка: ' + (r.error || ''));
    if (out && r.output) { out.style.display = ''; out.textContent = r.output.slice(-3000); }
  } catch (e) {
    if (st) st.textContent = 'Ошибка: ' + e.message;
  } finally {
    btn.disabled = false;
    btn.textContent = 'установить';
  }
}

async function startRun() {
  const file = $('#run-select').value;
  if (!file) return;
  const btn = $('#run-btn');
  btn.disabled = true;
  $('#run-status').textContent = 'старт…';
  try {
    state.runIdsBeforeStart = state.runs.map(r => r.id);
    state.runsBeforeStart = state.runs.length;
    state.timers.pendingNew = true;
    const yes = !$('#run-gate').checked;
    const res = await api('/api/run', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({file, yes}) });
    if (res && res.run) { state.timers.pendingNew = false; setTimeout(() => openRunDetail(res.run, true), 400); }
    $('#run-status').textContent = yes ? 'ран запущен (--yes) — ищем его в списке…' : 'ран запущен — гейты будут решать прямо здесь…';
  } catch (e) {
    $('#run-status').innerHTML = '<span style="color:var(--err)">' + esc(e.message) + '</span>';
    btn.disabled = false;
    state.timers.pendingNew = false;
  }
}

// ── деталка рана: таймлайн + контекст + журнал ───────────────────────────
let stateDetailStatus = '';
async function openRunDetail(id, force) {
  if (state.currentRun === id && !force) return;
  state.currentRun = id;
  renderRuns();
  let d;
  try { d = await api('/api/runs/' + id); } catch (e) { return; }
  state.detailStatus = d.status;
  const st = d.status === 'ok' ? 'ok' : d.status === 'running' ? 'run' : d.status === 'cancelled' ? 'skip' : 'err';
  const label = d.status === 'running' ? 'идёт…' : d.status === 'cancelled' ? 'отменён' : d.status;
  const cancelBtn = d.status === 'running' ? `<button class="btn" id="cancel-btn" data-action="cancel" data-run-id="${esc(id)}">■ отменить</button>` : '';
  const ctx = d.context || {};
  const steps = (ctx.steps && Object.keys(ctx.steps).length) || 0;
  const inp = ctx.input ? Object.keys(ctx.input).length : 0;
  // деталка отдаёт окно журнала (хвост, если журнал больше потолка ответа)
  const total = (typeof d.total === 'number') ? d.total : d.events.length;
  const trunc = d.truncated
    ? `<span class="sub">журнал обрезан: показаны последние ${d.events.length} из ${total} событий</span>`
    : '';

  $('#detail').innerHTML = `
    <div id="gate-card" style="display:none"></div>
    <div class="dhead">
      <button class="btn" data-action="close">←</button>
      <h2>${esc(d.pipeline || '?')}</h2>
      <span class="badge ${st}">${esc(label)}</span>
      ${cancelBtn}
      <span class="sub">${esc(id)} · контекст: input ${inp} полей, steps ${steps}</span>
      ${trunc}
    </div>
    <div class="cols">
      <div class="card">
        <h3>Таймлайн (журнал)</h3>
        <div class="tl" id="tl">${renderTimeline(d.events)}</div>
      </div>
      <div class="card">
        <h3>
          Контекст / Журнал
          <span class="rtabs">
            <button id="rt-ctx" class="active" data-action="tab" data-tab="ctx">контекст</button>
            <button id="rt-jnl" data-action="tab" data-tab="jnl">журнал <span id="jnl-n"></span></button>
          </span>
        </h3>
        <div class="ctx" id="rt-ctx-body">${renderCtx(ctx)}</div>
        <div class="jnl" id="rt-jnl-body" style="display:none">
          <div style="margin-bottom:8px"><span class="autochip on" id="autochip" data-action="auto">авто-скролл</span>
          <span style="font-size:10px;color:var(--dim);margin-left:8px">live: обновление каждые 2 c</span></div>
          <div id="jnl-lines"></div>
        </div>
      </div>
    </div>`;
  wireRunDetail(id);
  // журнал: окно событий + polling хвоста по курсору next/total
  state.journal = { events: d.events, since: total, total: total, truncated: !!d.truncated };
  renderJournal(d.events);
  updateGateCard(id);
  clearInterval(state.timers.journal);
  state.timers.journal = setInterval(async () => {
    if (state.currentRun !== id || state.tab !== 'runs') { clearInterval(state.timers.journal); return; }
    try {
      const tail = await api(`/api/runs/${id}/journal?since=${state.journal.since}`);
      if (tail.events.length) {
        // курсор: next — точное место конца окна (truncated=true), иначе total
        state.journal.since = (typeof tail.next === 'number') ? tail.next : tail.total;
        state.journal.events = state.journal.events.concat(tail.events);
        const tl = $('#tl'); if (tl) tl.innerHTML = renderTimeline(state.journal.events);
        renderJournal(tail.events, true);
        updateGateCard(id);
        const d2 = state.journal.events[state.journal.events.length - 1];
        if (d2 && (d2.type === 'run_end' || d2.type === 'run_failed' || d2.type === 'run_cancelled')) {
          const cb = $('#cancel-btn'); if (cb) cb.remove();
          const dd = await api('/api/runs/' + id);
          state.detailStatus = dd.status;
          // Деталка, открытая во время рана, показывает пустой контекст и
          // старый бейдж: контекст грузился один раз при открытии. При
          // завершении подтягиваем и то, и другое из свежей деталки.
          refreshRunHead(dd);
          const cb2 = $('#rt-ctx-body');
          if (cb2) cb2.innerHTML = renderCtx(dd.context || {});
          tickRuns();
        }
      }
      const jn = $('#jnl-n'); if (jn) jn.textContent = `(${state.journal.total})`;
    } catch {}
  }, 2000);
}

function wireRunDetail(id) {
  const root = $('#detail');
  const close = root.querySelector('[data-action="close"]');
  if (close) close.addEventListener('click', () => closeDetail());
  const cancel = root.querySelector('[data-action="cancel"]');
  if (cancel) cancel.addEventListener('click', () => cancelRun(id));
  root.querySelectorAll('[data-action="tab"]').forEach(button => button.addEventListener('click', () => showRTab(button.dataset.tab)));
  const auto = root.querySelector('[data-action="auto"]');
  if (auto) auto.addEventListener('click', () => toggleAuto());
}

// v0.9: отмена рана (POST /cancel; требует сессию человека)
window.cancelRun = async (id) => {
  const b = $('#cancel-btn'); if (b) { b.disabled = true; b.textContent = 'отмена…'; }
  try { await api('/api/runs/' + id + '/cancel', { method: 'POST' }); }
  catch (e) { if (b) { b.disabled = false; b.textContent = '■ отменить'; } alert(e.message); }
};

window.closeDetail = () => {
  state.currentRun = null;
  clearInterval(state.timers.journal);
  renderRuns();
  $('#detail').innerHTML = '<div class="empty">Выбери ран слева — таймлайн, контекст и live-журнал появятся здесь</div>';
};

function renderTimeline(events) {
  let html = '';
  let inItem = -1, inPar = false;
  const t = e => (e.ts || '').split('T')[1] || '';
  for (const e of events) {
    const type = e.type;
    if (type === 'item_start') {
      if (inItem >= 0) html += '</div>';
      if (inPar) { html += '</div>'; inPar = false; }
      inItem = e.item_index;
      html += `<div class="item"><div class="hd">элемент ${esc(String(e.item_index))}${e.item != null ? ' · ' + esc(JSON.stringify(e.item)) : ''}</div>`;
      continue;
    }
    if (type === 'item_end' || type === 'item_aborted') {
      if (inItem >= 0) { html += '</div>'; inItem = -1; }
      const bad = type === 'item_aborted';
      html += ev(t(e), bad ? 'err' : 'ok', `${bad ? 'элемент прерван' : 'элемент завершён'}`, esc(String(e.status || '')));
      continue;
    }
    if (type === 'parallel_start') { inPar = true; html += `<div class="par"><div class="hd">‖ параллельно: ${esc((e.steps || []).join(', '))}</div>`; continue; }
    if (type === 'parallel_end') { if (inPar) html += '</div>'; inPar = false; html += ev(t(e), 'par', `‖ группа завершена за ${esc(String(e.duration_ms ?? '?'))} мс`, e.statuses ? esc(JSON.stringify(e.statuses)) : ''); continue; }
    if (type === 'foreach_item_start') { html += ev(t(e), 'dim', `⤷ ${esc(e.step)} · элемент ${esc(String(e.item_index))}`, ''); continue; }
    if (type === 'foreach_item_end') { html += ev(t(e), 'dim', `⤷ ${esc(e.step)} · элемент ${esc(String(e.item_index))} → ${esc(e.status)}`, e.duration_ms != null ? esc(String(e.duration_ms)) + ' мс' : ''); continue; }
    if (type === 'step_start') { html += ev(t(e), 'run', `▶ ${esc(e.step)}`, e.attempt > 1 ? `повтор ${esc(String(e.attempt))}` : ''); continue; }
    if (type === 'step_end') {
      const cls = e.status === 'ok' ? 'ok' : 'err';
      html += ev(t(e), cls, `${e.status === 'ok' ? '✓' : '✗'} ${esc(e.step)}`, `${esc(String(e.duration_ms ?? '?'))} мс · exit ${esc(String(e.exit_code))}${e.error ? ' · ' + esc(errText(e.error)) : ''}`);
      continue;
    }
    if (type === 'step_failed') { html += ev(t(e), 'err', `✗ ${esc(e.step)}: ${esc(errText(e.error) || 'ошибка')}`, ''); continue; }
    if (type === 'step_skipped') { html += ev(t(e), 'skip', `↷ ${esc(e.step)} пропущен`, e.reason ? `reason: ${esc(e.reason)}${e.condition ? ' · ' + esc(e.condition) : ''}` : ''); continue; }
    if (type === 'gate_wait') { html += ev(t(e), 'run', `👤 гейт ${esc(e.step)}: ожидает решение (в браузере)`, esc((e.actions || []).map(a => String(a)).join('/'))); continue; }
    if (type === 'gate_retry') { html += ev(t(e), 'skip', `⚠ гейт ${esc(e.step)}: ${esc(e.reason || 'переспрос')}`, 'попытка ' + esc(String(e.attempt || '?'))); continue; }
    if (type === 'gate_decision') { html += ev(t(e), e.action === 'accept' ? 'ok' : 'skip', `👤 гейт ${esc(e.step)}: ${esc(e.action)}${e.auto ? ' (авто --yes)' : ''}${e.source ? ' · ' + esc(e.source) : ''}`, e.materialized ? esc(JSON.stringify(e.materialized)) : ''); continue; }
    if (type === 'run_start') { html += ev(t(e), 'dim', `ран: ${esc(e.pipeline || '?')}${e.foreach ? ' · foreach ' + esc(e.foreach) : ''}`, ''); continue; }
    if (type === 'run_resumed') { html += ev(t(e), 'par', `ран возобновлён (resume)`, ''); continue; }
    if (type === 'run_end') { html += ev(t(e), (e.aborted || 0) ? 'err' : 'ok', `■ ран завершён: ok=${esc(String(e.ok || 0))} aborted=${esc(String(e.aborted || 0))}`, ''); continue; }
    if (type === 'run_failed') { html += ev(t(e), 'err', `■ ран упал${e.code ? ' [' + esc(e.code) + ']' : ''}: ${esc(errText(e.error))}`, ''); continue; }
    if (type === 'run_cancelled') { html += ev(t(e), 'skip', '■ ран отменён (resume — продолжить с места остановки)', ''); continue; }
    if (type === 'post_phase_start') { html += ev(t(e), 'dim', 'post-фаза (после foreach)…', ''); continue; }
    if (type === 'post_phase_end') { html += ev(t(e), 'dim', 'post-фаза завершена', ''); continue; }
    if (type === 'foreach_item_failed') { html += ev(t(e), 'err', `⤷ ${esc(e.step)} · элемент ${esc(String(e.item_index))}: ${esc(errText(e.error) || 'ошибка')}`, ''); continue; }
    if (type === 'file_ref_warning' || type === 'contract_warning') { html += ev(t(e), 'dim', '· ' + esc(e.message || e.warning || JSON.stringify(e)), ''); continue; }
  }
  if (inItem >= 0) html += '</div>';
  if (inPar) html += '</div>';
  return html || '<div class="empty">пусто</div>';
}

function ev(time, cls, msg, extra) {
  return `<div class="ev ${cls}"><span class="t">${esc(time)}</span><span class="m">${msg}</span>${extra ? `<span class="x">${extra}</span>` : ''}</div>`;
}

// refreshRunHead — обновить бейдж статуса и счётчики контекста в шапке деталки
// после завершения рана. Без этого деталка, открытая во время выполнения,
// навсегда остаётся с бейджем «идёт…» и «input 0 полей».
function refreshRunHead(d) {
  const head = document.querySelector('#detail .dhead');
  if (!head) return;
  const st = d.status === 'ok' ? 'ok' : d.status === 'running' ? 'run' : d.status === 'cancelled' ? 'skip' : 'err';
  const label = d.status === 'running' ? 'идёт…' : d.status === 'cancelled' ? 'отменён' : d.status;
  const badge = head.querySelector('.badge');
  if (badge) { badge.className = 'badge ' + st; badge.textContent = label; }
  const ctx = d.context || {};
  const steps = (ctx.steps && Object.keys(ctx.steps).length) || 0;
  const inp = ctx.input ? Object.keys(ctx.input).length : 0;
  const sub = head.querySelector('.sub');
  if (sub) {
    const id = (sub.textContent || '').split(' · ')[0];
    sub.textContent = `${id} · контекст: input ${inp} полей, steps ${steps}`;
  }
}

// ── контекст ─────────────────────────────────────────────────────────────
function renderCtx(ctx) {
  if (!ctx || !Object.keys(ctx).length) return '<div class="empty">контекст пуст</div>';
  return `<div style="font-size:11px;color:var(--dim);margin-bottom:8px">снапшот context.json — input.* и steps.*</div>` +
    Object.entries(ctx).map(([k, v]) => `<details ${k === 'steps' ? 'open' : ''}><summary><span class="k">${esc(k)}</span></summary>${jsonTree(v, 1)}</details>`).join('');
}

function jsonTree(v, depth) {
  const pad = '· '.repeat(depth);
  if (v === null) return '<span class="m">null</span>';
  if (typeof v === 'string') return `<span class="s">"${esc(v.length > 200 ? v.slice(0, 200) + '…' : v)}"</span>`;
  if (typeof v === 'number') return `<span class="n">${v}</span>`;
  if (typeof v === 'boolean') return `<span class="b">${v}</span>`;
  if (Array.isArray(v)) {
    if (!v.length) return '<span class="m">[]</span>';
    if (v.every(x => typeof x !== 'object' || x === null)) return `<span class="m">[${v.map(x => typeof x === 'string' ? esc(x) : JSON.stringify(x)).join(', ')}]</span>`;
    return '<div>' + v.map((x, i) => `<div>${pad}[${i}] ${jsonTree(x, depth)}</div>`).join('') + '</div>';
  }
  const keys = Object.keys(v);
  if (!keys.length) return '<span class="m">{}</span>';
  return '<div>' + keys.map(k =>
    `<details ${depth < 2 ? 'open' : ''} style="margin-left:${depth * 10}px"><summary><span class="k">${esc(k)}</span>${renderInline(v[k], depth)}</summary>${typeof v[k] === 'object' && v[k] !== null ? jsonTree(v[k], depth + 1) : ''}</details>`
  ).join('') + '</div>';
}

function renderInline(v, depth) {
  if (v === null) return ' = <span class="m">null</span>';
  if (typeof v === 'string') return ` = <span class="s">"${esc(v.length > 60 ? v.slice(0, 60) + '…' : v)}"</span>`;
  if (typeof v === 'number' || typeof v === 'boolean') return ` = <span class="${typeof v === 'number' ? 'n' : 'b'}">${v}</span>`;
  if (Array.isArray(v)) return ` = <span class="m">[${v.length}]</span>`;
  return ` = <span class="m">{${Object.keys(v).length}}</span>`;
}

// ── live-журнал ──────────────────────────────────────────────────────────
window.showRTab = which => {
  $('#rt-ctx').classList.toggle('active', which === 'ctx');
  $('#rt-jnl').classList.toggle('active', which === 'jnl');
  $('#rt-ctx-body').style.display = which === 'ctx' ? '' : 'none';
  $('#rt-jnl-body').style.display = which === 'jnl' ? '' : 'none';
};
window.toggleAuto = () => {
  state.autoScroll = !state.autoScroll;
  $('#autochip').classList.toggle('on', state.autoScroll);
};

// Журнал: раньше каждая строка была «время + JSON.stringify(остальное)», то
// есть дамп во всю ширину без разбивки. Теперь это читаемая строка лога —
// событие, шаг, статус, длительность, — а полный JSON спрятан под
// «подробности», чтобы ничего не потерять.
const EV_TONE = {
  run_start: 'dim', run_end: 'dim',
  step_start: 'run', step_end: 'ok', step_error: 'err',
  item_start: 'dim', item_end: 'ok', item_aborted: 'err',
  gate_wait: 'skip', gate_resolved: 'ok',
};

function journalLine(e) {
  const ts = (e.ts || '').split('T')[1] || '';
  const tone = EV_TONE[e.type] || '';
  const bits = [];
  if (e.step) bits.push(esc(String(e.step)));
  if (e.item_index != null) bits.push('элемент ' + esc(String(e.item_index)));
  if (e.attempt > 1) bits.push('попытка ' + esc(String(e.attempt)));
  if (e.status) bits.push('<b class="s ' + (e.status === 'ok' ? 'g' : e.status === 'aborted' ? 'e' : '') + '">' + esc(String(e.status)) + '</b>');
  if (e.exit_code != null) bits.push('exit ' + esc(String(e.exit_code)));
  if (e.duration_ms != null) bits.push(esc(String(e.duration_ms)) + ' мс');
  if (e.network) bits.push('network: ' + esc(String(e.network)));
  if (e.error) bits.push('<span class="je">' + esc(errText(e.error)) + '</span>');
  const {ts: _t, ...rest} = e;
  const raw = Object.keys(rest).length > 1
    ? '<details class="jraw"><summary>подробности</summary><pre>' + esc(JSON.stringify(rest, null, 1)) + '</pre></details>'
    : '';
  return `<div class="ln ${tone}"><span class="ts">${esc(ts)}</span>`
    + `<span class="ty">${esc(String(e.type || ''))}</span>`
    + `<span class="bits">${bits.join(' · ')}</span>${raw}</div>`;
}

function renderJournal(newEvents, append) {
  const body = $('#jnl-lines');
  if (!body) return;
  const src = append ? newEvents : state.journal.events;
  const lines = src.map(journalLine).join('');
  if (append && body.firstChild) body.insertAdjacentHTML('beforeend', lines);
  else body.innerHTML = lines;
  if (state.autoScroll) $('#rt-jnl-body').scrollTop = $('#rt-jnl-body').scrollHeight;
}

// ── пайплайны + DAG ──────────────────────────────────────────────────────
async function openPipeline(file) {
  state.currentPipe = file;
  // Открытый в консоли пайплайн становится точкой входа в редактор: именно
  // к нему вернёмся, когда откроем консоль без адреса.
  rememberPipeline(file);
  setModeHighlight();
  renderPipList();
  $('#pdetail').innerHTML = '<div class="empty">загрузка…</div>';
  let yaml, plan;
  try {
    yaml = await api('/api/pipelines/' + file);
    const r = await fetch('/api/plan/pipeline', { method: 'POST', body: yaml, headers: {'Content-Type': 'text/yaml'} });
    plan = await r.json();
  } catch (e) {
    $('#pdetail').innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
    return;
  }
  const p = state.pipelines.find(x => x.file === file) || {};
  const sched = parseScheduleFromYaml(yaml);
  const schedBadge = sched ? `<span class="badge" style="border-color:var(--acc);color:var(--acc)">◷ ${esc(sched.cron || '')} ${sched.timezone ? '· ' + esc(sched.timezone) : ''}</span>` : `<span class="badge">⏺ соло</span>`;
  $('#pdetail').innerHTML = `
    <div class="dhead" style="margin-bottom:12px">
      <h2>${esc(p.name || file)}</h2>
      <span class="sub">${esc(p.steps)} ш.${p.foreach ? ' · foreach ' + esc(p.foreach) : ''}</span>
      ${plan.errors && plan.errors.length ? `<span class="badge err">errors: ${plan.errors.length}</span>` : '<span class="badge ok">валиден</span>'}
      ${schedBadge}
    </div>
    ${plan.errors && plan.errors.length ? `<div style="color:var(--err);font-size:12px;margin-bottom:10px">${plan.errors.map(esc).join('<br>')}</div>` : ''}
    ${plan.warnings && plan.warnings.length ? `<div style="color:var(--skip);font-size:12px;margin-bottom:10px">${plan.warnings.map(esc).join('<br>')}</div>` : ''}
    <div class="card" id="sched-card" style="margin-bottom:16px">
      <h3>Запуск <span style="text-transform:none;letter-spacing:0;font-weight:400;color:var(--faint)"> — соло vs по расписанию · твой сервер, без облака</span> <span id="sched-next" style="float:right;color:var(--faint);font-size:10px"></span></h3>
      <div style="padding:14px">
        <div style="display:flex;gap:6px;margin-bottom:10px;background:var(--sunken);border:1px solid var(--hair);border-radius:var(--r);padding:3px;width:fit-content">
          <button id="sched-mode-solo" class="btn" style="background:${sched ? 'transparent' : 'var(--acc)'};color:${sched ? 'var(--bone)' : 'var(--sunken)'};border-color:${sched ? 'transparent' : 'var(--acc)'};font-weight:600">▶ Соло — запустил, получил</button>
          <button id="sched-mode-cron" class="btn" style="background:${sched ? 'var(--acc)' : 'transparent'};color:${sched ? 'var(--sunken)' : 'var(--bone)'};border-color:${sched ? 'var(--acc)' : 'transparent'};font-weight:600">◷ По расписанию — крутится само</button>
        </div>
        <div id="sched-panel" style="display:${sched ? 'block' : 'none'}">
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:12px">
            <div>
              <label style="font-size:10px;letter-spacing:.14em;text-transform:uppercase;color:var(--faint)">CRON</label>
              <input id="sched-cron" value="${esc(sched ? sched.cron : '0 9 * * *')}" placeholder="* * * * *" style="width:100%;background:var(--sunken);color:var(--bone);border:1px solid var(--hair2);border-radius:var(--r);padding:7px 9px;font-family:var(--mono);font-size:12px;margin-top:4px"/>
              <div style="margin-top:6px;display:flex;gap:6px;flex-wrap:wrap">
                <button class="mini" data-cron="*/5 * * * *">каждые 5м</button>
                <button class="mini" data-cron="0 * * * *">час</button>
                <button class="mini" data-cron="0 9 * * *">09:00</button>
                <button class="mini" data-cron="0 9 * * 1">пн 09:00</button>
              </div>
            </div>
            <div>
              <label style="font-size:10px;letter-spacing:.14em;text-transform:uppercase;color:var(--faint)">Таймзона</label>
              <select id="sched-tz" style="width:100%;background:var(--sunken);color:var(--bone);border:1px solid var(--hair2);border-radius:var(--r);padding:7px 9px;font-size:12px;margin-top:4px">
                <option ${sched && sched.timezone==='Europe/Minsk' ? 'selected' : ''}>Europe/Minsk</option>
                <option ${!sched || !sched.timezone || sched.timezone==='UTC' ? '' : ''}>UTC</option>
                <option ${sched && sched.timezone==='Europe/Moscow' ? 'selected' : ''}>Europe/Moscow</option>
                <option ${sched && sched.timezone==='Local' ? 'selected' : ''}>Local</option>
              </select>
              <div class="hint" style="margin-top:6px">overlap: <select id="sched-overlap" style="background:var(--sunken);color:var(--bone);border:1px solid var(--hair);border-radius:3px;padding:2px 6px;font-size:11px"><option ${!sched || sched.overlap==='skip' ? 'selected' : ''}>skip</option><option ${sched && sched.overlap==='queue' ? 'selected' : ''}>queue</option><option ${sched && sched.overlap==='allow' ? 'selected' : ''}>allow</option></select>
              &nbsp; <label style="font-size:11px"><input type="checkbox" id="sched-catchup" ${sched && sched.catchUp ? 'checked' : ''}/> catchUp</label></div>
            </div>
          </div>
          <div style="margin-top:12px;background:var(--ink);border:1px solid var(--hair);border-radius:var(--r);padding:12px">
            <div style="font-size:10px;letter-spacing:.14em;text-transform:uppercase;color:var(--faint);margin-bottom:8px">Human Gate в расписании</div>
            <label style="display:flex;gap:8px;align-items:center;font-size:12px;cursor:pointer"><input type="radio" name="sched-gate" value="autoApprove" ${!sched || sched.humanGate!=='wait' ? 'checked' : ''}/> Авто-принять (как --yes)</label>
            <label style="display:flex;gap:8px;align-items:center;font-size:12px;cursor:pointer;margin-top:6px"><input type="radio" name="sched-gate" value="wait" ${sched && sched.humanGate==='wait' ? 'checked' : ''}/> Ждать человека — встанет в waiting</label>
            <div id="sched-alerts" style="display:${sched && sched.humanGate==='wait' ? 'block' : 'none'};margin-top:10px">
              <div style="font-size:11px;color:var(--ash)">Алерты когда встал на паузу:</div>
              <div style="margin-top:6px;display:flex;gap:8px;flex-wrap:wrap">
                <span class="badge">Telegram: <input id="sched-tg" placeholder="telegram_notifier / @channel" value="${esc(sched && sched.alerts && sched.alerts.on_waiting ? sched.alerts.on_waiting : 'telegram_notifier')}" style="background:transparent;border:none;color:var(--bone);width:140px;font-size:11px"/></span>
                <span class="badge">Webhook: <input id="sched-hook" placeholder="https://..." value="${esc(sched && sched.alerts && sched.alerts.webhook ? sched.alerts.webhook : '')}" style="background:transparent;border:none;color:var(--bone);width:160px;font-size:11px"/></span>
              </div>
              <div class="hint">Отправится плагином: ставишь рана в wait → в Telegram прилетит «⏸ {pipeline} ждет апрува · wedra approve {run_id}»</div>
            </div>
          </div>
          <div style="margin-top:12px;display:flex;gap:8px;align-items:center">
            <button id="sched-save" class="primary" style="width:auto;padding:8px 18px">Сохранить расписание</button>
            <span id="sched-status" class="hint" style="margin:0"></span>
            <span class="sp"></span>
            <span style="font-size:11px;color:var(--faint)">var/runs у тебя · next: <b id="sched-next2" style="color:var(--acc)">—</b></span>
          </div>
        </div>
        <div id="sched-solo-hint" style="display:${sched ? 'none' : 'block'};margin-top:10px" class="hint">Соло: жмёшь Запустить руками или <code>wedra pipeline run</code>. Для дебага. Включи «По расписанию» — cron + human_gate + алерты появятся здесь, в YAML добавится блок <code>schedule:</code>.</div>
      </div>
    </div>
    <div class="card" style="margin-bottom:16px">
      <h3>DAG</h3>
      <svg id="dag"></svg>
    </div>
    <div class="card"><h3>YAML</h3><pre class="yaml">${esc(yaml)}</pre></div>`;
  wireScheduleCard(file, yaml, sched);
  drawDag(plan.dag);
}

function drawDag(dag) {
  const svg = $('#dag');
  if (!dag || !dag.nodes.length) { svg.outerHTML = '<div class="empty">DAG пуст</div>'; return; }
  const W = 180, H = 64, GX = 60, GY = 34;
  // топологические слои
  const ids = dag.nodes.map(n => n.id);
  const indeg = Object.fromEntries(ids.map(i => [i, 0]));
  const adj = {};
  for (const e of dag.edges || []) {
    if (!adj[e.from]) adj[e.from] = [];
    adj[e.from].push(e.to);
    if (indeg[e.to] != null) indeg[e.to]++;
    if (indeg[e.from] == null) indeg[e.from] = 0;
  }
  const layer = {};
  let changed = true, guard = 0;
  for (const i of ids) layer[i] = 0;
  while (changed && guard++ < 50) {
    changed = false;
    for (const e of dag.edges || []) {
      if (layer[e.from] != null && layer[e.to] != null && layer[e.to] < layer[e.from] + 1) {
        layer[e.to] = layer[e.from] + 1;
        changed = true;
      }
    }
  }
  const byLayer = {};
  for (const n of dag.nodes) (byLayer[layer[n.id]] = byLayer[layer[n.id]] || []).push(n);
  const layers = Object.keys(byLayer).map(Number).sort((a, b) => a - b);
  const maxCol = Math.max(...layers.map(l => byLayer[l].length));
  const padTop = 30;
  const pos = {};
  layers.forEach((l, li) => {
    const col = byLayer[l];
    col.forEach((n, ci) => {
      pos[n.id] = { x: 40 + li * (W + GX), y: padTop + ci * (H + GY) + ((maxCol - col.length) * (H + GY)) / 2 };
    });
  });
  const width = 40 + layers.length * (W + GX) + 40;
  const height = padTop * 2 + maxCol * (H + GY) + 60;
  let s = `<svg id="dag" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}">` +
    `<defs><marker id="arr" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto"><path d="M0,0 L8,4 L0,8 z" style="fill:rgba(233,229,221,.45)"/></marker></defs>`;
  // параллельные группы: рамка
  const groups = {};
  for (const n of dag.nodes) if (n.parallel_group) (groups[n.parallel_group] = groups[n.parallel_group] || []).push(n);
  for (const [g, members] of Object.entries(groups)) {
    if (members.length < 2) continue;
    const xs = members.map(m => pos[m.id].x), ys = members.map(m => pos[m.id].y);
    const x = Math.min(...xs) - 10, y = Math.min(...ys) - 20;
    const w = Math.max(...xs) + W - x + 10, h = Math.max(...ys) + H - y + 30;
    s += `<rect class="daggrp" x="${x}" y="${y}" width="${w}" height="${h}" rx="10"/><text class="dagtxt par" x="${x + 8}" y="${y - 6}">‖ ${esc(g)}</text>`;
  }
  for (const e of dag.edges || []) {
    const a = pos[e.from], b = pos[e.to];
    if (!a || !b) continue;
    const x1 = a.x + W, y1 = a.y + H / 2, x2 = b.x, y2 = b.y + H / 2;
    const mx = (x1 + x2) / 2;
    s += `<path class="dagedge" d="M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}" marker-end="url(#arr)"/>`;
  }
  for (const n of dag.nodes) {
    const p = pos[n.id];
    const phase = ['pre', 'foreach', 'post'].includes(n.phase) ? n.phase : (n.parallel_group ? 'foreach' : 'foreach');
    s += `<rect class="dagnode ${phase}" x="${p.x}" y="${p.y}" width="${W}" height="${H}" rx="8"/>`;
    s += `<text class="dagtxt" x="${p.x + 10}" y="${p.y + 20}">${esc(n.id)}</text>`;
    const plug = String(n.plugin || '').split('/').pop();
    s += `<text class="dagtxt dim" x="${p.x + 10}" y="${p.y + 36}">${esc(plug)}</text>`;
    let ly = p.y + 51;
    if (n.when) s += `<text class="dagtxt acc" x="${p.x + 10}" y="${ly}">when: ${esc(n.when)}</text>`;
    if (n.foreach) s += `<text class="dagtxt acc" x="${p.x + 10 + (n.when ? 100 : 0)}" y="${ly}">foreach: ${esc(n.foreach)}</text>`;
    if (n.phase === 'pre' || n.phase === 'post') s += `<text class="dagtxt dim" x="${p.x + W - 44}" y="${p.y + 20}">${n.phase}</text>`;
  }
  s += '</svg>';
  svg.outerHTML = s;
}

// ── v0.24: гейт-карточка (решение из браузера) ────────────────────────────
function pendingGate(events) {
  let gw = null;
  for (const e of events) {
    if (e.type === 'gate_wait') gw = e;
    else if (e.type === 'gate_decision') gw = null;
  }
  return gw;
}

function updateGateCard(id) {
  const card = $('#gate-card');
  if (!card || state.currentRun !== id) return;
  const gw = pendingGate(state.journal.events);
  const key = gw ? gw.ts + ':' + gw.step : '';
  if (!gw) {
    if (card.dataset.submitted !== key && key === '') {
      // гейт решён (или его не было): если карточка была нашей — убрать
      if (card.dataset.gateFor && card.dataset.gateFor !== 'done') card.innerHTML = '';
      card.style.display = 'none';
      card.dataset.gateFor = '';
    }
    return;
  }
  if (card.dataset.gateFor === key) return; // уже отрисована (или отправлена) — не затираем ввод
  card.dataset.gateFor = key;
  card.style.display = '';
  const fields = (gw.form || []).map(f => {
    const val = String(f.value == null ? '' : f.value);
    if (f.editable) {
      return `<div class="gfield"><label>* ${esc(f.field)}${f.type ? ' · ' + esc(f.type) : ''} (JSON, пусто — оставить)</label>
        <input data-field="${esc(f.field)}" value="${esc(val)}" spellcheck="false"/></div>`;
    }
    return `<div class="gfield"><label>${esc(f.field)}</label>
      <input readonly value="${esc(val)}"/></div>`;
  }).join('');
  const actions = Array.isArray(gw.actions) ? gw.actions : ['accept', 'reject'];
  const btns = actions.filter(a => a === 'accept' || a === 'reject').map(a => {
    const cls = a === 'accept' ? 'ok' : 'err';
    const label = a === 'accept' ? '✓ принять' : '✗ отклонить';
    return `<button class="${cls}" data-gate-action="${esc(a)}">${label}</button>`;
  }).join('');
  card.innerHTML = `
    <h3>👤 human_gate · ${esc(gw.step)} — ран ждёт твоего решения</h3>
    ${fields || '<div class="gstatus">форма пуста</div>'}
    <div class="gactions">${btns}</div>
    <div class="gstatus" id="gate-status"></div>`;
  card.querySelectorAll('[data-gate-action]').forEach(button => button.addEventListener('click', () => submitGate(id, button.dataset.gateAction)));
}

window.submitGate = async (id, action) => {
  const card = $('#gate-card');
  const st = $('#gate-status');
  const edits = {};
  let bad = null;
  card.querySelectorAll('input[data-field]').forEach(inp => {
    const v = inp.value.trim();
    if (v === '') return; // пусто = оставить
    let parsed;
    try { parsed = JSON.parse(v); }
    catch { bad = inp.dataset.field; return; }
    edits[inp.dataset.field] = parsed;
  });
  if (bad) {
    st.className = 'gstatus error';
    st.textContent = 'не JSON: ' + bad + ' — поправь и нажми действие ещё раз';
    return;
  }
  try {
    await api('/api/runs/' + id + '/gate', {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ action, edits }),
    });
    card.dataset.gateFor = 'done'; // больше не перерисовывать
    card.querySelectorAll('button').forEach(b => b.disabled = true);
    card.querySelectorAll('input').forEach(i => i.readOnly = true);
    st.className = 'gstatus';
    st.textContent = 'решение «' + action + '» отправлено — ран продолжится…';
  } catch (e) {
    st.className = 'gstatus error';
    st.textContent = e.message;
  }
};

init();
