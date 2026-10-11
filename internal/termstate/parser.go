package termstate

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	ground = iota
	escape
	escInter
	csiEntry
	oscString
	oscEsc
	ctlString // DCS, SOS, PM, APC
	ctlStringEsc
	passString // a string beyond maxString: forwarded as it comes, not modelled
	passStringEsc
)

// maxString bounds a held OSC or DCS string; a longer one (an inline image,
// say) is forwarded as it comes and isn't modelled.
const maxString = 64 << 10

// maxCSI bounds a held control sequence; a longer one is malformed.
const maxCSI = 512

type parser struct {
	state int
	seq   []byte // the sequence in progress, held until it completes
	utf8  []byte // a character in progress
	g     [2]byte
	shift int // 0 for G0, 1 for G1 (SO)
}

// step models one byte and appends to fwd what clients are to receive. A
// sequence or character is held until it completes, so the bytes forwarded
// so far always end at a clean boundary, and the model is exactly the state
// they produce.
func (p *parser) step(t *Terminal, c byte, fwd []byte) []byte {
	switch p.state {
	case ground:
		if len(p.utf8) > 0 {
			if c >= 0x80 && c < 0xC0 {
				p.utf8 = append(p.utf8, c)
				if utf8.FullRune(p.utf8) {
					r, _ := utf8.DecodeRune(p.utf8)
					t.printRune(r)
					fwd = append(fwd, p.utf8...)
					p.utf8 = p.utf8[:0]
				}
				return fwd
			}
			t.printRune(utf8.RuneError) // an incomplete character, then something else
			fwd = append(fwd, p.utf8...)
			p.utf8 = p.utf8[:0]
		}
		switch {
		case c == 0x1b:
			p.state, p.seq = escape, append(p.seq[:0], c)
		case c < 0x20 || c == 0x7f:
			p.control(t, c)
			fwd = append(fwd, c)
		case c < 0x80:
			p.printASCII(t, c)
			fwd = append(fwd, c)
		case c >= 0xC0 && c < 0xF8:
			p.utf8 = append(p.utf8[:0], c)
		default: // a stray continuation byte
			t.printRune(utf8.RuneError)
			fwd = append(fwd, c)
		}
	case escape:
		p.seq = append(p.seq, c)
		switch {
		case c == '[':
			p.state = csiEntry
		case c == ']':
			p.state = oscString
		case c == 'P' || c == 'X' || c == '^' || c == '_':
			p.state = ctlString
		case c >= 0x20 && c <= 0x2f:
			p.state = escInter
		case c == 0x18 || c == 0x1a: // CAN, SUB: the sequence is abandoned
			fwd = append(fwd, p.seq...)
			p.state = ground
		case c < 0x20:
			p.control(t, c) // a control inside a sequence acts at once
		default:
			p.escDispatch(t, c)
			fwd = append(fwd, p.seq...)
			p.state = ground
		}
	case escInter:
		p.seq = append(p.seq, c)
		if c >= 0x20 && c <= 0x2f {
			break
		}
		p.escInterDispatch(t)
		fwd = append(fwd, p.seq...)
		p.state = ground
	case csiEntry:
		p.seq = append(p.seq, c)
		switch {
		case c >= 0x40 && c <= 0x7e:
			if !p.csiDispatch(t) {
				fwd = append(fwd, p.seq...)
			}
			p.state = ground
		case c == 0x18 || c == 0x1a || len(p.seq) > maxCSI:
			fwd = append(fwd, p.seq...)
			p.state = ground
		case c < 0x20:
			// a control byte inside a sequence acts at once; it stays in the
			// held bytes, so the client sees the same order
			p.control(t, c)
		}
	case oscString, ctlString:
		p.seq = append(p.seq, c)
		switch {
		case c == 0x07 && p.state == oscString:
			fwd = p.endString(t, fwd)
		case c == 0x1b:
			p.state++ // to the matching ...Esc state
		case c == 0x18 || c == 0x1a:
			fwd = append(fwd, p.seq...)
			p.state = ground
		case len(p.seq) > maxString:
			fwd = append(fwd, p.seq...)
			p.state = passString
		}
	case oscEsc, ctlStringEsc:
		p.seq = append(p.seq, c)
		if c == '\\' {
			p.state--
			fwd = p.endString(t, fwd)
		} else {
			p.state-- // an ESC inside the string that isn't its end
		}
	case passString:
		fwd = append(fwd, c)
		switch c {
		case 0x07, 0x18, 0x1a:
			p.state = ground
		case 0x1b:
			p.state = passStringEsc
		}
	case passStringEsc:
		fwd = append(fwd, c)
		p.state = ground
		if c != '\\' {
			p.state = passString
		}
	}
	return fwd
}

func (p *parser) printASCII(t *Terminal, c byte) {
	if p.g[p.shift] == '0' && c >= 0x5f && c <= 0x7e {
		t.print(string(decGraphics[c-0x5f]), 1)
		return
	}
	t.print(string(rune(c)), 1)
}

// decGraphics is the DEC special graphics set, 0x5f to 0x7e, as Unicode.
var decGraphics = []rune(" ◆▒␉␌␍␊°±␤␋┘┐┌└┼⎺⎻─⎼⎽├┤┴┬│≤≥π≠£·")

func (p *parser) control(t *Terminal, c byte) {
	switch c {
	case 0x08: // BS
		if t.cur.x > 0 {
			t.cur.x--
		}
		t.cur.wrapPending = false
	case 0x09: // HT
		t.cur.x = t.nextTab(t.cur.x)
		t.cur.wrapPending = false
	case 0x0a, 0x0b, 0x0c: // LF, VT, FF
		t.lineFeed()
		t.cur.wrapPending = false
	case 0x0d: // CR
		t.cur.x, t.cur.wrapPending = 0, false
	case 0x0e: // SO
		p.shift = 1
	case 0x0f: // SI
		p.shift = 0
	}
}

func (t *Terminal) nextTab(x int) int {
	for i := x + 1; i < t.cols; i++ {
		if t.tabs[i] {
			return i
		}
	}
	return t.cols - 1
}

func (t *Terminal) prevTab(x int) int {
	for i := x - 1; i > 0; i-- {
		if t.tabs[i] {
			return i
		}
	}
	return 0
}

func (p *parser) escDispatch(t *Terminal, c byte) {
	switch c {
	case '7': // DECSC
		t.saved, t.savedSet = t.cur, true
	case '8': // DECRC
		if t.savedSet {
			t.cur = t.saved
		} else {
			t.cur = cursor{}
		}
	case 'D': // IND
		t.lineFeed()
	case 'E': // NEL
		t.cur.x = 0
		t.lineFeed()
	case 'M': // RI
		t.reverseIndex()
	case 'H': // HTS
		t.tabs[t.cur.x] = true
	case 'c': // RIS
		t.reset()
		p.g, p.shift = [2]byte{'B', 'B'}, 0
	case '=':
		t.modes.AppKeypad = true
	case '>':
		t.modes.AppKeypad = false
	}
	t.cur.wrapPending = t.cur.wrapPending && (c == '=' || c == '>' || c == 'H' || c == '7')
}

func (p *parser) escInterDispatch(t *Terminal) {
	s := p.seq[1:]
	if len(s) != 2 {
		return
	}
	switch s[0] {
	case '(':
		p.g[0] = s[1]
	case ')':
		p.g[1] = s[1]
	case '#':
		if s[1] == '8' { // DECALN: fill the screen with E
			for y := range t.grid() {
				for x := range t.grid()[y] {
					t.grid()[y][x] = Cell{S: "E", W: 1}
				}
			}
		}
	}
}

// csiDispatch acts on a complete control sequence. It returns true when the
// sequence was a query the host answers, so it isn't forwarded.
func (p *parser) csiDispatch(t *Terminal) bool {
	body := p.seq[2 : len(p.seq)-1]
	final := p.seq[len(p.seq)-1]
	var prefix byte
	if len(body) > 0 && body[0] >= 0x3c && body[0] <= 0x3f {
		prefix, body = body[0], body[1:]
	}
	var inter string
	for len(body) > 0 && body[len(body)-1] >= 0x20 && body[len(body)-1] <= 0x2f {
		inter = string(body[len(body)-1]) + inter
		body = body[:len(body)-1]
	}
	ps := params(body)
	arg := func(i, def int) int {
		if i < len(ps) && ps[i] > 0 {
			return ps[i]
		}
		return def
	}
	g := t.grid()
	cur := &t.cur
	switch {
	case inter == "$" && final == 'p': // DECRQM: a query
		mode := arg(0, 0)
		state := t.modeState(prefix == '?', mode)
		if prefix == '?' {
			t.replies = append(t.replies, fmt.Sprintf("\x1b[?%d;%d$y", mode, state)...)
		} else {
			t.replies = append(t.replies, fmt.Sprintf("\x1b[%d;%d$y", mode, state)...)
		}
		return true
	case inter == "!" && final == 'p': // DECSTR
		t.modes = Modes{AutoWrap: true, CursorVisible: true}
		cur.attr, cur.origin = Attr{}, false
		t.top, t.bot = 0, t.rows-1
		t.saved, t.savedSet = cursor{}, false
		return false
	case inter == " " && final == 'q': // DECSCUSR: the cursor style
		t.cursorStyle = arg(0, 0)
		return false
	case inter != "":
		return false // other intermediates (cursor style and so on) aren't modelled
	}
	switch prefix {
	case '>':
		switch final {
		case 'c': // DA2
			t.replies = append(t.replies, "\x1b[>1;10;0c"...)
			return true
		case 'q': // XTVERSION
			t.replies = append(t.replies, "\x1bP>|pomar-termstate\x1b\\"...)
			return true
		}
		return false // key-modifier settings are forwarded, and listed in Limits
	case '=':
		return final == 'c' // DA3
	case '?':
		switch final {
		case 'h', 'l':
			for _, m := range ps {
				t.setPrivate(m, final == 'h')
			}
		case 'n':
			if arg(0, 0) == 6 { // DECXCPR
				y, x := t.reportPosition()
				t.replies = append(t.replies, fmt.Sprintf("\x1b[?%d;%dR", y, x)...)
			}
			return true
		case 'u': // keyboard protocol query: not supported, so no answer
			return true
		}
		return false
	case '<':
		return false
	}
	switch final {
	case '@': // ICH
		n := clamp(arg(0, 1), 1, t.cols-cur.x)
		row := g[cur.y]
		copy(row[cur.x+n:], row[cur.x:])
		for i := cur.x; i < cur.x+n; i++ {
			row[i] = blank(cur.attr)
		}
	case 'A':
		cur.y = clamp(cur.y-arg(0, 1), t.top0(), t.rows-1)
	case 'B', 'e':
		cur.y = clamp(cur.y+arg(0, 1), 0, t.bot0())
	case 'C', 'a':
		cur.x = clamp(cur.x+arg(0, 1), 0, t.cols-1)
	case 'D':
		cur.x = clamp(cur.x-arg(0, 1), 0, t.cols-1)
	case 'E':
		cur.y, cur.x = clamp(cur.y+arg(0, 1), 0, t.bot0()), 0
	case 'F':
		cur.y, cur.x = clamp(cur.y-arg(0, 1), t.top0(), t.rows-1), 0
	case 'G', '`':
		cur.x = clamp(arg(0, 1)-1, 0, t.cols-1)
	case 'H', 'f':
		t.moveTo(arg(1, 1)-1, arg(0, 1)-1)
	case 'I':
		for i := 0; i < arg(0, 1); i++ {
			cur.x = t.nextTab(cur.x)
		}
	case 'Z':
		for i := 0; i < arg(0, 1); i++ {
			cur.x = t.prevTab(cur.x)
		}
	case 'J':
		t.eraseDisplay(arg(0, 0))
	case 'K':
		t.eraseLine(arg(0, 0))
	case 'L', 'M':
		if cur.y >= t.top && cur.y <= t.bot {
			top := t.top
			t.top = cur.y
			if final == 'L' {
				t.scrollDown(arg(0, 1))
			} else {
				t.scrollUpNoHistory(arg(0, 1))
			}
			t.top = top
			cur.x = 0
		}
	case 'P': // DCH
		n := clamp(arg(0, 1), 1, t.cols-cur.x)
		row := g[cur.y]
		copy(row[cur.x:], row[cur.x+n:])
		for i := t.cols - n; i < t.cols; i++ {
			row[i] = blank(cur.attr)
		}
	case 'S':
		t.scrollUp(arg(0, 1))
	case 'T':
		if len(ps) <= 1 {
			t.scrollDown(arg(0, 1))
		}
	case 'X': // ECH
		n := clamp(arg(0, 1), 1, t.cols-cur.x)
		for i := cur.x; i < cur.x+n; i++ {
			g[cur.y][i] = blank(cur.attr)
		}
	case 'b': // REP
		if t.last != "" {
			for i := 0; i < clamp(arg(0, 1), 1, 1<<16); i++ {
				t.print(t.last, max(1, runeWidth([]rune(t.last)[0])))
			}
		}
	case 'c': // DA1: a query
		if arg(0, 0) == 0 {
			t.replies = append(t.replies, "\x1b[?62;22c"...)
		}
		return true
	case 'd':
		t.moveTo(cur.x, arg(0, 1)-1)
	case 'g':
		switch arg(0, 0) {
		case 0:
			t.tabs[cur.x] = false
		case 3:
			for i := range t.tabs {
				t.tabs[i] = false
			}
		}
	case 'h', 'l':
		for _, m := range ps {
			if m == 4 {
				t.modes.Insert = final == 'h'
			}
		}
	case 'm':
		t.sgr(ps, body)
		return false
	case 'n': // DSR: a query
		switch arg(0, 0) {
		case 5:
			t.replies = append(t.replies, "\x1b[0n"...)
		case 6:
			y, x := t.reportPosition()
			t.replies = append(t.replies, fmt.Sprintf("\x1b[%d;%dR", y, x)...)
		}
		return true
	case 'r': // DECSTBM
		top, bot := arg(0, 1)-1, arg(1, t.rows)-1
		if top < bot && bot < t.rows {
			t.top, t.bot = top, bot
			t.moveTo(0, 0)
		}
	case 's':
		if len(ps) == 0 {
			t.saved, t.savedSet = t.cur, true
		}
	case 'u':
		if t.savedSet {
			t.cur = t.saved
		}
	case 't': // window operations: answered for the text size, never forwarded
		if arg(0, 0) == 18 {
			t.replies = append(t.replies, fmt.Sprintf("\x1b[8;%d;%dt", t.rows, t.cols)...)
		}
		return true
	}
	if final == '@' || final == 'P' || final == 'X' || final == 'K' {
		mend(t.grid()[cur.y]) // shifting or erasing cells may split a wide character
	}
	if strings.IndexByte("@ABCDEFGHfIZJKLMPSTX`ader", final) >= 0 { // only moving the cursor or editing ends a pending wrap
		cur.wrapPending = false
	}
	return false
}

func (t *Terminal) top0() int {
	if t.cur.y >= t.top {
		return t.top
	}
	return 0
}

func (t *Terminal) bot0() int {
	if t.cur.y <= t.bot {
		return t.bot
	}
	return t.rows - 1
}

// moveTo puts the cursor at column x and row y, relative to the scroll
// region in origin mode.
func (t *Terminal) moveTo(x, y int) {
	if t.cur.origin {
		y = clamp(y+t.top, t.top, t.bot)
	} else {
		y = clamp(y, 0, t.rows-1)
	}
	t.cur.x, t.cur.y, t.cur.wrapPending = clamp(x, 0, t.cols-1), y, false
}

func (t *Terminal) reportPosition() (int, int) {
	y := t.cur.y + 1
	if t.cur.origin {
		y -= t.top
	}
	return y, t.cur.x + 1
}

// scrollUpNoHistory is DL: rows deleted inside the region never enter
// history.
func (t *Terminal) scrollUpNoHistory(n int) {
	g := t.grid()
	n = clamp(n, 0, t.bot-t.top+1)
	for i := 0; i < n; i++ {
		copy(g[t.top:t.bot], g[t.top+1:t.bot+1])
		g[t.bot] = newRow(t.cols, Attr{Bg: t.cur.attr.Bg})
	}
}

func (t *Terminal) eraseDisplay(mode int) {
	g := t.grid()
	b := blank(t.cur.attr)
	switch mode {
	case 0:
		t.eraseLine(0)
		for y := t.cur.y + 1; y < t.rows; y++ {
			for x := range g[y] {
				g[y][x] = b
			}
		}
	case 1:
		t.eraseLine(1)
		for y := 0; y < t.cur.y; y++ {
			for x := range g[y] {
				g[y][x] = b
			}
		}
	case 2:
		for y := range g {
			for x := range g[y] {
				g[y][x] = b
			}
		}
	case 3:
		if !t.altActive {
			t.history = nil
		}
	}
}

func (t *Terminal) eraseLine(mode int) {
	row := t.grid()[t.cur.y]
	b := blank(t.cur.attr)
	from, to := t.cur.x, t.cols
	switch mode {
	case 1:
		from, to = 0, t.cur.x+1
	case 2:
		from, to = 0, t.cols
	}
	t.clearWide(row, from)
	t.clearWide(row, to-1)
	for x := from; x < to && x < t.cols; x++ {
		row[x] = b
	}
}

func (t *Terminal) setPrivate(m int, on bool) {
	switch m {
	case 1:
		t.modes.AppCursorKeys = on
	case 5:
		t.modes.ReverseVideo = on
	case 6:
		t.cur.origin = on
		t.moveTo(0, 0)
	case 7:
		t.modes.AutoWrap = on
	case 25:
		t.modes.CursorVisible = on
	case 9, 1000, 1002, 1003:
		if on {
			t.modes.Mouse = m
		} else if t.modes.Mouse == m {
			t.modes.Mouse = 0
		}
	case 1005, 1006, 1015:
		if on {
			t.modes.MouseEncoding = m
		} else if t.modes.MouseEncoding == m {
			t.modes.MouseEncoding = 0
		}
	case 1004:
		t.modes.FocusEvents = on
	case 2004:
		t.modes.BracketedPaste = on
	case 47, 1047:
		t.switchScreen(on, false)
	case 1048:
		if on {
			t.saved, t.savedSet = t.cur, true
		} else if t.savedSet {
			t.cur = t.saved
		}
	case 1049:
		t.switchScreen(on, true)
	}
}

func (t *Terminal) switchScreen(alt, saveCursor bool) {
	if alt == t.altActive {
		return
	}
	if alt {
		if saveCursor {
			t.saved, t.savedSet = t.cur, true
		}
		t.altActive = true
		t.alt = newGrid(t.cols, t.rows, Attr{})
		return
	}
	t.altActive = false
	if saveCursor && t.savedSet {
		t.cur = t.saved
	}
}

// modeState is a DECRQM answer: 1 set, 2 reset, 0 not recognised.
func (t *Terminal) modeState(private bool, m int) int {
	b := func(v bool) int {
		if v {
			return 1
		}
		return 2
	}
	if !private {
		if m == 4 {
			return b(t.modes.Insert)
		}
		return 0
	}
	switch m {
	case 1:
		return b(t.modes.AppCursorKeys)
	case 5:
		return b(t.modes.ReverseVideo)
	case 6:
		return b(t.cur.origin)
	case 7:
		return b(t.modes.AutoWrap)
	case 25:
		return b(t.modes.CursorVisible)
	case 9, 1000, 1002, 1003:
		return b(t.modes.Mouse == m)
	case 1005, 1006, 1015:
		return b(t.modes.MouseEncoding == m)
	case 1004:
		return b(t.modes.FocusEvents)
	case 2004:
		return b(t.modes.BracketedPaste)
	case 47, 1047, 1049:
		return b(t.altActive)
	}
	return 0
}

// endString acts on a complete OSC or DCS string.
func (p *parser) endString(t *Terminal, fwd []byte) []byte {
	kind := p.seq[1]
	body := p.seq[2:]
	if n := len(body); n > 0 && body[n-1] == 0x07 {
		body = body[:n-1]
	} else if n >= 2 && body[n-2] == 0x1b && body[n-1] == '\\' {
		body = body[:n-2]
	}
	p.state = ground
	switch kind {
	case ']':
		num, rest, _ := strings.Cut(string(body), ";")
		switch num {
		case "0", "2":
			t.title = rest
		case "10", "11", "12": // a colour query: answered with the host's defaults, never forwarded
			if rest == "?" {
				col := map[string]string{"10": "ffff/ffff/ffff", "11": "0000/0000/0000", "12": "ffff/ffff/ffff"}[num]
				t.replies = append(t.replies, "\x1b]"+num+";rgb:"+col+"\x1b\\"...)
				return fwd
			}
		}
	case 'P':
		if strings.HasPrefix(string(body), "$q") { // DECRQSS: not answered as valid
			t.replies = append(t.replies, "\x1bP0$r\x1b\\"...)
			return fwd
		}
	}
	return append(fwd, p.seq...)
}

// params parses a parameter list; sub-parameters after ':' are dropped
// here (sgr reads them from the raw body).
func params(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	var out []int
	for _, f := range strings.Split(string(b), ";") {
		f, _, _ = strings.Cut(f, ":")
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			n = 0
		}
		if n > 65535 {
			n = 65535
		}
		out = append(out, n)
	}
	return out
}
