package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/thezzisu/taper/internal/engine"
)

func TestVerificationPreviewAndConfirmation(t *testing.T) {
	end1, end2 := uint64(12), uint64(24)
	p := engine.VerifyPlan{Label: "123456L6", Serial: "cartridge", Start: 2, End: 24, CheckEOD: true, Archives: []engine.VerifyArchive{
		{ID: strings.Repeat("1", 32), Name: "first\x1b[2J", Replica: engine.Replica{Begin: 2, End: &end1}},
		{ID: strings.Repeat("2", 32), Name: "second", Replica: engine.Replica{Begin: 13, End: &end2}},
	}}
	var b bytes.Buffer
	if e := VerificationPlan(&b, "/dev/example", p); e != nil {
		t.Fatal(e)
	}
	text := b.String()
	for _, s := range []string{"Tape Label : 123456L6", "2-11", "13-23", "<Boundary Check>", "<EOD Check>", strings.Repeat("2", 32)} {
		if !strings.Contains(text, s) {
			t.Fatal("preview missing scope", s, text)
		}
	}
	if strings.Contains(text, "\x1b") {
		t.Fatal("redirected preview contains terminal control", text)
	}
	if TapeStrip("custom\x1b", true) != "custom " {
		t.Fatal("unsafe custom label")
	}
	if TapeStrip("123456L6", true) == TapeStrip("123456L6", false) {
		t.Fatal("colored tape strip was not rendered")
	}
	for _, answer := range []string{"\n", "no\n", "", "yes without newline"} {
		if Confirm(strings.NewReader(answer), &b, true) == nil {
			t.Fatal("implicit confirmation", answer)
		}
	}
	for _, answer := range []string{"y\n", "YES\n"} {
		if e := Confirm(strings.NewReader(answer), &b, true); e != nil {
			t.Fatal(e)
		}
	}
}
