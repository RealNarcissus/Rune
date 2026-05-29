package index

import (
	"path"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// NormalizeUnicode normalizes a string using Unicode NFC (Normalization Form C).
func NormalizeUnicode(s string) string {
	return norm.NFC.String(s)
}

// NormalizePath cleans and lowercases a filesystem path consistently.
func NormalizePath(p string) string {
	p = NormalizeUnicode(p)
	p = strings.ReplaceAll(p, "\\", "/")
	cleaned := path.Clean(p)
	cleaned = strings.TrimRight(cleaned, "/")
	return strings.ToLower(cleaned)
}

// NormalizeFilename returns the lowercased base name of a path.
func NormalizeFilename(p string) string {
	p = NormalizeUnicode(p)
	parts := strings.Split(p, "/")
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(parts[len(parts)-1])
}
