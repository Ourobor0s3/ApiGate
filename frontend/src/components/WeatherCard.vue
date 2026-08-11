<script setup>
import { computed } from 'vue';
import { t } from '../i18n';
import { fmtWallClock, coord, windDir } from '../dates';
import { condName, WEATHER } from '../weather';

const props = defineProps({ weather: Object, place: String, loading: Boolean });

const cond = computed(() => props.weather ? condName(props.weather.current_weather?.weathercode) : null);
const icon = computed(() => {
  const cw = props.weather?.current_weather;
  if (cond.value && cw) return WEATHER[cw.weathercode][cw.is_day ? 'd' : 'n'];
  return '🌡️';
});
const label = computed(() => cond.value || (props.weather?.current_weather ? 'Code ' + (props.weather.current_weather.weathercode ?? '—') : null));
const cw = computed(() => props.weather?.current_weather || {});
const title = computed(() => t('card.weather') + (props.place ? ' — ' + props.place : ''));

const location = computed(() => {
  const w = props.weather;
  if (!w || w.latitude == null) return null;
  return coord(w.latitude, 'N', 'S') + ', ' + coord(w.longitude, 'E', 'W');
});
const wind = computed(() => cw.value.windspeed != null ? `${cw.value.windspeed} ${t('weather.kmh')}${windDir(cw.value.winddirection)}` : null);
const elevation = computed(() => props.weather?.elevation != null ? `${props.weather.elevation} ${t('weather.meters')}` : null);
const updated = computed(() => fmtWallClock(cw.value.time, true));

// Rolling hourly forecast: from the current hour on, up to HOURS entries.
// Two forecast days keep the strip full across midnight; wall-clock times
// (timezone=auto) drive the day/night icon from the raw hour.
const HOURS = 12;
const DAY_START = 7;
const DAY_END = 19;

const hourly = computed(() => {
  const h = props.weather?.hourly;
  if (!h || !Array.isArray(h.time) || !Array.isArray(h.temperature_2m)) return null;
  let start = 0;
  const now = cw.value.time;
  if (now) {
    const i = h.time.findIndex((tt) => tt >= now);
    if (i > 0) start = i;
  }
  const out = [];
  for (let i = start; i < h.time.length && out.length < HOURS; i++) {
    const m = /T(\d{2}):/.exec(h.time[i]);
    if (!m) continue;
    const hour = Number(m[1]);
    const code = h.weathercode?.[i];
    const icon = code != null && WEATHER[code]
      ? WEATHER[code][hour >= DAY_START && hour < DAY_END ? 'd' : 'n']
      : null;
    // Precipitation chance is always surfaced when present (0% included —
    // "no rain expected" is information), only null/undefined (some upstreams
    // omit the variable) render nothing.
    const precip = h.precipitation_probability?.[i];
    out.push({
      hour,
      icon,
      temp: h.temperature_2m?.[i],
      precip: typeof precip === 'number' ? precip : null,
    });
  }
  return out.length ? out : null;
});

const sun = computed(() => {
  const d = props.weather?.daily;
  if (!d || !Array.isArray(d.sunrise) || !Array.isArray(d.sunset)) return null;
  return { rise: fmtWallClock(d.sunrise[0]), set: fmtWallClock(d.sunset[0]) };
});
</script>

<template>
  <section class="card" id="weather">
    <header class="card-head">
      <h3><span class="chip">🌤️</span>{{ title }}</h3>
    </header>
    <div class="card-body">
      <div v-if="loading && !weather" class="skeleton" style="height:96px"></div>
      <div v-else-if="!weather" class="card-empty">{{ t('weather.noData') }}</div>
      <div v-else>
        <div class="weather-hero">
          <span class="icon">{{ icon }}</span>
          <div>
            <div class="temp">{{ cw.temperature }}°</div>
            <div class="cond">{{ label }}</div>
          </div>
        </div>
        <table class="stat-table">
          <tbody>
            <tr>
              <th scope="row">{{ t('weather.location') }}</th>
              <td>{{ location }}</td>
            </tr>
            <tr>
              <th scope="row">{{ t('weather.wind') }}</th>
              <td>{{ wind || '—' }}</td>
            </tr>
            <tr v-if="sun">
              <th scope="row">{{ t('weather.sunrise') }}</th>
              <td>{{ sun.rise }} 🌅</td>
            </tr>
            <tr v-if="sun">
              <th scope="row">{{ t('weather.sunset') }}</th>
              <td>{{ sun.set }} 🌇</td>
            </tr>
            <tr>
              <th scope="row">{{ t('weather.elevation') }}</th>
              <td>{{ elevation || '—' }}</td>
            </tr>
            <tr>
              <th scope="row">{{ t('weather.updated') }}</th>
              <td>{{ updated }}</td>
            </tr>
          </tbody>
        </table>
        <div v-if="hourly" class="hourly-strip" role="list" :aria-label="t('weather.hourly')">
          <div v-for="h in hourly" :key="h.hour" class="hrow" role="listitem">
            <span class="hh">{{ h.hour }}:00</span>
            <span class="hicon">{{ h.icon || '—' }}</span>
            <span class="htemp">{{ h.temp != null ? h.temp + '°' : '—' }}</span>
            <span v-if="h.precip != null" class="hprecip" :title="t('weather.precip')">💧{{ h.precip }}%</span>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>