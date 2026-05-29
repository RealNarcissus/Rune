# ᚱ Rune

Rune is a high-performance, keyboard-first desktop search utility for Linux. Designed to be lightweight, instant, and visually premium, it brings the speed of terminal-based search tools like `fzf` to a beautiful, translucent, glassmorphic desktop interface. 

Rune achieves **sub-5ms search latency** and crawls over **1,000,000 files in under 10 seconds**, all while keeping its persistent memory footprint **under 360MB RAM**.

---

## 🏗️ Architecture Overview

Rune uses a decoupled, performance-optimized multi-process architecture:

```
                  ┌──────────────────────────────────────────┐
                  │          Tauri v2 Desktop Shell          │
                  │  (translucent, borderless GTK window)   │
                  └──────┬────────────────────────────▲──────┘
                         │                            │
             [Spawns / Manages Sidecar]      [IPC / HTTP API]
                         │                            │
                         ▼                            │
  ┌──────────────────────────────────────────┐        │
  │        Go Backend Indexer Daemon         │────────┘
  │                 (runed)                  │
  └──────┬────────────────────────────┬──────┘
         │                            │
[Crawls Tree]                  [Listens to Events]
         │                            │
         ▼                            ▼
┌──────────────────┐        ┌──────────────────┐
│ Bounded Crawler  │        │ inotify Monitor  │
└────────┬─────────┘        └────────┬─────────┘
         │                           │
         └─────────────┬─────────────┘
                       │ [Writes Serialized Mutations]
                       ▼
            ┌─────────────────────┐
            │   LMDB Database     │
            │ (Memory-Mapped I/O) │
            └─────────────────────┘
```

* **Frontend**: Built with **Svelte 5** (utilizing reactive runes) and **TailwindCSS 4**, rendering a premium, frosted-glass overlay.
* **Desktop Shell**: Engineered with **Tauri v2 (Rust)**, handling global hotkey bindings (`Ctrl+Space`), focus-loss (blur) auto-hiding, transparent window compositing, and native OS file launching (`xdg-open`).
* **Search Engine Daemon**: A persistent **Go backend** sidecar (`runed`) that runs isolated from the UI, executing lightning-fast concurrent directory walks, real-time filesystem tracking, query ranking, and database persistence.

---

## ⚡ Key Engineering Highlights

### 1. Ultra-Compact LMDB Posting Lists & Path Interning
To scale smoothly up to 1,000,000 files within a tight **500MB RAM budget**, Rune implements a custom indexing architecture over the **Lightning Memory-Mapped Database (LMDB)**:
* **Path Interning**: Eliminates redundant string allocations by mapping absolute paths to unique `uint32` IDs.
* **Binary posting lists**: Replaces slow, high-overhead JSON path lists with sorted, compact binary arrays of 4-byte path IDs. This allows the search matching phase to run at raw memory speeds without heap allocations or JSON parsing.
* **Trie Memory Conservation**: Refactors the in-memory prefix trie to store path IDs *only* at terminal leaf nodes, eliminating string duplication down the ancestor tree.

### 2. Zero-Allocation, Bounded Top-N Heap Search
Rune prioritizes visual search-as-you-type feedback by optimizing the hot query path:
* **Bounded Min-Heap Ranking**: Keeps only the top 50 highest-ranked matching results using a bounded min-heap, avoiding the CPU and memory overhead of collecting and sorting thousands of intermediate matches.
* **Short Query Short-Circuit**: Queries shorter than 3 characters run prefix-only checks against the memory-mapped trie, completely skipping expensive trigram evaluations to guarantee sub-5ms UI responsiveness.
* **NFC Normalization**: All index insertions and searches undergo Unicode Normalization Form C (NFC) and lowercase mapping, ensuring correct matches for accented or regional characters natively.

### 3. Safe, Race-Free Filesystem Watcher
Traditional directory-indexing pipelines suffer from consistency gaps if files are created/modified while the initial crawler is running. Rune solves this with a **queue-and-replay startup sequence**:
1. **Start Watcher First**: Spawns an `fsnotify` (inotify-based) recursive monitor across the directory tree.
2. **Queue Live Mutations**: Any filesystem changes captured during the initial index crawl are temporarily stored in a thread-safe, in-memory queue.
3. **Run Bounded Crawl**: The multi-threaded directory crawler performs its initial walk, skipping hidden directories (like `.cargo`, `.var`, `.local`, `.cache`) to save watched handles and prevent inotify exhaustion.
4. **Replay & Steady-State**: Once the crawler finishes, all queued live mutations are replayed atomically, transitioning the system into real-time steady-state monitoring with zero consistency gaps.

---

## 🛠️ Technology Stack

* **UI Layer**: Svelte 5, TypeScript, Vite 6, TailwindCSS 4
* **System Integration**: Tauri v2, Rust
* **Query Engine**: Go 1.21+, LMDB (via `go-lmdb`), fsnotify

---

## 🚀 Local Development Setup

### Prerequisites
* Go 1.21 or later
* Node.js 18+ and `pnpm`
* Rust (Cargo) and system development libraries (GTK3, WebKit2GTK for Tauri on Linux)

### 1. Clone & Install Frontend Dependencies
```bash
git clone https://github.com/<your-username>/Rune.git
cd Rune
pnpm install
```

### 2. Build the Go Backend Daemon
```bash
cd backend
go build -o bin/runed cmd/runed/main.go
# Copy compiled sidecar binary to Tauri targets folder
cp bin/runed ../src-tauri/binaries/runed-x86_64-unknown-linux-gnu
cd ..
```

### 3. Launch Tauri in Dev Mode
```bash
pnpm tauri dev
```
* **Search Window Toggle**: Press `Ctrl+Space` globally.
* **Keyboard Navigation**: Use `ArrowUp` / `ArrowDown` to cycle search results, `Enter` to open files natively, and `Escape` to close the overlay.

---

## ⚖️ License

Distributed under the Permissive MIT License. See [LICENSE](file:///home/charleton/Desktop/agentProjects/Droid/Rune/LICENSE) for more details.
