package index

import (
	"fmt"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/bmatsuo/lmdb-go/lmdb"
)

const (
	// SchemaVersion is the current schema version. When opening an existing
	// LMDB environment, the stored version must match this value.
	SchemaVersion = 1

	// dbiNames for the named databases within the LMDB environment.
	dbiNamesPaths    = "paths"
	dbiNamesNames    = "names"
	dbiNamesTrigrams = "trigrams"
	dbiNamesMeta     = "_meta"

	// metaKeySchemaVersion is the key within the _meta DB that stores
	// the schema version.
	metaKeySchemaVersion = "schema_version"

	// Default LMDB map size: 1 GiB.
	defaultMapSize = int64(1 << 30)

	// Default maximum number of named databases.
	defaultMaxDBs = 8
)

// Store is the LMDB-backed index store. It manages on-disk persistence
// via LMDB and maintains in-memory search indexes (prefix trie, trigram
// inverted index, and a metadata cache).
//
// All public methods are safe for concurrent use.
type Store struct {
	env    *lmdb.Env
	dbis   storeDBIs
	path   string

	// In-memory indexes
	trie      *PrefixTrie
	trigrams  *TrigramIndex
	metaCache map[string]*FileMeta // path → metadata

	mu sync.RWMutex
}

// storeDBIs holds the open database handles.
type storeDBIs struct {
	paths    lmdb.DBI
	names    lmdb.DBI
	trigrams lmdb.DBI
	meta     lmdb.DBI
}

// Open opens or creates an LMDB index store at the given directory path.
// If the directory does not exist or contains no valid LMDB environment,
// a new one is created with the current schema version.
// If an existing environment is found, the schema version is validated.
// On success, in-memory indexes are built from the on-disk data.
func Open(path string) (*Store, error) {
	// Create data directory if it doesn't exist.
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("index: create data directory %s: %w", path, err)
	}

	// Detect if this is a new or existing environment by checking for LMDB data files.
	isNew := !envExists(path)

	// If the environment exists but may be corrupted, handle gracefully.
	if !isNew {
		if corrupted := checkEnvCorruption(path); corrupted {
			// Environment is corrupted — clean up and start fresh.
			if rmErr := removeEnvFiles(path); rmErr != nil {
				return nil, fmt.Errorf("index: remove corrupted LMDB files: %w", rmErr)
			}
			isNew = true
		}
	}

	env, err := lmdb.NewEnv()
	if err != nil {
		return nil, fmt.Errorf("index: create LMDB env: %w", err)
	}

	if err := env.SetMaxDBs(defaultMaxDBs); err != nil {
		env.Close()
		return nil, fmt.Errorf("index: set max DBs: %w", err)
	}
	if err := env.SetMapSize(defaultMapSize); err != nil {
		env.Close()
		return nil, fmt.Errorf("index: set map size: %w", err)
	}

	if err := env.Open(path, 0, 0644); err != nil {
		env.Close()
		return nil, fmt.Errorf("index: open LMDB env: %w", err)
	}

	s := &Store{
		env:       env,
		path:      path,
		trie:      NewPrefixTrie(),
		trigrams:  NewTrigramIndex(),
		metaCache: make(map[string]*FileMeta),
	}

	// Open or create named databases.
	if err := s.openDBIs(isNew); err != nil {
		env.Close()
		return nil, err
	}

	// Validate or write schema version.
	if isNew {
		if err := s.writeSchemaVersion(); err != nil {
			env.Close()
			return nil, err
		}
	} else {
		if err := s.validateSchemaVersion(); err != nil {
			env.Close()
			return nil, err
		}
	}

	// Build in-memory indexes from LMDB.
	if !isNew {
		if err := s.buildMemoryIndexes(); err != nil {
			env.Close()
			return nil, fmt.Errorf("index: build in-memory indexes: %w", err)
		}
	}

	return s, nil
}

// envExists checks if an LMDB environment exists at the given path.
func envExists(path string) bool {
	// LMDB creates data.mdb and lock.mdb files.
	_, err := os.Stat(path + "/data.mdb")
	if err == nil {
		return true
	}
	_, err = os.Stat(path + "/lock.mdb")
	return err == nil
}

// checkEnvCorruption attempts to detect if the existing LMDB environment is corrupted.
func checkEnvCorruption(path string) bool {
	// Try a quick open to see if the environment is valid.
	env, err := lmdb.NewEnv()
	if err != nil {
		return true
	}
	defer env.Close()

	if err := env.SetMaxDBs(defaultMaxDBs); err != nil {
		return true
	}
	if err := env.SetMapSize(defaultMapSize); err != nil {
		return true
	}

	err = env.Open(path, 0, 0644)
	if err != nil {
		return isCorruptedError(err)
	}

	return false
}

// Close closes the LMDB environment and releases resources.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.env != nil {
		s.env.Close()
		s.env = nil
	}
	return nil
}

// Put inserts or updates a file entry in all indexes (LMDB + in-memory).
// The path is normalized and lowercased before storage.
// This operation is atomic with respect to other Put/Delete calls.
func (s *Store) Put(meta *FileMeta) error {
	// Normalize and lowercase at index time.
	// Preserve the original filename (pre-lowercase) for CamelCase word-boundary detection.
	originalFilename := path.Base(meta.Path)
	normalizedPath := NormalizePath(meta.Path)
	normalizedFilename := strings.ToLower(originalFilename)

	// Update the meta's path to the normalized version.
	meta.Path = normalizedPath
	meta.Filename = normalizedFilename
	meta.OriginalFilename = originalFilename

	s.mu.Lock()
	defer s.mu.Unlock()

	// Write to LMDB in a single transaction.
	err := s.env.Update(func(txn *lmdb.Txn) error {
		// Store metadata in paths DB.
		data, err := meta.Marshal()
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		if err := txn.Put(s.dbis.paths, []byte(normalizedPath), data, 0); err != nil {
			return fmt.Errorf("put path %s: %w", normalizedPath, err)
		}

		// Update names DB.
		if err := s.updateNamesInTxn(txn, normalizedFilename, normalizedPath, true); err != nil {
			return err
		}

		// Update trigrams DB.
		trigrams := ExtractTrigrams(normalizedFilename)
		for _, t := range trigrams {
			if err := s.updateTrigramInTxn(txn, t, normalizedPath, true); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("index: put %s: %w", normalizedPath, err)
	}

	// Update in-memory indexes.
	s.trie.Insert(normalizedFilename, normalizedPath)
	s.trigrams.Insert(normalizedFilename, normalizedPath)
	s.metaCache[normalizedPath] = meta

	return nil
}

// Delete removes a path from all indexes (LMDB + in-memory).
func (s *Store) Delete(path string) error {
	normalizedPath := NormalizePath(path)
	normalizedFilename := NormalizeFilename(path)

	s.mu.Lock()
	defer s.mu.Unlock()

	// Get existing metadata to retrieve old filename for cleanup.
	oldMeta, exists := s.metaCache[normalizedPath]
	if !exists {
		// Try to read from LMDB.
		_ = s.env.View(func(txn *lmdb.Txn) error {
			val, err := txn.Get(s.dbis.paths, []byte(normalizedPath))
			if err == nil {
				m, uerr := UnmarshalFileMeta(val)
				if uerr == nil {
					oldMeta = m
				}
			}
			return nil
		})
	}

	// Write to LMDB in a single transaction.
	err := s.env.Update(func(txn *lmdb.Txn) error {
		// Remove from paths DB.
		if err := txn.Del(s.dbis.paths, []byte(normalizedPath), nil); err != nil && !lmdb.IsNotFound(err) {
			return fmt.Errorf("del path %s: %w", normalizedPath, err)
		}

		// Remove from names DB.
		if err := s.updateNamesInTxn(txn, normalizedFilename, normalizedPath, false); err != nil {
			return err
		}

		// Remove from trigrams DB.
		trigrams := ExtractTrigrams(normalizedFilename)
		for _, t := range trigrams {
			if err := s.updateTrigramInTxn(txn, t, normalizedPath, false); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("index: delete %s: %w", normalizedPath, err)
	}

	// Update in-memory indexes.
	if oldMeta != nil {
		s.trie.Delete(oldMeta.Filename, normalizedPath)
		s.trigrams.Delete(oldMeta.Filename, normalizedPath)
	}
	s.trie.Delete(normalizedFilename, normalizedPath)
	s.trigrams.Delete(normalizedFilename, normalizedPath)
	delete(s.metaCache, normalizedPath)

	return nil
}

// Get retrieves metadata for a normalized path.
func (s *Store) Get(path string) (*FileMeta, error) {
	normalizedPath := NormalizePath(path)

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check in-memory cache first.
	if meta, ok := s.metaCache[normalizedPath]; ok {
		return meta, nil
	}

	// Fall back to LMDB.
	var meta *FileMeta
	err := s.env.View(func(txn *lmdb.Txn) error {
		val, err := txn.Get(s.dbis.paths, []byte(normalizedPath))
		if err != nil {
			return err
		}
		m, err := UnmarshalFileMeta(val)
		if err != nil {
			return err
		}
		meta = m
		return nil
	})
	if err != nil {
		if lmdb.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("index: get %s: %w", normalizedPath, err)
	}

	return meta, nil
}

// Trie returns the in-memory prefix trie (read-only access).
func (s *Store) Trie() *PrefixTrie {
	return s.trie
}

// Trigrams returns the in-memory trigram index (read-only access).
func (s *Store) Trigrams() *TrigramIndex {
	return s.trigrams
}

// MetaCache returns the in-memory metadata cache (read-only access).
func (s *Store) MetaCache() map[string]*FileMeta {
	return s.metaCache
}

// Path returns the LMDB data directory path.
func (s *Store) Path() string {
	return s.path
}

// Stats returns basic statistics about the store.
type Stats struct {
	PathCount    int
	TrieNodes    int
	TrigramCount int
}

// Stats returns current statistics.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return Stats{
		PathCount:    len(s.metaCache),
		TrieNodes:    s.trie.NodeCount(),
		TrigramCount: s.trigrams.Size(),
	}
}

// openDBIs opens all named databases, creating them if isNew is true.
func (s *Store) openDBIs(isNew bool) error {
	flag := uint(0)
	if isNew {
		flag = lmdb.Create
	}

	return s.env.Update(func(txn *lmdb.Txn) error {
		var err error
		s.dbis.paths, err = txn.OpenDBI(dbiNamesPaths, flag)
		if err != nil {
			return fmt.Errorf("open paths DB: %w", err)
		}
		s.dbis.names, err = txn.OpenDBI(dbiNamesNames, flag)
		if err != nil {
			return fmt.Errorf("open names DB: %w", err)
		}
		s.dbis.trigrams, err = txn.OpenDBI(dbiNamesTrigrams, flag)
		if err != nil {
			return fmt.Errorf("open trigrams DB: %w", err)
		}
		s.dbis.meta, err = txn.OpenDBI(dbiNamesMeta, flag)
		if err != nil {
			return fmt.Errorf("open meta DB: %w", err)
		}
		return nil
	})
}

// writeSchemaVersion writes the current schema version to the meta DB.
func (s *Store) writeSchemaVersion() error {
	return s.env.Update(func(txn *lmdb.Txn) error {
		versionStr := fmt.Sprintf("%d", SchemaVersion)
		return txn.Put(s.dbis.meta, []byte(metaKeySchemaVersion), []byte(versionStr), 0)
	})
}

// validateSchemaVersion checks the stored schema version against the current one.
func (s *Store) validateSchemaVersion() error {
	return s.env.View(func(txn *lmdb.Txn) error {
		val, err := txn.Get(s.dbis.meta, []byte(metaKeySchemaVersion))
		if err != nil {
			if lmdb.IsNotFound(err) {
				return fmt.Errorf("index: schema version not found in existing LMDB environment; database may be corrupted")
			}
			return fmt.Errorf("index: read schema version: %w", err)
		}

		var storedVersion int
		if _, scanErr := fmt.Sscanf(string(val), "%d", &storedVersion); scanErr != nil {
			return fmt.Errorf("index: invalid schema version format: %q", string(val))
		}

		if storedVersion != SchemaVersion {
			if storedVersion > SchemaVersion {
				return fmt.Errorf("index: schema version mismatch: database version is %d but engine supports version %d; the database was created by a newer version of LuminaSearch and cannot be opened by this version", storedVersion, SchemaVersion)
			}
			return fmt.Errorf("index: schema version mismatch: database version is %d but engine requires version %d", storedVersion, SchemaVersion)
		}

		return nil
	})
}

// buildMemoryIndexes reads all entries from LMDB and builds in-memory indexes.
func (s *Store) buildMemoryIndexes() error {
	return s.env.View(func(txn *lmdb.Txn) error {
		cur, err := txn.OpenCursor(s.dbis.paths)
		if err != nil {
			return fmt.Errorf("open paths cursor: %w", err)
		}
		defer cur.Close()

		for {
			key, val, err := cur.Get(nil, nil, lmdb.Next)
			if lmdb.IsNotFound(err) {
				break
			}
			if err != nil {
				return fmt.Errorf("cursor iteration: %w", err)
			}

			meta, err := UnmarshalFileMeta(val)
			if err != nil {
				return fmt.Errorf("unmarshal metadata for %s: %w", string(key), err)
			}

			path := string(key)
			s.trie.Insert(meta.Filename, path)
			s.trigrams.Insert(meta.Filename, path)
			s.metaCache[path] = meta
		}

		return nil
	})
}

// updateNamesInTxn updates the names DB within a transaction.
// add=true adds the path; add=false removes it.
func (s *Store) updateNamesInTxn(txn *lmdb.Txn, filename, path string, add bool) error {
	key := []byte(filename)

	val, err := txn.Get(s.dbis.names, key)
	if add {
		var ps PathSet
		if err == nil {
			ps, err = UnmarshalPathSet(val)
			if err != nil {
				return fmt.Errorf("unmarshal name set for %s: %w", filename, err)
			}
		}
		// Add path if not present.
		found := false
		for _, p := range ps {
			if p == path {
				found = true
				break
			}
		}
		if !found {
			ps = append(ps, path)
			data, err := ps.Marshal()
			if err != nil {
				return fmt.Errorf("marshal name set: %w", err)
			}
			return txn.Put(s.dbis.names, key, data, 0)
		}
	} else {
		if err != nil {
			if lmdb.IsNotFound(err) {
				return nil // not found, nothing to remove
			}
			return fmt.Errorf("get name set for %s: %w", filename, err)
		}
		ps, err := UnmarshalPathSet(val)
		if err != nil {
			return fmt.Errorf("unmarshal name set: %w", err)
		}
		filtered := make(PathSet, 0, len(ps))
		for _, p := range ps {
			if p != path {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			return txn.Del(s.dbis.names, key, nil)
		}
		data, err := filtered.Marshal()
		if err != nil {
			return fmt.Errorf("marshal name set: %w", err)
		}
		return txn.Put(s.dbis.names, key, data, 0)
	}

	return nil
}

// updateTrigramInTxn updates the trigrams DB within a transaction.
func (s *Store) updateTrigramInTxn(txn *lmdb.Txn, trigram, path string, add bool) error {
	key := []byte(trigram)

	val, err := txn.Get(s.dbis.trigrams, key)
	if add {
		var ps PathSet
		if err == nil {
			ps, err = UnmarshalPathSet(val)
			if err != nil {
				return fmt.Errorf("unmarshal trigram set for %s: %w", trigram, err)
			}
		}
		found := false
		for _, p := range ps {
			if p == path {
				found = true
				break
			}
		}
		if !found {
			ps = append(ps, path)
			data, err := ps.Marshal()
			if err != nil {
				return fmt.Errorf("marshal trigram set: %w", err)
			}
			return txn.Put(s.dbis.trigrams, key, data, 0)
		}
	} else {
		if err != nil {
			if lmdb.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("get trigram set: %w", err)
		}
		ps, err := UnmarshalPathSet(val)
		if err != nil {
			return fmt.Errorf("unmarshal trigram set: %w", err)
		}
		filtered := make(PathSet, 0, len(ps))
		for _, p := range ps {
			if p != path {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			return txn.Del(s.dbis.trigrams, key, nil)
		}
		data, err := filtered.Marshal()
		if err != nil {
			return fmt.Errorf("marshal trigram set: %w", err)
		}
		return txn.Put(s.dbis.trigrams, key, data, 0)
	}

	return nil
}

// removeEnvFiles removes all LMDB files from the given directory.
func removeEnvFiles(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(path + "/" + entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

// isCorruptedError checks if the error indicates a corrupted LMDB environment.
func isCorruptedError(err error) bool {
	// Check for common corruption indicators.
	return lmdb.IsErrno(err, lmdb.Invalid) ||
		lmdb.IsErrno(err, lmdb.Panic) ||
		lmdb.IsErrno(err, lmdb.VersionMismatch) ||
		lmdb.IsErrno(err, lmdb.Corrupted)
}
