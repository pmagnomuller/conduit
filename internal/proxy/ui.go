package proxy

// uiHTML is the loopback-only control page served at /_gateway/ui: routing
// mode switch (auto / pinned / jev), provider+model pinning, live Jev routing
// decisions, breaker state and counters. Single self-contained document, no
// external assets, refreshes via fetch polling every 2s. Codes against the
// /_gateway/route + /_gateway/status contract and degrades when fields are
// missing (no "mode" => auto, no "jev" => disabled).
const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>conduit</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns=%22http://www.w3.org/2000/svg%22 viewBox=%220 0 512 512%22%3E %3Crect x=%2216%22 y=%2216%22 width=%22480%22 height=%22480%22 rx=%2296%22 fill=%22%23101418%22 stroke=%22%23263041%22 stroke-width=%2212%22/%3E %3Cg fill=%22none%22 stroke-linecap=%22round%22%3E %3Cpath d=%22M72 168 H168 Q264 168 296 256%22 stroke=%22%23f783ac%22 stroke-width=%2240%22/%3E %3Cpath d=%22M72 256 H296%22 stroke=%22%2374c0fc%22 stroke-width=%2240%22/%3E %3Cpath d=%22M72 344 H168 Q264 344 296 256%22 stroke=%22%2363e6be%22 stroke-width=%2240%22/%3E %3Cpath d=%22M296 256 H440%22 stroke=%22%238f9aa6%22 stroke-width=%2256%22/%3E %3C/g%3E %3Ccircle cx=%22296%22 cy=%22256%22 r=%2236%22 fill=%22%23101418%22 stroke=%22%23d8dee6%22 stroke-width=%2216%22/%3E %3C/svg%3E">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  :root {
    color-scheme: dark;
    --bg: #101418; --card: #171d24; --card2: #1c242e; --line: #263041; --line2: #33415a;
    --fg: #d8dee6; --muted: #7b8794; --accent: #4c6ef5; --accent-fg: #fff;
    --anthropic: #f783ac; --glm: #74c0fc; --deepseek: #63e6be;
    --ok: #51cf66; --bad: #ff6b6b; --warn: #fcc419; --info: #74c0fc;
    --shadow: 0 1px 0 rgba(255,255,255,.03) inset;
  }
  @media (prefers-color-scheme: light) {
    :root {
      color-scheme: light;
      --bg: #f4f6f9; --card: #ffffff; --card2: #eef1f5; --line: #d9dee6; --line2: #c2cad6;
      --fg: #1c232b; --muted: #5b6776; --accent: #3b5bdb; --accent-fg: #fff;
      --anthropic: #d6336c; --glm: #1c7ed6; --deepseek: #099268;
      --ok: #2f9e44; --bad: #e03131; --warn: #e67700; --info: #1c7ed6;
      --shadow: none;
    }
  }
  * { box-sizing: border-box; }
  html, body { margin: 0; }
  body {
    font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    background: var(--bg); color: var(--fg);
    padding: 20px 16px 40px; max-width: 900px; margin: 0 auto;
    overflow-x: hidden;
  }
  a { color: var(--accent); }
  h1 { font-size: 18px; margin: 0; letter-spacing: .5px; line-height: 1.2; }
  h2 { font-size: 12px; margin: 0 0 10px; color: var(--muted); font-weight: 600;
       text-transform: uppercase; letter-spacing: .08em; }
  .muted { color: var(--muted); }
  .small { font-size: 12px; }
  .card { background: var(--card); border: 1px solid var(--line); border-radius: 10px;
          padding: 14px 16px; margin-bottom: 14px; box-shadow: var(--shadow); }
  .row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  .grow { flex: 1 1 auto; }

  /* header */
  header { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 16px; }
  header svg { width: 40px; height: 40px; flex: 0 0 auto; }
  .title { display: flex; flex-direction: column; min-width: 0; }
  .title .sub { color: var(--muted); font-size: 12px; }
  .hstat { margin-left: auto; display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  @media (max-width: 560px) { .hstat { margin-left: 0; width: 100%; } }

  /* pills & badges */
  .pill { display: inline-flex; align-items: center; gap: 6px; padding: 1px 9px; border-radius: 999px;
          font-size: 12px; border: 1px solid var(--line2); color: var(--fg); white-space: nowrap;
          max-width: 100%; overflow: hidden; text-overflow: ellipsis; }
  .pill.anthropic { color: var(--anthropic); border-color: var(--anthropic); }
  .pill.glm { color: var(--glm); border-color: var(--glm); }
  .pill.deepseek { color: var(--deepseek); border-color: var(--deepseek); }
  .pill.none { color: var(--muted); }
  .pill .dot { width: 7px; height: 7px; border-radius: 50%; background: currentColor; flex: 0 0 auto; }
  .badge { display: inline-block; padding: 1px 9px; border-radius: 6px; font-size: 12px;
           font-weight: 600; letter-spacing: .04em; text-transform: uppercase;
           background: var(--card2); border: 1px solid var(--line2); color: var(--fg); }
  .badge.auto { color: var(--muted); }
  .badge.pinned { color: var(--warn); border-color: var(--warn); }
  .badge.jev { color: var(--accent-fg); background: var(--accent); border-color: var(--accent); }
  .live { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; color: var(--muted); }
  .live .pulse { width: 8px; height: 8px; border-radius: 50%; background: var(--ok);
                 box-shadow: 0 0 0 0 var(--ok); animation: pulse 2s ease-out infinite; }
  .live.stale .pulse { background: var(--muted); animation: none; box-shadow: none; }
  @keyframes pulse { 0% { box-shadow: 0 0 0 0 rgba(81,207,102,.6); } 100% { box-shadow: 0 0 0 8px rgba(81,207,102,0); } }

  /* controls */
  button, select {
    font: inherit; border-radius: 7px; border: 1px solid var(--line2);
    background: var(--card2); color: var(--fg); padding: 6px 12px; cursor: pointer;
    min-height: 32px;
  }
  button:hover:not(:disabled) { border-color: var(--accent); }
  button:disabled { opacity: .45; cursor: not-allowed; }
  button:focus-visible, select:focus-visible, .copy:focus-visible {
    outline: 2px solid var(--accent); outline-offset: 2px;
  }
  button.primary { background: var(--accent); border-color: var(--accent); color: var(--accent-fg); }
  button.prov[aria-pressed="true"] { border-color: currentColor; background: var(--card); font-weight: 600; }
  button.prov.anthropic { color: var(--anthropic); }
  button.prov.glm { color: var(--glm); }
  button.prov.deepseek { color: var(--deepseek); }
  select { min-width: 200px; }

  .seg { display: inline-flex; border: 1px solid var(--line2); border-radius: 8px; overflow: hidden;
         background: var(--card2); }
  .seg button { border: 0; border-radius: 0; background: transparent; padding: 7px 16px; min-width: 88px;
                border-right: 1px solid var(--line2); }
  .seg button:last-child { border-right: 0; }
  .seg button[aria-pressed="true"] { background: var(--accent); color: var(--accent-fg); font-weight: 600; }
  .seg button:focus-visible { outline-offset: -3px; }

  .panel { margin-top: 12px; padding-top: 12px; border-top: 1px dashed var(--line); }
  [hidden] { display: none !important; }

  .err { color: var(--bad); min-height: 18px; font-size: 12px; margin-top: 8px; }
  .banner { display: flex; align-items: center; gap: 10px; background: rgba(255,107,107,.12);
            border: 1px solid var(--bad); color: var(--bad); padding: 8px 12px; border-radius: 8px;
            margin-bottom: 14px; font-weight: 600; }
  .banner .dot { width: 9px; height: 9px; border-radius: 50%; background: var(--bad); }

  /* stat tiles */
  .tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(110px, 1fr)); gap: 8px; margin-top: 10px; }
  .tile { background: var(--card2); border: 1px solid var(--line); border-radius: 8px; padding: 8px 10px; }
  .tile .k { font-size: 11px; color: var(--muted); text-transform: uppercase; letter-spacing: .06em; }
  .tile .v { font-size: 18px; font-weight: 600; line-height: 1.3; }
  .tile.anthropic .v { color: var(--anthropic); }
  .tile.glm .v { color: var(--glm); }
  .tile.deepseek .v { color: var(--deepseek); }

  /* breaker */
  .brk { display: flex; flex-direction: column; gap: 6px; }
  .brk .e { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; font-size: 13px; }
  .state { display: inline-flex; align-items: center; gap: 6px; font-size: 12px; font-weight: 600;
           padding: 1px 8px; border-radius: 6px; border: 1px solid; }
  .state.closed { color: var(--ok); border-color: var(--ok); }
  .state.open { color: var(--bad); border-color: var(--bad); }
  .state.probe { color: var(--warn); border-color: var(--warn); }
  .state .dot { width: 7px; height: 7px; border-radius: 50%; background: currentColor; }
  .key { word-break: break-all; }

  /* tables */
  table { width: 100%; border-collapse: collapse; font-size: 13px; table-layout: fixed; }
  th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid var(--line); vertical-align: top; }
  th { color: var(--muted); font-weight: 600; font-size: 11px; text-transform: uppercase; letter-spacing: .06em; }
  tr:last-child td { border-bottom: 0; }
  td.trunc { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .cat col.c1 { width: 110px; } .cat col.c2 { width: 180px; }
  @media (max-width: 560px) { .cat col.c1 { width: 84px; } .cat col.c2 { width: 130px; } }

  /* decisions feed */
  .feed { display: flex; flex-direction: column; }
  .dec { display: grid; grid-template-columns: 66px minmax(0,1fr) 80px 86px 90px 60px;
         gap: 8px; align-items: center; padding: 7px 4px; border-bottom: 1px solid var(--line); font-size: 13px; }
  .dec.head { color: var(--muted); font-size: 11px; text-transform: uppercase; letter-spacing: .06em; font-weight: 600; }
  .dec:last-child { border-bottom: 0; }
  .dec .t { color: var(--muted); font-variant-numeric: tabular-nums; }
  .dec .rt { display: flex; align-items: center; gap: 6px; min-width: 0; flex-wrap: wrap; }
  .dec .rt .req { color: var(--muted); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; max-width: 100%; }
  .dec .rt .arrow { color: var(--muted); }
  .dec .lat { text-align: right; font-variant-numeric: tabular-nums; color: var(--muted); }
  .src { display: inline-block; padding: 0 7px; border-radius: 5px; font-size: 11px; font-weight: 600;
         letter-spacing: .04em; border: 1px solid; white-space: nowrap; }
  .src.jev { color: var(--info); border-color: var(--info); }
  .src.lease { color: var(--muted); border-color: var(--line2); border-style: dashed; }
  .src.fail_open { color: var(--warn); border-color: var(--warn); background: rgba(252,196,25,.1); }
  .bar { position: relative; height: 8px; background: var(--card2); border: 1px solid var(--line);
         border-radius: 4px; overflow: hidden; }
  .bar .fill { position: absolute; left: 0; top: 0; bottom: 0; background: var(--accent); }
  .bar[data-n="0"] { opacity: .35; }
  .conf { display: flex; align-items: center; gap: 6px; font-size: 11px; color: var(--muted); }
  .conf .bar { flex: 1 1 auto; }
  @media (max-width: 640px) {
    .dec.head { display: none; }
    .dec { grid-template-columns: 1fr 1fr; }
    .dec .rt { grid-column: 1 / -1; }
    .dec .lat { text-align: left; }
  }
  .empty { color: var(--muted); font-style: italic; padding: 8px 4px; }

  /* curl hint */
  pre { margin: 0; padding: 10px 12px; background: var(--card2); border: 1px solid var(--line);
        border-radius: 8px; white-space: pre-wrap; word-break: break-all; font-size: 12px; flex: 1 1 auto; }
  .copy { flex: 0 0 auto; }
  .hintrow { display: flex; gap: 8px; align-items: flex-start; }
  @media (max-width: 560px) { .hintrow { flex-direction: column; } }
  .vh { position: absolute; width: 1px; height: 1px; overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; }
</style>
</head>
<body>

<header>
  <svg viewBox="0 0 512 512" role="img" aria-label="conduit logo">
    <defs>
      <linearGradient id="flow" gradientUnits="userSpaceOnUse" x1="296" y1="256" x2="440" y2="256">
        <stop offset="0" stop-color="#f783ac"/><stop offset="0.5" stop-color="#74c0fc"/><stop offset="1" stop-color="#63e6be"/>
      </linearGradient>
    </defs>
    <rect x="16" y="16" width="480" height="480" rx="96" fill="#101418" stroke="#263041" stroke-width="8"/>
    <g fill="none" stroke-linecap="round">
      <path d="M 72 168 H 168 Q 264 168 296 256" stroke="#f783ac" stroke-width="30"/>
      <path d="M 72 256 H 296" stroke="#74c0fc" stroke-width="30"/>
      <path d="M 72 344 H 168 Q 264 344 296 256" stroke="#63e6be" stroke-width="30"/>
      <path d="M 296 256 H 440" stroke="url(#flow)" stroke-width="48"/>
    </g>
    <circle cx="296" cy="256" r="26" fill="#101418" stroke="#d8dee6" stroke-width="12"/>
  </svg>
  <div class="title">
    <h1>conduit</h1>
    <span class="sub">loopback gateway &middot; <span id="listen">127.0.0.1:8787</span></span>
  </div>
  <div class="hstat">
    <span class="badge auto" id="modeBadge" title="routing mode">auto</span>
    <span class="live stale" id="live"><span class="pulse"></span><span id="nowRouting">no requests yet</span></span>
  </div>
</header>

<div class="banner" id="banner" role="alert" hidden><span class="dot"></span>gateway unreachable &mdash; retrying</div>

<section class="card">
  <h2>Routing mode</h2>
  <div class="row">
    <div class="seg" role="group" aria-label="routing mode" id="seg">
      <button type="button" data-mode="auto" aria-pressed="false" onclick="setMode('auto')">Auto</button>
      <button type="button" data-mode="pinned" aria-pressed="false" onclick="setMode('pinned')">Pinned</button>
      <button type="button" data-mode="jev" aria-pressed="false" onclick="setMode('jev')">Jev</button>
    </div>
    <span class="muted small" id="modeHelp"></span>
  </div>
  <div class="panel" id="pinPanel" hidden>
    <div class="row" id="providers" style="margin-bottom:10px"></div>
    <div class="row">
      <label class="vh" for="model">model</label>
      <select id="model"></select>
      <button type="button" class="primary" onclick="force()">pin</button>
    </div>
  </div>
  <div class="err" id="err" role="status"></div>
</section>

<section class="card" id="jevCard" hidden>
  <h2>Jev routing</h2>
  <details id="catalogBox">
    <summary class="muted small" style="cursor:pointer">catalog (<span id="catCount">0</span> candidates)</summary>
    <table class="cat" style="margin-top:8px">
      <colgroup><col class="c1"><col class="c2"><col></colgroup>
      <thead><tr><th>provider</th><th>model</th><th>profile</th></tr></thead>
      <tbody id="catalog"></tbody>
    </table>
  </details>
  <div style="margin-top:12px">
    <div class="row" style="justify-content:space-between">
      <span class="muted small">decisions &middot; newest first</span>
      <span class="muted small" id="decCount"></span>
    </div>
    <div class="feed" id="feed" aria-live="polite" aria-relevant="additions">
      <div class="dec head"><span>time</span><span>requested &rarr; chosen</span><span>step</span><span>source</span><span>confidence</span><span class="lat">latency</span></div>
      <div id="decRows"></div>
    </div>
  </div>
</section>

<section class="card">
  <h2>Breaker</h2>
  <div class="brk" id="breaker"></div>
  <div class="tiles" id="tiles"></div>
</section>

<section class="card">
  <h2>Manual route</h2>
  <div class="hintrow">
    <pre id="curl"></pre>
    <button type="button" class="copy" onclick="copyCurl()" id="copyBtn">copy</button>
  </div>
</section>

<script>
'use strict';
var route = null;      // last GET /_gateway/route
var gwStatus = null;   // last GET /_gateway/status (not "status": window.status is a string)
var pinIntent = false; // user clicked "Pinned" while no provider is forced yet
var lastFeedKeys = '';
var PROVIDERS = ['anthropic', 'glm', 'deepseek'];

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c];
  });
}
function $(id) { return document.getElementById(id); }

function pill(p, extra) {
  var cls = PROVIDERS.indexOf(p) >= 0 ? p : 'none';
  return '<span class="pill ' + cls + '"><span class="dot"></span>' + esc(p || 'auto') +
    (extra ? ' <span class="muted">' + esc(extra) + '</span>' : '') + '</span>';
}
function modeOf(r) {
  var m = r && r.mode;
  if (m === 'auto' || m === 'pinned' || m === 'jev') return m;
  return (r && r.forced_provider) ? 'pinned' : 'auto';
}
function jevOf(r) {
  var j = (r && r.jev) || {};
  return { enabled: !!j.enabled, catalog: j.catalog || [], recent: j.recent || [] };
}
function fmtTime(iso) {
  var d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  var p = function (n) { return (n < 10 ? '0' : '') + n; };
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}
function fmtUntil(iso) {
  var d = new Date(iso);
  if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '';
  var secs = Math.round((d.getTime() - Date.now()) / 1000);
  var rel = secs > 0 ? 'in ' + (secs >= 90 ? Math.round(secs / 60) + 'm' : secs + 's') : 'expired';
  return 'until ' + fmtTime(iso) + ' (' + rel + ')';
}

async function refresh() {
  try {
    var res = await Promise.all([
      fetch('/_gateway/status').then(function (x) { return x.json(); }),
      fetch('/_gateway/route').then(function (x) { return x.json(); })
    ]);
    gwStatus = res[0] || {}; route = res[1] || {};
    $('banner').hidden = true;
    render(gwStatus, route);
  } catch (e) {
    $('banner').hidden = false;
  }
}

function render(s, r) {
  var mode = modeOf(r);
  var jev = jevOf(r);
  var fp = r.forced_provider || '', fm = r.forced_model || '';
  if (mode !== 'auto') pinIntent = false;

  // header
  $('listen').textContent = s.listen || location.host;
  var mb = $('modeBadge');
  mb.textContent = mode; mb.className = 'badge ' + mode;
  var lr = s.last_request || r.last_request || null;
  var live = $('live');
  if (lr && lr.provider) {
    $('nowRouting').innerHTML = 'now routing ' + pill(lr.provider, lr.upstream_model);
    var age = lr.at ? (Date.now() - new Date(lr.at).getTime()) / 1000 : 0;
    live.classList.toggle('stale', age > 60);
  } else {
    $('nowRouting').textContent = 'no requests yet';
    live.classList.add('stale');
  }

  // segmented control
  var showPin = mode === 'pinned' || pinIntent;
  Array.prototype.forEach.call($('seg').querySelectorAll('button'), function (b) {
    var m = b.dataset.mode;
    b.setAttribute('aria-pressed', String(m === mode || (m === 'pinned' && showPin)));
    if (m === 'jev') {
      b.disabled = !jev.enabled;
      b.title = jev.enabled ? 'ask Jev per request' : 'set TYPESAFE_API_KEY in ~/.config/conduit/.env';
    }
  });
  var fop = s.failover_provider || 'glm';
  $('modeHelp').textContent = {
    auto: 'anthropic; breaker open → ' + fop,
    pinned: fp ? 'all traffic → ' + fp + (fm ? ' / ' + fm : '') : 'pick a provider below',
    jev: 'per request: Jev picks provider+model; breaker still wins; fail-open → auto'
  }[showPin && mode !== 'pinned' ? 'pinned' : mode];

  // pin panel
  $('pinPanel').hidden = !showPin;
  if (showPin) {
    var box = $('providers');
    box.innerHTML = '';
    PROVIDERS.forEach(function (p) {
      var b = document.createElement('button');
      b.type = 'button'; b.className = 'prov ' + p; b.textContent = p;
      b.setAttribute('aria-pressed', String(fp === p));
      b.onclick = function () { pin(p); };
      box.appendChild(b);
    });
    var sel = $('model');
    var models = (r.available && r.available[fp]) || [];
    var want = fp ? models.map(function (m) { return m; }) : [];
    var cur = sel.dataset.sig || '';
    var sig = fp + '|' + want.join(',') + '|' + fm;
    if (cur !== sig) {
      sel.innerHTML = '';
      if (!fp) {
        sel.disabled = true;
        sel.appendChild(new Option('select a provider first', ''));
      } else {
        sel.disabled = false;
        sel.appendChild(new Option('default mapping', ''));
        want.forEach(function (m) { sel.appendChild(new Option(m, m, false, m === fm)); });
        if (fm && want.indexOf(fm) < 0) sel.appendChild(new Option(fm, fm, false, true));
      }
      sel.dataset.sig = sig;
    }
  }

  // jev card
  var jc = $('jevCard');
  jc.hidden = !(mode === 'jev' || jev.enabled);
  if (!jc.hidden) renderJev(jev);

  // breaker
  renderBreaker(s);

  // curl hint
  var host = s.listen || location.host;
  var body = mode === 'jev' ? '{"mode":"jev"}'
    : mode === 'pinned' ? '{"provider":"' + fp + '","model":"' + fm + '"}'
    : '{"clear":true}';
  $('curl').textContent = "curl -s -X POST http://" + host + "/_gateway/route \\\n  -H 'content-type: application/json' \\\n  -d '" + body + "'";
}

function renderJev(jev) {
  var cat = jev.catalog;
  $('catCount').textContent = cat.length;
  var catSig = JSON.stringify(cat);
  var tb = $('catalog');
  if (tb.dataset.sig !== catSig) {
    tb.innerHTML = cat.length ? cat.map(function (c) {
      return '<tr><td>' + pill(c.provider) + '</td><td class="trunc" title="' + esc(c.model) + '">' + esc(c.model) +
        '</td><td class="trunc" title="' + esc(c.profile) + '">' + esc(c.profile) + '</td></tr>';
    }).join('') : '<tr><td colspan="3" class="empty">empty catalog</td></tr>';
    tb.dataset.sig = catSig;
  }

  var recent = jev.recent.slice(0, 30);
  $('decCount').textContent = recent.length ? recent.length + ' shown' : '';
  var rows = $('decRows');
  var keys = recent.map(function (d) { return [d.at, d.requested_model, d.provider, d.model, d.source].join('|'); }).join('\n');
  if (keys === lastFeedKeys) return;
  lastFeedKeys = keys;
  if (!recent.length) {
    rows.innerHTML = '<div class="empty">no decisions yet</div>';
    return;
  }
  rows.innerHTML = recent.map(function (d) {
    var src = d.source || 'jev';
    var srcCls = src === 'lease' ? 'lease' : src === 'fail_open' ? 'fail_open' : 'jev';
    var conf = typeof d.confidence === 'number' ? Math.max(0, Math.min(1, d.confidence)) : 0;
    var pct = Math.round(conf * 100);
    var lat = typeof d.latency_ms === 'number' ? d.latency_ms + 'ms' : '—';
    var reason = d.reason ? ' title="' + esc(d.reason) + '"' : '';
    return '<div class="dec">' +
      '<span class="t">' + fmtTime(d.at) + '</span>' +
      '<span class="rt"><span class="req" title="' + esc(d.requested_model) + '">' + esc(d.requested_model || '?') +
        '</span><span class="arrow">&rarr;</span>' + pill(d.provider, d.model) + '</span>' +
      '<span class="muted">' + esc(d.step || '—') + (d.lease ? ' <span class="small">/' + esc(d.lease) + '</span>' : '') + '</span>' +
      '<span><span class="src ' + srcCls + '"' + reason + '>' + esc(src) + '</span></span>' +
      '<span class="conf"><span class="bar" data-n="' + pct + '"><span class="fill" style="width:' + pct + '%"></span></span>' + (conf ? pct + '%' : '') + '</span>' +
      '<span class="lat">' + esc(lat) + '</span>' +
      '</div>';
  }).join('');
}

function renderBreaker(s) {
  var entries = (s.breaker && s.breaker.entries) || {};
  var keys = Object.keys(entries).sort();
  var brk = $('breaker');
  if (!keys.length) {
    brk.innerHTML = '<div class="e"><span class="state closed"><span class="dot"></span>closed</span><span class="muted">all upstreams healthy</span></div>';
  } else {
    brk.innerHTML = keys.map(function (k) {
      var e = entries[k] || {};
      var st = String(e.state || 'CLOSED').toLowerCase();
      var cls = st === 'open' ? 'open' : st === 'probe' ? 'probe' : 'closed';
      var parts = k.split('|');
      var until = cls === 'open' && e.until ? fmtUntil(e.until) : '';
      return '<div class="e"><span class="state ' + cls + '"><span class="dot"></span>' + esc(st) + '</span>' +
        pill(parts[0], parts[1] && parts[1] !== '*' ? parts[1] : '') +
        (until ? '<span class="muted small">' + esc(until) + '</span>' : '') +
        (e.reason ? '<span class="muted small key">' + esc(e.reason) + '</span>' : '') + '</div>';
    }).join('');
  }
  var lq = s.breaker && s.breaker.last_quota;
  if (lq && lq.at) {
    brk.innerHTML += '<div class="e muted small">last quota event ' + fmtTime(lq.at) + ' &middot; ' + esc(lq.model) + ' &middot; ' + esc(lq.reason) + '</div>';
  }
  var c = s.counts || {};
  var tiles = [
    ['anthropic', 'anthropic', c.anthropic_requests],
    ['glm', 'glm', c.glm_requests],
    ['deepseek', 'deepseek', c.deepseek_requests],
    ['failovers', '', c.failovers],
    ['retries', '', c.transient_retries]
  ];
  $('tiles').innerHTML = tiles.map(function (t) {
    return '<div class="tile ' + t[1] + '"><div class="k">' + t[0] + '</div><div class="v">' + (t[2] || 0) + '</div></div>';
  }).join('');
}

async function setMode(m) {
  if (m === 'auto') { pinIntent = false; await post({clear: true}); return; }
  if (m === 'jev') { pinIntent = false; await post({mode: 'jev'}); return; }
  // pinned: needs a provider; reveal the panel first if none is forced yet
  if (route && route.forced_provider) { await post({mode: 'pinned'}); return; }
  pinIntent = true;
  $('err').textContent = '';
  if (route) render(gwStatus || {}, route);
}
async function pin(provider) {
  await post(provider ? {provider: provider, model: ''} : {clear: true});
}
async function force() {
  if (!route || !route.forced_provider) { $('err').textContent = 'pick a provider first'; return; }
  await post({provider: route.forced_provider, model: $('model').value});
}
async function post(body) {
  try {
    var res = await fetch('/_gateway/route', {
      method: 'POST', headers: {'content-type': 'application/json'},
      body: JSON.stringify(body)
    });
    var j = {};
    try { j = await res.json(); } catch (e) {}
    $('err').textContent = res.ok ? '' : (j.error || ('error ' + res.status));
    refresh();
  } catch (e) { $('err').textContent = String(e); }
}
function copyCurl() {
  var txt = $('curl').textContent;
  var done = function () { $('copyBtn').textContent = 'copied'; setTimeout(function () { $('copyBtn').textContent = 'copy'; }, 1200); };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(txt).then(done, function () { selectCurl(); });
  } else { selectCurl(); }
}
function selectCurl() {
  var r = document.createRange(); r.selectNodeContents($('curl'));
  var sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(r);
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
