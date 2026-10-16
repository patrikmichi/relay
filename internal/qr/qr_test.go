package qr

import (
	"bytes"
	"strings"
	"testing"
)

func TestReedSolomonMatchesTheStandardsWorkedExample(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := reedSolomon(data, 10); !bytes.Equal(got, want) {
		t.Fatalf("reedSolomon = %v, want %v", got, want)
	}
}

func TestFormatBitsForLevelL(t *testing.T) {
	if got := formatBits(0); got != 0x77C4 {
		t.Errorf("mask 0 = %#x, want 0x77c4", got)
	}
	if got := formatBits(1); got != 0x72F3 {
		t.Errorf("mask 1 = %#x, want 0x72f3", got)
	}
}

// decode reads a code back the way a scanner does: format information, the
// mask, the zigzag, de-interleaving, and a check of every block's error
// correction codewords.
func decode(t *testing.T, modules [][]bool) string {
	t.Helper()
	size := len(modules)
	version := (size - 17) / 4
	m := newMatrix(size, version)

	var format int
	for i := range 15 {
		var dark bool
		switch {
		case i < 6:
			dark = modules[i][8]
		case i == 6:
			dark = modules[7][8]
		case i == 7:
			dark = modules[8][8]
		case i == 8:
			dark = modules[8][7]
		default:
			dark = modules[8][14-i]
		}
		if dark {
			format |= 1 << i
		}
	}
	mask := -1
	for candidate := range 8 {
		if formatBits(candidate) == format {
			mask = candidate
		}
	}
	if mask < 0 {
		t.Fatalf("format information %#x matches no mask", format)
	}

	copy(m.modules, modules)
	m.applyMask(mask)
	stream := make([]byte, 0, versions[version].total)
	var bit int
	var cur byte
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := range size {
			for j := range 2 {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = size - 1 - vert
				}
				if m.function[y][x] {
					continue
				}
				cur = cur<<1 | byte(b2u(m.modules[y][x]))
				bit++
				if bit%8 == 0 {
					stream = append(stream, cur)
					cur = 0
				}
			}
		}
	}
	stream = stream[:versions[version].total]

	v := versions[version]
	var blockLens []int
	for _, g := range v.groups {
		for range g[0] {
			blockLens = append(blockLens, g[1])
		}
	}
	blocks := make([][]byte, len(blockLens))
	pos := 0
	for i := range blockLens[len(blockLens)-1] {
		for b, n := range blockLens {
			if i < n {
				blocks[b] = append(blocks[b], stream[pos])
				pos++
			}
		}
	}
	var data []byte
	for _, block := range blocks {
		data = append(data, block...)
	}
	for i := range v.ec {
		for b := range blocks {
			got := stream[pos]
			pos++
			if want := reedSolomon(blocks[b], v.ec)[i]; got != want {
				t.Fatalf("block %d error correction codeword %d = %d, want %d", b, i, got, want)
			}
		}
	}

	var r bitReader
	r.data = data
	if mode := r.read(4); mode != 0b0100 {
		t.Fatalf("mode = %b, want byte mode", mode)
	}
	countBits := 8
	if version >= 10 {
		countBits = 16
	}
	n := r.read(countBits)
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(r.read(8))
	}
	return string(out)
}

type bitReader struct {
	data []byte
	n    int
}

func (r *bitReader) read(count int) int {
	v := 0
	for range count {
		v = v<<1 | int(r.data[r.n/8]>>(7-r.n%8)&1)
		r.n++
	}
	return v
}

func TestEncodeRoundTripsAcrossEveryVersion(t *testing.T) {
	for _, n := range []int{1, 10, 17, 18, 32, 53, 78, 106, 134, 154, 192, 230, 271} {
		text := strings.Repeat("https://zuano.example/approvals/", 9)[:n]
		modules, err := Encode(text)
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if got := decode(t, modules); got != text {
			t.Errorf("%d bytes: decoded %q", n, got)
		}
	}
}

func TestEncodeDrawsTheFixedPatterns(t *testing.T) {
	modules, err := Encode("hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 21 {
		t.Fatalf("size = %d, want 21 for version 1", len(modules))
	}
	for _, corner := range [][2]int{{0, 0}, {14, 0}, {0, 14}} {
		x, y := corner[0], corner[1]
		for i := range 7 {
			if !modules[y][x+i] || !modules[y+6][x+i] || !modules[y+i][x] || !modules[y+i][x+6] {
				t.Fatalf("finder at %v has a light border module", corner)
			}
		}
		if !modules[y+3][x+3] || modules[y+1][x+1] {
			t.Fatalf("finder at %v is not ring, ring, dot", corner)
		}
	}
	if !modules[13][8] {
		t.Error("the dark module next to the bottom-left finder is missing")
	}
}

func TestEncodeRejectsTextThatDoesNotFit(t *testing.T) {
	if _, err := Encode(strings.Repeat("a", MaxBytes+1)); err != ErrTooLong {
		t.Fatalf("err = %v, want ErrTooLong", err)
	}
}

func TestRenderPairsRowsAndResetsColors(t *testing.T) {
	out := Render([][]bool{{true, false}, {false, true}})
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d text rows, want 3 for 6 module rows", len(lines))
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "\x1b[30;47m") || !strings.HasSuffix(l, "\x1b[0m") {
			t.Errorf("row %q is not wrapped in the color codes", l)
		}
	}
	if !strings.Contains(lines[1], "▀▄") {
		t.Errorf("middle row %q should show a top-dark then a bottom-dark module", lines[1])
	}
}
