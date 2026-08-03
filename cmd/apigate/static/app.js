const btn = document.getElementById('refresh');
const badge = document.getElementById('badge');
const metaEl = document.getElementById('meta');
const errBanner = document.getElementById('errBanner');
const errText = document.getElementById('errText');
const weatherBody = document.getElementById('weatherBody');
const weatherTitle = document.getElementById('weatherTitle');
const newsBody = document.getElementById('newsBody');
const ratesBody = document.getElementById('ratesBody');
const secretForm = document.getElementById('secretForm');
const secretName = document.getElementById('secretName');
const secretValue = document.getElementById('secretValue');
const secretList = document.getElementById('secretList');
const checkForm = document.getElementById('checkForm');
const checkUrl = document.getElementById('checkUrl');
const checkList = document.getElementById('checkList');
const checkInterval = document.getElementById('checkInterval');
const checkNow = document.getElementById('checkNow');

const WEATHER = {
  0:{d:'☀️','n':'🌙',t:'Clear sky'},1:{d:'🌤️','n':'🌙',t:'Mainly clear'},2:{d:'⛅','n':'⛅',t:'Partly cloudy'},
  3:{d:'☁️','n':'☁️',t:'Overcast'},45:{d:'🌫️','n':'🌫️',t:'Fog'},48:{d:'🌫️','n':'🌫️',t:'Rime fog'},
  51:{d:'🌦️','n':'🌦️',t:'Light drizzle'},53:{d:'🌦️','n':'🌦️',t:'Drizzle'},55:{d:'🌧️','n':'🌧️',t:'Dense drizzle'},
  61:{d:'🌧️','n':'🌧️',t:'Light rain'},63:{d:'🌧️','n':'🌧️',t:'Rain'},65:{d:'🌧️','n':'🌧️',t:'Heavy rain'},
  71:{d:'🌨️','n':'🌨️',t:'Light snow'},73:{d:'🌨️','n':'🌨️',t:'Snow'},75:{d:'🌨️','n':'🌨️',t:'Heavy snow'},
  80:{d:'🌦️','n':'🌦️',t:'Light showers'},81:{d:'🌧️','n':'🌧️',t:'Showers'},82:{d:'⛈️','n':'⛈️',t:'Violent showers'},
  95:{d:'⛈️','n':'⛈️',t:'Thunderstorm'},96:{d:'⛈️','n':'⛈️',t:'Thunderstorm, hail'},99:{d:'⛈️','n':'⛈️',t:'Heavy hail'}
};

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}
function stat(k, v) { return `<div class="stat"><span class="k">${esc(k)}</span><span class="v">${esc(v)}</span></div>`; }
function fmt(v, suffix) { return (v != null && v !== '') ? v + (suffix || '') : '—'; }

function windDir(deg) {
  if (deg == null) return '';
  const dirs = ['N','NNE','NE','ENE','E','ESE','SE','SSE','S','SSW','SW','WSW','W','WNW','NW','NNW'];
  return ' ' + dirs[Math.round(deg / 22.5) % 16];
}
function timeAgo(iso) {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000 | 0);
  if (s < 45) return 'just now';
  if (s < 3600) return Math.round(s / 60) + 'm ago';
  if (s < 86400) return Math.round(s / 3600) + 'h ago';
  return Math.round(s / 86400) + 'd ago';
}
function fmtDate(iso, withTime) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const date = d.toLocaleDateString('en-US', { day: '2-digit', month: 'short', year: withTime ? undefined : 'numeric' });
  if (!withTime) return date;
  const time = d.toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' });
  return date + ' · ' + time;
}
function coord(v, pos, neg) {
  if (v == null) return '—';
  return Math.abs(v).toFixed(2) + '° ' + (v >= 0 ? pos : neg);
}

function renderWeather(d) {
  if (!d) return '<div class="empty">No data</div>';
  const cw = d.current_weather || {};
  const w = WEATHER[cw.weathercode];
  const icon = (w && w[cw.is_day ? 'd' : 'n']) || '🌡️';
  const cond = (w && w.t) || 'Code ' + (cw.weathercode ?? '—');
  return `<div class="fade">
    <div class="weather-hero">
      <span class="icon">${icon}</span>
      <div>
        <div class="temp">${fmt(cw.temperature, '°')}</div>
        <div class="cond">${esc(cond)}</div>
      </div>
    </div>
    <div class="stat-grid">
      ${stat('Location', d.latitude != null ? coord(d.latitude, 'N', 'S') + ', ' + coord(d.longitude, 'E', 'W') : '—')}
      ${stat('Wind', fmt(cw.windspeed, ' km/h') + windDir(cw.winddirection))}
      ${stat('Elevation', fmt(d.elevation, ' m'))}
      ${stat('Updated', fmtDate(cw.time, true))}
    </div>
  </div>`;
}

function renderRates(d) {
  if (!d || !d.rates || typeof d.rates !== 'object') return '<div class="empty">No data</div>';
  const base = (d.base || 'USD').toUpperCase();
  const fmtRate = v => (typeof v === 'number' ? v.toFixed(4) : v);
  const popular = ['USD','EUR','GBP','JPY','CHF','CNY','CAD','AUD','INR','RUB','BRL','KRW','TRY','MXN','SGD','NZD'];
  const rows = [];
  const seen = new Set([base]);
  for (const code of [...popular, ...Object.keys(d.rates)]) {
    if (rows.length >= 10 || seen.has(code) || d.rates[code] == null) continue;
    rows.push(`<div class="rate"><span class="code">${esc(code)}</span><span class="val">${fmtRate(d.rates[code])}</span></div>`);
    seen.add(code);
  }
  return `<div class="fade">
    <div class="rates-head"><span class="base-group">Base: <span class="base">${esc(base)}</span></span><span class="date">${esc(fmtDate(d.date, false))}</span></div>
    <div class="rates-grid">${rows.join('')}</div>
  </div>`;
}

let newsData = null;
let newsKeyMissing = false;
let newsArticles = [];
let newsPage = 1;
const NEWS_PAGE_SIZE = 10;

function renderNews() {
  const d = newsData;
  const keyMissing = newsKeyMissing;
  if (keyMissing) return '<div class="empty">Missing API key — add <code>NEWS_API_KEY</code> in the 🔑 API Secrets section below.</div>';
  if (!d) return '<div class="empty">No data</div>';
  if (Array.isArray(d.articles)) newsArticles = d.articles; else newsArticles = [];
  if (newsArticles.length) {
    const totalPages = Math.max(1, Math.ceil(newsArticles.length / NEWS_PAGE_SIZE));
    if (newsPage > totalPages) newsPage = totalPages;
    const start = (newsPage - 1) * NEWS_PAGE_SIZE;
    const items = newsArticles.slice(start, start + NEWS_PAGE_SIZE).map(a => {
      const ago = timeAgo(a.publishedAt);
      return `<li><a href="${esc(a.url || '#')}" target="_blank" rel="noopener">${esc(a.title || 'Untitled')}</a>
        <span class="src"><span class="dot"></span>${esc((a.source && a.source.name) || 'unknown source')}${ago ? ' · ' + ago : ''}</span></li>`;
    }).join('');
    const pager = newsArticles.length > NEWS_PAGE_SIZE
      ? `<div class="pager">
          <span class="pager-info">${newsArticles.length} articles · page ${newsPage}/${totalPages}</span>
          <span class="pager-btns">
            <button type="button" class="neutral small" id="newsPrev" ${newsPage <= 1 ? 'disabled' : ''}>← Prev</button>
            <button type="button" class="neutral small" id="newsNext" ${newsPage >= totalPages ? 'disabled' : ''}>Next →</button>
          </span>
        </div>`
      : '';
    return `<ul class="articles fade">${items}</ul>${pager}`;
  }
  if (d.status === 'error') return '<div class="empty">Error: ' + esc(d.message || d.code || 'unknown') + '</div>';
  return '<div class="empty">No articles</div>';
}

newsBody.addEventListener('click', (e) => {
  const prev = e.target.closest('#newsPrev');
  const next = e.target.closest('#newsNext');
  if (!prev && !next) return;
  if (prev) newsPage = Math.max(1, newsPage - 1);
  if (next) newsPage = Math.min(Math.max(1, Math.ceil(newsArticles.length / NEWS_PAGE_SIZE)), newsPage + 1);
  newsBody.innerHTML = renderNews();
});

function skeletonLines(n) {
  return Array.from({length: n}, (_, i) => `<div class="skeleton" style="height:14px;margin-bottom:${i < n-1 ? '10px' : 0}"></div>`).join('');
}

async function load() {
  btn.classList.add('loading'); btn.disabled = true;
  badge.textContent = 'loading…'; badge.className = 'badge loading';
  errBanner.hidden = true;

  if (!weatherBody.innerHTML.includes('skeleton')) weatherBody.innerHTML = skeletonLines(5);
  if (!newsBody.innerHTML.includes('skeleton')) newsBody.innerHTML = skeletonLines(6);
  if (!ratesBody.innerHTML.includes('skeleton')) ratesBody.innerHTML = skeletonLines(6);

  try {
    const res = await fetch('/dashboard');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);

    const missing = data.missingSecrets || [];
    const hasWeather = !!data.weather;
    const hasNews = !!(data.news && Array.isArray(data.news.articles) && data.news.articles.length);
    const hasRates = !!(data.rates && data.rates.rates && typeof data.rates.rates === 'object');
    const allOk = hasWeather && hasNews && hasRates;

    if (missing.length) {
      badge.textContent = 'needs key'; badge.className = 'badge err';
      errText.textContent = 'Missing API key(s): ' + missing.join(', ') + ' — add them in the 🔑 API Secrets section below.';
      errBanner.hidden = false;
    } else if (data.error || !allOk) {
      badge.textContent = 'partial'; badge.className = 'badge err';
      if (data.error) { errText.textContent = data.error; errBanner.hidden = false; }
    } else {
      badge.textContent = 'ok'; badge.className = 'badge ok';
    }

    weatherBody.innerHTML = renderWeather(data.weather);
    weatherTitle.textContent = 'Weather' + (data.weatherPlace ? ' — ' + data.weatherPlace : '');
    newsData = data.news;
    newsKeyMissing = missing.includes('NEWS_API_KEY');
    newsBody.innerHTML = renderNews();
    ratesBody.innerHTML = renderRates(data.rates);
    loadChecks();

    metaEl.textContent = 'Updated ' + new Date().toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' }) +
      ' — weather:' + (hasWeather ? 'ok' : '✗') + ' · news:' + (hasNews ? 'ok' : '✗') + ' · rates:' + (hasRates ? 'ok' : '✗');
  } catch (err) {
    badge.textContent = 'error'; badge.className = 'badge err';
    errText.textContent = 'Failed to load: ' + err.message; errBanner.hidden = false;
    metaEl.textContent = 'Last attempt failed at ' + new Date().toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' });
  } finally {
    btn.classList.remove('loading'); btn.disabled = false;
  }
}

btn.addEventListener('click', load);

const autoRef = document.getElementById('autoRef');
let autoTimer = null;
function scheduleAuto() {
  clearInterval(autoTimer);
  if (autoRef.checked) autoTimer = setInterval(() => { if (!document.hidden) load(); }, 60000);
}
autoRef.addEventListener('change', () => {
  if (autoRef.checked) load();
  scheduleAuto();
});

load();
scheduleAuto();

async function loadSecrets() {
  try {
    const res = await fetch('/api/secrets');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const names = data.secrets || [];
    secretList.innerHTML = names.length
      ? names.map(n => `<li><code>${esc(n)}</code> <button class="danger" data-del="${esc(n)}">Delete</button></li>`).join('')
      : '<li class="empty">No secrets stored</li>';
  } catch (err) {
    secretList.innerHTML = '<li class="empty">Failed to load: ' + esc(err.message) + '</li>';
  }
}

secretForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const res = await fetch('/api/secrets', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: secretName.value.trim(), value: secretValue.value }),
    });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    secretName.value = ''; secretValue.value = '';
    loadSecrets();
    load();
  } catch (err) {
    secretList.innerHTML = '<li class="empty">Save failed: ' + esc(err.message) + '</li>';
  }
});

secretList.addEventListener('click', async (e) => {
  const del = e.target.closest('button[data-del]');
  if (!del) return;
  try {
    const res = await fetch('/api/secrets?name=' + encodeURIComponent(del.dataset.del), { method: 'DELETE' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadSecrets();
    load();
  } catch (err) {
    secretList.innerHTML = '<li class="empty">Delete failed: ' + esc(err.message) + '</li>';
  }
});

loadSecrets();

function renderCheckStatus(s) {
  if (!s) return '<span class="status"><span class="dot pending"></span>waiting…</span>';
  const cls = s.ok ? 'ok' : 'err';
  const label = s.ok ? 'up' : 'down';
  const code = s.code ? ' · ' + s.code : '';
  const ms = s.latencyMs != null ? ' · ' + s.latencyMs + 'ms' : '';
  const ago = s.checkedAt ? ' · ' + timeAgo(s.checkedAt) : '';
  return `<span class="status"><span class="dot ${cls}"></span>${label}${code}${ms}${ago}</span>`;
}

function renderChecks(data) {
  const items = data.checks || [];
  checkInterval.textContent = data.interval || '5m';
  checkList.innerHTML = items.length
    ? items.map(c => `<li>
        <span class="cname">${esc(c.name)}<small title="${esc(c.url)}">${esc(c.url)}</small></span>
        ${renderCheckStatus(c.status)}
        <button class="danger" data-del="${esc(c.url)}">Delete</button>
      </li>`).join('')
    : '<li class="empty">No sites monitored — add a URL above</li>';
}

async function loadChecks() {
  try {
    const res = await fetch('/api/checks');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);
    renderChecks(data);
  } catch (err) {
    checkList.innerHTML = '<li class="empty">Failed to load: ' + esc(err.message) + '</li>';
  }
}

checkForm.addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    const res = await fetch('/api/checks', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url: checkUrl.value.trim() }),
    });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    checkUrl.value = '';
    loadChecks();
  } catch (err) {
    checkList.innerHTML = '<li class="empty">Add failed: ' + esc(err.message) + '</li>';
  }
});

checkList.addEventListener('click', async (e) => {
  const del = e.target.closest('button[data-del]');
  if (!del) return;
  try {
    const res = await fetch('/api/checks?url=' + encodeURIComponent(del.dataset.del), { method: 'DELETE' });
    if (!res.ok) throw new Error('HTTP ' + res.status);
    loadChecks();
  } catch (err) {
    checkList.innerHTML = '<li class="empty">Delete failed: ' + esc(err.message) + '</li>';
  }
});

checkNow.addEventListener('click', async () => {
  checkNow.disabled = true; checkNow.classList.add('loading');
  try {
    const res = await fetch('/api/checks/run', { method: 'POST' });
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);
    renderChecks(data);
  } catch (err) {
    checkList.innerHTML = '<li class="empty">Check failed: ' + esc(err.message) + '</li>';
  } finally {
    checkNow.disabled = false; checkNow.classList.remove('loading');
  }
});

loadChecks();
