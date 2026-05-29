<script lang="ts">
  import { onMount } from "svelte";

  interface Result {
    path: string;
    filename: string;
    is_dir: boolean;
    depth: number;
    tier: number;
  }

  // Svelte 5 Reactive state using runes
  let searchQuery = $state("");
  let results = $state<Result[]>([]);
  let selectedIndex = $state(0);
  
  let indexingActive = $state(false);
  let fileCount = $state(0);
  let watchLimitReached = $state(false);
  let dbSizeBytes = $state(0);

  let currentGenerationID = 0;
  let debounceTimeout: any;
  let abortController: AbortController | null = null;
  let inputElement: HTMLInputElement | null = $state(null);

  // Format bytes helper
  function formatBytes(bytes: number): string {
    if (bytes === 0) return "0 B";
    const k = 1024;
    const sizes = ["B", "KB", "MB", "GB"];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
  }

  // Hide the search window
  function hideWindow() {
    const getCurrentWindow = (window as any).__TAURI__?.window?.getCurrentWindow;
    if (getCurrentWindow) {
      getCurrentWindow().hide();
    } else {
      console.log("Mock hide window (outside Tauri)");
    }
  }

  // Dispatch API search query
  function dispatchSearch() {
    const query = searchQuery.trim();
    if (!query) {
      results = [];
      selectedIndex = 0;
      return;
    }

    currentGenerationID++;
    const gen = currentGenerationID;

    // Abort outstanding requests to keep input ultra-responsive
    if (abortController) {
      abortController.abort();
    }
    abortController = new AbortController();

    fetch(`http://127.0.0.1:10382/search?q=${encodeURIComponent(query)}&gen=${gen}&limit=50`, {
      signal: abortController.signal
    })
      .then(res => res.json())
      .then(data => {
        // Only accept the latest query response
        if (Number(data.gen) >= currentGenerationID) {
          results = data.results || [];
          selectedIndex = 0;
        }
      })
      .catch(err => {
        if (err.name !== "AbortError") {
          console.error("Search query dispatch failed:", err);
        }
      });
  }

  // Handle input changes with 150ms debouncing and direct target reading
  function handleInput(e: Event) {
    searchQuery = (e.target as HTMLInputElement).value;
    if (searchQuery.trim() === "") {
      results = [];
      selectedIndex = 0;
      clearTimeout(debounceTimeout);
      return;
    }
    clearTimeout(debounceTimeout);
    debounceTimeout = setTimeout(() => {
      dispatchSearch();
    }, 150);
  }

  // Fetch status statistics from daemon
  function updateStatus() {
    fetch("http://127.0.0.1:10382/status")
      .then(res => res.json())
      .then(data => {
        const wasIndexing = indexingActive;
        indexingActive = data.indexing_active;
        fileCount = data.file_count;
        watchLimitReached = data.watch_limit_reached;
        dbSizeBytes = data.db_size_bytes;

        // If indexing just finished, refresh results list
        if (wasIndexing && !indexingActive && searchQuery.trim() !== "") {
          dispatchSearch();
        }
      })
      .catch(() => {
        // Daemon might not be running yet or starting up
      });
  }

  // Mount listeners and start status polling
  onMount(() => {
    updateStatus();
    const interval = setInterval(updateStatus, 1500);

    // Focus input on startup
    if (inputElement) {
      inputElement.focus();
    }

    return () => {
      clearInterval(interval);
    };
  });

  // Handle keyboard shortcuts
  function handleKeyDown(e: KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      if (results.length > 0) {
        selectedIndex = (selectedIndex + 1) % results.length;
        // Scroll selected item into view
        const el = document.getElementById(`res-${selectedIndex}`);
        if (el) el.scrollIntoView({ block: "nearest" });
      }
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      if (results.length > 0) {
        selectedIndex = (selectedIndex - 1 + results.length) % results.length;
        const el = document.getElementById(`res-${selectedIndex}`);
        if (el) el.scrollIntoView({ block: "nearest" });
      }
    } else if (e.key === "Enter") {
      e.preventDefault();
      if (results.length > 0 && selectedIndex >= 0 && selectedIndex < results.length) {
        openFile(results[selectedIndex].path);
      }
    } else if (e.key === "Escape") {
      e.preventDefault();
      hideWindow();
    }
  }

  // Launch file natively through Tauri shell
  function openFile(path: string) {
    const invoke = (window as any).__TAURI__?.core?.invoke;
    if (invoke) {
      invoke("open_file", { path })
        .then(() => {
          hideWindow();
        })
        .catch((err: any) => {
          console.error("Failed to open file natively:", err);
        });
    } else {
      console.log("Mock open file (outside Tauri):", path);
    }
  }

  // Helper to resolve rank tier badges
  function getTierName(tier: number): string {
    switch (tier) {
      case 1: return "Exact";
      case 2: return "Prefix";
      case 3: return "Boundary";
      case 4: return "Substring";
      default: return "";
    }
  }
</script>

<main class="w-[600px] h-screen flex flex-col p-2 select-none" onkeydown={handleKeyDown}>
  <div class="flex flex-col w-full rounded-2xl border border-white/10 bg-black/60 shadow-[0_24px_50px_rgba(0,0,0,0.6)] backdrop-blur-[24px] overflow-hidden transition-all duration-300
    {searchQuery.trim() === '' ? 'h-[50px] shadow-[0_12px_30px_rgba(0,0,0,0.5)]' : 'h-[430px]'}"
  >
    <!-- Search Bar Input (Preserves DOM focus constantly) -->
    <div class="flex items-center px-4 py-3 bg-white/5 {searchQuery.trim() !== '' ? 'border-b border-white/5' : ''}">
      <!-- Search Icon -->
      <svg class="w-5 h-5 text-white/40 mr-3 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg">
        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2.5" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"></path>
      </svg>
      
      <input
        bind:this={inputElement}
        bind:value={searchQuery}
        oninput={handleInput}
        type="text"
        placeholder="Type to search files and folders instantly..."
        class="w-full bg-transparent text-lg font-light text-white outline-none placeholder-white/30"
        autofocus
      />
      
      <!-- Pulsating dot indicator -->
      {#if indexingActive}
        <span class="flex h-2 w-2 relative flex-shrink-0 ml-2">
          <span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-cyan-400 opacity-75"></span>
          <span class="relative inline-flex rounded-full h-2 w-2 bg-cyan-500"></span>
        </span>
      {/if}
    </div>

    <!-- Panel Content (Visible only when query is active) -->
    {#if searchQuery.trim() !== ""}
      <!-- Results Panel -->
      {#if results.length > 0}
        <div class="flex-grow overflow-y-auto custom-scrollbar p-2 space-y-1">
          {#each results as item, index}
            <div
              id="res-{index}"
              class="flex items-center justify-between px-3 py-2.5 rounded-xl cursor-pointer transition-all duration-150 border border-transparent
                {index === selectedIndex 
                  ? 'bg-white/10 border-white/15 shadow-[inset_0_1px_1px_rgba(255,255,255,0.1),0_4px_12px_rgba(0,0,0,0.1)]' 
                  : 'hover:bg-white/5'}"
              onclick={() => openFile(item.path)}
              onmouseenter={() => selectedIndex = index}
            >
              <div class="flex items-center space-x-3 overflow-hidden">
                <!-- Icon based on directory or file -->
                {#if item.is_dir}
                  <!-- Folder Icon -->
                  <svg class="w-5 h-5 text-amber-400/80 flex-shrink-0" fill="currentColor" viewBox="0 0 20 20" xmlns="http://www.w3.org/2000/svg">
                    <path d="M2 6a2 2 0 012-2h5l2 2h5a2 2 0 012 2v6a2 2 0 01-2 2H4a2 2 0 01-2-2V6z"></path>
                  </svg>
                {:else}
                  <!-- File Icon -->
                  <svg class="w-5 h-5 text-sky-400/80 flex-shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg">
                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 21h10a2 2 0 002-2V9.414a1 1 0 00-.293-.707l-5.414-5.414A1 1 0 0012.586 3H7a2 2 0 00-2 2v14a2 2 0 002 2z"></path>
                  </svg>
                {/if}

                <!-- Details -->
                <div class="flex flex-col truncate">
                  <span class="text-sm font-medium text-white/95 truncate">
                    {item.filename}
                  </span>
                  <span class="text-xs text-white/40 truncate font-light mt-0.5">
                    {item.path}
                  </span>
                </div>
              </div>

              <!-- Rank/Tier Badge -->
              <div class="flex items-center space-x-2 flex-shrink-0 ml-3">
                <span class="text-[10px] tracking-wider uppercase px-2 py-0.5 rounded-md font-semibold text-white/50 border border-white/5 bg-white/5">
                  {getTierName(item.tier)}
                </span>
              </div>
            </div>
          {/each}
        </div>
      {:else}
        <!-- No Results State -->
        <div class="flex-grow flex flex-col items-center justify-center text-white/40 p-8">
          <svg class="w-12 h-12 text-white/10 mb-3" fill="none" stroke="currentColor" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1.5" d="M9.172 16.172a4 4 0 015.656 0M9 10h.01M15 10h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"></path>
          </svg>
          <span class="text-sm font-light">No matches found for "{searchQuery}"</span>
        </div>
      {/if}

      <!-- Elegant Footer Status Bar -->
      <div class="flex items-center justify-between px-4 py-2 border-t border-white/5 bg-black/40 text-[11px] text-white/40 font-light flex-shrink-0">
        <div class="flex items-center space-x-2">
          {#if indexingActive}
            <span class="text-cyan-400 font-normal">Indexing filesystem...</span>
          {:else}
            <span>Active & Ready</span>
          {/if}
          <span>•</span>
          <span>{fileCount.toLocaleString()} items indexed</span>
        </div>
        
        <div class="flex items-center space-x-2">
          {#if watchLimitReached}
            <span class="text-amber-400 font-normal flex items-center">
              <svg class="w-3 h-3 mr-1" fill="currentColor" viewBox="0 0 20 20" xmlns="http://www.w3.org/2000/svg">
                <path fill-rule="evenodd" d="M8.257 3.099c.765-1.36 2.722-1.36 3.486 0l5.58 9.92c.75 1.334-.213 2.98-1.742 2.98H4.42c-1.53 0-2.493-1.646-1.743-2.98l5.58-9.92zM11 13a1 1 0 11-2 0 1 1 0 012 0zm-1-8a1 1 0 00-1 1v3a1 1 0 002 0V6a1 1 0 00-1-1z" clip-rule="evenodd"></path>
              </svg>
              Limit Reached
            </span>
            <span>•</span>
          {/if}
          <span>DB: {formatBytes(dbSizeBytes)}</span>
        </div>
      </div>
    {/if}
  </div>
</main>
