package index

import (
	"path"
	"strings"
)

// NormalizePath cleans and lowercases a filesystem path for storage.
// Normalization happens exactly once at index time.
// The path is expected to be an absolute path; relative paths are
// normalized but not resolved against any base.
//
// Normalization steps:
//  1. Clean the path: remove /./, //, trailing slashes, resolve /../
//  2. Lowercase using Unicode-aware case folding
func NormalizePath(p string) string {
	cleaned := path.Clean(p)
	return strings.ToLower(cleaned)
}

// NormalizeFilename returns the lowercased base name of a path.
func NormalizeFilename(p string) string {
	return strings.ToLower(path.Base(p))
}
