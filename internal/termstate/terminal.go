// Package termstate is Pomar's own model of a terminal's state: the screen
// a program has drawn, both buffers, the cursor, the rendition, the modes
// and the history. A seat's session host feeds it every byte its program
// writes, so that a client joining later gets the screen as it stands
// (POMAR-CC SOW 27, made agent-agnostic by SOW 28), and so that queries
// the program sends its terminal are answered by the host rather than by
// whichever clients happen to be attached.
//
// It knows nothing about which program it models. What it can't represent
// is listed by Limits, never papered over.
package termstate

import "unicode/utf8"

// Color is a cell's foreground or background: the terminal's default, one
// of 256 indexed colours, or a 24-bit colour.
type Color struct {
	Kind    uint8 // 0 default, 1 indexed, 2 rgb
	N       uint8
	R, G, B uint8
}

// Rendition flags.
const (
	Bold uint16 = 1 << iota
	Dim
	Italic
	Underline
	Blink
	Reverse
	Hidden
	Strike
)

// Attr is a cell's rendition.
type Attr struct {
	Fg, Bg Color
	Flags  uint16
}

// Cell is one screen position. S is its text, a base character with any
// combining marks; empty is a blank. W is its width: 1, 2 for the first
// column of a wide character, and 0 for the column a wide one covers.
type Cell struct {
	S string
	W int8
	A Attr
}

func blank(a Attr) Cell { return Cell{W: 1, A: Attr{Bg: a.Bg}} }

// Line is one row of history, with a stable identity: IDs only grow, and a
// line keeps its ID however the history scrolls.
type Line struct {
	ID    uint64
	Cells []Cell
}

// Modes the model tracks, because a client's terminal must be in them to
// render the program's next bytes as the program meant.
type Modes struct {
	AutoWrap       bool // DECAWM, on by default
	Origin         bool // DECOM
	Insert         bool // IRM
	CursorVisible  bool // DECTCEM, on by default
	AppCursorKeys  bool // DECCKM
	AppKeypad      bool // DECKPAM
	BracketedPaste bool // 2004
	FocusEvents    bool // 1004
	Mouse          int  // 0, or 9, 1000, 1002 or 1003
	MouseEncoding  int  // 0, or 1005, 1006 or 1015
	ReverseVideo   bool // DECSCNM
}

type cursor struct {
	x, y        int
	attr        Attr
	origin      bool
	wrapPending bool
}

// Terminal is the model. It is not safe for concurrent use.
type Terminal struct {
	cols, rows  int
	main, alt   [][]Cell
	altActive   bool
	cur         cursor
	saved       cursor // DECSC / DECRC, and the cursor 1049 saves
	savedSet    bool
	top, bot    int // the scroll region, inclusive
	modes       Modes
	tabs        []bool
	history     []Line
	nextLine    uint64
	maxHistory  int
	title       string
	cursorStyle int    // DECSCUSR, 0 the terminal's default
	last        string // the last printed text, for REP
	exact       bool   // the last snapshot reconstructed everything for any program
	repExact    bool   // and the character a REP would repeat

	p       parser
	replies []byte
}

// New returns a terminal of cols by rows keeping up to maxHistory lines
// of history.
func New(cols, rows, maxHistory int) *Terminal {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	t := &Terminal{cols: cols, rows: rows, maxHistory: maxHistory}
	t.p.g = [2]byte{'B', 'B'}
	t.reset()
	return t
}

func (t *Terminal) reset() {
	t.main, t.alt = newGrid(t.cols, t.rows, Attr{}), newGrid(t.cols, t.rows, Attr{})
	t.altActive = false
	t.cur = cursor{}
	t.saved, t.savedSet = cursor{}, false
	t.top, t.bot = 0, t.rows-1
	t.modes = Modes{AutoWrap: true, CursorVisible: true}
	t.tabs = make([]bool, t.cols)
	for i := 8; i < t.cols; i += 8 {
		t.tabs[i] = true
	}
	t.title, t.last, t.cursorStyle = "", "", 0
}

func newGrid(cols, rows int, a Attr) [][]Cell {
	g := make([][]Cell, rows)
	for y := range g {
		g[y] = newRow(cols, a)
	}
	return g
}

func newRow(cols int, a Attr) []Cell {
	r := make([]Cell, cols)
	for x := range r {
		r[x] = blank(a)
	}
	return r
}

func (t *Terminal) grid() [][]Cell {
	if t.altActive {
		return t.alt
	}
	return t.main
}

// Size returns the terminal's columns and rows.
func (t *Terminal) Size() (int, int) { return t.cols, t.rows }

// Modes returns the modes in force.
func (t *Terminal) Modes() Modes {
	m := t.modes
	m.Origin = t.cur.origin // the cursor holds origin mode, so a restored cursor restores it
	return m
}

// AltScreen reports whether the alternate screen is active.
func (t *Terminal) AltScreen() bool { return t.altActive }

// Cursor returns the cursor's column and row, from 0.
func (t *Terminal) Cursor() (int, int) { return t.cur.x, t.cur.y }

// Title is the window title the program set, if any.
func (t *Terminal) Title() string { return t.title }

// History returns the lines scrolled off the main screen, oldest first.
func (t *Terminal) History() []Line { return append([]Line(nil), t.history...) }

// Row returns row y of the active screen as text, trailing blanks trimmed.
func (t *Terminal) Row(y int) string {
	if y < 0 || y >= t.rows {
		return ""
	}
	return rowText(t.grid()[y])
}

func rowText(r []Cell) string {
	var b []byte
	end := 0
	for _, c := range r {
		if c.W == 0 {
			continue
		}
		if c.S == "" {
			b = append(b, ' ')
		} else {
			b = append(b, c.S...)
			end = len(b)
		}
	}
	return string(b[:end])
}

// Resize changes the window size. Rows shrink from the top into history,
// as a terminal does when the program's window shrinks; columns are cut or
// padded, never rewrapped.
func (t *Terminal) Resize(cols, rows int) {
	if cols < 1 || rows < 1 || (cols == t.cols && rows == t.rows) {
		return
	}
	fit := func(g [][]Cell, history bool) [][]Cell {
		for len(g) > rows {
			if history && t.cur.y > 0 {
				t.pushHistory(g[0])
				g = g[1:]
				if !t.altActive {
					t.cur.y--
				}
			} else {
				g = g[:len(g)-1]
			}
		}
		for len(g) < rows {
			g = append(g, newRow(cols, Attr{}))
		}
		for y, r := range g {
			if len(r) > cols {
				r = r[:cols]
				if r[cols-1].W == 2 {
					r[cols-1] = blank(r[cols-1].A)
				}
			}
			for len(r) < cols {
				r = append(r, blank(Attr{}))
			}
			g[y] = r
		}
		return g
	}
	t.main = fit(t.main, true)
	t.alt = fit(t.alt, false)
	t.cols, t.rows = cols, rows
	t.top, t.bot = 0, rows-1
	tabs := make([]bool, cols)
	copy(tabs, t.tabs)
	for i := len(t.tabs); i < cols; i++ {
		tabs[i] = i%8 == 0 && i > 0
	}
	t.tabs = tabs
	t.cur.x, t.cur.y = clamp(t.cur.x, 0, cols-1), clamp(t.cur.y, 0, rows-1)
	t.cur.wrapPending = false
	t.saved.x, t.saved.y = clamp(t.saved.x, 0, cols-1), clamp(t.saved.y, 0, rows-1)
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

func (t *Terminal) pushHistory(r []Cell) {
	if t.maxHistory <= 0 {
		return
	}
	t.history = append(t.history, Line{ID: t.nextLine, Cells: r})
	t.nextLine++
	if over := len(t.history) - t.maxHistory; over > 0 {
		t.history = append([]Line(nil), t.history[over:]...)
	}
}

// print puts one character, a base with any combining marks, at the
// cursor.
func (t *Terminal) print(s string, w int) {
	g := t.grid()
	if w == 0 { // a combining mark joins the cell before the cursor
		x, y := t.cur.x-1, t.cur.y
		if t.cur.wrapPending {
			x = t.cur.x
		}
		if x >= 0 && g[y][x].W == 0 && x > 0 {
			x--
		}
		if x >= 0 { // onto a blank, it joins the space a terminal shows there
			if g[y][x].S == "" {
				g[y][x].S = " "
			}
			g[y][x].S += s
			t.last = g[y][x].S
		}
		return
	}
	if t.cur.wrapPending && t.modes.AutoWrap {
		t.cur.x = 0
		t.lineFeed()
		t.cur.wrapPending = false
	}
	if w == 2 && t.cur.x == t.cols-1 { // a wide character doesn't fit in the last column
		if t.modes.AutoWrap {
			g[t.cur.y][t.cur.x] = blank(t.cur.attr)
			t.cur.x = 0
			t.lineFeed()
		} else {
			return
		}
	}
	g = t.grid()
	row := g[t.cur.y]
	if t.modes.Insert {
		copy(row[t.cur.x+w:], row[t.cur.x:])
	}
	t.clearWide(row, t.cur.x)
	if w == 2 {
		t.clearWide(row, t.cur.x+1)
	}
	row[t.cur.x] = Cell{S: s, W: int8(w), A: t.cur.attr}
	if w == 2 {
		row[t.cur.x+1] = Cell{W: 0, A: t.cur.attr}
	}
	if t.modes.Insert {
		mend(row) // inserting shifted the row's cells, and may have split a wide character
	}
	t.last = s
	if t.cur.x+w >= t.cols {
		t.cur.x = t.cols - 1
		t.cur.wrapPending = t.modes.AutoWrap // with autowrap off, the margin just overwrites
	} else {
		t.cur.x += w
	}
}

// clearWide blanks the other half of a wide character that x overwrites.
func (t *Terminal) clearWide(row []Cell, x int) {
	if x < 0 || x >= len(row) {
		return
	}
	switch row[x].W {
	case 2:
		if x+1 < len(row) {
			row[x+1] = blank(row[x+1].A)
		}
	case 0:
		if x > 0 && row[x-1].W == 2 {
			row[x-1] = blank(row[x-1].A)
		}
	}
}

func (t *Terminal) lineFeed() {
	if t.cur.y == t.bot {
		t.scrollUp(1)
	} else if t.cur.y < t.rows-1 {
		t.cur.y++
	}
}

func (t *Terminal) reverseIndex() {
	if t.cur.y == t.top {
		t.scrollDown(1)
	} else if t.cur.y > 0 {
		t.cur.y--
	}
}

// scrollUp moves the scroll region's rows up by n. Rows leaving the top of
// the main screen's full-height region go into history.
func (t *Terminal) scrollUp(n int) {
	g := t.grid()
	n = clamp(n, 0, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		if !t.altActive && t.top == 0 {
			t.pushHistory(g[t.top])
		}
		copy(g[t.top:t.bot], g[t.top+1:t.bot+1])
		g[t.bot] = newRow(t.cols, Attr{Bg: t.cur.attr.Bg})
	}
}

func (t *Terminal) scrollDown(n int) {
	g := t.grid()
	n = clamp(n, 0, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		copy(g[t.top+1:t.bot+1], g[t.top:t.bot])
		g[t.top] = newRow(t.cols, Attr{Bg: t.cur.attr.Bg})
	}
}

// Feed models b, the program's next output bytes, which may end inside a
// character or a sequence. It returns the bytes to forward to clients:
// b without the queries the host answers itself, whose replies Replies
// returns. The cut between two calls is always clean: a sequence split
// across them is held, and forwarded whole when it completes.
func (t *Terminal) Feed(b []byte) []byte {
	var fwd []byte
	for _, c := range b {
		fwd = t.p.step(t, c, fwd)
	}
	return fwd
}

// Replies returns, and clears, the answers to queries the program sent: the
// host writes them to the program's terminal.
func (t *Terminal) Replies() []byte {
	r := t.replies
	t.replies = nil
	return r
}

// printRune prints one decoded rune.
func (t *Terminal) printRune(r rune) {
	if r == utf8.RuneError {
		r = '�'
	}
	t.print(string(r), runeWidth(r))
}

// mend blanks any half of a wide character that lost its other half when
// cells were inserted, deleted or erased around it, as a terminal does.
func mend(row []Cell) {
	for x := range row {
		switch row[x].W {
		case 2:
			if x+1 >= len(row) || row[x+1].W != 0 {
				row[x] = blank(row[x].A)
			}
		case 0:
			if x == 0 || row[x-1].W != 2 {
				row[x] = blank(row[x].A)
			}
		}
	}
}
