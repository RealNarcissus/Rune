package index

import "sync"

// TrieNode represents a node in the prefix trie.
// Each node maps the next character to its child node and
// holds the set of paths that end at or pass through this node.
type TrieNode struct {
	children map[rune]*TrieNode
	paths    map[string]struct{} // set of full paths
	mu       sync.RWMutex
}

// newTrieNode creates a new empty trie node.
func newTrieNode() *TrieNode {
	return &TrieNode{
		children: make(map[rune]*TrieNode),
		paths:    make(map[string]struct{}),
	}
}

// PrefixTrie is a compact prefix tree (trie) keyed by lowercase filenames.
// It enables O(k) prefix lookup where k is the query length.
// All operations are safe for concurrent use.
type PrefixTrie struct {
	root *TrieNode
	mu   sync.RWMutex
}

// NewPrefixTrie creates an empty prefix trie.
func NewPrefixTrie() *PrefixTrie {
	return &PrefixTrie{
		root: newTrieNode(),
	}
}

// Insert adds a filename→path mapping to the trie.
// The filename should already be lowercased.
func (t *PrefixTrie) Insert(filename, fullPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	node := t.root
	for _, ch := range filename {
		node.mu.Lock()
		child, ok := node.children[ch]
		if !ok {
			child = newTrieNode()
			node.children[ch] = child
		}
		node.mu.Unlock()
		node = child
	}

	// Mark this path at the terminal node and all ancestor nodes
	// to support prefix lookup at intermediate nodes.
	node.mu.Lock()
	node.paths[fullPath] = struct{}{}
	node.mu.Unlock()
}

// Delete removes a filename→path mapping from the trie.
func (t *PrefixTrie) Delete(filename, fullPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.deleteRecursive(t.root, []rune(filename), fullPath, 0)
}

func (t *PrefixTrie) deleteRecursive(node *TrieNode, chars []rune, fullPath string, depth int) bool {
	if depth == len(chars) {
		// Terminal node: remove this specific path
		node.mu.Lock()
		delete(node.paths, fullPath)
		hasPaths := len(node.paths) > 0
		hasChildren := len(node.children) > 0
		node.mu.Unlock()
		return !hasPaths && !hasChildren
	}

	ch := chars[depth]
	node.mu.RLock()
	child, ok := node.children[ch]
	node.mu.RUnlock()

	if !ok {
		return false
	}

	shouldPrune := t.deleteRecursive(child, chars, fullPath, depth+1)
	if shouldPrune {
		node.mu.Lock()
		delete(node.children, ch)
		node.mu.Unlock()
	}

	// Check if this node should be pruned too
	node.mu.RLock()
	hasChildren := len(node.children) > 0
	hasPaths := len(node.paths) > 0
	node.mu.RUnlock()
	return !hasChildren && !hasPaths && depth > 0
}

// Search returns all paths whose filenames start with the given prefix.
// The prefix should be lowercased.
func (t *PrefixTrie) Search(prefix string) map[string]struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()

	// Navigate to the prefix node
	node := t.root
	for _, ch := range prefix {
		node.mu.RLock()
		child, ok := node.children[ch]
		node.mu.RUnlock()
		if !ok {
			return nil
		}
		node = child
	}

	// Collect all paths in the subtree
	return t.collectPaths(node)
}

func (t *PrefixTrie) collectPaths(node *TrieNode) map[string]struct{} {
	result := make(map[string]struct{})

	// Collect paths at this node
	node.mu.RLock()
	for p := range node.paths {
		result[p] = struct{}{}
	}
	for _, child := range node.children {
		childPaths := t.collectPaths(child)
		for p := range childPaths {
			result[p] = struct{}{}
		}
	}
	node.mu.RUnlock()

	return result
}

// NodeCount returns the total number of nodes in the trie.
// Useful for testing that the trie was built correctly.
func (t *PrefixTrie) NodeCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodeCount(t.root)
}

func (t *PrefixTrie) nodeCount(node *TrieNode) int {
	count := 1
	node.mu.RLock()
	defer node.mu.RUnlock()
	for _, child := range node.children {
		count += t.nodeCount(child)
	}
	return count
}

// PathCount returns the total number of path references stored.
func (t *PrefixTrie) PathCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pathCount(t.root)
}

func (t *PrefixTrie) pathCount(node *TrieNode) int {
	node.mu.RLock()
	count := len(node.paths)
	for _, child := range node.children {
		count += t.pathCount(child)
	}
	node.mu.RUnlock()
	return count
}
