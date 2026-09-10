package proxy

// uiHTML is the loopback-only control page served at /_gateway/ui: current
// route visualization (breaker state, last request, counters) plus manual
// provider/model pinning. No external assets; refreshes via fetch polling.
const uiHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>conduit</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  :root { color-scheme: dark; }
  body { font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
         background: #101418; color: #d8dee6; margin: 0; padding: 24px;
         max-width: 720px; }
  h1 { font-size: 18px; margin: 0 0 4px; letter-spacing: .5px; }
  .sub { color: #7b8794; margin-bottom: 20px; }
  .card { background: #171d24; border: 1px solid #263041; border-radius: 8px;
          padding: 14px 16px; margin-bottom: 14px; }
  .row { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }
  button, select { font: inherit; border-radius: 6px; border: 1px solid #33415a;
                   background: #1c242e; color: #d8dee6; padding: 6px 12px;
                   cursor: pointer; }
  button.active { background: #2563eb; border-color: #2563eb; color: #fff; }
  button:hover { border-color: #4c6ef5; }
  .kv { display: grid; grid-template-columns: 170px 1fr; gap: 2px 12px; }
  .kv b { color: #7b8794; font-weight: normal; }
  .pill { display: inline-block; padding: 1px 8px; border-radius: 999px;
          font-size: 12px; border: 1px solid #33415a; }
  .pill.glm { color: #74c0fc; border-color: #74c0fc; }
  .pill.deepseek { color: #63e6be; border-color: #63e6be; }
  .pill.anthropic { color: #f783ac; border-color: #f783ac; }
  .pill.auto { color: #7b8794; }
  .err { color: #ff8787; min-height: 18px; }
</style>
</head>
<body>
<h1>conduit</h1>
<div class="sub">loopback gateway &middot; 127.0.0.1:8787</div>

<div class="card">
  <div class="row" id="providers" style="margin-bottom:10px"></div>
  <div class="row">
    <select id="model"></select>
    <button onclick="force()">pin model</button>
  </div>
  <div class="err" id="err"></div>
</div>

<div class="card"><div class="kv" id="status"></div></div>

<script>
let route = null;

async function refresh() {
  try {
    const [s, r] = await Promise.all([
      fetch('/_gateway/status').then(x => x.json()),
      fetch('/_gateway/route').then(x => x.json()),
    ]);
    route = r;
    render(s, r);
  } catch (e) { document.getElementById('err').textContent = 'gateway unreachable'; }
}

function pill(p) {
  const cls = ['anthropic','glm','deepseek'].includes(p) ? p : 'auto';
  return '<span class="pill ' + cls + '">' + (p || 'auto') + '</span>';
}

function render(s, r) {
  const fp = r.forced_provider, fm = r.forced_model;
  const box = document.getElementById('providers');
  box.innerHTML = '';
  [['','auto'],['anthropic','Anthropic'],['glm','GLM'],['deepseek','DeepSeek']].forEach(([v, label]) => {
    const b = document.createElement('button');
    b.textContent = label;
    if ((fp || '') === v) b.className = 'active';
    b.onclick = () => pin(v);
    box.appendChild(b);
  });

  const sel = document.getElementById('model');
  const models = (r.available && r.available[fp || 'glm']) || [];
  sel.innerHTML = '';
  if (!fp) { sel.disabled = true; sel.innerHTML = '<option>select a provider first</option>'; }
  else {
    sel.disabled = false;
    models.forEach(m => {
      const o = document.createElement('option');
      o.value = m; o.textContent = m;
      if (m === fm) o.selected = true;
      sel.appendChild(o);
    });
  }

  const lr = s.last_request || {};
  const c = s.counts || {};
  document.getElementById('status').innerHTML =
    '<b>routing</b><span>' + pill(s.routing) +
      (fp ? ' &middot; forced ' + pill(fp) + (fm ? ' ' + fm : '') : '') + '</span>' +
    '<b>last request</b><span>' + (lr.provider ? pill(lr.provider) + ' ' + lr.upstream_model : '—') + '</span>' +
    '<b>requests</b><span>anthropic ' + (c.anthropic_requests||0) +
      ' &middot; glm ' + (c.glm_requests||0) +
      ' &middot; deepseek ' + (c.deepseek_requests||0) +
      ' &middot; failovers ' + (c.failovers||0) + '</span>' +
    '<b>breaker</b><span>' + (s.breaker && Object.keys(s.breaker.entries||{}).length
      ? Object.entries(s.breaker.entries).map(([k,v]) => k + ' ' + v.state).join('<br>')
      : 'closed') + '</span>';
}

async function pin(provider) {
  await post(provider ? {provider, model: ''} : {clear: true});
}
async function force() {
  if (!route || !route.forced_provider) return;
  await post({provider: route.forced_provider, model: document.getElementById('model').value});
}
async function post(body) {
  try {
    const res = await fetch('/_gateway/route', {
      method: 'POST', headers: {'content-type': 'application/json'},
      body: JSON.stringify(body),
    });
    const j = await res.json();
    document.getElementById('err').textContent = res.ok ? '' : (j.error || 'error');
    refresh();
  } catch (e) { document.getElementById('err').textContent = String(e); }
}

refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
`
