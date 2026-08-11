<script setup>
import { ref, onMounted } from 'vue';
import { api } from '../api';
import { t } from '../i18n';

// API Secrets page: a table of every variable that can be changed at runtime.
// A Redis secret overrides the env var and the built-in default of the same
// name; "Set" stores one, "Clear" removes it so the fallback applies again.
const settings = ref([]);
const error = ref('');
const editing = ref(null);
const draft = ref('');

async function load() {
  try {
    const data = await api.secrets.list();
    settings.value = data.settings || [];
    error.value = '';
  } catch (err) {
    error.value = t('err.load', { msg: err.message });
  }
}

function beginSet(s) {
  draft.value = s.value || '';
  editing.value = s.name;
}

async function save() {
  const name = editing.value;
  if (!name || !draft.value.trim()) { editing.value = null; return; }
  try {
    await api.secrets.save(name, draft.value.trim());
    editing.value = null;
    await load();
  } catch (err) {
    error.value = t('secrets.saveFailed', { msg: err.message });
  }
}

async function clear(name) {
  try {
    await api.secrets.remove(name);
    await load();
  } catch (err) {
    error.value = t('secrets.deleteFailed', { msg: err.message });
  }
}

function displayValue(s) {
  if (s.stored) return s.masked ? t('secrets.maskedValue') : s.value;
  if (!s.masked && s.env) return s.env;
  return '—';
}

function srcLabel(s) {
  if (s.stored) return t('secrets.src.saved');
  if (s.env) return t('secrets.src.env');
  return t('secrets.src.default');
}
function srcClass(s) {
  if (s.stored) return 'saved';
  if (s.env) return 'env';
  return '';
}

onMounted(load);
</script>

<template>
  <section class="card" id="secrets">
    <header class="card-head">
      <h3><span class="chip">🔑</span>{{ t('card.secrets') }}</h3>
    </header>
    <div class="card-body">
      <p class="settings-hint">{{ t('secrets.hint') }}</p>
      <table class="settings-table">
        <thead>
          <tr>
            <th scope="col">{{ t('secrets.variable') }}</th>
            <th scope="col">{{ t('secrets.value') }}</th>
            <th scope="col">{{ t('secrets.def') }}</th>
            <th scope="col" class="act-col"></th>
          </tr>
        </thead>
        <tbody v-if="settings.length">
          <tr v-for="s in settings" :key="s.name" :class="{ editing: editing === s.name }">
            <td class="c-name"><code>{{ s.name }}</code></td>
            <td class="c-value">
              <template v-if="editing !== s.name">
                <span class="cur" :class="{ masked: s.stored && s.masked }">{{ displayValue(s) }}</span>
                <span class="src-chip" :class="srcClass(s)">{{ srcLabel(s) }}</span>
              </template>
              <input v-else :type="s.masked ? 'password' : 'text'" v-model="draft" :placeholder="t('secrets.valuePlaceholder')" @keyup.enter="save">
            </td>
            <td class="c-def"><code>{{ s.default || '—' }}</code></td>
            <td class="c-actions">
              <button v-if="editing === s.name" type="button" class="btn small primary" @click="save">{{ t('secrets.save') }}</button>
              <button v-else type="button" class="btn small" @click="beginSet(s)">{{ t('secrets.set') }}</button>
              <button v-if="s.stored" type="button" class="btn small danger" @click="clear(s.name)">{{ t('secrets.clear') }}</button>
            </td>
          </tr>
        </tbody>
      </table>
      <p v-if="error" class="hint" style="color:var(--gl-red)">{{ error }}</p>
    </div>
  </section>
</template>