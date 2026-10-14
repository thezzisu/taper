package archive

import (
	"path"
	"regexp"
	"strings"
)

// IsArchiveName selects user archives, not application packages or file internals.
// The same rule applies at the source root and at every nested archive boundary.
func IsArchiveName(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	if splitStart.MatchString(ext) {
		stem := name[:len(name)-len(ext)]
		return path.Ext(stem) == "" || IsArchiveName(stem)
	}
	switch ext {
	case ".7z", ".zip", ".zipx", ".rar", ".tar", ".cpio", ".arj", ".lha", ".lzh", ".pmd",
		".gz", ".gzip", ".tgz", ".bz2", ".bzip2", ".tbz", ".tbz2", ".xz", ".txz",
		".zst", ".zstd", ".tzst", ".lzma", ".lzma86", ".z", ".taz":
		return true
	}
	return false
}

// IsArchiveFormat prevents a misleading suffix from exposing a non-archive's
// implementation details. Split streams are already joined by the reader.
func IsArchiveFormat(format string) bool {
	switch format {
	case "7z", "zip", "Rar", "Rar5", "tar", "Cpio", "Arj", "Lzh", "Ppmd",
		"gzip", "bzip2", "xz", "zstd", "lzma", "lzma86", "Z":
		return true
	}
	return false
}

var splitStart = regexp.MustCompile(`(?i)^\.(?:0+[01]|[a-z]*aa)$`)
