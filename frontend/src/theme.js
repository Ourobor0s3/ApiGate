import { ref } from 'vue';

// Color theme: "light" | "dark". The choice persists in localStorage and
// defaults to the OS preference on first visit. The attribute is applied to
// <html> at module load (before Vue mounts), so the palette is already right
// when the first pixels paint.

const KEY = 'apigate.theme';

function initial() {
  try {
    const saved = localStorage.getItem(KEY);
    if (saved === 'light' || saved === 'dark') return saved;
    if (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches) return 'dark';
  } catch { /* localStorage / matchMedia unavailable */ }
  return 'light';
}

export const theme = ref(initial());

function apply(next) {
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem(KEY, next); } catch { /* ignore */ }
}

apply(theme.value);

export function toggleTheme() {
  theme.value = theme.value === 'dark' ? 'light' : 'dark';
  apply(theme.value);
}
