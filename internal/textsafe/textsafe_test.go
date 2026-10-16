package textsafe

import "testing"

func TestCleanDropsC0AndC1Controls(t *testing.T) {
	in := "ok\x1b[2J\x07\r\nline\u009b31m\u0085end\x7f"
	if got := Clean(in); got != "ok[2Jline31mend" {
		t.Fatalf("Clean = %q", got)
	}
}

func TestStrictAlsoDropsFormatCharacters(t *testing.T) {
	override, zeroWidth, tag := string(rune(0x202e)), string(rune(0x200b)), string(rune(0xe0041))
	in := "a" + override + "b" + zeroWidth + "c" + tag + "d\x1b[2Jeé"
	if got := Strict(in); got != "abcd[2Jeé" {
		t.Fatalf("Strict = %q", got)
	}
	if Clean(in) == Strict(in) {
		t.Fatal("Clean must keep format characters")
	}
}

func TestBlockKeepsNewlinesAndTabs(t *testing.T) {
	if got := Block("a\tb\nc\x1bd\u009be"); got != "a\tb\ncde" {
		t.Fatalf("Block = %q", got)
	}
}

func TestRevealMakesControlsVisible(t *testing.T) {
	if got := Reveal("rm\x1b[2K -rf\n"); got != "rm\\x1b[2K -rf\n" {
		t.Fatalf("Reveal = %q", got)
	}
}
