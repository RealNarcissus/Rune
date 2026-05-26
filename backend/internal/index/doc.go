// Package index manages the LMDB index store for LuminaSearch.
// It provides operations for creating/opening the LMDB environment,
// reading and writing file metadata records, and managing the
// in-memory search indexes (prefix trie, trigram inverted index).
//
// The LMDB store uses three named databases:
//   - "paths":    path → JSON metadata (source of truth)
//   - "names":    filename → path set
//   - "trigrams": trigram → path set
package index
