import { ref, computed } from 'vue';
import { api } from './api';
import { t, lang } from './i18n';
import { setLocTz } from './dates';

// Shared dashboard state: App.vue owns the 60s refresh loop, pages consume the
// reactive values below (dashboard payload, NewsAPI budget, status summary).

export const dashboard = ref(null);
export const quota = ref(null);
export const loading = ref(true);
export const lastError = ref(null);

// The backend serves news per UI language (news = EN sources, newsRu = RU);
// each block falls back to the other when its own store is empty.
export const activeNews = computed(() => {
  if (!dashboard.value) return null;
  return lang.value === 'ru'
    ? (dashboard.value.newsRu || dashboard.value.news)
    : (dashboard.value.news || dashboard.value.newsRu);
});

const missingKeys = computed(() => dashboard.value?.missingSecrets || []);

const hasWeather = computed(() => !!dashboard.value?.weather);
const hasNews = computed(() => !!activeNews.value?.articles?.length);
const hasRates = computed(() => !!dashboard.value?.rates?.rates);

export const status = computed(() => {
  if (loading.value) return 'loading';
  if (!dashboard.value) return 'error';
  if (missingKeys.value.length) return 'key';
  if (dashboard.value.error || !(hasWeather.value && hasNews.value && hasRates.value)) return 'partial';
  return 'ok';
});

export const bannerText = computed(() => {
  if (lastError.value) return lastError.value;
  if (missingKeys.value.length) return t('err.needsKeys', { keys: missingKeys.value.join(', ') });
  if (dashboard.value?.error) return dashboard.value.error;
  if (status.value === 'partial') return t('err.partial');
  return null;
});

let inFlight = false;
async function refresh(silent) {
  if (inFlight) return;
  inFlight = true;
  if (!silent) loading.value = true;
  lastError.value = null;
  try {
    const [d, m] = await Promise.all([api.dashboard(), api.newsQuota()]);
    dashboard.value = d;
    quota.value = m;
    setLocTz(d.weather?.timezone);
  } catch (err) {
    lastError.value = t('err.load', { msg: err.message });
  } finally {
    loading.value = false;
    inFlight = false;
  }
}

let timer = null;
export function startAutoRefresh() {
  refresh(false);
  // Silent auto-update: cards refresh in place every minute; hidden tabs skip.
  timer = setInterval(() => { if (!document.hidden) refresh(true); }, 60000);
}
export function stopAutoRefresh() {
  if (timer) { clearInterval(timer); timer = null; }
}