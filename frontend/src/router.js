import { createRouter, createWebHashHistory } from 'vue-router';
import OverviewPage from './pages/OverviewPage.vue';
import NewsPage from './pages/NewsPage.vue';
import ChecksCard from './components/ChecksCard.vue';
import SecretsCard from './components/SecretsCard.vue';

// Hash-based routing: no server-side fallback needed and the Go server keeps
// serving everything from "/" on the single port. Single-card pages route the
// cards directly; only the dashboard route shows a page heading.
export const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/', component: OverviewPage, meta: { title: 'nav.dashboard', subtitle: 'header.subtitle' } },
    { path: '/news', component: NewsPage, meta: { head: false } },
    { path: '/checks', component: ChecksCard, meta: { head: false } },
    { path: '/secrets', component: SecretsCard, meta: { head: false } },
  ],
});