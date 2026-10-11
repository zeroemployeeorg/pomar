package termstate

import (
	"fmt"
	"strings"
)

// SnapshotVersion names the format of Snapshot's output.
const SnapshotVersion = "pomar-termstate-ansi/1"

// Snapshot returns the terminal's state as ANSI: fed to a terminal of the
// same size, then followed by the bytes Feed forwards next, it renders
// what the program's own terminal shows. It reconstructs:
//   - both screens;
//   - the cursor's position, including a pending wrap, and its rendition;
//   - the saved cursor;
//   - the scroll region and tab stops;
//   - the modes;
//   - the character sets;
//   - the cursor style and the title.
//
// History isn't in it: clients fetch history by range. Every mode is set
// explicitly, on or off, so the result doesn't depend on the client
// terminal's prior state. Nothing in it asks the terminal anything.
func (t *Terminal) Snapshot() []byte {
	var b strings.Builder
	// A known start: the main screen, a soft reset, the default rendition
	// and character sets, and a clear screen. No RIS, so the client's own
	// scrollback survives.
	b.WriteString("\x1b[?1049l\x1b[!p\x1b[0m\x1b(B\x1b)B\x0f\x1b[r\x1b[?6l\x1b[?7h\x1b[H\x1b[2J")
	t.drawGrid(&b, t.main)
	m := t.modes
	private := func(n int, on bool) {
		if on {
			fmt.Fprintf(&b, "\x1b[?%dh", n)
		} else {
			fmt.Fprintf(&b, "\x1b[?%dl", n)
		}
	}
	// The saved cursor is saved while the scroll region is still the whole
	// screen, so any row can be placed in origin mode too; and only if the
	// model has one. The screen is then switched with 47, which saves
	// nothing, so a later 1049l restores, or doesn't, exactly as it would
	// have.
	if t.savedSet {
		private(6, t.saved.origin)
		if t.saved.wrapPending {
			// as for the live cursor, reprinting the last column leaves a wrap pending
			x := t.cols - 1
			if t.main[t.saved.y][x].W == 0 && x > 0 {
				x--
			}
			c := t.main[t.saved.y][x]
			b.WriteString(cup(t.saved.y, x) + sgrString(c.A))
			if c.S == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(c.S)
			}
			b.WriteString(sgrString(t.saved.attr) + "\x1b7\x1b[?6l")
		} else {
			b.WriteString(cup(t.saved.y, t.saved.x) + sgrString(t.saved.attr) + "\x1b7\x1b[?6l")
		}
	}
	if t.altActive {
		b.WriteString("\x1b[?47h\x1b[0m\x1b[H\x1b[2J")
		t.drawGrid(&b, t.alt)
	}
	if t.top != 0 || t.bot != t.rows-1 {
		fmt.Fprintf(&b, "\x1b[%d;%dr", t.top+1, t.bot+1)
	}
	if !defaultTabs(t.tabs) {
		b.WriteString("\x1b[3g")
		for x, on := range t.tabs {
			if on {
				b.WriteString(cup(0, x) + "\x1bH")
			}
		}
	}
	private(1, m.AppCursorKeys)
	private(5, m.ReverseVideo)
	private(25, m.CursorVisible)
	private(1004, m.FocusEvents)
	private(2004, m.BracketedPaste)
	for _, n := range []int{9, 1000, 1002, 1003} {
		private(n, m.Mouse == n)
	}
	for _, n := range []int{1005, 1006, 1015} {
		private(n, m.MouseEncoding == n)
	}
	if m.AppKeypad {
		b.WriteString("\x1b=")
	} else {
		b.WriteString("\x1b>")
	}
	fmt.Fprintf(&b, "\x1b[%d q", t.cursorStyle)
	if t.title != "" {
		b.WriteString("\x1b]2;" + t.title + "\x07")
	}
	private(6, t.cur.origin) // homes the cursor, so the position comes after it
	y := t.cur.y
	if t.cur.origin {
		y -= t.top
	}
	// A cursor restored in origin mode can sit outside the scroll region,
	// where no positioning sequence can put it back: said, not hidden.
	t.exact = !t.cur.origin || (t.cur.y >= t.top && t.cur.y <= t.bot)
	t.repExact = true
	if t.cur.wrapPending {
		// A pending wrap can't be set directly: reprinting the last column's
		// character leaves the cursor in that same state.
		g := t.grid()
		x := t.cols - 1
		if g[t.cur.y][x].W == 0 && x > 0 {
			x--
		}
		c := g[t.cur.y][x]
		b.WriteString(cup(y, x) + sgrString(c.A))
		if c.S != t.last && !(c.S == "" && t.last == " ") {
			t.repExact = false // REP would repeat this character, not the one the program printed last
		}
		if c.S == "" {
			b.WriteString(" ")
		} else {
			b.WriteString(c.S)
		}
	} else {
		// REP repeats the last character printed, so the snapshot's last print
		// must be that character: a cell already showing it is reprinted.
		if t.last != "" {
			if cy, cx, ok := t.findCell(t.last); ok {
				c := t.grid()[cy][cx]
				b.WriteString("\x1b[?6l" + cup(cy, cx) + sgrString(c.A) + c.S)
				private(6, t.cur.origin)
			} else {
				t.repExact = false // REP's character is no longer on screen
			}
		} else if t.drawsAny() {
			t.repExact = false // the program printed nothing yet, but drawing the screen does, so REP could repeat it
		}
		b.WriteString(cup(y, t.cur.x))
	}
	private(7, m.AutoWrap)
	if m.Insert {
		b.WriteString("\x1b[4h")
	} else {
		b.WriteString("\x1b[4l")
	}
	b.WriteString(sgrString(t.cur.attr))
	fmt.Fprintf(&b, "\x1b(%c\x1b)%c", charset(t.p.g[0]), charset(t.p.g[1]))
	if t.p.shift == 1 {
		b.WriteString("\x0e")
	}
	return []byte(b.String())
}

func charset(c byte) byte {
	if c == 0 {
		return 'B'
	}
	return c
}

func cup(y, x int) string { return fmt.Sprintf("\x1b[%d;%dH", y+1, x+1) }

func defaultTabs(tabs []bool) bool {
	for x, on := range tabs {
		if on != (x > 0 && x%8 == 0) {
			return false
		}
	}
	return true
}

// drawGrid writes every row, rendition included, from the default
// rendition. Trailing blanks in the default rendition are left out.
func (t *Terminal) drawGrid(b *strings.Builder, g [][]Cell) {
	for y, row := range g {
		end := len(row)
		for end > 0 && row[end-1].S == "" && row[end-1].W == 1 && row[end-1].A == (Attr{}) {
			end--
		}
		if end == 0 {
			continue
		}
		b.WriteString(cup(y, 0))
		cur := Attr{}
		b.WriteString("\x1b[0m")
		for x := 0; x < end; x++ {
			c := row[x]
			if c.W == 0 {
				continue
			}
			if c.A != cur {
				b.WriteString(sgrString(c.A))
				cur = c.A
			}
			if c.S == "" {
				b.WriteString(" ")
			} else {
				b.WriteString(c.S)
			}
		}
		b.WriteString("\x1b[0m")
	}
}

// Limits are what the model doesn't reconstruct, so a client can say so
// rather than claim an exact resume.
func Limits() []string {
	return []string{
		"history is not in the snapshot; it is fetched by range",
		"keyboard protocol settings (modifyOtherKeys, kitty flags) are forwarded but not restored",
		"hyperlinks (OSC 8) and other unmodelled OSC or DCS strings are forwarded but not restored",
		"an OSC or DCS string longer than 64 KiB (an inline image) is forwarded unmodelled",
		"colour queries are answered with the host's defaults, not the client terminal's colours",
		"character widths follow this package's East Asian Wide and emoji table",
		"the saved cursor's character set is not restored",
	}
}

// findCell finds a cell of the active screen showing s, nearest before the
// cursor first.
func (t *Terminal) findCell(s string) (int, int, bool) {
	g := t.grid()
	for y := t.cur.y; y >= 0; y-- {
		start := t.cols - 1
		if y == t.cur.y {
			start = t.cur.x - 1
		}
		for x := start; x >= 0; x-- {
			if g[y][x].S == s && g[y][x].W != 0 {
				return y, x, true
			}
		}
	}
	for y := t.rows - 1; y > t.cur.y; y-- {
		for x := t.cols - 1; x >= 0; x-- {
			if g[y][x].S == s && g[y][x].W != 0 {
				return y, x, true
			}
		}
	}
	return 0, 0, false
}

// Exact reports whether the last Snapshot reconstructed everything the
// model tracks for any program. It is false only when the cursor is in
// origin mode outside the scroll region, where no positioning sequence can
// put it back.
func (t *Terminal) Exact() bool { return t.exact }

// RepExact reports whether a REP (repeat the last character) right after
// the last Snapshot repeats what it would have: it is false when that
// character is no longer on screen to reprint, or nothing was printed yet
// but drawing the screen prints. Only a program that sends REP is affected.
func (t *Terminal) RepExact() bool { return t.repExact }

// drawsAny reports whether drawing the screens prints any character.
func (t *Terminal) drawsAny() bool {
	grids := [][][]Cell{t.main}
	if t.altActive {
		grids = append(grids, t.alt)
	}
	for _, g := range grids {
		for _, row := range g {
			for _, c := range row {
				if c.S != "" || c.A != (Attr{}) {
					return true
				}
			}
		}
	}
	return false
}
