<script setup>
import { activeNews, quota } from '../store';
import NewsCard from '../components/NewsCard.vue';
</script>

<template>
  <NewsCard :news="activeNews" :quota="quota" />
</template>