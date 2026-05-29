package index

import (
	"sort"
	"sync"
)

// TrieNode represents a node in the prefix trie.
// Each node maps the next character to its child node and
// holds the compact list of path IDs ending at this node.
type TrieNode struct {
	children map[rune]*TrieNode
	pathIDs  []uint32 // Compact sorted slice of path IDs ending at this node
	mu       sync.RWMutex
}

// newTrieNode creates a new empty trie node.
func newTrieNode() *TrieNode {
	return &TrieNode{
		children: make(map[rune]*TrieNode),
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

// Insert adds a filename→pathID mapping to the trie.
// The filename should already be lowercased.
func (t *PrefixTrie) Insert(filename string, pathID uint32) {
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

	node.mu.Lock()
	// Insert pathID in sorted order if not present
	idx := sort.Search(len(node.pathIDs), func(i int) bool {
		return node.pathIDs[i] >= pathID
	})
	if idx < len(node.pathIDs) && node.pathIDs[idx] == pathID {
		// Already present
	} else {
		// Insert at idx
		node.pathIDs = append(node.pathIDs, 0)
		copy(node.pathIDs[idx+1:], node.pathIDs[idx:])
		node.pathIDs[idx] = pathID
	}
	node.mu.Unlock()
}

// Delete removes a filename→pathID mapping from the trie.
func (t *PrefixTrie) Delete(filename string, pathID uint32) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.deleteRecursive(t.root, []rune(filename), pathID, 0)
}

func (t *PrefixTrie) deleteRecursive(node *TrieNode, chars []rune, pathID uint32, depth int) bool {
	if depth == len(chars) {
		node.mu.Lock()
		// Remove pathID from slice
		idx := sort.Search(len(node.pathIDs), func(i int) bool {
			return node.pathIDs[i] >= pathID
		})
		if idx < len(node.pathIDs) && node.pathIDs[idx] == pathID {
			node.pathIDs = append(node.pathIDs[:idx], node.pathIDs[idx+1:]...)
		}
		hasPaths := len(node.pathIDs) > 0
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

	shouldPrune := t.deleteRecursive(child, chars, pathID, depth+1)
	if shouldPrune {
		node.mu.Lock()
		delete(node.children, ch)
		node.mu.Unlock()
	}

	node.mu.RLock()
	hasChildren := len(node.children) > 0
	hasPaths := len(node.pathIDs) > 0
	node.mu.RUnlock()
	return !hasChildren && !hasPaths && depth > 0
}

// Search returns all path IDs whose filenames start with the given prefix.
// The prefix should be lowercased.
// Results are returned as a sorted slice of uint32 IDs.
func (t *PrefixTrie) Search(prefix string) []uint32 {
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

	// Collect all paths in the subtree recursively
	return t.collectPaths(node)
}

func (t *PrefixTrie) collectPaths(node *TrieNode) []uint32 {
	var result []uint32
	seen := make(map[uint32]bool)

	var walk func(*TrieNode)
	walk = func(n *TrieNode) {
		n.mu.RLock()
		for _, id := range n.pathIDs {
			if !seen[id] {
				seen[id] = true
				result = append(result, id)
			}
		}
		for _, child := range n.children {
			walk(child)
		}
		n.mu.RUnlock()
	}

	walk(node)

	// Sort the resulting IDs to keep them predictable/comparable
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})

	return result
}

// NodeCount returns the total number of nodes in the trie.
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

// PathCount returns the total number of path ID references stored at terminal nodes.
func (t *PrefixTrie) PathCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.pathCount(t.root)
}

func (t *PrefixTrie) pathCount(node *TrieNode) int {
	node.mu.RLock()
	count := len(node.pathIDs)
	for _, child := range node.children {
		count += t.pathCount(child)
	}
	node.mu.RUnlock()
	return count
}
