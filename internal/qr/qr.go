// Package qr encodes short text, such as a URL, as a QR code that a terminal
// can show. It supports byte mode at error correction level L, versions 1 to
// 10, which holds up to 271 bytes.
package qr

import (
	"errors"
	"strings"
)

// MaxBytes is the longest text Encode accepts.
const MaxBytes = 271

// ErrTooLong reports text that does not fit in a version 10 code.
var ErrTooLong = errors.New("qr: text is longer than 271 bytes")

// block layout of error correction level L, indexed by version: total
// codewords, error correction codewords per block, and the blocks as
// (count, data codewords) groups.
var versions = [...]struct {
	total, ec int
	groups    [2][2]int
}{
	1:  {26, 7, [2][2]int{{1, 19}}},
	2:  {44, 10, [2][2]int{{1, 34}}},
	3:  {70, 15, [2][2]int{{1, 55}}},
	4:  {100, 20, [2][2]int{{1, 80}}},
	5:  {134, 26, [2][2]int{{1, 108}}},
	6:  {172, 18, [2][2]int{{2, 68}}},
	7:  {196, 20, [2][2]int{{2, 78}}},
	8:  {242, 24, [2][2]int{{2, 97}}},
	9:  {292, 30, [2][2]int{{2, 116}}},
	10: {346, 18, [2][2]int{{2, 68}, {2, 69}}},
}

var alignment = [...][]int{
	1: nil, 2: {6, 18}, 3: {6, 22}, 4: {6, 26}, 5: {6, 30}, 6: {6, 34},
	7: {6, 22, 38}, 8: {6, 24, 42}, 9: {6, 26, 46}, 10: {6, 28, 50},
}

func dataCodewords(version int) int {
	v := versions[version]
	n := 0
	for _, g := range v.groups {
		n += g[0] * g[1]
	}
	return n
}

// Encode returns the code as rows of modules; true is a dark module.
func Encode(text string) ([][]bool, error) {
	data := []byte(text)
	version := 0
	for v := 1; v < len(versions); v++ {
		countBits := 8
		if v >= 10 {
			countBits = 16
		}
		if 4+countBits+8*len(data) <= 8*dataCodewords(v) {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, ErrTooLong
	}

	codewords := interleave(version, dataStream(version, data))
	size := 17 + 4*version
	m := newMatrix(size, version)
	m.placeData(codewords)

	best, bestScore := 0, -1
	for mask := range 8 {
		m.applyMask(mask)
		m.drawFormat(mask)
		if s := m.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = mask, s
		}
		m.applyMask(mask)
	}
	m.applyMask(best)
	m.drawFormat(best)
	return m.modules, nil
}

// dataStream builds the data codewords: mode, length, bytes, terminator and
// pad codewords.
func dataStream(version int, data []byte) []byte {
	var bits bitBuffer
	bits.add(0b0100, 4)
	if version >= 10 {
		bits.add(len(data), 16)
	} else {
		bits.add(len(data), 8)
	}
	for _, b := range data {
		bits.add(int(b), 8)
	}
	capacity := 8 * dataCodewords(version)
	bits.add(0, min(4, capacity-bits.n))
	bits.add(0, (8-bits.n%8)%8)
	out := bits.bytes()
	for pad := byte(0xEC); len(out) < dataCodewords(version); pad ^= 0xEC ^ 0x11 {
		out = append(out, pad)
	}
	return out
}

type bitBuffer struct {
	buf []byte
	n   int
}

func (b *bitBuffer) add(value, count int) {
	for i := count - 1; i >= 0; i-- {
		if b.n%8 == 0 {
			b.buf = append(b.buf, 0)
		}
		if value>>i&1 == 1 {
			b.buf[b.n/8] |= 0x80 >> (b.n % 8)
		}
		b.n++
	}
}

func (b *bitBuffer) bytes() []byte { return b.buf }

// interleave splits the data into blocks, appends each block's error
// correction codewords and interleaves them in the order the standard sets.
func interleave(version int, data []byte) []byte {
	v := versions[version]
	var blocks, checks [][]byte
	offset := 0
	for _, g := range v.groups {
		for range g[0] {
			block := data[offset : offset+g[1]]
			offset += g[1]
			blocks = append(blocks, block)
			checks = append(checks, reedSolomon(block, v.ec))
		}
	}
	var out []byte
	longest := 0
	for _, b := range blocks {
		longest = max(longest, len(b))
	}
	for i := range longest {
		for _, b := range blocks {
			if i < len(b) {
				out = append(out, b[i])
			}
		}
	}
	for i := range v.ec {
		for _, c := range checks {
			out = append(out, c[i])
		}
	}
	return out
}

var exp, logt = func() (e [512]byte, l [256]int) {
	x := 1
	for i := range 255 {
		e[i] = byte(x)
		l[x] = i
		x <<= 1
		if x >= 256 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		e[i] = e[i-255]
	}
	return
}()

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return exp[logt[a]+logt[b]]
}

// reedSolomon returns the n error correction codewords of data.
func reedSolomon(data []byte, n int) []byte {
	gen := []byte{1}
	for i := range n {
		next := make([]byte, len(gen)+1)
		for j, c := range gen {
			next[j] ^= c
			next[j+1] ^= gfMul(c, exp[i])
		}
		gen = next
	}
	rem := make([]byte, n)
	for _, d := range data {
		factor := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i := range n {
			rem[i] ^= gfMul(gen[i+1], factor)
		}
	}
	return rem
}

type matrix struct {
	size     int
	version  int
	modules  [][]bool
	function [][]bool
}

func newMatrix(size, version int) *matrix {
	m := &matrix{size: size, version: version}
	m.modules = make([][]bool, size)
	m.function = make([][]bool, size)
	for i := range size {
		m.modules[i] = make([]bool, size)
		m.function[i] = make([]bool, size)
	}
	m.drawFunctionPatterns()
	return m
}

func (m *matrix) set(x, y int, dark bool) {
	m.modules[y][x] = dark
	m.function[y][x] = true
}

func (m *matrix) drawFunctionPatterns() {
	for i := range m.size {
		m.set(6, i, i%2 == 0)
		m.set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {m.size - 4, 3}, {3, m.size - 4}} {
		m.drawFinder(c[0], c[1])
	}
	pos := alignment[m.version]
	for i, y := range pos {
		for j, x := range pos {
			if (i == 0 && j == 0) || (i == 0 && j == len(pos)-1) || (i == len(pos)-1 && j == 0) {
				continue
			}
			m.drawAlignment(x, y)
		}
	}
	m.drawFormat(0)
	m.drawVersion()
}

func (m *matrix) drawFinder(cx, cy int) {
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= m.size || y >= m.size {
				continue
			}
			dist := max(abs(dx), abs(dy))
			m.set(x, y, dist != 2 && dist != 4)
		}
	}
}

func (m *matrix) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			m.set(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// formatBits is the 15-bit format information for level L and the mask.
func formatBits(mask int) int {
	data := 1<<3 | mask
	rem := data
	for range 10 {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	return (data<<10 | rem) ^ 0x5412
}

// drawFormat writes the format information for level L and the mask.
func (m *matrix) drawFormat(mask int) {
	bits := formatBits(mask)
	bit := func(i int) bool { return bits>>i&1 == 1 }
	for i := range 6 {
		m.set(8, i, bit(i))
	}
	m.set(8, 7, bit(6))
	m.set(8, 8, bit(7))
	m.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		m.set(14-i, 8, bit(i))
	}
	for i := range 8 {
		m.set(m.size-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		m.set(8, m.size-15+i, bit(i))
	}
	m.set(8, m.size-8, true)
}

func (m *matrix) drawVersion() {
	if m.version < 7 {
		return
	}
	rem := m.version
	for range 12 {
		rem = rem<<1 ^ (rem>>11)*0x1F25
	}
	bits := m.version<<12 | rem
	for i := range 18 {
		dark := bits>>i&1 == 1
		a, b := m.size-11+i%3, i/3
		m.set(a, b, dark)
		m.set(b, a, dark)
	}
}

// placeData writes the codeword bits in the zigzag order of the standard.
func (m *matrix) placeData(codewords []byte) {
	bit := 0
	total := len(codewords) * 8
	for right := m.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := range m.size {
			for j := range 2 {
				x := right - j
				upward := (right+1)&2 == 0
				y := vert
				if upward {
					y = m.size - 1 - vert
				}
				if m.function[y][x] || bit >= total {
					continue
				}
				m.modules[y][x] = codewords[bit/8]>>(7-bit%8)&1 == 1
				bit++
			}
		}
	}
}

func (m *matrix) applyMask(mask int) {
	for y := range m.size {
		for x := range m.size {
			if m.function[y][x] {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (x+y)%2 == 0
			case 1:
				invert = y%2 == 0
			case 2:
				invert = x%3 == 0
			case 3:
				invert = (x+y)%3 == 0
			case 4:
				invert = (x/3+y/2)%2 == 0
			case 5:
				invert = x*y%2+x*y%3 == 0
			case 6:
				invert = (x*y%2+x*y%3)%2 == 0
			case 7:
				invert = ((x+y)%2+x*y%3)%2 == 0
			}
			if invert {
				m.modules[y][x] = !m.modules[y][x]
			}
		}
	}
}

// penalty scores the mask by the standard's four rules; lower is better.
func (m *matrix) penalty() int {
	score := 0
	dark := 0
	line := func(get func(i int) bool) {
		run := 1
		var window uint32
		for i := range m.size {
			if i > 0 && get(i) == get(i-1) {
				run++
				if run == 5 {
					score += 3
				} else if run > 5 {
					score++
				}
			} else {
				run = 1
			}
			window = window<<1 | b2u(get(i))
			if i >= 10 {
				if p := window & 0x7FF; p == 0b10111010000 || p == 0b00001011101 {
					score += 40
				}
			}
		}
	}
	for y := range m.size {
		line(func(i int) bool { return m.modules[y][i] })
	}
	for x := range m.size {
		line(func(i int) bool { return m.modules[i][x] })
	}
	for y := range m.size {
		for x := range m.size {
			if m.modules[y][x] {
				dark++
			}
			if x+1 < m.size && y+1 < m.size {
				c := m.modules[y][x]
				if c == m.modules[y][x+1] && c == m.modules[y+1][x] && c == m.modules[y+1][x+1] {
					score += 3
				}
			}
		}
	}
	total := m.size * m.size
	score += ((abs(dark*20-total*10)+total-1)/total - 1) * 10
	return score
}

func b2u(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}

// Render draws the code for a terminal, two module rows per text row, black
// on white with a two-module quiet zone, so it scans on any terminal theme.
func Render(modules [][]bool) string {
	const quiet = 2
	size := len(modules) + 2*quiet
	at := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return y >= 0 && x >= 0 && y < len(modules) && x < len(modules) && modules[y][x]
	}
	var b strings.Builder
	for y := 0; y < size; y += 2 {
		b.WriteString("\x1b[30;47m")
		for x := range size {
			top, bottom := at(x, y), at(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}
