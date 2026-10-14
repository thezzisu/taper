package ui

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/thezzisu/taper/internal/archive"
	"github.com/thezzisu/taper/internal/engine"
)

type browseEntry struct {
	path             string
	size             int64
	hash             string
	offset           int64
	directory        bool
	state, container string
}

func browseEntries(m engine.Manifest, index []engine.ArchiveEntry) []browseEntry {
	var result []browseEntry
	for _, c := range m.Contents {
		for _, f := range c.Files {
			result = append(result, browseEntry{path: f.Path, size: c.Size, hash: c.Hash, offset: c.Data})
		}
	}
	for _, entry := range index {
		for _, outer := range m.Contents[entry.Ordinal].Files {
			if !archive.IsArchiveName(outer.Path) {
				continue
			}
			p := outer.Path + "!/" + entry.Path
			if entry.Entry != "" {
				result = append(result, browseEntry{path: p, size: max(entry.Size, 0), directory: entry.Kind == "directory", state: entry.State, container: outer.Path})
			}
			if entry.Kind == "archive" {
				if entry.Entry != "" {
					p += "!/"
				}
				result = append(result, browseEntry{path: p, directory: true, state: entry.State, container: outer.Path})
			}
		}
	}
	return result
}

// Browse only uses catalog data, including cached container directory trees.
func Browse(m engine.Manifest, reps []engine.Replica, index []engine.ArchiveEntry, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	entries := browseEntries(m, index)
	prefix := ""
	for {
		type node struct {
			name string
			dir  bool
			size int64
		}
		children := map[string]*node{}
		for _, f := range entries {
			if !strings.HasPrefix(f.path, prefix) {
				continue
			}
			rest := strings.TrimPrefix(f.path, prefix)
			if rest == "" {
				continue
			}
			parts := strings.SplitN(rest, "/", 2)
			n := children[parts[0]]
			if n == nil {
				n = &node{name: parts[0]}
				children[n.name] = n
			}
			n.dir = n.dir || len(parts) > 1 || f.directory
			if !f.directory {
				n.size += f.size
			}
		}
		names := make([]string, 0, len(children))
		for n := range children {
			names = append(names, n)
		}
		sort.Strings(names)
		if IsTerminal(out) {
			fmt.Fprint(out, "\033[2J\033[H")
		}
		fmt.Fprintf(out, "%s / %s\n\n", m.Name, prefix)
		for i, n := range names {
			child := children[n]
			suffix := ""
			if child.dir {
				suffix = "/"
			}
			fmt.Fprintf(out, "%4d %-55s %.2f MiB\n", i+1, n+suffix, float64(child.size)/(1<<20))
		}
		fmt.Fprintln(out, "\nReplica tapes:")
		for _, r := range reps {
			fmt.Fprintf(out, "  %s [%s] verified=%t\n", TapeLabel(r.Label, ColorEnabled(out), false), r.Media, r.Verified)
		}
		fmt.Fprint(out, "\nNumber: open | u: parent | p: details | q: quit > ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimSpace(line)
		switch line {
		case "q":
			return nil
		case "u":
			prefix = path.Dir(strings.TrimSuffix(prefix, "/"))
			if prefix == "." {
				prefix = ""
			} else {
				prefix += "/"
			}
		case "p":
			writeDetails(out, m, entries, strings.TrimSuffix(prefix, "/"))
			fmt.Fprint(out, "Press Enter to return: ")
			reader.ReadString('\n')
		default:
			n, err := strconv.Atoi(line)
			if err == nil && n > 0 && n <= len(names) {
				child := children[names[n-1]]
				if child.dir {
					prefix += child.name + "/"
				} else {
					writeDetails(out, m, entries, prefix+child.name)
					fmt.Fprint(out, "Press Enter to return: ")
					reader.ReadString('\n')
				}
			}
		}
	}
}

func writeDetails(out io.Writer, m engine.Manifest, entries []browseEntry, prefix string) {
	fmt.Fprintf(out, "\nArchive: %s\nPath: /%s\n", m.ID.String(), prefix)
	count := 0
	var size int64
	for _, f := range entries {
		p := strings.TrimSuffix(f.path, "/")
		if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			if !f.directory {
				count++
				size += f.size
			}
			if p == prefix {
				if f.hash != "" {
					fmt.Fprintf(out, "SHA-256: %s\nStream offset: %d\n", f.hash, f.offset)
				}
				if f.container != "" {
					fmt.Fprintf(out, "Container: %s\n", f.container)
				}
				if f.state != "" {
					fmt.Fprintf(out, "Index: %s\n", f.state)
				}
			}
		}
	}
	fmt.Fprintf(out, "Files: %d; listed bytes: %d\n", count, size)
}
