// ---------------------------------------------------------------------------
// ApiGate dashboard frontend.
// All user-facing strings come from the I18N tables in i18n.js; the language
// (en/ru) is chosen in the header switch and persisted in localStorage. News
// headlines are served per language by the backend: /dashboard returns both
// "news" (English sources) and "newsRu" (lenta/rbc/rt) blocks and this file
// renders the one matching the UI language. Every timestamp is rendered in the
// weather location's IANA zone (locTz, from open-meteo's timezone=auto field);
// the weather "Updated" time is that zone's wall clock and is rendered as-is.
// ---------------------------------------------------------------------------

const badge = document.getElementById('badge');
const metaEl = document.getElementById('meta');
const errBanner = document.getElementById('errBanner');
const errText = document.getElementById('errText');
const weatherBody = document.getElementById('weatherBody');
const weatherTitle = document.getElementById('weatherTitle');
const newsBody = document.getElementById('newsBody');
const newsCount = document.getElementById('newsCount');
const newsPrev = document.getElementById('newsPrev');
const newsNext = document.getElementById('newsNext');
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
const mNewsQuota = document.getElementById('mNewsQuota');
const quotaBar = document.getElementById('quotaBar');
const mNewsQuotaHint = document.getElementById('mNewsQuotaHint');

// --- i18n -------------------------------------------------------------------
// String tables live in i18n.js (window.I18N / window.I18N_RU_WEATHER): all
// user-facing copy is data, not code, and the language switch only flips which
// table t() reads. News headlines are served per UI language by the backend
// (data.news for EN, data.newsRu for RU), not translated client-side.

const I18N = window.I18N || { en: {}, ru: {} };
const RU_WEATHER = window.I18N_RU_WEATHER || {};

let lang = 'en';
try {
  const saved = localStorage.getItem('apigate.lang');
  if (saved === 'ru' || saved === 'en') lang = saved;
  else if (navigator.language && navigator.language.toLowerCase().startsWith('ru')) lang = 'ru';
} catch { /* localStorage unavailable — keep default */ }

let LOCALE = lang === 'ru' ? 'ru-RU' : 'en-US';

function t(key, vars) {
  let s = (I18N[lang] && I18N[lang][key]) || I18N.en[key] || key;
  if (vars) for (const k in vars) s = s.split('{' + k + '}').join(vars[k]);
  return s;
}

function applyStatic() {
  document.documentElement.lang = lang;
  document.title = 'ApiGate ' + t('title.suffix');
  // Only translate leaf nodes: textContent on a node with element children
  // (e.g. the Check now button with its spinner) would wipe them.
  document.querySelectorAll('[data-i18n]').forEach(el => {
    if (el.id !== 'weatherTitle' && el.childElementCount === 0) el.textContent = t(el.dataset.i18n);
  });
  document.querySelectorAll('[data-i18n-ph]').forEach(el => {
    el.setAttribute('placeholder', t(el.dataset.i18nPh));
  });
  document.querySelectorAll('.lang-btn').forEach(b => {
    const active = b.dataset.lang === lang;
    b.classList.toggle('active', active);
    b.setAttribute('aria-pressed', String(active));
  });
  metaEl.textContent = t('meta.initial');
}

document.querySelectorAll('.lang-btn').forEach(b => {
  b.addEventListener('click', () => {
    if (b.dataset.lang === lang) return;
    lang = b.dataset.lang;
    LOCALE = lang === 'ru' ? 'ru-RU' : 'en-US';
    try { localStorage.setItem('apigate.lang', lang); } catch { /* ignore */ }
    applyStatic();
    load(false); // full refresh re-renders every card in the new language
    loadSecrets();
  });
});

// --- helpers ----------------------------------------------------------------

function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function stat(k, v) { return `<div class="stat"><span class="k">${esc(k)}</span><span class="v">${esc(v)}</span></div>`; }
function fmt(v, suffix) { return (v != null && v !== '') ? v + (suffix || '') : '—'; }
function windDir(deg) {
  if (deg == null) return '';
  const dirs = lang === 'ru'
    ? ['С', 'ССВ', 'СВ', 'ВСВ', 'В', 'ВЮВ', 'ЮВ', 'ЮЮВ', 'Ю', 'ЮЮЗ', 'ЮЗ', 'ЗЮЗ', 'З', 'ЗСЗ', 'СЗ', 'ССЗ']
    : ['N', 'NNE', 'NE', 'ENE', 'E', 'ESE', 'SE', 'SSE', 'S', 'SSW', 'SW', 'WSW', 'W', 'WNW', 'NW', 'NNW'];
  return ' ' + dirs[Math.round(deg / 22.5) % 16];
}

// --- dates ------------------------------------------------------------------
// locTz is the IANA zone of the weather location (open-meteo `timezone` field,
// populated with timezone=auto, e.g. "Europe/Moscow"). Timestamps carrying a
// zone designator (news, checks — RFC3339 Z) are rendered in it; falls back to
// the browser's zone until the first dashboard load.
let locTz = undefined;

// parseUTCDate parses upstream timestamps. open-meteo's naive times get 'Z'
// appended (they are UTC); times that already carry a zone pass through.
function parseUTCDate(iso) {
  if (/Z$|[+-]\d{2}:?\d{2}$/i.test(iso)) return new Date(iso);
  return new Date(iso + 'Z');
}

// fmtDateTime renders a UTC-instant timestamp in the location zone.
// withYear adds the year (news headlines); time=false renders the date only.
function fmtDateTime(iso, withYear, time) {
  if (!iso) return '—';
  const d = parseUTCDate(iso);
  if (isNaN(d.getTime())) return '—';
  const dateOpts = { timeZone: locTz, day: '2-digit', month: 'short' };
  if (withYear) dateOpts.year = 'numeric';
  const ds = d.toLocaleDateString(LOCALE, dateOpts);
  if (time === false) return ds;
  const ts = d.toLocaleTimeString(LOCALE, { timeZone: locTz, hour: '2-digit', minute: '2-digit' });
  return ds + ' · ' + ts;
}

// fmtWallClock renders a naive wall-clock time (open-meteo with timezone=auto
// reports local times without a zone suffix). The string is already the
// location's local time, so it is parsed as UTC and rendered in UTC — the
// digits come out exactly as reported, with no double shift.
function fmtWallClock(iso, withDate) {
  if (!iso) return '—';
  const m = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2})/.exec(iso);
  if (!m) return '—';
  const d = new Date(m[1] + 'T' + m[2] + 'Z');
  if (isNaN(d.getTime())) return '—';
  const opts = { timeZone: 'UTC', day: '2-digit', month: 'short' };
  let s = d.toLocaleDateString(LOCALE, opts);
  const ts = d.toLocaleTimeString(LOCALE, { timeZone: 'UTC', hour: '2-digit', minute: '2-digit' });
  return withDate ? s + ' · ' + ts : ts;
}

// fmtDay renders a bare calendar date ("YYYY-MM-DD", e.g. the rates date)
// without timezone shifting — such a value has no time component and must
// never flip a day when the location is west of the browser.
function fmtDay(iso) {
  if (!iso) return '—';
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso);
  if (!m) return fmtDateTime(iso, true, false);
  const d = new Date(Date.UTC(+m[1], +m[2] - 1, +m[3]));
  return d.toLocaleDateString(LOCALE, { timeZone: 'UTC', day: '2-digit', month: 'short', year: 'numeric' });
}

function timeAgo(iso) {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - parseUTCDate(iso).getTime()) / 1000 | 0);
  if (s < 45) return lang === 'ru' ? 'только что' : 'just now';
  if (s < 3600) { const m = Math.round(s / 60); return lang === 'ru' ? m + ' мин назад' : m + 'm ago'; }
  if (s < 86400) { const h = Math.round(s / 3600); return lang === 'ru' ? h + ' ч назад' : h + 'h ago'; }
  const d = Math.round(s / 86400); return lang === 'ru' ? d + ' дн назад' : d + 'd ago';
}

function coord(v, pos, neg) {
  if (v == null) return '—';
  return Math.abs(v).toFixed(2) + '° ' + (v >= 0 ? pos : neg);
}

// --- cards ------------------------------------------------------------------

const WEATHER = {
  0: { d: '☀️', n: '🌙', t: 'Clear sky' }, 1: { d: '🌤️', n: '🌙', t: 'Mainly clear' }, 2: { d: '⛅', n: '⛅', t: 'Partly cloudy' },
  3: { d: '☁️', n: '☁️', t: 'Overcast' }, 45: { d: '🌫️', n: '🌫️', t: 'Fog' }, 48: { d: '🌫️', n: '🌫️', t: 'Rime fog' },
  51: { d: '🌦️', n: '🌦️', t: 'Light drizzle' }, 53: { d: '🌦️', n: '🌦️', t: 'Drizzle' }, 55: { d: '🌧️', n: '🌧️', t: 'Dense drizzle' },
  61: { d: '🌧️', n: '🌧️', t: 'Light rain' }, 63: { d: '🌧️', n: '🌧️', t: 'Rain' }, 65: { d: '🌧️', n: '🌧️', t: 'Heavy rain' },
  71: { d: '🌨️', n: '🌨️', t: 'Light snow' }, 73: { d: '🌨️', n: '🌨️', t: 'Snow' }, 75: { d: '🌨️', n: '🌨️', t: 'Heavy snow' },
  80: { d: '🌦️', n: '🌦️', t: 'Light showers' }, 81: { d: '🌧️', n: '🌧️', t: 'Showers' }, 82: { d: '⛈️', n: '⛈️', t: 'Violent showers' },
  95: { d: '⛈️', n: '⛈️', t: 'Thunderstorm' }, 96: { d: '⛈️', n: '⛈️', t: 'Thunderstorm, hail' }, 99: { d: '⛈️', n: '⛈️', t: 'Heavy hail' },
};

function condName(code) {
  const w = WEATHER[code];
  if (!w) return null;
  if (lang === 'ru') return RU_WEATHER[code] || w.t;
  return w.t;
}

function renderWeather(d) {
  if (!d) return '<div class="empty">' + esc(t('weather.noData')) + '</div>';
  const cw = d.current_weather || {};
  const cond = condName(cw.weathercode);
  const icon = (cond && WEATHER[cw.weathercode][cw.is_day ? 'd' : 'n']) || '🌡️';
  const label = cond || 'Code ' + (cw.weathercode ?? '—');
  const updated = cw.time ? fmtWallClock(cw.time, true) : '—';
  return `<div class="fade">
    <div class="weather-hero">
      <span class="icon">${icon}</span>
      <div>
        <div class="temp">${fmt(cw.temperature, '°')}</div>
        <div class="cond">${esc(label)}</div>
      </div>
    </div>
    <div class="stat-grid">
      ${stat(t('weather.location'), d.latitude != null ? coord(d.latitude, 'N', 'S') + ', ' + coord(d.longitude, 'E', 'W') : '—')}
      ${stat(t('weather.wind'), fmt(cw.windspeed, ' km/h') + windDir(cw.winddirection))}
      ${stat(t('weather.elevation'), fmt(d.elevation, ' m'))}
      ${stat(t('weather.updated'), updated)}
    </div>
  </div>`;
}

function renderRates(d) {
  if (!d || !d.rates || typeof d.rates !== 'object') return '<div class="empty">' + esc(t('weather.noData')) + '</div>';
  const base = (d.base || 'USD').toUpperCase();
  const fmtRate = v => (typeof v === 'number' ? v.toFixed(4) : v);
  const popular = ['USD', 'EUR', 'CAD', 'RUB', 'CNY', 'VND', 'THB', 'KRW', 'JPY'];
  const rows = [];
  const seen = new Set([base]);
  for (const code of [...popular, ...Object.keys(d.rates)]) {
    if (rows.length >= 10 || seen.has(code) || d.rates[code] == null) continue;
    rows.push(`<div class="rate"><span class="code">${esc(code)}</span><span class="val">${fmtRate(d.rates[code])}</span></div>`);
    seen.add(code);
  }
  return `<div class="fade">
    <div class="rates-head"><span class="base-group">${esc(t('rates.base'))} <span class="base">${esc(base)}</span></span><span class="date">${esc(fmtDay(d.date))}</span></div>
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
  if (keyMissing) return '<div class="empty">' + esc(t('news.missingKey')) + '</div>';
  if (!d) return '<div class="empty">' + esc(t('weather.noData')) + '</div>';
  if (Array.isArray(d.articles)) newsArticles = d.articles; else newsArticles = [];
  if (newsArticles.length) {
    const totalPages = Math.max(1, Math.ceil(newsArticles.length / NEWS_PAGE_SIZE));
    if (newsPage > totalPages) newsPage = totalPages;
    const start = (newsPage - 1) * NEWS_PAGE_SIZE;
    const items = newsArticles.slice(start, start + NEWS_PAGE_SIZE).map(a => {
      const published = fmtDateTime(a.publishedAt, true);
      const src = (a.source && a.source.name) || t('news.unknownSource');
      return `<li><a href="${esc(a.url || '#')}" target="_blank" rel="noopener">${esc(a.title || t('news.untitled'))}</a>
        <span class="src"><span class="dot"></span>${esc(src)}${published !== '—' ? ' · ' + esc(published) : ''}</span></li>`;
    }).join('');
    return `<ul class="articles fade">${items}</ul>`;
  }
  if (d.status === 'error') {
    if (d.code === 'dailyQuotaExhausted') return '<div class="empty">' + esc(t('news.quotaExhausted')) + '</div>';
    return '<div class="empty">' + esc(t('news.error', { msg: d.message || d.code || '' })) + '</div>';
  }
  return '<div class="empty">' + esc(t('news.empty')) + '</div>';
}

// renderNewsCards re-renders the list and syncs the count + Prev/Next buttons
// that live in the card header.
function renderNewsCards() {
  newsBody.innerHTML = renderNews();
  const totalPages = Math.max(1, Math.ceil(newsArticles.length / NEWS_PAGE_SIZE));
  if (newsPage > totalPages) newsPage = totalPages;
  if (newsArticles.length) {
    newsCount.textContent = t('news.count', { n: newsArticles.length, p: newsPage, total: totalPages });
    newsPrev.disabled = newsPage <= 1;
    newsNext.disabled = newsPage >= totalPages;
  } else {
    newsCount.textContent = '';
    newsPrev.disabled = true;
    newsNext.disabled = true;
  }
}

newsPrev.addEventListener('click', () => {
  if (newsPage <= 1) return;
  newsPage--;
  renderNewsCards();
});

newsNext.addEventListener('click', () => {
  const totalPages = Math.max(1, Math.ceil(newsArticles.length / NEWS_PAGE_SIZE));
  if (newsPage >= totalPages) return;
  newsPage++;
  renderNewsCards();
});

function skeletonLines(n) {
  return Array.from({ length: n }, (_, i) => `<div class="skeleton" style="height:14px;margin-bottom:${i < n - 1 ? '10px' : 0}"></div>`).join('');
}

// load fetches the dashboard and swaps the cards in place. With silent=true
// (automatic updates) nothing flashes: no skeletons, no badge spinner — the
// content just refreshes every minute. The first call is never silent.
async function load(silent) {
  if (!silent) {
    badge.textContent = t('badge.loading'); badge.className = 'badge loading';
    if (!weatherBody.innerHTML.includes('skeleton')) weatherBody.innerHTML = skeletonLines(5);
    if (!newsBody.innerHTML.includes('skeleton')) newsBody.innerHTML = skeletonLines(6);
    if (!ratesBody.innerHTML.includes('skeleton')) ratesBody.innerHTML = skeletonLines(6);
  }
  errBanner.hidden = true;

  try {
    const res = await fetch('/dashboard');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);

    // Sync every rendered time to the weather location's zone (open-meteo
    // reports it per location with timezone=auto). Falls back to the browser
    // zone when the weather block is unavailable.
    if (data.weather && data.weather.timezone) locTz = data.weather.timezone;

    const missing = data.missingSecrets || [];
    const hasWeather = !!data.weather;
    // The backend serves news per UI language (data.news = EN sources,
    // data.newsRu = lenta/rbc/rt); each falls back to the other block.
    const newsBlock = lang === 'ru' ? (data.newsRu || data.news) : (data.news || data.newsRu);
    const hasNews = !!(newsBlock && Array.isArray(newsBlock.articles) && newsBlock.articles.length);
    const hasRates = !!(data.rates && data.rates.rates && typeof data.rates.rates === 'object');
    const allOk = hasWeather && hasNews && hasRates;

    if (missing.length) {
      badge.textContent = t('badge.key'); badge.className = 'badge err';
      errText.textContent = t('err.needsKeys', { keys: missing.join(', ') });
      errBanner.hidden = false;
    } else if (data.error || !allOk) {
      badge.textContent = t('badge.partial'); badge.className = 'badge err';
      if (data.error) { errText.textContent = data.error; errBanner.hidden = false; }
      else { errText.textContent = t('err.partial'); errBanner.hidden = false; }
    } else {
      badge.textContent = t('badge.ok'); badge.className = 'badge ok';
    }

    weatherBody.innerHTML = renderWeather(data.weather);
    weatherTitle.textContent = t('card.weather') + (data.weatherPlace ? ' — ' + data.weatherPlace : '');
    newsData = newsBlock;
    newsKeyMissing = missing.includes('NEWS_API_KEY');
    renderNewsCards();
    ratesBody.innerHTML = renderRates(data.rates);
    loadChecks();
    loadMetrics();

    metaEl.textContent = t('meta.updated', {
      time: new Date().toLocaleTimeString(LOCALE, { timeZone: locTz, hour: '2-digit', minute: '2-digit' }),
    });
  } catch (err) {
    badge.textContent = t('badge.error'); badge.className = 'badge err';
    errText.textContent = t('err.load', { msg: err.message }); errBanner.hidden = false;
    metaEl.textContent = t('meta.failed', {
      time: new Date().toLocaleTimeString(LOCALE, { timeZone: locTz, hour: '2-digit', minute: '2-digit' }),
    });
  }
}

// Silent auto-update: weather and currency refresh in place every minute, and
// news arrives with the same payload from the Redis store (refilled on its own
// schedule). Hidden tabs skip the fetch; the browser throttles them anyway.
setInterval(() => { if (!document.hidden) load(true); }, 60000);

applyStatic();
load();

// --- secrets ----------------------------------------------------------------

async function loadSecrets() {
  try {
    const res = await fetch('/api/secrets');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const names = data.secrets || [];
    secretList.innerHTML = names.length
      ? names.map(n => `<li><code>${esc(n)}</code> <button class="danger" data-del="${esc(n)}">${esc(t('secrets.delete'))}</button></li>`).join('')
      : '<li class="empty">' + esc(t('secrets.empty')) + '</li>';
  } catch (err) {
    secretList.innerHTML = '<li class="empty">' + esc(t('secrets.loadFailed', { msg: err.message })) + '</li>';
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
    secretList.innerHTML = '<li class="empty">' + esc(t('secrets.saveFailed', { msg: err.message })) + '</li>';
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
    secretList.innerHTML = '<li class="empty">' + esc(t('secrets.deleteFailed', { msg: err.message })) + '</li>';
  }
});

loadSecrets();

// --- checks -----------------------------------------------------------------

function renderCheckStatus(s) {
  if (!s) return '<span class="status"><span class="dot pending"></span>' + esc(t('checks.waiting')) + '</span>';
  const cls = s.ok ? 'ok' : 'err';
  const label = t(s.ok ? 'checks.up' : 'checks.down');
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
        <span class="cname" title="${esc(c.url)}">${esc(c.name)}</span>
        ${renderCheckStatus(c.status)}
        ${c.uptime ? `<span class="uptime" title="${esc(t('checks.uptime'))}">${Math.round(c.uptime)}%</span>` : ''}
        <button class="danger" data-del="${esc(c.url)}">${esc(t('secrets.delete'))}</button>
      </li>`).join('')
    : '<li class="empty">' + esc(t('checks.empty')) + '</li>';
}

async function loadChecks() {
  try {
    const res = await fetch('/api/checks');
    const data = await res.json();
    if (!res.ok) throw new Error('HTTP ' + res.status);
    renderChecks(data);
  } catch (err) {
    checkList.innerHTML = '<li class="empty">' + esc(t('checks.loadFailed', { msg: err.message })) + '</li>';
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
    checkList.innerHTML = '<li class="empty">' + esc(t('checks.addFailed', { msg: err.message })) + '</li>';
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
    checkList.innerHTML = '<li class="empty">' + esc(t('checks.deleteFailed', { msg: err.message })) + '</li>';
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
    checkList.innerHTML = '<li class="empty">' + esc(t('checks.checkFailed', { msg: err.message })) + '</li>';
  } finally {
    checkNow.disabled = false; checkNow.classList.remove('loading');
  }
});

// --- NewsAPI budget ---------------------------------------------------------

const NEWS_PAGE_SIZE_UPSTREAM = 50;

async function loadMetrics() {
  try {
    const res = await fetch('/api/metrics');
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    const used = d.news_quota_used;
    const limit = d.news_quota_limit;
    if (used != null && limit != null) {
      const pct = Math.min(100, Math.round(used / limit * 100));
      mNewsQuota.textContent = used + ' / ' + limit;
      quotaBar.style.width = pct + '%';
      quotaBar.classList.toggle('warn', pct >= 80);
      const left = Math.max(0, limit - used);
      mNewsQuotaHint.textContent = t('quota.left', { n: left }) + ' · ' + t('quota.oneCall');
    } else {
      mNewsQuota.textContent = '–';
      quotaBar.style.width = '0%';
      mNewsQuotaHint.textContent = '';
    }
  } catch {
    // leave the counter showing its last known value
  }
}
