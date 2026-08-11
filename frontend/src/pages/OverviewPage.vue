<script setup>
import { dashboard, loading } from '../store';
import WeatherCard from '../components/WeatherCard.vue';
import RatesCard from '../components/RatesCard.vue';
</script>

<template>
  <div class="cards">
    <WeatherCard :weather="dashboard?.weather" :place="dashboard?.weatherPlace" :loading="loading" />
    <RatesCard :rates="dashboard?.rates" :loading="loading" />
  </div>
</template>