package termstate

import (
	"strconv"
	"strings"
)

// sgr applies Select Graphic Rendition. raw is the parameter text, read
// again for the colon forms (38:2::r:g:b and 38:5:n).
func (t *Terminal) sgr(ps []int, raw []byte) {
	a := &t.cur.attr
	if len(ps) == 0 {
		*a = Attr{}
		return
	}
	fields := strings.Split(string(raw), ";")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if strings.Contains(f, ":") {
			sub := strings.Split(f, ":")
			n, _ := strconv.Atoi(sub[0])
			if n == 38 || n == 48 || n == 58 {
				if c, ok := colonColor(sub[1:]); ok && n != 58 {
					if n == 38 {
						a.Fg = c
					} else {
						a.Bg = c
					}
				}
				continue
			}
			if n == 4 { // underline styles: any style but 0 is underlined
				if len(sub) > 1 && sub[1] == "0" {
					a.Flags &^= Underline
				} else {
					a.Flags |= Underline
				}
				continue
			}
			f = sub[0]
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			n = 0
		}
		switch {
		case n == 0:
			*a = Attr{}
		case n == 1:
			a.Flags |= Bold
		case n == 2:
			a.Flags |= Dim
		case n == 3:
			a.Flags |= Italic
		case n == 4:
			a.Flags |= Underline
		case n == 5 || n == 6:
			a.Flags |= Blink
		case n == 7:
			a.Flags |= Reverse
		case n == 8:
			a.Flags |= Hidden
		case n == 9:
			a.Flags |= Strike
		case n == 21:
			a.Flags |= Underline
		case n == 22:
			a.Flags &^= Bold | Dim
		case n == 23:
			a.Flags &^= Italic
		case n == 24:
			a.Flags &^= Underline
		case n == 25:
			a.Flags &^= Blink
		case n == 27:
			a.Flags &^= Reverse
		case n == 28:
			a.Flags &^= Hidden
		case n == 29:
			a.Flags &^= Strike
		case n >= 30 && n <= 37:
			a.Fg = Color{Kind: 1, N: uint8(n - 30)}
		case n == 39:
			a.Fg = Color{}
		case n >= 40 && n <= 47:
			a.Bg = Color{Kind: 1, N: uint8(n - 40)}
		case n == 49:
			a.Bg = Color{}
		case n >= 90 && n <= 97:
			a.Fg = Color{Kind: 1, N: uint8(n - 90 + 8)}
		case n >= 100 && n <= 107:
			a.Bg = Color{Kind: 1, N: uint8(n - 100 + 8)}
		case n == 38 || n == 48 || n == 58:
			c, used := semicolonColor(fields[i+1:])
			i += used
			if used > 0 && n != 58 {
				if n == 38 {
					a.Fg = c
				} else {
					a.Bg = c
				}
			}
		}
	}
}

func atoiByte(s string) (uint8, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 255 {
		return 0, false
	}
	return uint8(n), true
}

// semicolonColor reads 5;n or 2;r;g;b and returns how many fields it used.
func semicolonColor(f []string) (Color, int) {
	if len(f) >= 2 && f[0] == "5" {
		if n, ok := atoiByte(f[1]); ok {
			return Color{Kind: 1, N: n}, 2
		}
		return Color{}, 2
	}
	if len(f) >= 4 && f[0] == "2" {
		r, ok1 := atoiByte(f[1])
		g, ok2 := atoiByte(f[2])
		b, ok3 := atoiByte(f[3])
		if ok1 && ok2 && ok3 {
			return Color{Kind: 2, R: r, G: g, B: b}, 4
		}
		return Color{}, 4
	}
	return Color{}, 0
}

// colonColor reads 5:n, 2::r:g:b or 2:r:g:b.
func colonColor(f []string) (Color, bool) {
	if len(f) >= 2 && f[0] == "5" {
		n, ok := atoiByte(f[1])
		return Color{Kind: 1, N: n}, ok
	}
	if len(f) >= 4 && f[0] == "2" {
		rgb := f[1:]
		if len(rgb) >= 4 { // a colour-space ID before r:g:b
			rgb = rgb[1:]
		}
		r, ok1 := atoiByte(rgb[0])
		g, ok2 := atoiByte(rgb[1])
		b, ok3 := atoiByte(rgb[2])
		return Color{Kind: 2, R: r, G: g, B: b}, ok1 && ok2 && ok3
	}
	return Color{}, false
}

// sgrString is the SGR sequence that sets a exactly, from a reset.
func sgrString(a Attr) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	flags := []struct {
		f uint16
		n string
	}{{Bold, "1"}, {Dim, "2"}, {Italic, "3"}, {Underline, "4"}, {Blink, "5"}, {Reverse, "7"}, {Hidden, "8"}, {Strike, "9"}}
	for _, f := range flags {
		if a.Flags&f.f != 0 {
			b.WriteString(";" + f.n)
		}
	}
	color := func(c Color, base int) {
		switch c.Kind {
		case 1:
			b.WriteString(";" + strconv.Itoa(base) + ";5;" + strconv.Itoa(int(c.N)))
		case 2:
			b.WriteString(";" + strconv.Itoa(base) + ";2;" + strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B)))
		}
	}
	color(a.Fg, 38)
	color(a.Bg, 48)
	b.WriteString("m")
	return b.String()
}
