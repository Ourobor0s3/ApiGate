<script setup>
import { computed } from 'vue';
import { t } from '../i18n';
import { currencyName } from '../currency';
import { fmtDay } from '../dates';

const props = defineProps({ rates: Object, loading: Boolean });

const POPULAR = ['USD', 'EUR', 'CAD', 'RUB', 'CNY', 'VND', 'THB', 'KRW', 'JPY'];

const base = computed(() => (props.rates?.base || 'USD').toUpperCase());

// With an amount prefix (e.g. MAIN_CURRENCY=100RUB) the backend scales rates
// per that amount; show it so "0.92" reads as "for 100 RUB".
const amount = computed(() => Number(props.rates?.amount) || 1);
const baseLabel = computed(() => (amount.value > 1 ? `${amount.value} ${base.value}` : base.value));

// Up to 10 rates: popular codes first, then the rest of the payload.
const rows = computed(() => {
  const r = props.rates?.rates;
  if (!r || typeof r !== 'object') return [];
  const out = [];
  const seen = new Set([base.value]);
  for (const code of [...POPULAR, ...Object.keys(r)]) {
    if (out.length >= 10 || seen.has(code) || r[code] == null) continue;
    out.push({ code, value: typeof r[code] === 'number' ? r[code].toFixed(4) : r[code] });
    seen.add(code);
  }
  return out;
});
</script>

<template>
  <section class="card" id="rates">
    <header class="card-head">
      <h3><span class="chip">💱</span>{{ t('card.rates') }}</h3>
    </header>
    <div class="card-body">
      <div v-if="loading && !rates" class="skeleton" style="height:160px"></div>
      <div v-else-if="!rows.length" class="card-empty">{{ t('weather.noData') }}</div>
      <div v-else>
        <div class="rates-head">
          <span>{{ t('rates.base') }} <span class="base">{{ baseLabel }}</span></span>
          <span>{{ fmtDay(rates.date) }}</span>
        </div>
        <div class="rates-grid">
          <div v-for="r in rows" :key="r.code" class="rate">
            <span class="code" tabindex="0">{{ r.code }}<span class="tip">{{ currencyName(r.code) }}</span></span>
            <span class="val">{{ r.value }}</span>
          </div>
        </div>
      </div>
    </div>
  </section>
</template>