// Package crawler implements the concurrent directory crawler for Rune.
// It walks directory trees using a bounded goroutine pool, collecting file
// metadata while handling permission errors, symlink cycles, and ignore patterns.
//
// The crawler uses standard os.ReadDir and os.Lstat system calls, relying on
// the kernel's VFS layer for correctness across all filesystem types.
package crawler
