<script setup>
import { ref, onMounted, onUnmounted } from 'vue';
import { api } from '../api';
import { t } from '../i18n';
import { timeAgo } from '../dates';

const checks = ref(null);
const interval = ref('');
const error = ref('');
const running = ref(false);
const url = ref('');

// refreshTimer backs the post-add status pull: the /api/checks probe runs in
// the background on the server, so the list is re-read until the new target
// reports a real status instead of showing "waiting" forever.
let refreshTimer = null;

function clearRefreshTimer() {
  if (refreshTimer) { clearTimeout(refreshTimer); refreshTimer = null; }
}

async function load() {
  try {
    const data = await api.checks.list();
    checks.value = data.checks || [];
    interval.value = data.interval || '5m';
  } catch (err) {
    error.value = t('err.load', { msg: err.message });
  }
}

function label(status) {
  if (!status) return { cls: 'pending', text: t('checks.waiting') };
  return status.ok
    ? { cls: 'ok', text: t('checks.up') }
    : { cls: 'err', text: t('checks.down') };
}

function statusText(s) {
  if (!s) return '';
  const parts = [];
  if (s.code) parts.push(String(s.code));
  if (s.latencyMs != null) parts.push(s.latencyMs + 'ms');
  if (s.checkedAt) parts.push(timeAgo(s.checkedAt));
  return parts.length ? ' · ' + parts.join(' · ') : '';
}

async function add() {
  const u = url.value.trim();
  if (!u) return;
  try {
    await api.checks.add(u);
    url.value = '';
    error.value = '';
    await load();
    trackNewTarget(u);
  } catch (err) {
    error.value = t('checks.addFailed', { msg: err.message });
  }
}

// trackNewTarget re-polls the list after a new target is added: the first
// load() almost always races the background probe and shows "waiting", so
// keep refreshing a couple of times until the probe result lands (up to ~9s).
function trackNewTarget(u) {
  clearRefreshTimer();
  let attempts = 0;
  const tick = async () => {
    attempts++;
    await load();
    const item = (checks.value || []).find((c) => c.url === u);
    if (item && item.status) return;
    if (attempts < 6) refreshTimer = setTimeout(tick, 1500);
  };
  refreshTimer = setTimeout(tick, 2000);
}

async function remove(u) {
  try {
    await api.checks.remove(u);
    error.value = '';
    await load();
  } catch (err) {
    error.value = t('checks.deleteFailed', { msg: err.message });
  }
}

async function runNow() {
  running.value = true;
  try {
    const data = await api.checks.run();
    checks.value = data.checks || [];
  } catch (err) {
    error.value = t('checks.checkFailed', { msg: err.message });
  } finally {
    running.value = false;
  }
}

onMounted(load);
onUnmounted(clearRefreshTimer);
</script>

<template>
  <section class="card" id="checks">
    <header class="card-head">
      <h3><span class="chip">📡</span>{{ t('card.checks') }}</h3>
      <button type="button" class="btn small" :class="{ loading: running }" :disabled="running" @click="runNow">
        {{ t('checks.now') }}
      </button>
    </header>
    <div class="card-body">
      <p class="checks-hint">{{ t('checks.hint') }} <span class="interval">{{ interval || '5m' }}</span>.</p>
      <form class="form-row" @submit.prevent="add">
        <input v-model="url" type="url" :placeholder="t('checks.urlPlaceholder')" required>
        <button type="submit" class="btn primary">{{ t('checks.add') }}</button>
      </form>
      <ul class="check-list">
        <li v-for="c in checks || []" :key="c.url">
          <span class="cname" :title="c.url">{{ c.name }}</span>
          <span class="status"><span class="dot" :class="label(c.status).cls"></span>{{ label(c.status).text }}<template v-if="c.status">{{ statusText(c.status) }}</template></span>
          <span v-if="c.uptime" class="uptime" :title="t('checks.uptime')">{{ Math.round(c.uptime) }}%</span>
          <button type="button" class="btn danger small" @click="remove(c.url)">{{ t('checks.remove') }}</button>
        </li>
        <li v-if="checks && !checks.length" class="card-empty">{{ t('checks.empty') }}</li>
      </ul>
      <p v-if="error" class="hint" style="color:var(--gl-red)">{{ error }}</p>
    </div>
  </section>
</template>