# THINKING.md — GStack Phase 1: Linux Filesystem Constraints Analysis

## LuminaSearch: Design Rationale for User-Space Walking

### Problem Statement

LuminaSearch needs to discover and track every file on the user's filesystem with
near-zero latency. The naive approach — a full recursive directory walk on every
query — is far too slow for a search-as-you-type experience. This document
captures the Phase 1 design analysis of why we chose user-space directory walking
backed by kernel monitoring (inotify) over direct block-device parsing.

## Filesystem Landscape on Linux

Modern Linux desktops predominantly use one of two filesystems:

### ext4 (Fourth Extended Filesystem)

- **On-disk layout:** Superblock → block group descriptors → block bitmap →
  inode bitmap → inode table → data blocks. Extents replace the old indirect
  block mapping for large files.
- **Directory entries:** Stored as linked lists of `ext4_dir_entry_2`
  structures within directory data blocks. Hash-tree directories (HTree) index
  large directories via a constant-depth B-tree keyed on filename hash.
- **Key obstacle:** Directories using HTree (default since ext3 with
  `dir_index`) require traversing the hash tree to resolve names. The hash is
  the `TEA` (Tiny Encryption Algorithm) hash of the filename — irreversible.
  You cannot walk the tree without knowing the filename you're looking for. A
  full enumeration requires reading every directory block linearly, which is
  still an O(n) scan per directory.

### XFS

- **On-disk layout:** Allocation groups (AGs) with independent free-space
  btrees. Directories use B+tree structures with variable-length records.
- **Directory entries:** Stored in B+trees keyed on a hash of the filename.
  Short-form directories (< ~156 bytes of entries) are stored inline in the
  inode; block directories use B+trees in data extents.
- **Key obstacle:** The B+tree for directories is keyed on filename hash
  (CRC32c-based). Like ext4 HTree, you cannot enumerate entries in creation
  order or path order without understanding the hash function specifics and
  potentially colliding with different XFS versions/proc-dir-version settings.

## Why Direct Block Parsing Was Rejected

### 1. Hash-Ordered Directories Are Not Enumerable by Path

Both ext4 (HTree) and XFS (B+tree) store directory entries sorted by filename
**hash**, not by filename or inode order. Without the filename, you cannot
traverse the tree to find all entries. Full enumeration requires linear
scanning of every directory block — which is equivalent in cost to `readdir()`
but with vastly more complexity.

### 2. Kernel Buffer Cache Invalidation Races

Reading a block device directly (`/dev/sda1` or `/dev/nvme0n1p2`) bypasses the
VFS layer. You get a snapshot of the raw bytes at the moment of read, but:

- The kernel may have dirty buffers not yet flushed to disk.
- The block you just read may be stale by the time you parse it.
- The filesystem driver may be in the middle of a journal transaction.

To get a consistent view, you would need to freeze the filesystem
(`FIFREEZE`/`FITHAW` ioctls, requiring `CAP_SYS_ADMIN`) or use
filesystem-specific journal replay logic. Neither is acceptable for a
user-facing desktop application.

### 3. Requires Root or CAP_SYS_ADMIN

Reading raw block devices requires either:
- `root` (or `CAP_SYS_ADMIN` + read access to the device node)
- Or membership in the `disk` group (which grants raw device read)

LuminaSearch runs as a user-level application. Requiring elevated privileges
for indexing is a non-starter for security and UX reasons.

### 4. Cross-Filesystem Compatibility Explosion

A direct block parser would need to support at minimum:
- ext4 (with and without extents, with and without HTree, with and without
  `flex_bg`, with and without `metadata_csum`, etc.)
- XFS (v4 and v5, with and without CRC, with different directory versions)
- btrfs (COW B-trees with entirely different on-disk format)
- Potentially: ZFS, F2FS, NTFS (dual-boot), exFAT, Bcachefs

Each filesystem has its own on-disk layout, directory structure, and journal
mechanism. Maintaining parsers for even ext4 and XFS would be thousands of
lines of unsafe code — and would still break on the next kernel update that
changes an on-disk feature flag.

### 5. Not Actually Faster

The perceived advantage of direct block parsing is avoiding syscall overhead.
But:

- `getdents64()` is a single syscall that returns multiple directory entries
  per call. The amortized cost per entry is negligible.
- `statx()` / `newfstatat()` fetches metadata efficiently.
- The actual bottleneck for filesystem indexing is **disk I/O**, not syscall
  count. A cold-cache `readdir()` + `stat()` walk and a cold-cache block
  device scan both pay the same I/O cost.
- Modern Linux kernels (~6.x+) have heavily optimized `getdents64()` and the
  dentry cache makes repeated access essentially free.

### 6. No Incremental Update Mechanism

If you parse the block device directly, you get a point-in-time snapshot. To
detect changes, you must either:
- Re-parse the entire device periodically (wasteful)
- Watch the block device for writes and re-parse affected blocks (complex,
  requires understanding journal formats, still racy)

inotify gives us precise, event-driven notifications of every filesystem
change — create, delete, modify, rename — with zero polling overhead.

## The Chosen Approach: User-Space Walking + Kernel Monitoring

### Phase 1: Full Recursive Directory Walk

At startup, LuminaSearch performs a bounded-concurrency directory walk using
standard system calls:

```
os.ReadDir() → for each entry → os.Lstat() → normalize → index
```

This approach:
- Uses the kernel's own VFS layer, guaranteeing correctness regardless of
  underlying filesystem.
- Requires no special permissions.
- Is filesystem-agnostic — works identically on ext4, XFS, btrfs, ZFS, tmpfs,
  NFS, FUSE, and any future filesystem.
- Benefits from the kernel's dentry cache for repeated path lookups.
- Handles permission errors, broken symlinks, and mounted filesystems
  gracefully.

### Phase 2: inotify for Real-Time Updates

After the initial crawl, an fsnotify/inotify watcher maintains a live index:

- **Recursive watches:** Each directory gets its own inotify watch.
  `IN_CREATE`, `IN_DELETE`, `IN_MOVED_FROM`, `IN_MOVED_TO`, `IN_MODIFY`.
- **New directories:** When `IN_CREATE | IN_ISDIR` fires, the new directory
  is immediately added to the watch set and crawled.
- **Overflow handling:** If the inotify event queue overflows
  (`IN_Q_OVERFLOW`), a lightweight reconciliation crawl is triggered for the
  affected subtree. This is the safety net that ensures eventual consistency.

### Why This Is Sufficient

| Concern | Mitigation |
|---------|-----------|
| Cold-start time | One-time cost. Subsequent restarts load from LMDB. |
| Missed events during downtime | Reconciliation crawl on startup catches all changes. |
| inotify watch limit | User can increase `max_user_watches`. Default 524288 is ample for most trees. |
| Event coalescing (rapid creates) | LMDB batch writes + in-memory index updates handle bursts efficiently. |
| Rename across filesystems | Detected as create+delete pair; index updated atomically. |

## Filesystem Constraints Summary

### Hard Constraints (Avoided by Our Approach)

| Constraint | ext4 | XFS | Impact on Our Design |
|-----------|------|-----|---------------------|
| Max filename length | 255 bytes | 255 bytes | Store filenames as-is (UTF-8). Truncation is kernel's job. |
| Max path length | 4096 bytes (PATH_MAX) | 4096 bytes | Buffer paths appropriately; gracefully skip longer paths. |
| Directory hash ordering | TEA (HTree) | CRC32c (B+tree) | Not relevant — we walk via `readdir()`, not raw blocks. |
| inode number stability | 32-bit (64-bit with `ea_inode`) | 64-bit | Use (device, inode) for symlink cycle detection. |
| Timestamp granularity | nanoseconds (ext4) | nanoseconds (XFS v5) | Store modification time with full precision. |
| Case sensitivity | Case-sensitive (default) | Case-sensitive (default) | Our index lowercases everything for case-insensitive search. |
| Sparse file support | Yes | Yes | Not relevant — we index filenames only, not file contents. |

### Soft Constraints (Handled at Application Level)

- **Permission denied on directories:** Skip gracefully; log a warning. The
  user may not own every directory on their system, and that's expected.
- **Symlink cycles:** Detect via (device, inode) visited set. Skip cycles
  with a warning.
- **Symlinks outside root:** Skip. The user asked us to index `/home/user`,
  not `/etc` or `/proc`.
- **Removable media / network mounts:** The kernel's VFS layer makes these
  transparent. Mount points are followed like any other directory. Network
  latency may slow the initial crawl but doesn't affect correctness.
- **Filesystem-specific ignore patterns:** `.git/`, `node_modules/`,
  `.cache/`, `target/`, `dist/`, `build/`, `venv/`, `__pycache__/` — these
  are ignored regardless of the underlying filesystem.

## Conclusion

Direct block-device parsing for filesystem indexing on Linux is not merely
complex — it is fundamentally the wrong approach for a user-space desktop
application. The VFS layer exists precisely to abstract away filesystem
differences. By using `readdir()` + `stat()` for the initial scan and inotify
for real-time updates, we get correctness, portability, and security without
any of the pitfalls of raw block parsing:

- **Correctness:** We see exactly what the kernel sees — no stale blocks, no
  journal races.
- **Portability:** Works on every Linux filesystem, past and future.
- **Security:** Runs as a normal user process — no `root`, no
  `CAP_SYS_ADMIN`, no `disk` group.
- **Maintainability:** ~100 lines of crawler logic instead of ~5000 lines of
  per-filesystem block parsers.
- **Performance:** The bottleneck is disk I/O, not syscall overhead.
  `getdents64()` is fast enough. inotify gives us real-time updates for free.

This design decision is the foundation of LuminaSearch's architecture and
informs every subsequent implementation choice. The LMDB index store, the
in-memory search indexes, the query engine — all are built on the assumption
that file discovery happens through the kernel's VFS layer, not around it.
