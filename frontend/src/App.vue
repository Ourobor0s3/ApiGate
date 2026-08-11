<script setup>
import { computed, onMounted, onUnmounted } from 'vue';
import { useRoute } from 'vue-router';
import { t, LOCALE, lang, setLang } from './i18n';
import { locTz } from './dates';
import { status, bannerText, startAutoRefresh, stopAutoRefresh, loading, lastError, dashboard } from './store';
import Sidebar from './components/Sidebar.vue';

const route = useRoute();

const NAV = [
  { group: 'nav.overview', items: [
    { to: '/', key: 'nav.dashboard', icon: 'dashboard' },
    { to: '/news', key: 'nav.news', icon: 'news' },
  ] },
  { group: 'nav.service', items: [
    { to: '/checks', key: 'nav.checks', icon: 'checks' },
  ] },
  { group: 'nav.system', items: [
    { to: '/secrets', key: 'nav.secrets', icon: 'secrets' },
  ] },
];

const badgeClass = computed(() =>
  status.value === 'ok' ? 'badge ok'
    : status.value === 'loading' ? 'badge'
    : status.value === 'key' || status.value === 'error' ? 'badge err'
    : 'badge warn');

const pageTitle = computed(() => t(route.meta.title || 'nav.dashboard'));
const pageSubtitle = computed(() => t(route.meta.subtitle || 'header.subtitle'));

const metaText = computed(() => {
  if (loading.value) return t('meta.initial');
  const time = new Date().toLocaleTimeString(LOCALE.value, { timeZone: locTz, hour: '2-digit', minute: '2-digit' });
  return lastError.value || !dashboard.value ? t('meta.failed', { time }) : t('meta.updated', { time });
});

onMounted(startAutoRefresh);
onUnmounted(stopAutoRefresh);
</script>

<template>
  <header class="topbar">
    <div class="brand">
      <svg width="22" height="22" viewBox="0 0 32 32" aria-hidden="true">
        <defs>
          <linearGradient id="gl-logo" x1="0" y1="0" x2="1" y2="1">
            <stop offset="0" stop-color="#fc6d26"/>
            <stop offset="1" stop-color="#ffb199"/>
          </linearGradient>
        </defs>
        <path d="M16 2 29 9.5v13L16 30 3 22.5v-13L16 2Z" fill="url(#gl-logo)"/>
        <path d="M11.5 21V11l9 7h-6.2l-2.8 3Z" fill="#fff"/>
      </svg>
      <span>ApiGate</span>
      <span class="subtle">{{ t('title.suffix') }}</span>
    </div>
    <div class="topbar-right">
      <span class="badge" :class="badgeClass"><span class="dot"></span>{{ t('badge.' + status) }}</span>
      <div class="lang-switch" role="group" aria-label="Language">
        <button type="button" :class="{ active: lang === 'en' }" @click="setLang('en')">EN</button>
        <button type="button" :class="{ active: lang === 'ru' }" @click="setLang('ru')">RU</button>
      </div>
    </div>
  </header>

  <div class="layout">
    <Sidebar :groups="NAV" />

    <main class="main">
      <div class="content">
        <div v-if="route.meta.head !== false" class="page-head">
          <h2>{{ pageTitle }}</h2>
          <p>{{ pageSubtitle }}</p>
        </div>

        <div class="banner" :hidden="!bannerText">{{ bannerText }}</div>

        <router-view />

        <p class="meta-line">{{ metaText }}</p>
      </div>
    </main>
  </div>
</template>