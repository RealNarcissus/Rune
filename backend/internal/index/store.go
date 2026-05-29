package index

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/bmatsuo/lmdb-go/lmdb"
)

const (
	// SchemaVersion is the current schema version. When opening an existing
	// LMDB environment, the stored version must match this value.
	SchemaVersion = 1

	// dbiNames for the named databases within the LMDB environment.
	dbiNamesPaths    = "paths"    // path string -> uint32 ID
	dbiNamesNames    = "names"    // filename string -> []uint32 posting list
	dbiNamesTrigrams = "trigrams" // trigram string -> []uint32 posting list
	dbiNamesMetadata = "metadata" // uint32 ID -> FileMeta JSON
	dbiNamesMeta     = "_meta"    // schema version & next_path_id counter

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
	env  *lmdb.Env
	dbis storeDBIs
	path string

	// In-memory indexes
	trie      *PrefixTrie
	trigrams  *TrigramIndex
	metaCache map[uint32]*FileMeta // ID → metadata
	pathCache map[string]uint32    // path → ID
	idCache   map[uint32]string    // ID → path

	mu sync.RWMutex
}

// storeDBIs holds the open database handles.
type storeDBIs struct {
	paths    lmdb.DBI
	names    lmdb.DBI
	trigrams lmdb.DBI
	metadata lmdb.DBI
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
		metaCache: make(map[uint32]*FileMeta),
		pathCache: make(map[string]uint32),
		idCache:   make(map[uint32]string),
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

// BulkPutBatchSize is the number of entries processed per LMDB write
// transaction when using BulkPut. Larger batches are faster but consume
// more transaction memory.
const BulkPutBatchSize = 10000

// BulkPut inserts multiple file entries efficiently by batching LMDB
// write transactions. This is significantly faster than calling Put
// for each entry individually, especially for large bulk loads.
//
// The paths are normalized and lowercased before storage. All entries
// are inserted atomically per batch; a failure in any batch stops the
// operation and returns an error.
//
// All index mutations flow through a single serialized write pipeline
// to completely avoid LMDB writer contention.
func (s *Store) BulkPut(entries []*FileMeta) error {
	if len(entries) == 0 {
		return nil
	}

	// Pre-normalize all entries outside the lock.
	type prepared struct {
		meta             *FileMeta
		trigrams         []string
		normalizedPath   string
		normalizedName   string
		originalFilename string
	}

	preparedEntries := make([]prepared, len(entries))
	for i, meta := range entries {
		originalFilename := path.Base(meta.Path)
		normalizedPath := NormalizePath(meta.Path)
		normalizedName := strings.ToLower(originalFilename)

		meta.Path = normalizedPath
		meta.Filename = normalizedName
		meta.OriginalFilename = originalFilename

		preparedEntries[i] = prepared{
			meta:             meta,
			trigrams:         ExtractTrigrams(normalizedName),
			normalizedPath:   normalizedPath,
			normalizedName:   normalizedName,
			originalFilename: originalFilename,
		}
	}

	// Lock the write pipeline globally to avoid LMDB writer contention.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Maps to aggregate all posting list additions in memory
	trigramAdditions := make(map[string][]uint32)
	nameAdditions := make(map[string][]uint32)

	// We process path ID allocation and metadata writes in batches of BulkPutBatchSize (10,000)
	// to keep transaction memory bounded, while keeping posting list additions in memory.
	for start := 0; start < len(preparedEntries); {
		end := start + BulkPutBatchSize
		if end > len(preparedEntries) {
			end = len(preparedEntries)
		}
		batch := preparedEntries[start:end]

		// Transaction for this batch (paths & metadata writes)
		err := s.env.Update(func(txn *lmdb.Txn) error {
			for _, pe := range batch {
				// Allocate or look up Path ID
				pathID, err := s.getOrCreatePathID(txn, pe.normalizedPath)
				if err != nil {
					return err
				}

				pe.meta.ID = pathID

				// Store metadata in metadata DB
				data, err := pe.meta.Marshal()
				if err != nil {
					return fmt.Errorf("marshal metadata: %w", err)
				}
				if err := txn.Put(s.dbis.metadata, Uint32ToBytes(pathID), data, 0); err != nil {
					return fmt.Errorf("put metadata for ID %d: %w", pathID, err)
				}

				// Aggregate filename in memory
				nameAdditions[pe.normalizedName] = append(nameAdditions[pe.normalizedName], pathID)

				// Aggregate trigrams in memory
				for _, t := range pe.trigrams {
					trigramAdditions[t] = append(trigramAdditions[t], pathID)
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("index: bulk metadata batch [%d:%d]: %w", start, end, err)
		}

		start = end
	}

	// 2. Now, merge and write all accumulated Name posting lists in batches of 10,000 keys
	type nameMergeItem struct {
		filename string
		ids      []uint32
	}
	var nameMergeList []nameMergeItem
	for filename, ids := range nameAdditions {
		nameMergeList = append(nameMergeList, nameMergeItem{filename: filename, ids: ids})
	}

	for start := 0; start < len(nameMergeList); {
		end := start + BulkPutBatchSize
		if end > len(nameMergeList) {
			end = len(nameMergeList)
		}
		batch := nameMergeList[start:end]

		err := s.env.Update(func(txn *lmdb.Txn) error {
			for _, item := range batch {
				key := []byte(item.filename)
				val, err := txn.Get(s.dbis.names, key)
				var merged []uint32
				if err == nil {
					existing, err := UnmarshalPostingList(val)
					if err == nil {
						merged = mergeSortedSlices(existing, item.ids)
					} else {
						merged = item.ids
					}
				} else {
					merged = item.ids
				}
				data := MarshalPostingList(merged)
				if err := txn.Put(s.dbis.names, key, data, 0); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("index: bulk merge names batch [%d:%d]: %w", start, end, err)
		}
		sidebarEnd := end
		_ = sidebarEnd
		start = end
	}

	// 3. Merge and write all accumulated Trigram posting lists in batches of 10,000 keys
	type trigramMergeItem struct {
		trigram string
		ids     []uint32
	}
	var trigramMergeList []trigramMergeItem
	for trigram, ids := range trigramAdditions {
		trigramMergeList = append(trigramMergeList, trigramMergeItem{trigram: trigram, ids: ids})
	}

	for start := 0; start < len(trigramMergeList); {
		end := start + BulkPutBatchSize
		if end > len(trigramMergeList) {
			end = len(trigramMergeList)
		}
		batch := trigramMergeList[start:end]

		err := s.env.Update(func(txn *lmdb.Txn) error {
			for _, item := range batch {
				key := []byte(item.trigram)
				val, err := txn.Get(s.dbis.trigrams, key)
				var merged []uint32
				if err == nil {
					existing, err := UnmarshalPostingList(val)
					if err == nil {
						merged = mergeSortedSlices(existing, item.ids)
					} else {
						merged = item.ids
					}
				} else {
					merged = item.ids
				}
				data := MarshalPostingList(merged)
				if err := txn.Put(s.dbis.trigrams, key, data, 0); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("index: bulk merge trigrams batch [%d:%d]: %w", start, end, err)
		}
		start = end
	}

	// 4. Update in-memory indexes
	for _, pe := range preparedEntries {
		s.trie.Insert(pe.normalizedName, pe.meta.ID)
		s.trigrams.Insert(pe.normalizedName, pe.meta.ID)
		s.metaCache[pe.meta.ID] = pe.meta
		s.pathCache[pe.normalizedPath] = pe.meta.ID
		s.idCache[pe.meta.ID] = pe.normalizedPath
	}

	return nil
}

func mergeSortedSlices(a, b []uint32) []uint32 {
	result := make([]uint32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			result = append(result, a[i])
			i++
			j++
		} else if a[i] < b[j] {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	for i < len(a) {
		result = append(result, a[i])
		i++
	}
	for j < len(b) {
		result = append(result, b[j])
		j++
	}
	return result
}


// Put inserts or updates a file entry in all indexes (LMDB + in-memory).
// The path is normalized and lowercased before storage.
//
// All index mutations flow through a single serialized write pipeline
// to completely avoid LMDB writer contention.
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
	trigrams := ExtractTrigrams(normalizedFilename)

	s.mu.Lock()
	defer s.mu.Unlock()

	var pathID uint32
	// Write to LMDB in a single transaction.
	err := s.env.Update(func(txn *lmdb.Txn) error {
		var err error
		pathID, err = s.getOrCreatePathID(txn, normalizedPath)
		if err != nil {
			return err
		}

		meta.ID = pathID

		// Store metadata in metadata DB.
		data, err := meta.Marshal()
		if err != nil {
			return fmt.Errorf("marshal metadata: %w", err)
		}
		if err := txn.Put(s.dbis.metadata, Uint32ToBytes(pathID), data, 0); err != nil {
			return fmt.Errorf("put metadata for ID %d: %w", pathID, err)
		}

		// Update names DB.
		if err := s.updateNamesInTxn(txn, normalizedFilename, pathID, true); err != nil {
			return err
		}

		// Update trigrams DB.
		for _, t := range trigrams {
			if err := s.updateTrigramInTxn(txn, t, pathID, true); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("index: put %s: %w", normalizedPath, err)
	}

	// Update in-memory indexes.
	s.trie.Insert(normalizedFilename, pathID)
	s.trigrams.Insert(normalizedFilename, pathID)
	s.metaCache[pathID] = meta
	s.pathCache[normalizedPath] = pathID
	s.idCache[pathID] = normalizedPath

	return nil
}

// Delete removes a path from all indexes (LMDB + in-memory).
func (s *Store) Delete(path string) error {
	normalizedPath := NormalizePath(path)
	normalizedFilename := NormalizeFilename(path)
	trigrams := ExtractTrigrams(normalizedFilename)

	s.mu.Lock()
	defer s.mu.Unlock()

	pathID, exists := s.pathCache[normalizedPath]
	if !exists {
		// Try to read from paths DB.
		_ = s.env.View(func(txn *lmdb.Txn) error {
			val, err := txn.Get(s.dbis.paths, []byte(normalizedPath))
			if err == nil {
				pathID = BytesToUint32(val)
				exists = true
			}
			return nil
		})
	}

	if !exists {
		return nil // Not found, nothing to delete
	}

	oldMeta := s.metaCache[pathID]

	// Write to LMDB in a single transaction.
	err := s.env.Update(func(txn *lmdb.Txn) error {
		// Remove from paths mapping
		if err := txn.Del(s.dbis.paths, []byte(normalizedPath), nil); err != nil && !lmdb.IsNotFound(err) {
			return fmt.Errorf("del path mapping %s: %w", normalizedPath, err)
		}

		// Remove from metadata DB
		if err := txn.Del(s.dbis.metadata, Uint32ToBytes(pathID), nil); err != nil && !lmdb.IsNotFound(err) {
			return fmt.Errorf("del metadata for ID %d: %w", pathID, err)
		}

		// Remove from names DB.
		if err := s.updateNamesInTxn(txn, normalizedFilename, pathID, false); err != nil {
			return err
		}

		// Remove from trigrams DB.
		for _, t := range trigrams {
			if err := s.updateTrigramInTxn(txn, t, pathID, false); err != nil {
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
		s.trie.Delete(oldMeta.Filename, pathID)
		s.trigrams.Delete(oldMeta.Filename, pathID)
	} else {
		s.trie.Delete(normalizedFilename, pathID)
		s.trigrams.Delete(normalizedFilename, pathID)
	}
	delete(s.metaCache, pathID)
	delete(s.pathCache, normalizedPath)
	delete(s.idCache, pathID)

	return nil
}

// Get retrieves metadata for a normalized path.
func (s *Store) Get(path string) (*FileMeta, error) {
	normalizedPath := NormalizePath(path)

	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check in-memory cache first.
	if pathID, ok := s.pathCache[normalizedPath]; ok {
		if meta, found := s.metaCache[pathID]; found {
			return meta, nil
		}
	}

	// Fall back to LMDB.
	var meta *FileMeta
	err := s.env.View(func(txn *lmdb.Txn) error {
		idVal, err := txn.Get(s.dbis.paths, []byte(normalizedPath))
		if err != nil {
			return err
		}
		pathID := BytesToUint32(idVal)

		val, err := txn.Get(s.dbis.metadata, Uint32ToBytes(pathID))
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

// GetByID retrieves metadata by its unique uint32 path ID.
func (s *Store) GetByID(pathID uint32) (*FileMeta, error) {
	s.mu.RLock()
	// Check in-memory cache first.
	if meta, ok := s.metaCache[pathID]; ok {
		s.mu.RUnlock()
		return meta, nil
	}
	s.mu.RUnlock()

	// Fall back to LMDB.
	var meta *FileMeta
	err := s.env.View(func(txn *lmdb.Txn) error {
		val, err := txn.Get(s.dbis.metadata, Uint32ToBytes(pathID))
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
		return nil, fmt.Errorf("index: get by ID %d: %w", pathID, err)
	}

	return meta, nil
}

// ResolveIDToPath resolves a uint32 path ID to its full path string.
func (s *Store) ResolveIDToPath(pathID uint32) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idCache[pathID]
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
func (s *Store) MetaCache() map[uint32]*FileMeta {
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
		s.dbis.metadata, err = txn.OpenDBI(dbiNamesMetadata, flag)
		if err != nil {
			return fmt.Errorf("open metadata DB: %w", err)
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
				return fmt.Errorf("index: schema version mismatch: database version is %d but engine supports version %d; the database was created by a newer version of Rune and cannot be opened by this version", storedVersion, SchemaVersion)
			}
			return fmt.Errorf("index: schema version mismatch: database version is %d but engine requires version %d", storedVersion, SchemaVersion)
		}

		return nil
	})
}

// buildMemoryIndexes reads all entries from LMDB and builds in-memory indexes.
func (s *Store) buildMemoryIndexes() error {
	return s.env.View(func(txn *lmdb.Txn) error {
		cur, err := txn.OpenCursor(s.dbis.metadata)
		if err != nil {
			return fmt.Errorf("open metadata cursor: %w", err)
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

			pathID := BytesToUint32(key)
			meta, err := UnmarshalFileMeta(val)
			if err != nil {
				return fmt.Errorf("unmarshal metadata for ID %d: %w", pathID, err)
			}

			s.trie.Insert(meta.Filename, pathID)
			s.trigrams.Insert(meta.Filename, pathID)
			s.metaCache[pathID] = meta
			s.pathCache[meta.Path] = pathID
			s.idCache[pathID] = meta.Path
		}

		return nil
	})
}

// getOrCreatePathID resolves a path to its unique uint32 ID, generating and persisting a new one if not found.
func (s *Store) getOrCreatePathID(txn *lmdb.Txn, normalizedPath string) (uint32, error) {
	// 1. Check if path already exists in paths DB
	val, err := txn.Get(s.dbis.paths, []byte(normalizedPath))
	if err == nil {
		return BytesToUint32(val), nil
	}
	if !lmdb.IsNotFound(err) {
		return 0, fmt.Errorf("get path ID: %w", err)
	}

	// 2. Allocate a new ID using "_meta" key "next_path_id"
	var nextID uint32 = 1
	metaVal, err := txn.Get(s.dbis.meta, []byte("next_path_id"))
	if err == nil {
		nextID = BytesToUint32(metaVal)
	} else if !lmdb.IsNotFound(err) {
		return 0, fmt.Errorf("get next path ID: %w", err)
	}

	// 3. Save new next_path_id
	newNextID := nextID + 1
	if err := txn.Put(s.dbis.meta, []byte("next_path_id"), Uint32ToBytes(newNextID), 0); err != nil {
		return 0, fmt.Errorf("put next path ID: %w", err)
	}

	// 4. Save path -> ID mapping
	if err := txn.Put(s.dbis.paths, []byte(normalizedPath), Uint32ToBytes(nextID), 0); err != nil {
		return 0, fmt.Errorf("put path: %w", err)
	}

	return nextID, nil
}

// updateNamesInTxn updates the names DB within a transaction.
// add=true adds the pathID; add=false removes it.
func (s *Store) updateNamesInTxn(txn *lmdb.Txn, filename string, pathID uint32, add bool) error {
	key := []byte(filename)

	val, err := txn.Get(s.dbis.names, key)
	var ids []uint32
	if err == nil {
		ids, err = UnmarshalPostingList(val)
		if err != nil {
			return fmt.Errorf("unmarshal name posting list: %w", err)
		}
	} else if !lmdb.IsNotFound(err) {
		return fmt.Errorf("get name posting list: %w", err)
	}

	idx := sort.Search(len(ids), func(i int) bool {
		return ids[i] >= pathID
	})

	if add {
		if idx < len(ids) && ids[idx] == pathID {
			// Already exists
			return nil
		}
		// Insert at idx
		ids = append(ids, 0)
		copy(ids[idx+1:], ids[idx:])
		ids[idx] = pathID

		data := MarshalPostingList(ids)
		return txn.Put(s.dbis.names, key, data, 0)
	} else {
		if idx < len(ids) && ids[idx] == pathID {
			// Remove from slice
			ids = append(ids[:idx], ids[idx+1:]...)
			if len(ids) == 0 {
				return txn.Del(s.dbis.names, key, nil)
			}
			data := MarshalPostingList(ids)
			return txn.Put(s.dbis.names, key, data, 0)
		}
		return nil
	}
}

// updateTrigramInTxn updates the trigrams DB within a transaction.
func (s *Store) updateTrigramInTxn(txn *lmdb.Txn, trigram string, pathID uint32, add bool) error {
	key := []byte(trigram)

	val, err := txn.Get(s.dbis.trigrams, key)
	var ids []uint32
	if err == nil {
		ids, err = UnmarshalPostingList(val)
		if err != nil {
			return fmt.Errorf("unmarshal trigram posting list: %w", err)
		}
	} else if !lmdb.IsNotFound(err) {
		return fmt.Errorf("get trigram posting list: %w", err)
	}

	idx := sort.Search(len(ids), func(i int) bool {
		return ids[i] >= pathID
	})

	if add {
		if idx < len(ids) && ids[idx] == pathID {
			// Already exists
			return nil
		}
		// Insert at idx
		ids = append(ids, 0)
		copy(ids[idx+1:], ids[idx:])
		ids[idx] = pathID

		data := MarshalPostingList(ids)
		return txn.Put(s.dbis.trigrams, key, data, 0)
	} else {
		if idx < len(ids) && ids[idx] == pathID {
			// Remove from slice
			ids = append(ids[:idx], ids[idx+1:]...)
			if len(ids) == 0 {
				return txn.Del(s.dbis.trigrams, key, nil)
			}
			data := MarshalPostingList(ids)
			return txn.Put(s.dbis.trigrams, key, data, 0)
		}
		return nil
	}
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
