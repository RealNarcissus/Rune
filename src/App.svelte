<script lang="ts">
  // LuminaSearch - Main application shell
  // Scaffolding phase: empty window with frosted glass background
</script>

<main class="flex h-screen items-center justify-center">
  <div
    class="w-full max-w-[600px] rounded-xl border border-white/20 bg-white/10 px-6 py-4 shadow-2xl backdrop-blur-xl"
  >
    <h1 class="text-center text-2xl font-light text-white/90">LuminaSearch</h1>
    <p class="mt-2 text-center text-sm text-white/60">
      Press Ctrl+Space to search
    </p>
  </div>
</main>

<style>
  /* Frosted glass: uses Tailwind utility classes above */
  /* Graceful fallback for unsupported compositors: */
  /* If backdrop-filter unsupported, bg-white/10 provides semi-transparent bg */
</style>
