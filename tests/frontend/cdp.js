// Управление браузером по CDP без зависимостей: Node 26 даёт глобальный
// WebSocket, Chrome умеет --remote-debugging-port. Этого хватает и на скриншот,
// и на настоящие клики/протяжки — то есть проверять интерфейс можно глазами, а
// не «структура в HTML выглядит правильно».
//
// node cdp.js <url> <out.png> [actions.json]
//   action: {"wait":500} {"click":[x,y]} {"drag":[x1,y1,x2,y2]} {"eval":"js"}
//           {"shot":"other.png"} {"key":"Enter"} {"text":"строка"}
//           {"metrics":[1760,900]} {"nav":"url"}
//
// CDP_VIEWPORT=1760x900 — размер САМОГО снимка в пикселях. Без него размер
//   окна не совпадает с картинкой: --window-size задаёт ОКНО, а в снимок
//   попадает область страницы, и она меньше на рамку. Точный размер нужен
//   публикуемым скриншотам: под README их читают как «экран 1760x900».
//   {"metrics":[w,h]} внутри сценария — то же самое посреди сессии, чтобы
//   снять один и тот же экран на двух ширинах.
const fs = require('fs');
const path = require('path');
const { spawn } = require('child_process');
const os = require('os');

const CHROME = [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
].find(p => fs.existsSync(p));
if (!CHROME) { console.error('браузер не найден'); process.exit(1); }

const [, , URL_, OUT, ACTIONS] = process.argv;
const PORT = 9222 + (Number(process.env.CDP_OFFSET) || 0);
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'cdp-'));
const sleep = ms => new Promise(r => setTimeout(r, ms));

const chrome = spawn(CHROME, [
  '--headless=new', '--disable-gpu', '--no-first-run', '--no-default-browser-check',
  '--remote-debugging-port=' + PORT, '--user-data-dir=' + profile,
  '--window-size=1680,1050', '--hide-scrollbars', '--force-device-scale-factor=1',
  '--disable-features=Translate,MediaRouter', 'about:blank',
], { stdio: 'ignore' });

let ws = null, id = 0;
const pending = new Map();

function send(method, params = {}) {
  const msgId = ++id;
  ws.send(JSON.stringify({ id: msgId, method, params }));
  return new Promise((res, rej) => pending.set(msgId, { res, rej }));
}

async function shot(file) {
  const r = await send('Page.captureScreenshot', { format: 'png' });
  fs.writeFileSync(file, Buffer.from(r.data, 'base64'));
  const kb = (fs.statSync(file).size / 1024).toFixed(0);
  console.log('  снимок ' + file + ' (' + kb + ' КиБ)');
}

// metrics — задать размер области страницы ровно в пикселях снимка.
async function metrics(w, h) {
  await send('Emulation.setDeviceMetricsOverride', {
    width: w, height: h, deviceScaleFactor: 1, mobile: false,
  });
}

async function main() {
  // ждём, пока браузер поднимет порт отладки
  let target = null;
  for (let i = 0; i < 60 && !target; i++) {
    await sleep(250);
    try {
      const list = await (await fetch('http://127.0.0.1:' + PORT + '/json/list')).json();
      target = list.find(t => t.type === 'page');
    } catch (e) { /* ещё не слушает */ }
  }
  if (!target) throw new Error('CDP не поднялся');

  ws = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((res, rej) => { ws.onopen = res; ws.onerror = rej; });
  ws.onmessage = ev => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const p = pending.get(m.id);
      pending.delete(m.id);
      m.error ? p.rej(new Error(m.method + ': ' + JSON.stringify(m.error))) : p.res(m.result);
    }
  };

  await send('Page.enable');
  await send('Runtime.enable');
  await send('Log.enable');

  const VIEWPORT = /^(\d+)x(\d+)$/.exec(process.env.CDP_VIEWPORT || '');
  if (VIEWPORT) await metrics(+VIEWPORT[1], +VIEWPORT[2]);

  await send('Page.navigate', { url: URL_ });
  await sleep(2200); // дать отработать загрузке и первым запросам

  if (OUT) await shot(OUT);

  if (ACTIONS) {
    const list = JSON.parse(fs.readFileSync(ACTIONS, 'utf8'));
    for (const a of list) {
      if (a.wait) { await sleep(a.wait); continue; }
      if (a.metrics) { await metrics(a.metrics[0], a.metrics[1]); await sleep(400); continue; }
      if (a.nav) {
        await send('Page.navigate', { url: a.nav });
        await sleep(a.navWait || 2000);
        continue;
      }
      if (a.shot) { await shot(a.shot); continue; }
      if (a.eval) {
        const r = await send('Runtime.evaluate', { expression: a.eval, returnByValue: true, awaitPromise: true });
        // Исключение в eval молча выглядело бы как «вернулось undefined» —
        // а это читается как «проверка прошла». Поэтому текст ошибки печатается.
        const ex = r.exceptionDetails;
        const v = r.result && r.result.value;
        console.log('  eval: ' + (ex
          ? 'ИСКЛЮЧЕНИЕ ' + (ex.exception && ex.exception.description || ex.text)
          : (typeof v === 'string' ? v : JSON.stringify(v))));
        continue;
      }
      if (a.click) {
        const [x, y] = a.click;
        await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, buttons: 0 });
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button: 'left', buttons: 1, clickCount: 1 });
        await sleep(60);
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button: 'left', buttons: 0, clickCount: 1 });
        await sleep(180);
        continue;
      }
      if (a.drag) {
        const [x1, y1, x2, y2] = a.drag;
        await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: x1, y: y1, buttons: 0 });
        await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: x1, y: y1, button: 'left', buttons: 1, clickCount: 1 });
        const steps = 14;
        for (let i = 1; i <= steps; i++) {
          await send('Input.dispatchMouseEvent', {
            type: 'mouseMoved', button: 'left', buttons: 1,
            x: x1 + (x2 - x1) * i / steps, y: y1 + (y2 - y1) * i / steps,
          });
          await sleep(25);
        }
        await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: x2, y: y2, button: 'left', buttons: 0, clickCount: 1 });
        await sleep(250);
        continue;
      }
      if (a.text) {
        for (const ch of a.text) {
          await send('Input.dispatchKeyEvent', { type: 'char', text: ch });
        }
        await sleep(150);
        continue;
      }
    }
  }
  ws.close();
  chrome.kill();
  await sleep(300);
  try { fs.rmSync(profile, { recursive: true, force: true }); } catch (e) { /* профиль не нужен */ }
}

main().then(() => process.exit(0)).catch(e => {
  console.error('ОШИБКА:', e.message);
  try { chrome.kill(); } catch (x) { /* уже мёртв */ }
  process.exit(1);
});