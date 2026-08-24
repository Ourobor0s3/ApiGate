import { lang, LOCALE } from './i18n';

// Rendering helpers for upstream timestamps. locTz is the weather location's
// IANA zone (open-meteo `timezone` field, e.g. "Europe/Moscow"); timestamps
// with a zone designator (news, checks — RFC3339) are rendered in it. Falls
// back to the browser's zone until the first dashboard load.

export let locTz = undefined;
export function setLocTz(tz) {
  if (tz) locTz = tz;
}

// parseUTCDate parses upstream timestamps. open-meteo's naive times get 'Z'
// appended (they are UTC); times that already carry a zone pass through.
function parseUTCDate(iso) {
  if (/Z$|[+-]\d{2}:?\d{2}$/i.test(iso)) return new Date(iso);
  return new Date(iso + 'Z');
}

// fmtDateTime renders a UTC-instant timestamp in the location zone.
export function fmtDateTime(iso, withYear, time = true) {
  if (!iso) return '—';
  const d = parseUTCDate(iso);
  if (isNaN(d.getTime())) return '—';
  const dateOpts = { timeZone: locTz, day: '2-digit', month: 'short' };
  if (withYear) dateOpts.year = 'numeric';
  const ds = d.toLocaleDateString(LOCALE.value, dateOpts);
  if (!time) return ds;
  const ts = d.toLocaleTimeString(LOCALE.value, { timeZone: locTz, hour: '2-digit', minute: '2-digit' });
  return ds + ' · ' + ts;
}

// fmtWallClock renders a naive wall-clock time (open-meteo with timezone=auto
// reports local times without a zone suffix). Parsed as UTC and rendered in
// UTC so the digits come out exactly as reported, with no double shift.
export function fmtWallClock(iso, withDate) {
  if (!iso) return '—';
  const m = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2})/.exec(iso);
  if (!m) return '—';
  const d = new Date(m[1] + 'T' + m[2] + 'Z');
  if (isNaN(d.getTime())) return '—';
  const opts = { timeZone: 'UTC', day: '2-digit', month: 'short' };
  const s = d.toLocaleDateString(LOCALE.value, opts);
  const ts = d.toLocaleTimeString(LOCALE.value, { timeZone: 'UTC', hour: '2-digit', minute: '2-digit' });
  return withDate ? s + ' · ' + ts : ts;
}

// fmtDay renders a bare calendar date ("YYYY-MM-DD", e.g. the rates date)
// without timezone shifting — it has no time component and must never flip a
// day when the location is west of the browser.
export function fmtDay(iso) {
  if (!iso) return '—';
  const m = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso);
  if (!m) return fmtDateTime(iso, true, false);
  const d = new Date(Date.UTC(+m[1], +m[2] - 1, +m[3]));
  return d.toLocaleDateString(LOCALE.value, { timeZone: 'UTC', day: '2-digit', month: 'short', year: 'numeric' });
}

export function timeAgo(iso) {
  if (!iso) return '';
  const s = Math.max(0, (Date.now() - parseUTCDate(iso).getTime()) / 1000 | 0);
  if (s < 45) return lang.value === 'ru' ? 'только что' : 'just now';
  if (s < 3600) { const m = Math.round(s / 60); return lang.value === 'ru' ? m + ' мин назад' : m + 'm ago'; }
  if (s < 86400) { const h = Math.round(s / 3600); return lang.value === 'ru' ? h + ' ч назад' : h + 'h ago'; }
  const d = Math.round(s / 86400);
  return lang.value === 'ru' ? d + ' дн назад' : d + 'd ago';
}

export function coord(v, pos, neg) {
  if (v == null) return '—';
  return Math.abs(v).toFixed(2) + '°' + (v >= 0 ? pos : neg);
}

export function windDir(deg) {
  if (deg == null) return '';
  const dirs = lang.value === 'ru'
    ? ['С', 'ССВ', 'СВ', 'ВСВ', 'В', 'ВЮВ', 'ЮВ', 'ЮЮВ', 'Ю', 'ЮЮЗ', 'ЮЗ', 'ЗЮЗ', 'З', 'ЗСЗ', 'СЗ', 'ССЗ']
    : ['N', 'NNE', 'NE', 'ENE', 'E', 'ESE', 'SE', 'SSE', 'S', 'SSW', 'SW', 'WSW', 'W', 'WNW', 'NW', 'NNW'];
  return ' ' + dirs[Math.round(deg / 22.5) % 16];
}