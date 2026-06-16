// Package altscreen reconstructs the current visible state of an
// alt-screen TUI (e.g. Claude Code) from a window of raw pty output,
// and serializes it back into a self-contained repaint sequence.
//
// Why this exists: the reattach replay (computeReplayWindow) ships a
// raw byte window. An alt-screen app draws stable content (status
// footer, box borders) once and then only emits tiny relative redraws
// of the active region, so that stable content is OLDER than any
// bounded byte window — a reattaching client clears its alt-screen and
// rebuilds it WITHOUT the footer. A 2-D screen cannot be rebuilt from a
// mid-stream byte offset; it must be reconstructed from the cell grid.
// This package emulates the (small) subset of VT operations Claude
// emits, replays the retained ring through it, and emits a deterministic
// full-screen repaint that reproduces the exact present screen.
package altscreen

import (
	"strconv"
	"unicode/utf8"
)

type attr struct {
	fg, bg                            int // -1 default, 0..255 indexed
	bold, dim, italic, underline, rev bool
}

var defaultAttr = attr{fg: -1, bg: -1}

type cell struct {
	r rune
	a attr
}

// Screen is a fixed rows×cols alt-screen grid with a cursor.
type Screen struct {
	rows, cols int
	grid       [][]cell
	x, y       int
	cur        attr
	wrapNext   bool
	// faithful is cleared if Feed sees a content-restructuring op the model
	// does NOT emulate (insert/delete line, scroll region, insert/delete char,
	// reverse index…). Claude never emits those; vim/htop do. The caller uses
	// it to fall back to the raw byte-window replay instead of a wrong screen.
	faithful bool
}

func New(rows, cols int) *Screen {
	s := &Screen{rows: rows, cols: cols, cur: defaultAttr, faithful: true}
	s.grid = make([][]cell, rows)
	for r := range s.grid {
		s.grid[r] = make([]cell, cols)
		for c := range s.grid[r] {
			s.grid[r][c] = cell{r: ' ', a: defaultAttr}
		}
	}
	return s
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Feed replays a chunk of raw pty bytes into the grid.
func (s *Screen) Feed(p []byte) {
	i := 0
	for i < len(p) {
		b := p[i]
		switch {
		case b == 0x1b:
			i += s.escape(p[i:])
		case b == '\r':
			s.x = 0
			s.wrapNext = false
			i++
		case b == '\n':
			s.lineFeed()
			i++
		case b == '\b':
			if s.x > 0 {
				s.x--
			}
			s.wrapNext = false
			i++
		case b == '\t':
			s.x = clamp((s.x/8+1)*8, 0, s.cols-1)
			i++
		case b < 0x20:
			i++ // ignore other C0 controls
		default:
			r, sz := utf8.DecodeRune(p[i:])
			if r == utf8.RuneError && sz <= 1 {
				i++
				continue
			}
			s.putRune(r)
			i += sz
		}
	}
}

func (s *Screen) lineFeed() {
	s.wrapNext = false
	if s.y >= s.rows-1 {
		s.scrollUp()
	} else {
		s.y++
	}
}

func (s *Screen) scrollUp() {
	copy(s.grid[0:], s.grid[1:])
	last := make([]cell, s.cols)
	for c := range last {
		last[c] = cell{r: ' ', a: defaultAttr}
	}
	s.grid[s.rows-1] = last
}

func (s *Screen) putRune(r rune) {
	if s.wrapNext {
		s.x = 0
		s.lineFeed()
		s.wrapNext = false
	}
	if s.x >= s.cols {
		s.x = s.cols - 1
	}
	s.grid[s.y][s.x] = cell{r: r, a: s.cur}
	if s.x == s.cols-1 {
		s.wrapNext = true // DECAWM: defer the wrap until the next rune
	} else {
		s.x++
	}
}

// escape consumes one escape sequence starting at p[0]==0x1b, returns
// the number of bytes consumed.
func (s *Screen) escape(p []byte) int {
	if len(p) < 2 {
		return 1
	}
	switch p[1] {
	case '[': // CSI
		return s.csi(p)
	case ']': // OSC — skip to BEL or ST
		j := 2
		for j < len(p) && p[j] != 0x07 {
			if p[j] == 0x1b && j+1 < len(p) && p[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		if j < len(p) {
			return j + 1
		}
		return j
	case '(', ')', '*', '+': // charset designation: ESC ( X
		return 3
	case '=', '>': // keypad mode
		return 2
	case 'D': // IND — index (line feed)
		s.lineFeed()
		return 2
	case 'E': // NEL — next line (CR + line feed)
		s.x = 0
		s.lineFeed()
		return 2
	case 'M': // RI — reverse index; scrolls at the top margin (not modeled)
		s.faithful = false
		return 2
	default:
		return 2
	}
}

func (s *Screen) csi(p []byte) int {
	// p[0]=ESC p[1]='['; params until a final byte 0x40..0x7e
	j := 2
	priv := byte(0)
	if j < len(p) && (p[j] == '?' || p[j] == '>' || p[j] == '!') {
		priv = p[j]
		j++
	}
	ps := j
	for j < len(p) && !(p[j] >= 0x40 && p[j] <= 0x7e) {
		j++
	}
	if j >= len(p) {
		return len(p)
	}
	final := p[j]
	params := parseParams(p[ps:j])
	consumed := j + 1
	if priv == '?' {
		// private modes (cursor vis, mouse, bracketed paste, alt-screen…)
		// none affect the reconstructed grid except a hypothetical 1049,
		// which doesn't appear within the ring; ignore.
		return consumed
	}
	p1 := param(params, 0, 1)
	switch final {
	case 'H', 'f': // CUP row;col (1-based)
		s.y = clamp(param(params, 0, 1)-1, 0, s.rows-1)
		s.x = clamp(param(params, 1, 1)-1, 0, s.cols-1)
		s.wrapNext = false
	case 'A': // CUU
		s.y = clamp(s.y-p1, 0, s.rows-1)
		s.wrapNext = false
	case 'B': // CUD
		s.y = clamp(s.y+p1, 0, s.rows-1)
		s.wrapNext = false
	case 'C': // CUF
		s.x = clamp(s.x+p1, 0, s.cols-1)
		s.wrapNext = false
	case 'D': // CUB
		s.x = clamp(s.x-p1, 0, s.cols-1)
		s.wrapNext = false
	case 'G', '`': // CHA — column absolute (1-based)
		s.x = clamp(param(params, 0, 1)-1, 0, s.cols-1)
		s.wrapNext = false
	case 'd': // VPA — row absolute
		s.y = clamp(param(params, 0, 1)-1, 0, s.rows-1)
		s.wrapNext = false
	case 'E': // CNL
		s.y = clamp(s.y+p1, 0, s.rows-1)
		s.x = 0
	case 'F': // CPL
		s.y = clamp(s.y-p1, 0, s.rows-1)
		s.x = 0
	case 'J': // ED
		s.eraseDisplay(param(params, 0, 0))
	case 'K': // EL
		s.eraseLine(param(params, 0, 0))
	case 'm': // SGR
		s.sgr(params)
	default:
		// CSI we don't model. Most unknown finals are benign (cursor style,
		// etc.), but a few RESTRUCTURE content — insert/delete line (L/M),
		// insert/delete char (@/P), scroll up/down (S/T), scroll region (r),
		// erase char (X). Claude emits none; vim/htop rely on them. Seeing one
		// means the reconstruction can't be trusted, so mark unfaithful and let
		// the caller keep the raw byte-window replay.
		switch final {
		case 'L', 'M', '@', 'P', 'S', 'T', 'r', 'X':
			s.faithful = false
		}
	}
	return consumed
}

func (s *Screen) eraseLine(mode int) {
	row := s.grid[s.y]
	switch mode {
	case 0: // cursor → end
		for c := s.x; c < s.cols; c++ {
			row[c] = cell{r: ' ', a: defaultAttr}
		}
	case 1: // start → cursor
		for c := 0; c <= s.x && c < s.cols; c++ {
			row[c] = cell{r: ' ', a: defaultAttr}
		}
	case 2: // whole line
		for c := 0; c < s.cols; c++ {
			row[c] = cell{r: ' ', a: defaultAttr}
		}
	}
}

func (s *Screen) eraseDisplay(mode int) {
	blank := func(r int) {
		for c := 0; c < s.cols; c++ {
			s.grid[r][c] = cell{r: ' ', a: defaultAttr}
		}
	}
	switch mode {
	case 0: // cursor → end of screen
		s.eraseLine(0)
		for r := s.y + 1; r < s.rows; r++ {
			blank(r)
		}
	case 1: // start → cursor
		for r := 0; r < s.y; r++ {
			blank(r)
		}
		s.eraseLine(1)
	case 2, 3: // whole screen
		for r := 0; r < s.rows; r++ {
			blank(r)
		}
	}
}

func (s *Screen) sgr(params []int) {
	if len(params) == 0 {
		s.cur = defaultAttr
		return
	}
	for i := 0; i < len(params); i++ {
		switch n := params[i]; {
		case n == 0:
			s.cur = defaultAttr
		case n == 1:
			s.cur.bold = true
		case n == 2:
			s.cur.dim = true
		case n == 3:
			s.cur.italic = true
		case n == 4:
			s.cur.underline = true
		case n == 7:
			s.cur.rev = true
		case n == 22:
			s.cur.bold, s.cur.dim = false, false
		case n == 23:
			s.cur.italic = false
		case n == 24:
			s.cur.underline = false
		case n == 27:
			s.cur.rev = false
		case n == 38:
			if i+2 < len(params) && params[i+1] == 5 {
				s.cur.fg = params[i+2]
				i += 2
			}
		case n == 48:
			if i+2 < len(params) && params[i+1] == 5 {
				s.cur.bg = params[i+2]
				i += 2
			}
		case n == 39:
			s.cur.fg = -1
		case n == 49:
			s.cur.bg = -1
		case n >= 30 && n <= 37:
			s.cur.fg = n - 30
		case n >= 40 && n <= 47:
			s.cur.bg = n - 40
		case n >= 90 && n <= 97:
			s.cur.fg = n - 90 + 8
		case n >= 100 && n <= 107:
			s.cur.bg = n - 100 + 8
		}
	}
}

func parseParams(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	out := []int{}
	cur, has := 0, false
	for _, c := range b {
		if c >= '0' && c <= '9' {
			cur = cur*10 + int(c-'0')
			has = true
		} else if c == ';' {
			out = append(out, cur)
			cur, has = 0, false
		}
	}
	out = append(out, cur)
	_ = has
	return out
}

func param(p []int, idx, def int) int {
	if idx < len(p) && p[idx] != 0 {
		return p[idx]
	}
	if idx < len(p) {
		return p[idx] // explicit 0 stays 0
	}
	return def
}

func (a attr) sgrCodes() []int {
	if a == defaultAttr {
		return []int{0}
	}
	out := []int{0}
	if a.bold {
		out = append(out, 1)
	}
	if a.dim {
		out = append(out, 2)
	}
	if a.italic {
		out = append(out, 3)
	}
	if a.underline {
		out = append(out, 4)
	}
	if a.rev {
		out = append(out, 7)
	}
	if a.fg >= 0 {
		out = append(out, 38, 5, a.fg)
	}
	if a.bg >= 0 {
		out = append(out, 48, 5, a.bg)
	}
	return out
}

// Repaint serializes the current grid into a self-contained repaint:
// home, erase, then per-row positioned text with SGR runs, trailing
// blanks trimmed. The cursor is restored last. The caller is expected
// to have already switched the client into the alt buffer.
func (s *Screen) Repaint() []byte {
	var out []byte
	out = append(out, []byte("\x1b[H\x1b[2J")...)
	last := attr{fg: -2} // force first SGR emit
	emit := func(a attr) {
		if a == last {
			return
		}
		out = append(out, []byte("\x1b[")...)
		codes := a.sgrCodes()
		for i, c := range codes {
			if i > 0 {
				out = append(out, ';')
			}
			out = append(out, []byte(strconv.Itoa(c))...)
		}
		out = append(out, 'm')
		last = a
	}
	for r := 0; r < s.rows; r++ {
		// rightmost non-blank column on this row
		end := -1
		for c := s.cols - 1; c >= 0; c-- {
			g := s.grid[r][c]
			if g.r != ' ' || g.a != defaultAttr {
				end = c
				break
			}
		}
		if end < 0 {
			continue
		}
		out = append(out, []byte("\x1b["+strconv.Itoa(r+1)+";1H")...)
		buf := make([]byte, 4)
		for c := 0; c <= end; c++ {
			g := s.grid[r][c]
			emit(g.a)
			n := utf8.EncodeRune(buf, g.r)
			out = append(out, buf[:n]...)
		}
	}
	emit(defaultAttr)
	out = append(out, []byte("\x1b["+strconv.Itoa(s.y+1)+";"+strconv.Itoa(s.x+1)+"H")...)
	return out
}

// Reconstruct replays ring through a fresh Screen and returns the repaint plus
// whether the model stayed faithful (false if it hit a content-restructuring op
// it doesn't emulate — the caller should then fall back to the raw byte-window
// replay rather than ship a possibly-wrong screen).
func Reconstruct(ring []byte, rows, cols int) (repaint []byte, faithful bool) {
	s := New(rows, cols)
	s.Feed(ring)
	return s.Repaint(), s.faithful
}
