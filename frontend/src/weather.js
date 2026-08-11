import { lang, I18N_RU_WEATHER } from './i18n';

// open-meteo WMO weather codes with day/night icons and English names.
export const WEATHER = {
  0: { d: '☀️', n: '🌙', t: 'Clear sky' }, 1: { d: '🌤️', n: '🌙', t: 'Mainly clear' }, 2: { d: '⛅', n: '⛅', t: 'Partly cloudy' },
  3: { d: '☁️', n: '☁️', t: 'Overcast' }, 45: { d: '🌫️', n: '🌫️', t: 'Fog' }, 48: { d: '🌫️', n: '🌫️', t: 'Rime fog' },
  51: { d: '🌦️', n: '🌦️', t: 'Light drizzle' }, 53: { d: '🌦️', n: '🌦️', t: 'Drizzle' }, 55: { d: '🌧️', n: '🌧️', t: 'Dense drizzle' },
  61: { d: '🌧️', n: '🌧️', t: 'Light rain' }, 63: { d: '🌧️', n: '🌧️', t: 'Rain' }, 65: { d: '🌧️', n: '🌧️', t: 'Heavy rain' },
  71: { d: '🌨️', n: '🌨️', t: 'Light snow' }, 73: { d: '🌨️', n: '🌨️', t: 'Snow' }, 75: { d: '🌨️', n: '🌨️', t: 'Heavy snow' },
  80: { d: '🌦️', n: '🌦️', t: 'Light showers' }, 81: { d: '🌧️', n: '🌧️', t: 'Showers' }, 82: { d: '⛈️', n: '⛈️', t: 'Violent showers' },
  95: { d: '⛈️', n: '⛈️', t: 'Thunderstorm' }, 96: { d: '⛈️', n: '⛈️', t: 'Thunderstorm, hail' }, 99: { d: '⛈️', n: '⛈️', t: 'Heavy hail' },
};

// The dashboard shows a dash for codes it doesn't know (null = no label).
export function condName(code) {
  const w = WEATHER[code];
  if (!w) return null;
  if (lang.value === 'ru') return I18N_RU_WEATHER[code] || w.t;
  return w.t;
}