import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

// The compiled bundle lands in frontend/dist (gitignored) and is served by the
// Go server from disk at runtime — run `npm run build` before `go run`
// (without it the server shows a stub page). base:'./' keeps asset paths
// relative and works under any mount path.
export default defineConfig({
  plugins: [vue()],
  base: './',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
});