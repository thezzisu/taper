package ui

import (
	"bufio"
	"fmt"
	"github.com/thezzisu/taper/internal/engine"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Browse lists one directory at a time; the catalog remains usable with no tape.
func Browse(m engine.Manifest, reps []engine.Replica, in io.Reader, out io.Writer) error {
	reader := bufio.NewReader(in)
	prefix := ""
	for {
		type node struct {
			name string
			dir  bool
			size int64
		}
		children := map[string]*node{}
		for _, c := range m.Contents {
			for _, f := range c.Files {
				if !strings.HasPrefix(f.Path, prefix) {
					continue
				}
				rest := strings.TrimPrefix(f.Path, prefix)
				parts := strings.SplitN(rest, "/", 2)
				n := children[parts[0]]
				if n == nil {
					n = &node{name: parts[0], dir: len(parts) > 1}
					children[n.name] = n
				}
				n.size += c.Size
			}
		}
		names := []string{}
		for n := range children {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprint(out, "\033[2J\033[H")
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
			fmt.Fprintf(out, "  %s [%s] verified=%t\n", r.Label, r.Media, r.Verified)
		}
		fmt.Fprint(out, "\n编号进入目录，u 上一级，p 查看详情，q 退出： ")
		line, e := reader.ReadString('\n')
		if e != nil {
			return e
		}
		line = strings.TrimSpace(line)
		switch line {
		case "q":
			return nil
		case "u":
			prefix = strings.TrimSuffix(prefix, "/")
			prefix = path.Dir(prefix)
			if prefix == "." {
				prefix = ""
			} else {
				prefix += "/"
			}
		case "p":
			writeDetails(out, m, strings.TrimSuffix(prefix, "/"))
			fmt.Fprint(out, "Enter 返回： ")
			reader.ReadString('\n')
		default:
			n, e := strconv.Atoi(line)
			if e == nil && n > 0 && n <= len(names) {
				child := children[names[n-1]]
				if child.dir {
					prefix += child.name + "/"
				} else {
					writeDetails(out, m, prefix+child.name)
					fmt.Fprint(out, "Enter 返回： ")
					reader.ReadString('\n')
				}
			}
		}
	}
}

func writeDetails(out io.Writer, m engine.Manifest, prefix string) {
	fmt.Fprintf(out, "\nArchive: %s\nPath: /%s\n", m.ID.String(), prefix)
	count := 0
	var size int64
	for _, c := range m.Contents {
		for _, f := range c.Files {
			if prefix == "" || f.Path == prefix || strings.HasPrefix(f.Path, prefix+"/") {
				count++
				size += c.Size
				if f.Path == prefix {
					fmt.Fprintf(out, "SHA-256: %s\nStream offset: %d\n", c.Hash, c.Data)
				}
			}
		}
	}
	fmt.Fprintf(out, "Files: %d; bytes: %d\n", count, size)
}
