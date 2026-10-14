package ui

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/rivo/uniseg"
	"github.com/thezzisu/taper/internal/engine"
	"golang.org/x/term"
)

func IsTerminal(v any) bool {
	f, ok := v.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}

func ColorEnabled(w io.Writer) bool {
	return IsTerminal(w) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

func Safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

var ltoLabel = regexp.MustCompile(`^[A-Z0-9]{6}(L[1-9A]|M8)$`)
var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// IBM-style digit colors in the standard 256-color terminal palette:
// red, yellow, light green, light blue, gray, orange, pink, dark green,
// light orange, purple. Letters and the media identifier stay neutral.
var digitColors = [...]int{196, 226, 113, 117, 250, 208, 205, 28, 215, 141}

func TapeLabel(label string, color, strip bool) string {
	if !color || !ltoLabel.MatchString(label) {
		return Safe(label)
	}
	var b strings.Builder
	for i := 0; i < 7; i++ {
		part := label[i : i+1]
		if i == 6 {
			part = label[6:]
		}
		bg, fg := 255, 16
		if i < 6 && part[0] >= '0' && part[0] <= '9' {
			bg = digitColors[part[0]-'0']
			if part == "7" || part == "9" {
				fg = 255
			}
		}
		if strip {
			part = "[ " + part + " ]"
		}
		fmt.Fprintf(&b, "\x1b[38;5;%d;48;5;%dm%s\x1b[0m", fg, bg, part)
	}
	return b.String()
}

func TapeHeader(w io.Writer, label, serial, device string) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "Tape Label : %s\n", Safe(label))
	if ColorEnabled(w) && ltoLabel.MatchString(label) {
		fmt.Fprintf(&b, "             %s\n", TapeLabel(label, true, true))
	}
	fmt.Fprintf(&b, "Tape Serial: %s\nDevice     : %s\n", Safe(serial), Safe(device))
	_, err := w.Write(b.Bytes())
	return err
}

type Row struct {
	Cells []string
	Muted bool
}

func Table(w io.Writer, headers []string, rows []Row) error {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = uniseg.StringWidth(h)
	}
	for _, r := range rows {
		for i, cell := range r.Cells {
			widths[i] = max(widths[i], uniseg.StringWidth(sgr.ReplaceAllString(cell, "")))
		}
	}
	var b bytes.Buffer
	line := func(cells []string, muted bool) {
		if muted && ColorEnabled(w) {
			b.WriteString("\x1b[90m")
		}
		for i, cell := range cells {
			b.WriteString(cell)
			if i+1 < len(cells) {
				b.WriteString(strings.Repeat(" ", widths[i]-uniseg.StringWidth(sgr.ReplaceAllString(cell, ""))+2))
			}
		}
		if muted && ColorEnabled(w) {
			b.WriteString("\x1b[0m")
		}
		b.WriteByte('\n')
	}
	line(headers, false)
	for _, r := range rows {
		line(r.Cells, r.Muted)
	}
	_, err := w.Write(b.Bytes())
	return err
}

func VerificationPlan(w io.Writer, device string, p engine.VerifyPlan) error {
	if err := TapeHeader(w, p.Label, p.Serial, device); err != nil {
		return err
	}
	fmt.Fprintln(w)
	var rows []Row
	if p.Whole {
		rows = append(rows, Row{[]string{"<Label Check>", "0-1", "Tape label and filemark"}, true})
	}
	previous := p.Start
	if p.Whole {
		previous = 2
	}
	for _, a := range p.Archives {
		if a.Replica.Begin > previous {
			rows = append(rows, Row{[]string{"<Boundary Check>", "-", "Inter-archive filemarks"}, true})
		}
		rows = append(rows, Row{[]string{a.ID, fmt.Sprintf("%d-%d", a.Replica.Begin, *a.Replica.End-1), Safe(a.Name)}, false})
		previous = *a.Replica.End
	}
	if p.CheckEOD {
		rows = append(rows, Row{[]string{"<EOD Check>", "-", "End of data"}, true})
	}
	return Table(w, []string{"ARCHIVE ID", "OBJECTS", "NAME"}, rows)
}

func Confirm(in io.Reader, out io.Writer, interactive bool) error {
	if !interactive {
		return errors.New("confirmation requires a terminal; use --yes or --dry-run")
	}
	if _, err := fmt.Fprint(out, "\nContinue? [y/N] "); err != nil {
		return err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return fmt.Errorf("confirmation cancelled: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return errors.New("cancelled")
	}
}
