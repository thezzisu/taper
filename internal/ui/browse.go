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
		fmt.Fprint(out, "\n编号进入目录，u 上一级，p 显示当前恢复命令，q 退出： ")
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
			writeCommands(out, m.ID.String(), strings.TrimSuffix(prefix, "/"))
			fmt.Fprint(out, "Enter 返回： ")
			reader.ReadString('\n')
		default:
			n, e := strconv.Atoi(line)
			if e == nil && n > 0 && n <= len(names) {
				child := children[names[n-1]]
				if child.dir {
					prefix += child.name + "/"
				} else {
					writeCommands(out, m.ID.String(), prefix+child.name)
					fmt.Fprint(out, "Enter 返回： ")
					reader.ReadString('\n')
				}
			}
		}
	}
}

func writeCommands(out io.Writer, batch, prefix string) {
	suffix := ""
	if prefix != "" {
		suffix = " '" + strings.ReplaceAll(prefix, "'", "'\\''") + "'"
	}
	fmt.Fprintf(out, "\ntaper restore --plan --batch %s%s\ntaper restore --batch %s --device /dev/sg1 --dest /path/to/restore%s\n", batch, suffix, batch, suffix)
}
