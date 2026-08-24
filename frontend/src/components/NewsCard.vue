<script setup>
import { ref, computed } from 'vue';
import { t } from '../i18n';
import { fmtDateTime } from '../dates';

const props = defineProps({ news: Object, quota: Object });

const PAGE_SIZE = 8;
const page = ref(1);

// NewsAPI daily budget, shown here because it is a property of the news
// pipeline (the poller and this card share one quota), not of any other card.
const used = computed(() => props.quota?.news_quota_used);
const limit = computed(() => props.quota?.news_quota_limit);
const hasBudget = computed(() => used.value != null && limit.value != null);
const budgetPct = computed(() => (hasBudget.value ? Math.min(100, Math.round(used.value / limit.value * 100)) : 0));
const budgetLeft = computed(() => Math.max(0, limit.value - used.value));

const articles = computed(() => (Array.isArray(props.news?.articles) ? props.news.articles : []));
const totalPages = computed(() => Math.max(1, Math.ceil(articles.value.length / PAGE_SIZE)));
const pageItems = computed(() => {
  const p = Math.min(page.value, totalPages.value);
  return articles.value.slice((p - 1) * PAGE_SIZE, p * PAGE_SIZE);
});

const pageLabel = computed(() => t('news.count', { n: articles.value.length, p: Math.min(page.value, totalPages.value), total: totalPages.value }));

function prev() { if (page.value > 1) page.value--; }
function next() { if (page.value < totalPages.value) page.value++; }

const state = computed(() => {
  if (articles.value.length) return 'articles';
  if (props.news?.status === 'error') {
    if (props.news.code === 'dailyQuotaExhausted') return 'quotaExhausted';
    return 'error';
  }
  return 'empty';
});

const errorMessage = computed(() => t('news.error', { msg: props.news?.message || props.news?.code || '' }));
</script>

<template>
  <section class="card" id="news">
    <header class="card-head">
      <h3><span class="chip">📰</span>{{ t('card.news') }}</h3>
      <div v-if="state === 'articles'" class="news-tools">
        <span class="news-count">{{ pageLabel }}</span>
        <button type="button" class="btn small" :disabled="page <= 1" @click="prev">{{ t('news.prev') }}</button>
        <button type="button" class="btn small" :disabled="page >= totalPages" @click="next">{{ t('news.next') }}</button>
      </div>
    </header>
    <div class="card-body">
      <div v-if="hasBudget" class="quota-bar" :title="t('quota.oneCall')">
        <span>📬 {{ t('quota.today') }} <b>{{ used }} / {{ limit }}</b></span>
        <span class="progress"><span class="progress-fill" :class="{ warn: budgetPct >= 80 }" :style="{ width: budgetPct + '%' }"></span></span>
        <span class="quota-left">{{ t('quota.left', { n: budgetLeft }) }}</span>
      </div>
      <div v-if="state === 'articles'" class="news-list">
        <article v-for="a in pageItems" :key="a.url || a.title" class="ncard">
          <a class="ntitle" :href="a.url || '#'" target="_blank" rel="noopener noreferrer">{{ a.title || t('news.untitled') }}</a>
          <p v-if="a.description" class="ndesc">{{ a.description }}</p>
          <span class="nmeta">
            <span class="dot"></span>{{ (a.source && a.source.name) || t('news.unknownSource') }}
            <template v-if="fmtDateTime(a.publishedAt) !== '—'"> · {{ fmtDateTime(a.publishedAt) }}</template>
          </span>
        </article>
      </div>
      <div v-else class="card-empty">
        <template v-if="state === 'quotaExhausted'">{{ t('news.quotaExhausted') }}</template>
        <template v-else-if="state === 'error'">{{ errorMessage }}</template>
        <template v-else>{{ t('news.empty') }}</template>
      </div>
    </div>
  </section>
</template>