// Package query implements the search query engine for Rune.
// It performs 4-tier ranking (exact > prefix > word-boundary > substring)
// with directory boosting, all executing entirely in RAM against
// in-memory indexes built from the LMDB store at startup.
//
// The query engine never touches LMDB at search time — all lookups are
// against the in-memory prefix trie, trigram inverted index, and metadata
// cache. This ensures sub-5ms search latency regardless of index size.
package query
