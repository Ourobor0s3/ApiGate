<script setup>
import { useRoute } from 'vue-router';
import { t } from '../i18n';
import NavIcon from './NavIcon.vue';

// Sidebar navigation (GitLab-style groups). Items are hash routes — the app
// splits the former single dashboard into pages: Overview, News, Checks,
// Secrets. The active item is computed from the current route.
defineProps({ groups: { type: Array, required: true } });

const route = useRoute();
</script>

<template>
  <nav class="sidebar">
    <div v-for="g in groups" :key="g.group" class="nav-group">
      <h4>{{ t(g.group) }}</h4>
      <router-link v-for="item in g.items" :key="item.to" :to="item.to" class="nav-item"
        :class="{ active: route.path === item.to }">
        <NavIcon :name="item.icon" />
        <span>{{ t(item.key) }}</span>
      </router-link>
    </div>
  </nav>
</template>