package main

// pageHTML is the whole panel. It is one page on purpose: the point is to see
// the node's state and change it without going anywhere else.
const pageHTML = `<!doctype html>
<html lang="ru"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Нода OpenFlux</title>
<style>
:root{--ink:#16202b;--soft:#4a5a6a;--faint:#8194a5;--bg:#f2f0ea;--card:#fff;--rule:#d8d5cc;--ok:#2f6b52;--warn:#9a5a1c;--accent:#b06a12}
@media(prefers-color-scheme:dark){:root{--ink:#e4eaf0;--soft:#9fb0c0;--faint:#6b7d8e;--bg:#101820;--card:#18222c;--rule:#2c3a47;--ok:#62b08c;--warn:#d79a52;--accent:#e5a44b}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",system-ui,sans-serif;padding:24px 16px 48px}
.wrap{max-width:900px;margin:0 auto}
h1{font-size:1.4rem;margin:0 0 4px}
.sub{color:var(--soft);margin:0 0 20px}
h2{font-size:.75rem;text-transform:uppercase;letter-spacing:.1em;color:var(--faint);margin:26px 0 10px;padding-bottom:6px;border-bottom:1px solid var(--rule)}
.card{background:var(--card);border:1px solid var(--rule);border-radius:4px;padding:14px 15px}
.grid{display:grid;gap:1px;background:var(--rule);border:1px solid var(--rule);border-radius:4px;overflow:hidden;grid-template-columns:repeat(auto-fit,minmax(150px,1fr))}
.cell{background:var(--card);padding:11px 13px}
.k{font-size:.72rem;color:var(--faint)}
.v{font-size:1.05rem;font-weight:600;font-variant-numeric:tabular-nums;margin-top:2px}
label{display:block;margin:0 0 12px}
.lab{font-size:.78rem;color:var(--soft);margin-bottom:4px}
select,textarea,input[type=file]{width:100%;font:inherit;padding:7px 8px;border:1px solid var(--rule);border-radius:3px;background:var(--bg);color:var(--ink)}
textarea{min-height:110px;font-family:ui-monospace,Menlo,monospace;font-size:12px;resize:vertical}
.row{display:grid;gap:12px;grid-template-columns:repeat(auto-fit,minmax(160px,1fr))}
.checks{display:flex;flex-wrap:wrap;gap:16px;margin:4px 0 14px}
.checks label{display:flex;align-items:center;gap:7px;margin:0;font-size:.86rem}
button{font:inherit;font-weight:600;padding:8px 15px;border:1px solid var(--rule);border-radius:3px;background:var(--card);color:var(--ink);cursor:pointer}
button.go{background:var(--accent);border-color:var(--accent);color:#fff}
.bar{display:flex;gap:9px;flex-wrap:wrap;margin-top:12px}
pre{background:var(--card);border:1px solid var(--rule);border-radius:4px;padding:11px;overflow:auto;max-height:420px;font-family:ui-monospace,Menlo,monospace;font-size:11.5px;line-height:1.45;margin:0}
.note{padding:10px 13px;border-radius:3px;border-left:3px solid var(--ok);background:var(--card);margin-bottom:14px}
.note.bad{border-left-color:var(--warn)}
.hint{font-size:.78rem;color:var(--faint);margin-top:6px}
code{font-family:ui-monospace,Menlo,monospace;font-size:.92em;background:var(--bg);padding:1px 5px;border-radius:2px}
</style></head><body><div class="wrap">

<h1>Нода OpenFlux</h1>
<p class="sub">Выходной узел: параметры, состояние и журнал.</p>

{{if .Notice}}<div class="note">{{.Notice}}</div>{{end}}
{{if .Problem}}<div class="note bad">{{.Problem}}</div>{{end}}

<h2>Состояние</h2>
<div class="grid">
  <div class="cell"><div class="k">служба</div><div class="v">{{.Health.Active}}</div></div>
  <div class="cell"><div class="k">каналы</div><div class="v">{{.Health.Links}}</div></div>
  <div class="cell"><div class="k">входы без ответа</div><div class="v">{{.Health.Joins}}</div></div>
  <div class="cell"><div class="k">обрывов за 20 мин</div><div class="v">{{.Health.Closures}}</div></div>
  <div class="cell"><div class="k">сторож сработал</div><div class="v">{{.Health.Watchdog}}</div></div>
  <div class="cell"><div class="k">потерянных пачек</div><div class="v">{{.Health.Batches}}</div></div>
</div>
<p class="hint">запущена: {{.Health.Since}} · сборка <code>{{.Health.BinarySum}}</code>,
  {{.Health.BinarySize}}{{if .Health.SameBuild}} · присланная сборка — та же самая{{end}}</p>

<form method="post" action="/unit?t={{.Token}}">
  <div class="bar">
    <button name="action" value="restart">Перезапустить</button>
    <button name="action" value="start">Запустить</button>
    <button name="action" value="stop">Остановить</button>
  </div>
  <p class="hint">Эта страница и ssh до ноды идут через тот самый туннель, которым она управляет.
    При перезапуске страница пропадёт секунд на тридцать — это нормально, просто обновите её.
    А остановка уберёт туннель совсем: сюда и по ssh вы уже не попадёте, запускать ноду
    придётся вторым путём до машины.</p>
</form>

{{if and .Health.StagedSize (not .Health.SameBuild)}}
<h2>Ждёт установки</h2>
<div class="card">
  <p style="margin:0 0 4px">Присланная сборка <code>{{.Health.StagedSum}}</code>,
    {{.Health.StagedSize}}, {{.Health.StagedTime}}.</p>
  <p class="hint" style="margin-top:0">Работает <code>{{.Health.BinarySum}}</code> — это другая сборка.
    Установка перезапустит ноду, а вместе с ней и туннель: страница вернётся через полминуты.</p>
  <form method="post" action="/unit?t={{.Token}}">
    <div class="bar"><button class="go" name="action" value="install">Установить и перезапустить</button></div>
  </form>
</div>
{{end}}

<h2>Параметры</h2>
<form method="post" action="/apply?t={{.Token}}" class="card">
  <div class="row">
    <label><div class="lab">Транспорт</div>
      <select name="transport" id="transport" onchange="showDocs()">
        {{$t := .Settings.Transport}}
        <option value="mailru" {{if eq $t "mailru"}}selected{{end}}>Mail.ru — документы</option>
        <option value="yandex" {{if eq $t "yandex"}}selected{{end}}>Яндекс.Документы</option>
        <option value="vyandex" {{if eq $t "vyandex"}}selected{{end}}>Яндекс.Волга</option>
        <option value="cupsonline" {{if eq $t "cupsonline"}}selected{{end}}>cups.online — комнаты создаёт нода</option>
      </select></label>
    <label><div class="lab">Режим</div>
      <select name="mode">
        <option value="l4" {{if eq .Settings.Mode "l4"}}selected{{end}}>l4 — терминация TCP</option>
        <option value="l3" {{if eq .Settings.Mode "l3"}}selected{{end}}>l3 — пересылка пакетов</option>
      </select></label>
    <label><div class="lab">Кодек</div>
      <select name="codec">
        <option value="batched" {{if eq .Settings.Codec "batched"}}selected{{end}}>batched</option>
        <option value="legacy" {{if eq .Settings.Codec "legacy"}}selected{{end}}>legacy</option>
      </select></label>
  </div>

  <div class="checks">
    <label><input type="checkbox" name="encrypt" {{if .Settings.Encrypt}}checked{{end}}> шифрование</label>
    <label><input type="checkbox" name="debug" {{if .Settings.Debug}}checked{{end}}> подробный лог</label>
  </div>

  <div id="docs">
    <label><div class="lab">Документы, по одному в строке</div>
      <textarea name="urls" spellcheck="false">{{.Settings.URLs}}</textarea></label>
    <p class="hint">Тот же набор должен стоять у клиента, иначе стороны не найдут друг друга.</p>
  </div>
  <p class="hint" id="nodocs" hidden>cups.online не берёт готовых ссылок: нода сама создаёт комнаты
    при запуске и печатает строку для клиента — она появится ниже, в разделе «Комнаты для клиента».</p>

  <div class="bar"><button class="go" type="submit">Применить и перезапустить</button></div>
</form>

{{if .Health.Rooms}}
<h2>Комнаты для клиента</h2>
<div class="card">
  <p class="hint" style="margin-top:0">Нода создала комнаты сама. Эту строку нужно вставить в поле ссылок
    у клиента — без неё ему не к чему подключаться. При каждом перезапуске она новая.</p>
  <textarea readonly onclick="this.select()" style="min-height:64px">{{.Health.Rooms}}</textarea>
</div>
{{end}}

<h2>Журнал, последние 120 строк</h2>
<pre>{{.Log}}</pre>

<script>
// The documents box belongs to the transports that are driven by documents.
// Switching the picker changes which of the two notes applies, so the page
// does not ask for a list that the chosen transport will never read.
function showDocs(){
  var t = document.getElementById('transport').value;
  var needs = (t !== 'cupsonline');
  document.getElementById('docs').hidden = !needs;
  document.getElementById('nodocs').hidden = needs;
}
showDocs();
</script>
</div></body></html>`
