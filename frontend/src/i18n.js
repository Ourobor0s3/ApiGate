import { ref, computed } from 'vue';

// ApiGate UI translations. The `lang` ref is reactive: every component that
// uses t() re-renders when the user switches EN/RU (persisted in localStorage).
// The news card itself is multilingual through the backend, not through
// translation: /dashboard carries both "news" (English sources) and "newsRu"
// (Russian sources — lenta, rbc, rt) and the UI renders whichever matches the
// selected language.

const I18N = {
  en: {
    'title.suffix': 'Dashboard',
    'header.subtitle': 'Weather and currency rates — updated automatically.',
    'nav.overview': 'Overview',
    'nav.service': 'Services',
    'nav.system': 'System',
    'nav.dashboard': 'Dashboard',
    'nav.news': 'News',
    'nav.checks': 'Site Checks',
    'nav.secrets': 'API Secrets',
    'badge.loading': 'loading…',
    'badge.ok': 'ok',
    'badge.key': 'needs key',
    'badge.partial': 'partial',
    'badge.error': 'error',
    'meta.initial': 'Loading…',
    'meta.updated': 'Updated {time}',
    'meta.failed': 'Last attempt failed at {time}',
    'err.needsKeys': 'Missing API key(s): {keys} — add them in the API Secrets section below.',
    'err.partial': 'Some services are unavailable right now.',
    'err.load': 'Failed to load: {msg}',
    'card.weather': 'Weather',
    'card.rates': 'Currency Rates',
    'card.news': 'News',
    'card.checks': 'Site Checks',
    'card.secrets': 'API Secrets',
    'weather.noData': 'No data',
    'weather.location': 'Location',
    'weather.wind': 'Wind',
    'weather.kmh': 'km/h',
    'weather.elevation': 'Elevation',
    'weather.meters': 'm',
    'weather.updated': 'Updated',
    'weather.sunrise': 'Sunrise',
    'weather.sunset': 'Sunset',
    'weather.hourly': 'Hourly forecast',
    'weather.precip': 'Precipitation chance',
    'rates.base': 'Base:',
    'news.prev': '← Prev',
    'news.next': 'Next →',
    'news.count': '{n} articles · page {p}/{total}',
    'news.empty': 'No articles yet',
    'news.error': 'Error: {msg}',
    'news.quotaExhausted': 'Daily news budget is exhausted — the stored history keeps serving until midnight.',
    'news.unknownSource': 'unknown source',
    'news.untitled': 'Untitled',
    'checks.hint': 'Monitored URLs are probed every',
    'checks.now': 'Check now',
    'checks.add': 'Add',
    'checks.urlPlaceholder': 'https://example.com',
    'checks.waiting': 'waiting…',
    'checks.up': 'up',
    'checks.down': 'down',
    'checks.uptime': 'Uptime over recent probes',
    'checks.empty': 'No sites monitored — add a URL above.',
    'quota.today': 'Requests today',
    'quota.left': '{n} left · resets at midnight',
    'quota.oneCall': '1 poll = 2 requests (EN + RU), up to 100 articles',
    'secrets.save': 'Save',
    'secrets.set': 'Set',
    'secrets.clear': 'Clear',
    'secrets.hint': 'Stored secrets override the env var of the same name at request time; clearing reverts to the default.',
    'secrets.variable': 'Variable',
    'secrets.value': 'Current',
    'secrets.def': 'Default',
    'secrets.maskedValue': '••••••',
    'secrets.valuePlaceholder': 'Value',
    'secrets.src.saved': 'Redis',
    'secrets.src.env': 'env',
    'secrets.src.default': 'default',
    'secrets.saveFailed': 'Save failed: {msg}',
    'secrets.deleteFailed': 'Delete failed: {msg}',
    'checks.addFailed': 'Add failed: {msg}',
    'checks.deleteFailed': 'Delete failed: {msg}',
    'checks.checkFailed': 'Check failed: {msg}',
    'checks.remove': 'Delete',
  },
  ru: {
    'title.suffix': 'Дашборд',
    'header.subtitle': 'Погода и курсы валют — обновляются автоматически.',
    'nav.overview': 'Обзор',
    'nav.service': 'Сервисы',
    'nav.system': 'Система',
    'nav.dashboard': 'Дашборд',
    'nav.news': 'Новости',
    'nav.checks': 'Проверка сайтов',
    'nav.secrets': 'API Secrets',
    'badge.loading': 'загрузка…',
    'badge.ok': 'ок',
    'badge.key': 'нужен ключ',
    'badge.partial': 'частично',
    'badge.error': 'ошибка',
    'meta.initial': 'Загрузка…',
    'meta.updated': 'Обновлено в {time}',
    'meta.failed': 'Последняя попытка не удалась в {time}',
    'err.needsKeys': 'Отсутствуют ключи: {keys} — добавьте их в разделе API Secrets ниже.',
    'err.partial': 'Некоторые сервисы сейчас недоступны.',
    'err.load': 'Не удалось загрузить: {msg}',
    'card.weather': 'Погода',
    'card.rates': 'Курсы валют',
    'card.news': 'Новости',
    'card.checks': 'Проверка сайтов',
    'card.secrets': 'API Secrets',
    'weather.noData': 'Нет данных',
    'weather.location': 'Местоположение',
    'weather.wind': 'Ветер',
    'weather.kmh': 'км/ч',
    'weather.elevation': 'Высота',
    'weather.meters': 'м',
    'weather.updated': 'Обновлено',
    'weather.sunrise': 'Восход',
    'weather.sunset': 'Закат',
    'weather.hourly': 'Прогноз по часам',
    'weather.precip': 'Шанс осадков',
    'rates.base': 'База:',
    'news.prev': '← Назад',
    'news.next': 'Вперёд →',
    'news.count': '{n} статей · стр. {p}/{total}',
    'news.empty': 'Новостей пока нет',
    'news.error': 'Ошибка: {msg}',
    'news.quotaExhausted': 'Дневной бюджет новостей исчерпан — сохранённая история продолжит показываться до сброса.',
    'news.unknownSource': 'неизвестный источник',
    'news.untitled': 'Без названия',
    'checks.hint': 'Проверка сайтов выполняется каждые',
    'checks.now': 'Проверить',
    'checks.add': 'Добавить',
    'checks.urlPlaceholder': 'https://example.com',
    'checks.waiting': 'ожидание…',
    'checks.up': 'работает',
    'checks.down': 'недоступен',
    'checks.uptime': 'Аптайм за последние проверки',
    'checks.empty': 'Сайты не добавлены — добавьте URL выше.',
    'quota.today': 'Запросов сегодня',
    'quota.left': 'осталось {n} · сброс в полночь',
    'quota.oneCall': '1 опрос = 2 запроса (EN + RU), до 100 статей',
    'secrets.save': 'Сохранить',
    'secrets.set': 'Установить',
    'secrets.clear': 'Сбросить',
    'secrets.hint': 'Сохранённые секреты переопределяют env-переменную с тем же именем при запросе; сброс возвращает значение по умолчанию.',
    'secrets.variable': 'Переменная',
    'secrets.value': 'Текущее значение',
    'secrets.def': 'По умолчанию',
    'secrets.maskedValue': '••••••',
    'secrets.valuePlaceholder': 'Значение',
    'secrets.src.saved': 'Redis',
    'secrets.src.env': 'env',
    'secrets.src.default': 'по умолч.',
    'secrets.saveFailed': 'Не удалось сохранить: {msg}',
    'secrets.deleteFailed': 'Не удалось удалить: {msg}',
    'checks.addFailed': 'Не удалось добавить: {msg}',
    'checks.deleteFailed': 'Не удалось удалить: {msg}',
    'checks.checkFailed': 'Не удалось проверить: {msg}',
    'checks.remove': 'Удалить',
  },
};

// open-meteo condition names for weather codes (EN from WEATHER table in
// weather.js, RU overrides here).
export const I18N_RU_WEATHER = {
  0: 'Ясно', 1: 'Преимущественно ясно', 2: 'Переменная облачность', 3: 'Пасмурно',
  45: 'Туман', 48: 'Изморозь', 51: 'Лёгкая морось', 53: 'Морось', 55: 'Сильная морось',
  61: 'Лёгкий дождь', 63: 'Дождь', 65: 'Сильный дождь',
  71: 'Лёгкий снег', 73: 'Снег', 75: 'Сильный снег',
  80: 'Лёгкие ливни', 81: 'Ливни', 82: 'Сильные ливни',
  95: 'Гроза', 96: 'Гроза, град', 99: 'Град',
};

function storedLang() {
  try {
    const saved = localStorage.getItem('apigate.lang');
    if (saved === 'ru' || saved === 'en') return saved;
    if (navigator.language && navigator.language.toLowerCase().startsWith('ru')) return 'ru';
  } catch { /* localStorage unavailable */ }
  return 'en';
}

export const lang = ref(storedLang());
export const LOCALE = computed(() => (lang.value === 'ru' ? 'ru-RU' : 'en-US'));

export function setLang(next) {
  if (next !== 'en' && next !== 'ru') return;
  lang.value = next;
  document.documentElement.lang = next;
  try { localStorage.setItem('apigate.lang', next); } catch { /* ignore */ }
}

// t(lang-key, {vars}) resolves from the current language table, falling back
// to English, then to the key itself.
export function t(key, vars) {
  let s = (I18N[lang.value] && I18N[lang.value][key]) || I18N.en[key] || key;
  if (vars) for (const k in vars) s = s.split('{' + k + '}').join(vars[k]);
  return s;
}