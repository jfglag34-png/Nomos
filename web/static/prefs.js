// Настройки интерфейса Nomos — общий модуль для консоли, редактора и
// артефактов. Подключается одним <script src="/prefs.js"> и сам применяет
// сохранённое к :root; при изменении настройки перерисовывает мгновенно.
//
// ПОЧЕМУ ОТДЕЛЬНЫМ ФАЙЛОМ, А НЕ В app.js каждой страницы:
//   tests/frontend/drag_harness.js и artifacts_harness.js исполняют
//   editor/app.js и artifacts/app.js в vm с МОК-контекстом. Если бы app.js
//   ссылался на настройки, падение ссылки стало бы ReferenceError и стенд
//   упал бы не по своей причине. Здесь настройки живут вне app.js — и стенды
//   не затронуты, и правка applies сразу на всех трёх страницах.
//
// ХРАНИЛИЩЕ — localStorage. Решение согласовано: файл в репозитории — это
// подсистема и формат данных, а наблюдаемости ноль. Ключ с версией, чтобы
// будущая смена формата не читала старые значения; на неизвестной версии
// возвращаемся к умолчаниям.
(function () {
  'use strict';
  var KEY = 'wedra.prefs.v1';

  // Пресеты плотности. «Стандартный» — ровно текущие значения, поэтому у того,
  // кто ничего не настраивал, вид не меняется ни на пиксель.
  var DENSITY = {
    std:  { label: 'Стандартный', fs: '12.5px', hdr: '42px', rowPy: '7px',  rowPx: '12px', itemPy: '9px',  itemPx: '16px' },
    compact: { label: 'Компактный', fs: '11.5px', hdr: '34px', rowPy: '4px',  rowPx: '10px', itemPy: '5px',  itemPx: '14px' },
    roomy:  { label: 'Просторный', fs: '13.5px', hdr: '50px', rowPy: '10px', rowPx: '14px', itemPy: '13px', itemPx: '18px' },
  };

  // Акцентные палитры. Каждая проверена: акцент ≥4.5 на всех четырёх
  // поверхностях. Тёплые оттенки сюда НЕ входят: они заняты семантикой
  // (--err/--ok/--skip), и «зелёный акцент» был бы неотличим от «успеха».
  var ACCENT = {
    teal:   { label: 'Бирюза',  acc: '#2fa8a0', hi: '#38bdb4' },
    blue:   { label: 'Синий',   acc: '#379bcd', hi: '#55b1de' },
    plum:   { label: 'Слива',   acc: '#aa86b6', hi: '#bd9cc8' },
  };

  var RAIL = {
    collapsed: { label: 'Свёрнута', w: '52px' },
    normal:    { label: 'Обычная',  w: '206px' },
    wide:      { label: 'Широкая',  w: '280px' },
  };

  var DEFAULTS = { density: 'std', accent: 'teal', rail: 'normal', roles: true, roleLabels: true };
  var cur = null;

  function clone(o) { return JSON.parse(JSON.stringify(o)); }

  // Любое незнакомое значение молча схлопывается в умолчание. Мусор в
  // localStorage не должен ломать страницу — ни SyntaxError, ни TypeError.
  function sanitize(raw) {
    var d = clone(DEFAULTS);
    if (!raw || typeof raw !== 'object') return d;
    if (DENSITY[raw.density]) d.density = raw.density;
    if (ACCENT[raw.accent]) d.accent = raw.accent;
    if (RAIL[raw.rail]) d.rail = raw.rail;
    if (typeof raw.roles === 'boolean') d.roles = raw.roles;
    if (typeof raw.roleLabels === 'boolean') d.roleLabels = raw.roleLabels;
    return d;
  }

  function load() {
    var raw = null;
    try {
      var s = window.localStorage.getItem(KEY);
      if (s) raw = JSON.parse(s);
    } catch (e) {
      // Приватный режим, переполнение, битый JSON — не повод ломать страницу.
      raw = null;
    }
    // Версия в ключе. Если формат сменится, ключ поменяется и старые значения
    // просто не найдутся: это и есть тихий откат к умолчаниям.
    return sanitize(raw);
  }

  function save(p) {
    try { window.localStorage.setItem(KEY, JSON.stringify(p)); } catch (e) { /* приватный */ }
  }

  // Токены, которые prefs.js вообще трогает.
  var DENSITY_VARS = ['--fs-base', '--hdr', '--row-py', '--row-px', '--item-py', '--item-px'];

  // Применение — только через CSS-переменные на :root. Ни одно правило вёрстки
  // не переписывается.
  //
  // ВАЖНО, почему плотность пишется ТОЛЬКО для нестандартного пресета: у трёх
  // страниц разный базовый кегль (консоль 12.5px, редактор 13px, артефакты
  // 12.5px). Если бы «Стандартный» тоже выставлял --fs-base:12.5px, редактор
  // поехал бы на полпикселя — а требование «умолчания дают ТОЧНО текущий вид»
  // запрещает это (на нём держатся опубликованные скриншоты README и демо-гиф).
  // Поэтому при «Стандартном» инлайн-переменные снимаются, и каждая страница
  // берёт своё собственное значение из своего :root.
  function apply(p) {
    var d = DENSITY[p.density] || DENSITY.std;
    var a = ACCENT[p.accent] || ACCENT.teal;
    var r = RAIL[p.rail] || RAIL.normal;
    var root = document.documentElement;

    if (p.density === DEFAULTS.density) {
      DENSITY_VARS.forEach(function (v) { root.style.removeProperty(v); });
    } else {
      root.style.setProperty('--fs-base', d.fs);
      root.style.setProperty('--hdr', d.hdr);
      root.style.setProperty('--row-py', d.rowPy);
      root.style.setProperty('--row-px', d.rowPx);
      root.style.setProperty('--item-py', d.itemPy);
      root.style.setProperty('--item-px', d.itemPx);
    }
    // Акцент и ширину рельсы выставляем всегда: их умолчания совпадают с
    // нынешними значениями, а без них палитра не применилась бы к артефактам
    // и редактору, где нет блока :root с --rail-w.
    root.style.setProperty('--acc', a.acc);
    root.style.setProperty('--acc-hi', a.hi);
    root.style.setProperty('--rail-w', r.w);
    // Роли и подписи ролей — выключатели классом на <html>: палитра узлов не
    // должна исчезать, а подписи обязаны уметь пропасть.
    root.classList.toggle('no-roles', !p.roles);
    root.classList.toggle('no-role-labels', !p.roleLabels);
    root.classList.toggle('rail-collapsed', p.rail === 'collapsed');
    cur = p;
  }

  function get() { return clone(cur || load()); }

  function set(patch) {
    var p = sanitize(Object.assign(get(), patch || {}));
    save(p);
    apply(p);
    if (window.WPrefs && window.WPrefs.onChange) {
      try { window.WPrefs.onChange(p); } catch (e) { /* панель не должна ронять страницу */ }
    }
    return p;
  }

  function reset() {
    try { window.localStorage.removeItem(KEY); } catch (e) { /* приватный */ }
    apply(clone(DEFAULTS));
    if (window.WPrefs && window.WPrefs.onChange) {
      try { window.WPrefs.onChange(clone(DEFAULTS)); } catch (e) { /* */ }
    }
    return get();
  }

  apply(load());

  window.WPrefs = {
    DENSITY: DENSITY, ACCENT: ACCENT, RAIL: RAIL, DEFAULTS: clone(DEFAULTS),
    get: get, set: set, reset: reset, apply: apply, onChange: null,
  };
})();