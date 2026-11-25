// muhfm — a terminal file manager in one file. Go standard library only.
//
// Build:   go build -o muhfm .        (or: go build -o ~/.local/bin/muhfm main.go)
// Default: muhfm --set-default       (folders open in muhfm, from any app)
// Keys:    muhfm --keys              (or ? inside muhfm)
//
// No dependencies, no plugins, no config framework. Colours are the
// terminal's 16 ANSI colours, so muhfm follows whatever theme the terminal
// has. Optional runtime helpers are used when present and skipped when not:
// wl-copy/wl-paste (system clipboard), udisksctl (mounting), bsdtar (exotic
// archives), pdftotext (PDF preview).
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

const version = "2.0.0"

// ---------------------------------------------------------------- paths

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/"
}

func xdg(env, def string) string {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v
	}
	return filepath.Join(home(), def)
}

func configHome() string { return xdg("XDG_CONFIG_HOME", ".config") }
func dataHome() string   { return xdg("XDG_DATA_HOME", ".local/share") }
func stateHome() string  { return xdg("XDG_STATE_HOME", ".local/state") }

func tilde(p string) string {
	h := home()
	if p == h {
		return "~"
	}
	if h != "/" && strings.HasPrefix(p, h+"/") {
		return "~" + p[len(h):]
	}
	return p
}

// expand turns ~, file:// URIs and relative paths into an absolute path.
func expand(p, base string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "file://") {
		if u, err := url.Parse(p); err == nil {
			p = u.Path
		}
	}
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home(), p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func isDirPath(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// isInside reports whether child is parent or below it.
func isInside(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

func splitExt(name string) (string, string) {
	low := strings.ToLower(name)
	for _, e := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(low, e) && len(name) > len(e) {
			return name[:len(name)-len(e)], name[len(name)-len(e):]
		}
	}
	ext := filepath.Ext(name)
	if ext == name {
		return name, ""
	}
	return name[:len(name)-len(ext)], ext
}

// uniqueName returns p, or "name (1).ext", "name (2).ext"… if p exists.
func uniqueName(p string) string {
	if !exists(p) {
		return p
	}
	dir, base := filepath.Split(p)
	stem, ext := splitExt(base)
	for i := 1; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if !exists(c) {
			return c
		}
	}
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func human(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, u := range []string{"K", "M", "G", "T", "P"} {
		f /= 1024
		if f < 1024 {
			if f < 10 {
				return fmt.Sprintf("%.1f%s", f, u)
			}
			return fmt.Sprintf("%.0f%s", f, u)
		}
	}
	return fmt.Sprintf("%.0fE", f/1024)
}

func when(t time.Time) string {
	now := time.Now()
	switch {
	case t.Year() == now.Year() && t.YearDay() == now.YearDay():
		return t.Format("today 15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 02 15:04")
	}
	return t.Format("2006-01-02")
}

func have(cmd string) bool { _, err := exec.LookPath(cmd); return err == nil }

func firstOf(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- config

// ~/.config/muhfm/config — plain "key = value" lines, every key optional.
type config struct {
	hidden, preview, places, reverse, dirsFirst, details bool
	sortBy, icons, sixel, terminal, editor               string
}

func loadConfig() config {
	c := config{preview: true, places: true, dirsFirst: true, details: true,
		sortBy: "name", icons: "none", sixel: "auto"}
	c.terminal = firstOf(os.Getenv("TERMINAL"), "foot")
	c.editor = firstOf(os.Getenv("VISUAL"), os.Getenv("EDITOR"), "vi")
	f, err := os.Open(filepath.Join(configHome(), "muhfm", "config"))
	if err != nil {
		return c
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		b := v == "true" || v == "yes" || v == "on" || v == "1"
		switch k {
		case "hidden":
			c.hidden = b
		case "preview":
			c.preview = b
		case "places", "sidebar":
			c.places = b
		case "reverse":
			c.reverse = b
		case "dirs_first", "dirsfirst":
			c.dirsFirst = b
		case "details":
			c.details = b
		case "sort":
			c.sortBy = v
		case "icons":
			c.icons = v
		case "sixel", "images":
			c.sixel = v
		case "terminal":
			c.terminal = v
		case "editor":
			c.editor = v
		}
	}
	return c
}

// ---------------------------------------------------------------- terminal

type term struct {
	f            *os.File
	fd           int
	orig         syscall.Termios
	paused       atomic.Bool
	cellW, cellH int
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func isatty(fd int) bool {
	var t syscall.Termios
	return ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

func openTerm() (*term, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	t := &term{f: f, fd: int(f.Fd())}
	if err := ioctl(t.fd, syscall.TCGETS, unsafe.Pointer(&t.orig)); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *term) raw() {
	r := t.orig
	r.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	r.Oflag &^= syscall.OPOST
	r.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	r.Cflag &^= syscall.CSIZE | syscall.PARENB
	r.Cflag |= syscall.CS8
	r.Cc[syscall.VMIN] = 1
	r.Cc[syscall.VTIME] = 0
	_ = ioctl(t.fd, syscall.TCSETS, unsafe.Pointer(&r))
	// alt screen, hide cursor, SGR mouse, bracketed paste
	t.f.WriteString("\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[?2004h")
}

func (t *term) restore() {
	t.f.WriteString("\x1b[?2004l\x1b[?1006l\x1b[?1000l\x1b[0m\x1b[?25h\x1b[?1049l")
	_ = ioctl(t.fd, syscall.TCSETS, unsafe.Pointer(&t.orig))
}

func (t *term) size() (int, int) {
	var ws struct{ row, col, x, y uint16 }
	if ioctl(t.fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) != nil || ws.col == 0 || ws.row == 0 {
		return 80, 24
	}
	t.cellW, t.cellH = 0, 0
	if ws.x > 0 && ws.y > 0 {
		t.cellW, t.cellH = int(ws.x)/int(ws.col), int(ws.y)/int(ws.row)
	}
	return int(ws.col), int(ws.row)
}

// readLoop polls the tty so it can stop reading while a child program
// (editor, shell) owns the terminal.
func (t *term) readLoop(ch chan<- []byte) {
	buf := make([]byte, 8192)
	for {
		if t.paused.Load() {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var rfds syscall.FdSet
		rfds.Bits[t.fd/64] |= int64(1) << (uint(t.fd) % 64)
		tv := syscall.Timeval{Usec: 40000}
		n, err := syscall.Select(t.fd+1, &rfds, nil, nil, &tv)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return
		}
		if n <= 0 || t.paused.Load() {
			continue
		}
		m, err := syscall.Read(t.fd, buf)
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			return
		}
		if m == 0 {
			return
		}
		b := make([]byte, m)
		copy(b, buf[:m])
		ch <- b
	}
}

// ---------------------------------------------------------------- keys

type key struct {
	k          string // "j", "ctrl+a", "enter", "alt+left", "mouse", "paste"
	r          rune
	text       string
	mb, mx, my int
	mrel       bool
}

func ctrlKey(c byte) string {
	switch c {
	case 0:
		return "ctrl+space"
	case 9:
		return "tab"
	case 10, 13:
		return "enter"
	case 8, 127:
		return "backspace"
	case 27:
		return "esc"
	}
	if c <= 26 {
		return "ctrl+" + string(rune('a'+c-1))
	}
	return "ctrl+" + string(rune(c+64))
}

func parseKeys(b []byte) ([]key, []byte) {
	var ks []key
	for len(b) > 0 {
		c := b[0]
		if c == 0x1b {
			if len(b) == 1 {
				ks = append(ks, key{k: "esc"})
				break
			}
			if bytes.HasPrefix(b, []byte("\x1b[200~")) {
				end := bytes.Index(b, []byte("\x1b[201~"))
				if end < 0 {
					if len(b) < 1<<20 {
						return ks, b
					}
					ks = append(ks, key{k: "paste", text: string(b[6:])})
					return ks, nil
				}
				ks = append(ks, key{k: "paste", text: string(b[6:end])})
				b = b[end+6:]
				continue
			}
			if b[1] == '[' || b[1] == 'O' {
				k, n := parseCSI(b)
				if n == 0 {
					if len(b) < 32 {
						return ks, b
					}
					ks = append(ks, key{k: "esc"})
					b = b[1:]
					continue
				}
				if k.k != "" {
					ks = append(ks, k)
				}
				b = b[n:]
				continue
			}
			if b[1] == 0x1b {
				ks = append(ks, key{k: "esc"})
				b = b[1:]
				continue
			}
			if b[1] < 0x20 || b[1] == 0x7f {
				ks = append(ks, key{k: "alt+" + ctrlKey(b[1])})
				b = b[2:]
				continue
			}
			r, n := utf8.DecodeRune(b[1:])
			ks = append(ks, key{k: "alt+" + string(r), r: r})
			b = b[1+n:]
			continue
		}
		if c == ' ' {
			ks = append(ks, key{k: "space", r: ' '})
			b = b[1:]
			continue
		}
		if c < 0x20 || c == 0x7f {
			ks = append(ks, key{k: ctrlKey(c)})
			b = b[1:]
			continue
		}
		if !utf8.FullRune(b) {
			return ks, b
		}
		r, n := utf8.DecodeRune(b)
		ks = append(ks, key{k: string(r), r: r})
		b = b[n:]
	}
	return ks, nil
}

func parseCSI(b []byte) (key, int) {
	if b[1] == 'O' {
		if len(b) < 3 {
			return key{}, 0
		}
		m := map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left", 'H': "home",
			'F': "end", 'P': "f1", 'Q': "f2", 'R': "f3", 'S': "f4"}
		return key{k: m[b[2]]}, 3
	}
	i := 2
	for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
		i++
	}
	if i >= len(b) {
		return key{}, 0
	}
	params, fin, n := string(b[2:i]), b[i], i+1
	if strings.HasPrefix(params, "<") {
		f := strings.Split(params[1:], ";")
		if len(f) == 3 {
			bt, _ := strconv.Atoi(f[0])
			x, _ := strconv.Atoi(f[1])
			y, _ := strconv.Atoi(f[2])
			return key{k: "mouse", mb: bt, mx: x - 1, my: y - 1, mrel: fin == 'm'}, n
		}
		return key{}, n
	}
	ps := strings.Split(params, ";")
	mod := 1
	if len(ps) > 1 {
		mod, _ = strconv.Atoi(ps[1])
	}
	name := ""
	switch fin {
	case 'A':
		name = "up"
	case 'B':
		name = "down"
	case 'C':
		name = "right"
	case 'D':
		name = "left"
	case 'H':
		name = "home"
	case 'F':
		name = "end"
	case 'Z':
		return key{k: "shift+tab"}, n
	case 'P', 'Q', 'R', 'S':
		name = "f" + string(rune('1'+fin-'P'))
	case '~':
		name = map[string]string{"1": "home", "7": "home", "2": "insert", "3": "delete", "4": "end",
			"8": "end", "5": "pgup", "6": "pgdn", "15": "f5", "17": "f6", "18": "f7", "19": "f8",
			"20": "f9", "21": "f10", "23": "f11", "24": "f12"}[ps[0]]
	}
	if name == "" {
		return key{}, n
	}
	pre, m := "", mod-1
	if m&4 != 0 {
		pre += "ctrl+"
	}
	if m&2 != 0 {
		pre += "alt+"
	}
	if m&1 != 0 {
		pre += "shift+"
	}
	return key{k: pre + name}, n
}

// ---------------------------------------------------------------- cells

const (
	aBold = 1 << iota
	aDim
	aItal
	aUnder
	aRev
	aHole // sixel image area: never repainted while the image stays
)

type style struct {
	fg, bg int8
	attr   uint8
}

var plain = style{fg: -1, bg: -1}

func fg(c int8) style { return style{fg: c, bg: -1} }

// dim is secondary text. It uses the terminal's faint attribute, not colour
// 8: many themes make "bright black" nearly the background colour.
func dim() style                   { return plain.with(aDim) }
func (s style) with(a uint8) style { s.attr |= a; return s }
func (s style) on(c int8) style    { s.bg = c; return s }

func sgr(s style) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	for i, code := range []string{"1", "2", "3", "4", "7"} {
		if s.attr&(1<<i) != 0 {
			b.WriteString(";" + code)
		}
	}
	if s.fg >= 0 {
		if s.fg < 8 {
			fmt.Fprintf(&b, ";%d", 30+int(s.fg))
		} else {
			fmt.Fprintf(&b, ";%d", 90+int(s.fg)-8)
		}
	}
	if s.bg >= 0 {
		if s.bg < 8 {
			fmt.Fprintf(&b, ";%d", 40+int(s.bg))
		} else {
			fmt.Fprintf(&b, ";%d", 100+int(s.bg)-8)
		}
	}
	b.WriteString("m")
	return b.String()
}

func runeWidth(r rune) int {
	switch {
	case r < 0x20 || r == 0x7f:
		return 0
	case r < 0x300:
		return 1
	case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d || (r >= 0xfe00 && r <= 0xfe0f):
		return 0
	case r >= 0x1100 && (r <= 0x115f || r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) || (r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) || (r >= 0xfe30 && r <= 0xfe6f) || (r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) || (r >= 0x1f300 && r <= 0x1f64f) || (r >= 0x1f680 && r <= 0x1f6ff) ||
		(r >= 0x1f900 && r <= 0x1f9ff) || (r >= 0x20000 && r <= 0x3fffd)):
		return 2
	}
	return 1
}

func clean(r rune) rune {
	if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
		return '?'
	}
	return r
}

func strWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(clean(r))
	}
	return n
}

// fit shortens s to w columns. Names keep their extension: "long_na….txt".
func fit(s string, w int, keepExt bool) string {
	if w <= 0 {
		return ""
	}
	if strWidth(s) <= w {
		return s
	}
	ext := ""
	if keepExt {
		if _, e := splitExt(s); e != "" && strWidth(e) < w/2 && strWidth(e) <= 8 {
			ext = e
			s = s[:len(s)-len(e)]
		}
	}
	room := w - 1 - strWidth(ext)
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(clean(r))
		if n+rw > room {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	return b.String() + "…" + ext
}

type cell struct {
	s  string
	w  uint8
	st style
}

type grid struct {
	w, h int
	c    []cell
}

func newGrid(w, h int) *grid {
	g := &grid{w: w, h: h, c: make([]cell, w*h)}
	for i := range g.c {
		g.c[i] = cell{" ", 1, plain}
	}
	return g
}

func (g *grid) set(x, y int, c cell) {
	i := y*g.w + x
	if g.c[i].w == 0 && x > 0 { // overwriting the right half of a wide char
		g.c[i-1] = cell{" ", 1, g.c[i-1].st}
	}
	if g.c[i].w == 2 && x+1 < g.w {
		g.c[i+1] = cell{" ", 1, g.c[i+1].st}
	}
	g.c[i] = c
}

// text draws s at (x,y), clipped at maxX; returns the next column.
func (g *grid) text(x, y, maxX int, s string, st style) int {
	if y < 0 || y >= g.h {
		return x
	}
	if maxX > g.w {
		maxX = g.w
	}
	for _, r := range s {
		r = clean(r)
		w := runeWidth(r)
		if w == 0 {
			if x > 0 && x <= maxX {
				px := x - 1
				if g.c[y*g.w+px].w == 0 && px > 0 {
					px--
				}
				g.c[y*g.w+px].s += string(r)
			}
			continue
		}
		if x < 0 {
			x += w
			continue
		}
		if x+w > maxX {
			break
		}
		g.set(x, y, cell{string(r), uint8(w), st})
		if w == 2 {
			g.set(x+1, y, cell{"", 0, st})
		}
		x += w
	}
	return x
}

func (g *grid) fill(x0, y0, x1, y1 int, st style) {
	for y := max(y0, 0); y < min(y1, g.h); y++ {
		for x := max(x0, 0); x < min(x1, g.w); x++ {
			g.set(x, y, cell{" ", 1, st})
		}
	}
}

func (g *grid) vline(x, y0, y1 int, st style) {
	for y := y0; y < y1; y++ {
		if x >= 0 && x < g.w && y >= 0 && y < g.h {
			g.set(x, y, cell{"│", 1, st})
		}
	}
}

func (g *grid) box(x0, y0, x1, y1 int, title string, st, tst style) {
	g.fill(x0, y0, x1, y1, plain)
	for x := x0; x < x1; x++ {
		g.set(x, y0, cell{"─", 1, st})
		g.set(x, y1-1, cell{"─", 1, st})
	}
	for y := y0; y < y1; y++ {
		g.set(x0, y, cell{"│", 1, st})
		g.set(x1-1, y, cell{"│", 1, st})
	}
	g.set(x0, y0, cell{"╭", 1, st})
	g.set(x1-1, y0, cell{"╮", 1, st})
	g.set(x0, y1-1, cell{"╰", 1, st})
	g.set(x1-1, y1-1, cell{"╯", 1, st})
	if title != "" {
		g.text(x0+2, y0, x1-2, " "+title+" ", tst)
	}
}

// ---------------------------------------------------------------- entries

type trashItem struct {
	info, file, orig string
	when             time.Time
}

type entry struct {
	name  string      // what is shown (a relative path in search results)
	path  string      // absolute
	fi    fs.FileInfo // lstat; nil if it vanished or is unreadable
	ti    fs.FileInfo // stat of a symlink's target; nil if broken
	link  string
	count int // items in a directory, -1 unknown
	note  string
	trash *trashItem
}

func mkEntry(name, p string) *entry {
	e := &entry{name: name, path: p, count: -1}
	fi, err := os.Lstat(p)
	if err != nil {
		return e
	}
	e.fi = fi
	if fi.Mode()&fs.ModeSymlink != 0 {
		e.link, _ = os.Readlink(p)
		e.ti, _ = os.Stat(p)
	}
	return e
}

func (e *entry) isDir() bool {
	if e.fi == nil {
		return false
	}
	return e.fi.IsDir() || (e.ti != nil && e.ti.IsDir())
}
func (e *entry) isLink() bool { return e.fi != nil && e.fi.Mode()&fs.ModeSymlink != 0 }
func (e *entry) info() fs.FileInfo {
	if e.ti != nil {
		return e.ti
	}
	return e.fi
}
func (e *entry) size() int64 {
	if i := e.info(); i != nil {
		return i.Size()
	}
	return 0
}
func (e *entry) mtime() time.Time {
	if i := e.info(); i != nil {
		return i.ModTime()
	}
	return time.Time{}
}

const (
	tFile = iota
	tDir
	tLink
	tBroken
	tExec
	tImage
	tVideo
	tAudio
	tArchive
	tDoc
	tCode
)

var extKind = map[string]int{}

func init() {
	for k, exts := range map[int]string{
		tImage:   "png jpg jpeg gif webp bmp svg ico tif tiff avif heic jxl xcf psd",
		tVideo:   "mp4 mkv webm avi mov wmv flv m4v mpg mpeg ogv 3gp",
		tAudio:   "mp3 flac ogg opus wav m4a aac wma aiff mid midi",
		tArchive: "zip tar gz tgz bz2 tbz2 xz txz zst 7z rar iso deb rpm cab lz4 lzma jar apk zstd",
		tDoc:     "pdf epub djvu doc docx odt xls xlsx ods ppt pptx odp rtf md txt org tex csv",
		tCode:    "go c h cpp hpp cc rs py js ts jsx tsx lua sh bash zsh fish java kt rb php pl html css scss json yaml yml toml xml ini conf cfg vim sql mk cmake nix zig swift hs ml el diff patch",
	} {
		for _, e := range strings.Fields(exts) {
			extKind[e] = k
		}
	}
}

func kindOf(e *entry) int {
	switch {
	case e.fi == nil:
		return tBroken
	case e.isLink() && e.ti == nil:
		return tBroken
	case e.isDir():
		return tDir
	}
	if k, ok := extKind[strings.TrimPrefix(strings.ToLower(filepath.Ext(e.name)), ".")]; ok {
		return k
	}
	if e.isLink() {
		return tLink
	}
	if e.info().Mode()&0o111 != 0 {
		return tExec
	}
	return tFile
}

func kindStyle(k int) style {
	switch k {
	case tDir:
		return fg(4).with(aBold)
	case tLink:
		return fg(6)
	case tBroken:
		return fg(1).with(aItal)
	case tExec:
		return fg(2).with(aBold)
	case tImage:
		return fg(5)
	case tVideo, tAudio:
		return fg(13)
	case tArchive:
		return fg(1)
	case tDoc:
		return fg(3)
	case tCode:
		return fg(6)
	}
	return plain
}

var icons = map[string][11]string{
	//       file  dir   link  broken exec  image video audio archive doc  code
	"nerd":  {"", "", "", "", "", "", "", "", "", "", ""},
	"emoji": {"📄", "📁", "🔗", "⛓", "⚙", "🖼", "🎞", "🎵", "📦", "📕", "📝"},
	"ascii": {"-", "/", "@", "!", "*", "~", ">", "♪", "#", "=", "%"},
}

func natCmp(a, b string) int {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		if a[0] >= '0' && a[0] <= '9' && b[0] >= '0' && b[0] <= '9' {
			i, j := 0, 0
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(na) != len(nb) {
				return len(na) - len(nb)
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			a, b = a[i:], b[j:]
			continue
		}
		ra, sa := utf8.DecodeRuneInString(a)
		rb, sb := utf8.DecodeRuneInString(b)
		if ra != rb {
			return int(ra) - int(rb)
		}
		a, b = a[sa:], b[sb:]
	}
	return len(a) - len(b)
}

func sortEntries(es []*entry, c config) {
	cmp := func(x, y *entry) int {
		switch c.sortBy {
		case "size":
			if d := y.size() - x.size(); d != 0 {
				if d > 0 {
					return 1
				}
				return -1
			}
		case "time", "mtime", "date":
			if !x.mtime().Equal(y.mtime()) {
				if x.mtime().After(y.mtime()) {
					return -1
				}
				return 1
			}
		case "ext", "type":
			if d := strings.Compare(strings.ToLower(filepath.Ext(x.name)), strings.ToLower(filepath.Ext(y.name))); d != 0 {
				return d
			}
		case "deleted":
			if x.trash != nil && y.trash != nil && !x.trash.when.Equal(y.trash.when) {
				if x.trash.when.After(y.trash.when) {
					return -1
				}
				return 1
			}
		}
		return natCmp(x.name, y.name)
	}
	sort.SliceStable(es, func(i, j int) bool {
		x, y := es[i], es[j]
		if c.dirsFirst {
			if dx, dy := x.isDir(), y.isDir(); dx != dy {
				return dx
			}
		}
		r := cmp(x, y)
		if c.reverse {
			r = -r
		}
		return r < 0
	})
}

func readDir(dir string, hidden bool) ([]*entry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil && len(names) == 0 {
		return nil, err
	}
	out := make([]*entry, 0, len(names))
	for _, n := range names {
		if !hidden && strings.HasPrefix(n, ".") {
			continue
		}
		out = append(out, mkEntry(n, filepath.Join(dir, n)))
	}
	return out, nil
}

func modeString(m fs.FileMode) string {
	t := "-"
	switch {
	case m.IsDir():
		t = "d"
	case m&fs.ModeSymlink != 0:
		t = "l"
	case m&fs.ModeNamedPipe != 0:
		t = "p"
	case m&fs.ModeSocket != 0:
		t = "s"
	case m&fs.ModeCharDevice != 0:
		t = "c"
	case m&fs.ModeDevice != 0:
		t = "b"
	}
	b := []byte(t + "rwxrwxrwx")
	for i := 0; i < 9; i++ {
		if m&(1<<uint(8-i)) == 0 {
			b[i+1] = '-'
		}
	}
	if m&fs.ModeSetuid != 0 {
		b[3] = 's'
	}
	if m&fs.ModeSetgid != 0 {
		b[6] = 's'
	}
	if m&fs.ModeSticky != 0 {
		b[9] = 't'
	}
	return string(b)
}

var idCache sync.Map

func ownerOf(fi fs.FileInfo) (string, string) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "?", "?"
	}
	look := func(kind string, id uint32) string {
		k := kind + strconv.Itoa(int(id))
		if v, ok := idCache.Load(k); ok {
			return v.(string)
		}
		name := strconv.Itoa(int(id))
		if kind == "u" {
			if u, err := user.LookupId(name); err == nil {
				name = u.Username
			}
		} else if g, err := user.LookupGroupId(name); err == nil {
			name = g.Name
		}
		idCache.Store(k, name)
		return name
	}
	return look("u", st.Uid), look("g", st.Gid)
}

func freeSpace(p string) (free, total uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0, 0
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize)
}

func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil {
			if !fi.IsDir() { // worktree: "gitdir: path"
				b, _ := os.ReadFile(g)
				g = expand(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir: "), d)
			}
			b, err := os.ReadFile(filepath.Join(g, "HEAD"))
			if err != nil {
				return ""
			}
			h := strings.TrimSpace(string(b))
			if strings.HasPrefix(h, "ref: refs/heads/") {
				return h[16:]
			}
			if len(h) > 7 {
				return h[:7]
			}
			return h
		}
		if d == "/" || d == "." {
			return ""
		}
	}
}

// ---------------------------------------------------------------- mime

type mimeDB struct {
	once  sync.Once
	ext   map[string]string // "png" -> image/png
	names map[string]string
	globs [][2]string
	sub   map[string][]string // subclass -> parents
}

var mimes mimeDB

func (m *mimeDB) load() {
	m.once.Do(func() {
		m.ext, m.names, m.sub = map[string]string{}, map[string]string{}, map[string][]string{}
		weight := map[string]int{}
		for _, d := range dataDirs() {
			f, err := os.Open(filepath.Join(d, "mime", "globs2"))
			if err != nil {
				continue
			}
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				l := sc.Text()
				if l == "" || l[0] == '#' {
					continue
				}
				p := strings.SplitN(l, ":", 4)
				if len(p) < 3 {
					continue
				}
				w, _ := strconv.Atoi(p[0])
				t, g := p[1], p[2]
				switch {
				case strings.HasPrefix(g, "*.") && !strings.ContainsAny(g[2:], "*?["):
					e := strings.ToLower(g[2:])
					if old, ok := m.ext[e]; !ok || w > weight[e] {
						_ = old
						m.ext[e], weight[e] = t, w
					}
				case !strings.ContainsAny(g, "*?["):
					if _, ok := m.names[g]; !ok {
						m.names[g] = t
					}
				default:
					m.globs = append(m.globs, [2]string{g, t})
				}
			}
			f.Close()
			if b, err := os.ReadFile(filepath.Join(d, "mime", "subclasses")); err == nil {
				for _, l := range strings.Split(string(b), "\n") {
					if f := strings.Fields(l); len(f) == 2 {
						m.sub[f[0]] = append(m.sub[f[0]], f[1])
					}
				}
			}
		}
		for e, t := range map[string]string{"txt": "text/plain", "md": "text/markdown", "png": "image/png",
			"jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp", "svg": "image/svg+xml",
			"pdf": "application/pdf", "zip": "application/zip", "mp4": "video/mp4", "mkv": "video/x-matroska",
			"webm": "video/webm", "mp3": "audio/mpeg", "flac": "audio/flac", "ogg": "audio/ogg", "html": "text/html",
			"go": "text/x-go", "py": "text/x-python", "sh": "application/x-shellscript", "json": "application/json",
			"gz": "application/gzip", "tar": "application/x-tar", "epub": "application/epub+zip"} {
			if _, ok := m.ext[e]; !ok {
				m.ext[e] = t
			}
		}
	})
}

func dataDirs() []string {
	ds := []string{dataHome()}
	x := firstOf(os.Getenv("XDG_DATA_DIRS"), "/usr/local/share:/usr/share")
	for _, d := range strings.Split(x, ":") {
		if d != "" {
			ds = append(ds, d)
		}
	}
	return ds
}

func mimeOf(p string) string {
	fi, err := os.Stat(p)
	if err != nil {
		return "inode/symlink"
	}
	if fi.IsDir() {
		return "inode/directory"
	}
	mimes.load()
	base := filepath.Base(p)
	if t, ok := mimes.names[base]; ok {
		return t
	}
	low := strings.ToLower(base)
	for i := 0; i < len(low); i++ { // longest extension first: .tar.gz before .gz
		if low[i] == '.' {
			if t, ok := mimes.ext[low[i+1:]]; ok {
				return t
			}
		}
	}
	for _, g := range mimes.globs {
		if ok, _ := filepath.Match(g[0], base); ok {
			return g[1]
		}
	}
	if fi.Size() == 0 {
		return "application/x-zerosize"
	}
	f, err := os.Open(p)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	return sniff(buf[:n])
}

func sniff(b []byte) string {
	for _, m := range []struct{ magic, t string }{
		{"\x89PNG", "image/png"}, {"\xff\xd8\xff", "image/jpeg"}, {"GIF8", "image/gif"},
		{"%PDF", "application/pdf"}, {"PK\x03\x04", "application/zip"}, {"\x1f\x8b", "application/gzip"},
		{"\x7fELF", "application/x-executable"}, {"BZh", "application/x-bzip2"},
		{"\xfd7zXZ", "application/x-xz"}, {"7z\xbc\xaf", "application/x-7z-compressed"},
	} {
		if bytes.HasPrefix(b, []byte(m.magic)) {
			return m.t
		}
	}
	if bytes.HasPrefix(b, []byte("#!")) {
		return "application/x-shellscript"
	}
	if isText(b) {
		return "text/plain"
	}
	return "application/octet-stream"
}

func isText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	bad := 0
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 && len(b) > 3 {
			bad++
		}
		b = b[n:]
	}
	return bad < 4
}

// mime family: text/x-go -> text/plain -> application/octet-stream
func mimeChain(t string) []string {
	mimes.load()
	out, seen := []string{}, map[string]bool{}
	var walk func(string)
	walk = func(t string) {
		if seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
		for _, p := range mimes.sub[t] {
			walk(p)
		}
	}
	walk(t)
	if strings.HasPrefix(t, "text/") {
		walk("text/plain")
	}
	if t == "application/x-zerosize" || t == "application/x-shellscript" {
		walk("text/plain")
	}
	return out
}

// ---------------------------------------------------------------- applications

type desktopApp struct {
	id, name, exec string
	terminal       bool
	mimes          []string
	hidden         bool
}

var appsOnce struct {
	sync.Once
	apps map[string]*desktopApp
}

func loadApps() map[string]*desktopApp {
	appsOnce.Do(func() {
		apps := map[string]*desktopApp{}
		for _, d := range dataDirs() {
			root := filepath.Join(d, "applications")
			filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
				if err != nil || de.IsDir() || !strings.HasSuffix(p, ".desktop") {
					return nil
				}
				rel, _ := filepath.Rel(root, p)
				id := strings.ReplaceAll(rel, "/", "-")
				if _, ok := apps[id]; ok {
					return nil // earlier dirs (user) win
				}
				if a := parseDesktop(p); a != nil {
					a.id = id
					apps[id] = a
				}
				return nil
			})
		}
		appsOnce.apps = apps
	})
	return appsOnce.apps
}

func parseDesktop(p string) *desktopApp {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	a, in, typ := &desktopApp{}, false, ""
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = l == "[Desktop Entry]"
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Name":
			a.name = v
		case "Exec":
			a.exec = v
		case "Terminal":
			a.terminal = v == "true"
		case "MimeType":
			for _, m := range strings.Split(v, ";") {
				if m != "" {
					a.mimes = append(a.mimes, m)
				}
			}
		case "NoDisplay", "Hidden":
			if v == "true" {
				a.hidden = true
			}
		case "Type":
			typ = v
		}
	}
	if typ != "Application" || a.exec == "" {
		return nil
	}
	return a
}

func mimeappsFiles() []string {
	return []string{filepath.Join(configHome(), "mimeapps.list"), "/etc/xdg/mimeapps.list",
		filepath.Join(dataHome(), "applications", "mimeapps.list"),
		"/usr/local/share/applications/mimeapps.list", "/usr/share/applications/mimeapps.list"}
}

// defaults and added associations, per freedesktop mime-apps spec (simplified)
func mimeassoc() (def, added, removed map[string][]string) {
	def, added, removed = map[string][]string{}, map[string][]string{}, map[string][]string{}
	for _, f := range mimeappsFiles() {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		sec := ""
		for _, l := range strings.Split(string(b), "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "[") {
				sec = l
				continue
			}
			k, v, ok := strings.Cut(l, "=")
			if !ok {
				continue
			}
			var m map[string][]string
			switch sec {
			case "[Default Applications]":
				m = def
			case "[Added Associations]":
				m = added
			case "[Removed Associations]":
				m = removed
			default:
				continue
			}
			for _, id := range strings.Split(v, ";") {
				if id != "" {
					m[k] = append(m[k], id)
				}
			}
		}
	}
	return
}

// appsFor returns the apps for a mime type, default first.
func appsFor(t string) []*desktopApp {
	apps := loadApps()
	def, added, removed := mimeassoc()
	var out []*desktopApp
	seen := map[string]bool{}
	add := func(id string) {
		if a, ok := apps[id]; ok && !seen[id] {
			seen[id] = true
			out = append(out, a)
		}
	}
	chain := mimeChain(t)
	for _, m := range chain {
		for _, id := range def[m] {
			add(id)
		}
	}
	for _, m := range chain {
		for _, id := range added[m] {
			add(id)
		}
		var ids []string
		for id, a := range apps {
			for _, am := range a.mimes {
				if am == m && !a.hidden {
					ids = append(ids, id)
				}
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			gone := false
			for _, r := range removed[m] {
				gone = gone || r == id
			}
			if !gone {
				add(id)
			}
		}
	}
	return out
}

// setDefaultApp writes "mime=id;" into ~/.config/mimeapps.list.
func setDefaultApp(mime, id string) error {
	p := filepath.Join(configHome(), "mimeapps.list")
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(b) == 0 {
		lines = nil
	}
	out, in, done, hasSec := []string{}, false, false, false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			if in && !done {
				out = append(out, mime+"="+id+";")
				done = true
			}
			in = t == "[Default Applications]"
			hasSec = hasSec || in
		} else if in && strings.HasPrefix(t, mime+"=") {
			if !done {
				out = append(out, mime+"="+id+";")
				done = true
			}
			continue
		}
		out = append(out, l)
	}
	if !done {
		if !hasSec {
			out = append(out, "[Default Applications]")
		}
		out = append(out, mime+"="+id+";")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o644)
}

// execArgs expands a desktop Exec line. One command per file if it uses %f/%u.
func execArgs(execLine string, files []string) [][]string {
	var toks []string
	var cur strings.Builder
	inq, have := false, false
	for i := 0; i < len(execLine); i++ {
		c := execLine[i]
		switch {
		case c == '"':
			inq, have = !inq, true
		case c == '\\' && inq && i+1 < len(execLine):
			i++
			cur.WriteByte(execLine[i])
		case c == ' ' && !inq:
			if have || cur.Len() > 0 {
				toks = append(toks, cur.String())
			}
			cur.Reset()
			have = false
		default:
			cur.WriteByte(c)
		}
	}
	if have || cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	single, multi := false, false
	for _, t := range toks {
		single = single || strings.Contains(t, "%f") || strings.Contains(t, "%u")
		multi = multi || strings.Contains(t, "%F") || strings.Contains(t, "%U")
	}
	build := func(fs []string) []string {
		var out []string
		for _, t := range toks {
			switch t {
			case "%f", "%u":
				out = append(out, fs...)
				continue
			case "%F", "%U":
				out = append(out, fs...)
				continue
			case "%i", "%c", "%k", "%d", "%D", "%n", "%N", "%v", "%m":
				continue
			}
			t = strings.ReplaceAll(t, "%f", strings.Join(fs, " "))
			t = strings.ReplaceAll(t, "%u", strings.Join(fs, " "))
			t = strings.ReplaceAll(t, "%%", "%")
			out = append(out, t)
		}
		if !single && !multi {
			out = append(out, fs...)
		}
		return out
	}
	if single && !multi && len(files) > 1 {
		var cmds [][]string
		for _, f := range files {
			cmds = append(cmds, build([]string{f}))
		}
		return cmds
	}
	return [][]string{build(files)}
}

func spawn(dir string, args ...string) error {
	if len(args) == 0 {
		return errors.New("empty command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// ---------------------------------------------------------------- trash (freedesktop spec)

func homeTrash() string { return filepath.Join(dataHome(), "Trash") }

func devOf(p string) (uint64, bool) {
	var st syscall.Stat_t
	if syscall.Lstat(p, &st) != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

func mountTop(p string) string {
	d, _ := devOf(p)
	cur := p
	for {
		par := filepath.Dir(cur)
		if par == cur {
			return cur
		}
		if pd, ok := devOf(par); !ok || pd != d {
			return cur
		}
		cur = par
	}
}

func trashDirFor(p string) string {
	ht := homeTrash()
	os.MkdirAll(ht, 0o700)
	hd, _ := devOf(ht)
	if d, _ := devOf(filepath.Dir(p)); d == hd {
		return ht
	}
	return filepath.Join(mountTop(filepath.Dir(p)), fmt.Sprintf(".Trash-%d", os.Getuid()))
}

func trashPut(p string) (*trashItem, error) {
	td := trashDirFor(p)
	if err := os.MkdirAll(filepath.Join(td, "files"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(td, "info"), 0o700); err != nil {
		return nil, err
	}
	base := filepath.Base(p)
	name := base
	var info string
	for i := 1; ; i++ {
		info = filepath.Join(td, "info", name+".trashinfo")
		f, err := os.OpenFile(info, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			now := time.Now()
			fmt.Fprintf(f, "[Trash Info]\nPath=%s\nDeletionDate=%s\n",
				(&url.URL{Path: p}).EscapedPath(), now.Format("2006-01-02T15:04:05"))
			f.Close()
			file := filepath.Join(td, "files", name)
			if err := os.Rename(p, file); err != nil {
				os.Remove(info)
				return nil, err
			}
			return &trashItem{info: info, file: file, orig: p, when: now}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		stem, ext := splitExt(base)
		name = fmt.Sprintf("%s.%d%s", stem, i, ext)
	}
}

func trashDirs() []string {
	ds := []string{homeTrash()}
	b, _ := os.ReadFile("/proc/mounts")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		mp := strings.ReplaceAll(f[1], `\040`, " ")
		t := filepath.Join(mp, fmt.Sprintf(".Trash-%d", os.Getuid()))
		if isDirPath(t) && t != homeTrash() {
			ds = append(ds, t)
		}
	}
	return ds
}

func trashList() []*entry {
	var out []*entry
	for _, td := range trashDirs() {
		infos, _ := filepath.Glob(filepath.Join(td, "info", "*.trashinfo"))
		for _, info := range infos {
			b, err := os.ReadFile(info)
			if err != nil {
				continue
			}
			it := &trashItem{info: info, file: filepath.Join(td, "files", strings.TrimSuffix(filepath.Base(info), ".trashinfo"))}
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "Path="); ok {
					if u, err := url.PathUnescape(v); err == nil {
						v = u
					}
					if !filepath.IsAbs(v) {
						v = filepath.Join(filepath.Dir(td), v)
					}
					it.orig = v
				}
				if v, ok := strings.CutPrefix(l, "DeletionDate="); ok {
					it.when, _ = time.ParseInLocation("2006-01-02T15:04:05", v, time.Local)
				}
			}
			e := mkEntry(filepath.Base(it.orig), it.file)
			e.trash = it
			e.note = tilde(filepath.Dir(it.orig))
			out = append(out, e)
		}
	}
	return out
}

func trashRestore(it *trashItem) (string, error) {
	dst := uniqueName(it.orig)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(it.file, dst); err != nil {
		return "", err
	}
	os.Remove(it.info)
	return dst, nil
}

// ---------------------------------------------------------------- copying

type job struct {
	name        string
	done, total atomic.Int64
	cancel      context.CancelFunc
	ctx         context.Context
}

func duSize(p string) (size, files int64) {
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			files++
			if i, err := d.Info(); err == nil && i.Mode().IsRegular() {
				size += i.Size()
			}
		}
		return nil
	})
	return
}

func copyTree(j *job, src, dst string) error {
	if j.ctx.Err() != nil {
		return j.ctx.Err()
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(l, dst)
	case fi.IsDir():
		if err := os.Mkdir(dst, fi.Mode().Perm()|0o700); err != nil && !os.IsExist(err) {
			return err
		}
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		names, _ := f.Readdirnames(-1)
		f.Close()
		for _, n := range names {
			if err := copyTree(j, filepath.Join(src, n), filepath.Join(dst, n)); err != nil {
				return err
			}
		}
		os.Chmod(dst, fi.Mode().Perm())
		os.Chtimes(dst, fi.ModTime(), fi.ModTime())
		return nil
	case fi.Mode().IsRegular():
		return copyFile(j, src, dst, fi)
	}
	return nil // devices, sockets, fifos are skipped
}

func copyFile(j *job, src, dst string, fi fs.FileInfo) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		if j.ctx.Err() != nil {
			out.Close()
			os.Remove(dst)
			return j.ctx.Err()
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(dst)
				return werr
			}
			j.done.Add(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(dst)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}

// ---------------------------------------------------------------- archives

func archiveKind(p string) string {
	l := strings.ToLower(p)
	switch {
	case strings.HasSuffix(l, ".zip"), strings.HasSuffix(l, ".jar"), strings.HasSuffix(l, ".epub"), strings.HasSuffix(l, ".apk"):
		return "zip"
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tgz"
	case strings.HasSuffix(l, ".tar.bz2"), strings.HasSuffix(l, ".tbz2"), strings.HasSuffix(l, ".tbz"):
		return "tbz"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	case strings.HasSuffix(l, ".gz"):
		return "gz"
	}
	for _, e := range []string{".tar.xz", ".txz", ".tar.zst", ".7z", ".rar", ".xz", ".zst", ".iso", ".cab", ".lz4", ".lzma", ".deb", ".rpm", ".cpio"} {
		if strings.HasSuffix(l, e) {
			return "bsdtar"
		}
	}
	return ""
}

func tarReader(p, kind string) (*tar.Reader, io.Closer, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	var r io.Reader = f
	switch kind {
	case "tgz":
		g, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		r = g
	case "tbz":
		r = bzip2.NewReader(f)
	}
	return tar.NewReader(r), f, nil
}

func archiveList(p string, limit int) ([]string, error) {
	var out []string
	switch k := archiveKind(p); k {
	case "zip":
		z, err := zip.OpenReader(p)
		if err != nil {
			return nil, err
		}
		defer z.Close()
		for _, f := range z.File {
			if len(out) >= limit {
				out = append(out, fmt.Sprintf("… %d entries", len(z.File)))
				break
			}
			out = append(out, fmt.Sprintf("%7s  %s", human(int64(f.UncompressedSize64)), f.Name))
		}
	case "tar", "tgz", "tbz":
		tr, c, err := tarReader(p, k)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		for len(out) < limit {
			h, err := tr.Next()
			if err != nil {
				break
			}
			out = append(out, fmt.Sprintf("%7s  %s", human(h.Size), h.Name))
		}
	case "gz":
		out = append(out, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
	case "bsdtar":
		if !have("bsdtar") {
			return []string{"(install libarchive for this format)"}, nil
		}
		b, err := exec.Command("bsdtar", "-tf", p).Output()
		if err != nil {
			return nil, err
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if len(out) >= limit {
				break
			}
			out = append(out, l)
		}
	}
	return out, nil
}

func safeJoin(dst, name string) (string, error) {
	p := filepath.Join(dst, name)
	if !isInside(p, dst) {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	return p, nil
}

func extract(j *job, p, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	switch k := archiveKind(p); k {
	case "zip":
		z, err := zip.OpenReader(p)
		if err != nil {
			return err
		}
		defer z.Close()
		for _, f := range z.File {
			if j.ctx.Err() != nil {
				return j.ctx.Err()
			}
			t, err := safeJoin(dst, f.Name)
			if err != nil {
				return err
			}
			if f.FileInfo().IsDir() {
				os.MkdirAll(t, 0o755)
				continue
			}
			os.MkdirAll(filepath.Dir(t), 0o755)
			rc, err := f.Open()
			if err != nil {
				return err
			}
			mode := f.Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				rc.Close()
				return err
			}
			n, err := io.Copy(out, rc)
			j.done.Add(n)
			rc.Close()
			out.Close()
			if err != nil {
				return err
			}
		}
	case "tar", "tgz", "tbz":
		tr, c, err := tarReader(p, k)
		if err != nil {
			return err
		}
		defer c.Close()
		for {
			if j.ctx.Err() != nil {
				return j.ctx.Err()
			}
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			t, err := safeJoin(dst, h.Name)
			if err != nil {
				return err
			}
			switch h.Typeflag {
			case tar.TypeDir:
				os.MkdirAll(t, fs.FileMode(h.Mode).Perm()|0o700)
			case tar.TypeSymlink:
				os.MkdirAll(filepath.Dir(t), 0o755)
				os.Symlink(h.Linkname, t)
			case tar.TypeReg:
				os.MkdirAll(filepath.Dir(t), 0o755)
				out, err := os.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(h.Mode).Perm())
				if err != nil {
					return err
				}
				n, err := io.Copy(out, tr)
				j.done.Add(n)
				out.Close()
				if err != nil {
					return err
				}
				os.Chtimes(t, h.ModTime, h.ModTime)
			}
		}
	case "gz":
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		g, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(dst, strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))))
		if err != nil {
			return err
		}
		defer out.Close()
		n, err := io.Copy(out, g)
		j.done.Add(n)
		return err
	case "bsdtar":
		if !have("bsdtar") {
			return errors.New("this format needs bsdtar (libarchive)")
		}
		cmd := exec.CommandContext(j.ctx, "bsdtar", "-xf", p, "-C", dst)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("bsdtar: %s", strings.TrimSpace(string(out)))
		}
	default:
		return errors.New("not an archive")
	}
	return nil
}

func compress(j *job, out string, base string, items []string) error {
	l := strings.ToLower(out)
	switch {
	case strings.HasSuffix(l, ".zip"):
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		zw := zip.NewWriter(f)
		for _, it := range items {
			err := filepath.Walk(it, func(p string, fi fs.FileInfo, err error) error {
				if err != nil || j.ctx.Err() != nil {
					return firstErr(err, j.ctx.Err())
				}
				rel, _ := filepath.Rel(base, p)
				h, err := zip.FileInfoHeader(fi)
				if err != nil {
					return err
				}
				h.Name = filepath.ToSlash(rel)
				if fi.IsDir() {
					h.Name += "/"
					_, err = zw.CreateHeader(h)
					return err
				}
				if !fi.Mode().IsRegular() {
					return nil
				}
				h.Method = zip.Deflate
				w, err := zw.CreateHeader(h)
				if err != nil {
					return err
				}
				src, err := os.Open(p)
				if err != nil {
					return err
				}
				n, err := io.Copy(w, src)
				src.Close()
				j.done.Add(n)
				return err
			})
			if err != nil {
				zw.Close()
				f.Close()
				os.Remove(out)
				return err
			}
		}
		if err := zw.Close(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"), strings.HasSuffix(l, ".tar"):
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		var w io.Writer = f
		var gz *gzip.Writer
		if !strings.HasSuffix(l, ".tar") {
			gz = gzip.NewWriter(f)
			w = gz
		}
		tw := tar.NewWriter(w)
		for _, it := range items {
			err := filepath.Walk(it, func(p string, fi fs.FileInfo, err error) error {
				if err != nil || j.ctx.Err() != nil {
					return firstErr(err, j.ctx.Err())
				}
				rel, _ := filepath.Rel(base, p)
				link := ""
				if fi.Mode()&fs.ModeSymlink != 0 {
					link, _ = os.Readlink(p)
				}
				h, err := tar.FileInfoHeader(fi, link)
				if err != nil {
					return err
				}
				h.Name = filepath.ToSlash(rel)
				if err := tw.WriteHeader(h); err != nil {
					return err
				}
				if !fi.Mode().IsRegular() {
					return nil
				}
				src, err := os.Open(p)
				if err != nil {
					return err
				}
				n, err := io.Copy(tw, src)
				src.Close()
				j.done.Add(n)
				return err
			})
			if err != nil {
				f.Close()
				os.Remove(out)
				return err
			}
		}
		tw.Close()
		if gz != nil {
			gz.Close()
		}
		return f.Close()
	}
	if !have("bsdtar") {
		return errors.New("use .zip, .tar or .tar.gz (other formats need bsdtar)")
	}
	args := []string{"-acf", out, "-C", base}
	for _, it := range items {
		rel, _ := filepath.Rel(base, it)
		args = append(args, rel)
	}
	if b, err := exec.CommandContext(j.ctx, "bsdtar", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("bsdtar: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

func firstErr(es ...error) error {
	for _, e := range es {
		if e != nil {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------- preview

type sline struct {
	s  string
	st style
}

type preview struct {
	head    string
	lines   []sline
	img     image.Image
	six     string
	sixKey  string
	loading bool
}

func buildPreview(e *entry, hidden bool) *preview {
	pv := &preview{}
	if e.fi == nil {
		pv.head = "unreadable"
		return pv
	}
	if e.isDir() {
		es, err := readDir(e.path, hidden)
		if err != nil {
			pv.head = err.Error()
			return pv
		}
		sortEntries(es, config{dirsFirst: true, sortBy: "name"})
		pv.head = fmt.Sprintf("%d items", len(es))
		if len(es) == 1 {
			pv.head = "1 item"
		}
		for i, c := range es {
			if i >= 400 {
				break
			}
			pv.lines = append(pv.lines, sline{c.name, kindStyle(kindOf(c))})
		}
		return pv
	}
	mt := mimeOf(e.path)
	pv.head = mt + " · " + human(e.size())
	if strings.HasPrefix(mt, "image/") {
		if f, err := os.Open(e.path); err == nil {
			if img, _, err := image.Decode(f); err == nil {
				b := img.Bounds()
				pv.head = fmt.Sprintf("%s · %d×%d · %s", mt, b.Dx(), b.Dy(), human(e.size()))
				pv.img = img
			}
			f.Close()
		}
		if pv.img != nil {
			return pv
		}
	}
	if archiveKind(e.path) != "" {
		names, err := archiveList(e.path, 300)
		if err != nil {
			pv.lines = []sline{{err.Error(), fg(1)}}
		}
		for _, n := range names {
			pv.lines = append(pv.lines, sline{n, plain})
		}
		return pv
	}
	if mt == "application/pdf" && have("pdftotext") {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if b, err := exec.CommandContext(ctx, "pdftotext", "-l", "3", "-layout", e.path, "-").Output(); err == nil {
			pv.lines = textLines(b, 400)
			return pv
		}
	}
	f, err := os.Open(e.path)
	if err != nil {
		pv.lines = []sline{{err.Error(), fg(1)}}
		return pv
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	if isText(buf[:min(n, 4096)]) {
		pv.lines = textLines(buf, 600)
		return pv
	}
	for off := 0; off < min(n, 1024); off += 16 {
		row := buf[off:min(off+16, n)]
		var asc strings.Builder
		for _, c := range row {
			if c >= 0x20 && c < 0x7f {
				asc.WriteByte(c)
			} else {
				asc.WriteByte('.')
			}
		}
		pv.lines = append(pv.lines, sline{fmt.Sprintf("%08x  %-47s  %s", off, spaced(hex.EncodeToString(row)), asc.String()), dim()})
	}
	return pv
}

func spaced(h string) string {
	var b strings.Builder
	for i := 0; i < len(h); i += 2 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(h[i:min(i+2, len(h))])
	}
	return b.String()
}

func textLines(b []byte, max int) []sline {
	var out []sline
	for _, l := range strings.Split(string(b), "\n") {
		if len(out) >= max {
			break
		}
		l = strings.ReplaceAll(strings.TrimRight(l, "\r"), "\t", "    ")
		out = append(out, sline{l, plain})
	}
	return out
}

// sixel encodes img to fit pw×ph pixels using a 6×6×6 colour cube with
// ordered dithering. Transparent pixels stay transparent.
func sixel(img image.Image, pw, ph int) string {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw == 0 || sh == 0 || pw < 6 || ph < 6 {
		return ""
	}
	scale := min(float64(pw)/float64(sw), float64(ph)/float64(sh), 1)
	tw, th := max(int(float64(sw)*scale), 1), max(int(float64(sh)*scale), 1)
	// box-average the source into a float buffer, then quantize to the 6x6x6
	// cube with Floyd–Steinberg error diffusion (smoother than ordered dither)
	buf := make([][3]float64, tw*th)
	alpha := make([]bool, tw*th)
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			x0, x1 := b.Min.X+x*sw/tw, b.Min.X+max((x+1)*sw/tw, x*sw/tw+1)
			y0, y1 := b.Min.Y+y*sh/th, b.Min.Y+max((y+1)*sh/th, y*sh/th+1)
			stepX, stepY := max((x1-x0)/4, 1), max((y1-y0)/4, 1)
			var r, g, bl, a, n float64
			for sy := y0; sy < y1; sy += stepY {
				for sx := x0; sx < x1; sx += stepX {
					c := color.NRGBAModel.Convert(img.At(sx, sy)).(color.NRGBA)
					r, g, bl, a, n = r+float64(c.R), g+float64(c.G), bl+float64(c.B), a+float64(c.A), n+1
				}
			}
			buf[y*tw+x] = [3]float64{r / n, g / n, bl / n}
			alpha[y*tw+x] = a/n >= 128
		}
	}
	idx := make([]int16, tw*th)
	spread := func(x, y int, e [3]float64, w float64) {
		if x < 0 || x >= tw || y >= th {
			return
		}
		p := &buf[y*tw+x]
		for c := 0; c < 3; c++ {
			p[c] += e[c] * w
		}
	}
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			i := y*tw + x
			if !alpha[i] {
				idx[i] = -1
				continue
			}
			var q [3]int
			var e [3]float64
			for c := 0; c < 3; c++ {
				v := min(max(buf[i][c], 0), 255)
				q[c] = int(v*5/255 + 0.5)
				e[c] = v - float64(q[c]*51)
			}
			idx[i] = int16(q[0]*36 + q[1]*6 + q[2])
			spread(x+1, y, e, 7.0/16)
			spread(x-1, y+1, e, 3.0/16)
			spread(x, y+1, e, 5.0/16)
			spread(x+1, y+1, e, 1.0/16)
		}
	}
	var o strings.Builder
	fmt.Fprintf(&o, "\x1bP0;1;0q\"1;1;%d;%d", tw, th)
	for i := 0; i < 216; i++ {
		fmt.Fprintf(&o, "#%d;2;%d;%d;%d", i, i/36*20, i/6%6*20, i%6*20)
	}
	rows := map[int16][]byte{}
	for band := 0; band < th; band += 6 {
		for k := range rows {
			delete(rows, k)
		}
		var order []int16
		for r := 0; r < 6 && band+r < th; r++ {
			for x := 0; x < tw; x++ {
				c := idx[(band+r)*tw+x]
				if c < 0 {
					continue
				}
				row, ok := rows[c]
				if !ok {
					row = make([]byte, tw)
					rows[c] = row
					order = append(order, c)
				}
				row[x] |= 1 << uint(r)
			}
		}
		for i, c := range order {
			if i > 0 {
				o.WriteByte('$')
			}
			fmt.Fprintf(&o, "#%d", c)
			row := rows[c]
			for x := 0; x < tw; {
				ch, run := row[x], 1
				for x+run < tw && row[x+run] == ch {
					run++
				}
				if run > 3 {
					fmt.Fprintf(&o, "!%d%c", run, 63+ch)
				} else {
					for k := 0; k < run; k++ {
						o.WriteByte(63 + ch)
					}
				}
				x += run
			}
		}
		o.WriteByte('-')
	}
	o.WriteString("\x1b\\")
	return o.String()
}

// ---------------------------------------------------------------- places

type place struct {
	label, path, icon string
	dev               *device
	section           string
}

type device struct {
	dev, label, size, fstype, mount string
}

func userDirs() map[string]string {
	out := map[string]string{}
	b, _ := os.ReadFile(filepath.Join(configHome(), "user-dirs.dirs"))
	for _, l := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(k), "#") {
			continue
		}
		v = strings.ReplaceAll(strings.Trim(v, `"`), "$HOME", home())
		out[strings.TrimSuffix(strings.TrimPrefix(k, "XDG_"), "_DIR")] = v
	}
	return out
}

func gtkBookmarksFile() string { return filepath.Join(configHome(), "gtk-3.0", "bookmarks") }

// Bookmarks are GTK's file, so every GTK file picker shares them.
func loadBookmarks() []place {
	var out []place
	b, _ := os.ReadFile(gtkBookmarksFile())
	for _, l := range strings.Split(string(b), "\n") {
		uri, label, _ := strings.Cut(strings.TrimSpace(l), " ")
		if !strings.HasPrefix(uri, "file://") {
			continue
		}
		p := expand(uri, "/")
		if label == "" {
			label = filepath.Base(p)
		}
		out = append(out, place{label: label, path: p, icon: "★", section: "Bookmarks"})
	}
	return out
}

func toggleBookmark(dir string) (bool, error) {
	f := gtkBookmarksFile()
	b, _ := os.ReadFile(f)
	var keep []string
	found := false
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if l == "" {
			continue
		}
		uri, _, _ := strings.Cut(l, " ")
		if strings.HasPrefix(uri, "file://") && expand(uri, "/") == dir {
			found = true
			continue
		}
		keep = append(keep, l)
	}
	if !found {
		keep = append(keep, "file://"+(&url.URL{Path: dir}).EscapedPath())
	}
	os.MkdirAll(filepath.Dir(f), 0o755)
	data := strings.Join(keep, "\n")
	if data != "" {
		data += "\n"
	}
	return !found, os.WriteFile(f, []byte(data), 0o644)
}

// devices lists removable or unmounted partitions via lsblk (util-linux).
func listDevices() []device {
	if !have("lsblk") {
		return nil
	}
	b, err := exec.Command("lsblk", "-P", "-p", "-o", "NAME,LABEL,SIZE,FSTYPE,MOUNTPOINT,HOTPLUG,TYPE,RM").Output()
	if err != nil {
		return nil
	}
	var out []device
	for _, l := range strings.Split(string(b), "\n") {
		kv := map[string]string{}
		for _, m := range strings.Split(l, `" `) {
			k, v, ok := strings.Cut(m, `="`)
			if ok {
				kv[k] = strings.TrimSuffix(v, `"`)
			}
		}
		if kv["NAME"] == "" || kv["FSTYPE"] == "" || kv["FSTYPE"] == "swap" || kv["FSTYPE"] == "crypto_LUKS" {
			continue
		}
		if kv["TYPE"] != "part" && kv["TYPE"] != "disk" && kv["TYPE"] != "rom" {
			continue
		}
		mp := kv["MOUNTPOINT"]
		removable := kv["HOTPLUG"] == "1" || kv["RM"] == "1"
		if mp == "/" || mp == "/boot" || mp == "/efi" || mp == "/home" || mp == "/boot/efi" || strings.HasPrefix(mp, "[") {
			continue
		}
		if !removable && mp == "" && !strings.HasPrefix(kv["FSTYPE"], "ntfs") && kv["FSTYPE"] != "vfat" && kv["FSTYPE"] != "exfat" {
			continue // an internal partition nobody mounted on purpose
		}
		label := firstOf(kv["LABEL"], filepath.Base(kv["NAME"]))
		out = append(out, device{dev: kv["NAME"], label: label, size: kv["SIZE"], fstype: kv["FSTYPE"], mount: mp})
	}
	return out
}

func udisks(args ...string) (string, error) {
	if !have("udisksctl") {
		return "", errors.New("mounting needs udisks2 (udisksctl)")
	}
	b, err := exec.Command("udisksctl", args...).CombinedOutput()
	s := strings.TrimSpace(string(b))
	if err != nil {
		return "", errors.New(firstOf(s, err.Error()))
	}
	return s, nil
}

// ---------------------------------------------------------------- app state

const (
	kDir = iota
	kSearch
	kTrash
)

type pane struct {
	dir, title string
	kind       int
	all, view  []*entry
	cur, top   int
	sel        map[string]bool
	filter     string
	back, fwd  []string
	stamp      time.Time
	err        string
	cancel     context.CancelFunc
	loading    bool
	visual     int
	origin     string // dir a search started from
}

func newPane(dir string) *pane { return &pane{dir: dir, sel: map[string]bool{}, visual: -1} }

func (p *pane) entry() *entry {
	if p.cur >= 0 && p.cur < len(p.view) {
		return p.view[p.cur]
	}
	return nil
}

func (p *pane) stopSearch() {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.loading = false
}

type tab struct {
	panes  [2]*pane
	split  bool
	active int
}

func (t *tab) pane() *pane { return t.panes[t.active] }

type undo struct {
	desc string
	fn   func() error
}

type prompt struct {
	label    string
	buf      []rune
	pos      int
	done     func(string)
	change   func(string)
	cancel   func()
	complete bool
}

type mitem struct {
	label, hint string
	hot         string
	st          style
}

type menu struct {
	title      string
	items      []mitem
	cur, top   int
	filter     string
	filterable bool
	keys       map[string]string // extra keys -> hint in the footer
	pick       func(i int, key string)
	shown      []int
}

type overlay struct {
	title  string
	lines  []sline
	scroll int
	keys   map[string]func()
	footer string
}

type hit struct {
	x0, y0, x1, y1 int
	kind           string // pane, place, tab, preview
	pane           *pane
	row0           int
	idx            int
}

type app struct {
	t          *term
	w, h       int
	cfg        config
	tabs       []*tab
	ti         int
	clip       []string
	cut        bool
	msg        string
	msgErr     bool
	prompt     *prompt
	menu       *menu
	over       *overlay
	pending    string
	count      int
	jobs       []*job
	undos      []undo
	ev         chan func()
	quit       bool
	marks      map[string]string
	recent     []string
	devices    []device
	pick       bool
	picked     []string
	choose     string // --choosefiles file
	chdir      string // --choosedir file
	prev       *grid
	pv         map[string]*preview
	hits       []hit
	places     []place
	lastClk    time.Time
	lastIdx    int
	binds      map[string]func()
	prefixes   map[string]bool
	sixOn      bool
	sixShown   string
	sixBox     [4]int
	sixPending string
	pvScroll   int
	pvFor      string
	counting   map[string]bool
}

func (a *app) tab() *tab   { return a.tabs[a.ti] }
func (a *app) pane() *pane { return a.tab().pane() }
func (a *app) other() *pane {
	t := a.tab()
	if !t.split {
		return nil
	}
	return t.panes[1-t.active]
}

func (a *app) post(f func()) { a.ev <- f }

func (a *app) say(f string, v ...any) { a.msg, a.msgErr = fmt.Sprintf(f, v...), false }
func (a *app) fail(err error) {
	if err != nil {
		a.msg, a.msgErr = err.Error(), true
	}
}

func (a *app) takeCount() int {
	n := a.count
	a.count = 0
	if n < 1 {
		return 1
	}
	return n
}

// ---------------------------------------------------------------- loading

func (a *app) load(p *pane, keep string) {
	switch p.kind {
	case kDir:
		es, err := readDir(p.dir, a.cfg.hidden)
		p.err = ""
		if err != nil {
			p.err = err.Error()
		}
		sortEntries(es, a.cfg)
		p.all = es
		if fi, err := os.Stat(p.dir); err == nil {
			p.stamp = fi.ModTime()
		}
		a.countDirs(p)
	case kTrash:
		p.all = trashList()
		c := a.cfg
		if c.sortBy == "name" {
			c.sortBy = "deleted"
		}
		sortEntries(p.all, c)
		p.err = ""
	case kSearch:
		// results stream in; keep them
	}
	a.applyView(p, keep)
}

func (a *app) applyView(p *pane, keep string) {
	if keep == "" {
		if e := p.entry(); e != nil {
			keep = e.path
		}
	}
	p.view = p.view[:0:0]
	f := p.filter
	smart := strings.ToLower(f) == f
	glob := strings.ContainsAny(f, "*?[")
	for _, e := range p.all {
		if f != "" {
			n := e.name
			if smart {
				n = strings.ToLower(n)
			}
			if glob {
				if ok, _ := filepath.Match(f, n); !ok {
					continue
				}
			} else if !strings.Contains(n, f) {
				continue
			}
		}
		p.view = append(p.view, e)
	}
	p.cur = min(p.cur, max(len(p.view)-1, 0))
	for i, e := range p.view {
		if e.path == keep {
			p.cur = i
			break
		}
	}
	for s := range p.sel {
		found := false
		for _, e := range p.all {
			if e.path == s {
				found = true
				break
			}
		}
		if !found {
			delete(p.sel, s)
		}
	}
}

// countDirs fills in "N items" for directories in the background.
func (a *app) countDirs(p *pane) {
	var dirs []*entry
	for _, e := range p.all {
		if e.isDir() && len(dirs) < 3000 {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) == 0 {
		return
	}
	hidden := a.cfg.hidden
	go func() {
		res := make(map[*entry]int, len(dirs))
		for _, e := range dirs {
			f, err := os.Open(e.path)
			if err != nil {
				continue
			}
			names, _ := f.Readdirnames(-1)
			f.Close()
			n := 0
			for _, s := range names {
				if hidden || !strings.HasPrefix(s, ".") {
					n++
				}
			}
			res[e] = n
		}
		a.post(func() {
			for e, n := range res {
				e.count = n
			}
		})
	}()
}

func (a *app) reloadAll() {
	for _, t := range a.tabs {
		for i, p := range t.panes {
			if p != nil && (i == 0 || t.split) {
				a.load(p, "")
			}
		}
	}
}

func (a *app) cd(p *pane, dir string, push bool) {
	dir = filepath.Clean(dir)
	fi, err := os.Stat(dir)
	if err != nil {
		a.fail(err)
		return
	}
	sel := ""
	if !fi.IsDir() {
		sel, dir = dir, filepath.Dir(dir)
	}
	if f, err := os.Open(dir); err != nil {
		a.fail(err)
		return
	} else {
		f.Close()
	}
	old := p.dir
	if push && (p.kind != kDir || old != dir) {
		p.back = append(p.back, old)
		if len(p.back) > 100 {
			p.back = p.back[1:]
		}
		p.fwd = nil
	}
	p.stopSearch()
	p.kind, p.dir, p.filter, p.visual, p.title = kDir, dir, "", -1, ""
	p.sel = map[string]bool{}
	p.cur, p.top = 0, 0
	if sel == "" && filepath.Dir(old) == dir {
		sel = old
	}
	a.load(p, sel)
	a.addRecent(dir)
	a.pvScroll = 0
}

func (a *app) addRecent(dir string) {
	out := []string{dir}
	for _, d := range a.recent {
		if d != dir && len(out) < 50 {
			out = append(out, d)
		}
	}
	a.recent = out
}

// ---------------------------------------------------------------- state files

func stateFile(name string) string { return filepath.Join(stateHome(), "muhfm", name) }

func (a *app) loadState() {
	a.marks = map[string]string{}
	if b, err := os.ReadFile(stateFile("marks")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(l, " "); ok {
				a.marks[k] = v
			}
		}
	}
	if b, err := os.ReadFile(stateFile("recent")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				a.recent = append(a.recent, l)
			}
		}
	}
}

func (a *app) saveState() {
	os.MkdirAll(filepath.Join(stateHome(), "muhfm"), 0o755)
	var b strings.Builder
	keys := make([]string, 0, len(a.marks))
	for k := range a.marks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %s\n", k, a.marks[k])
	}
	os.WriteFile(stateFile("marks"), []byte(b.String()), 0o644)
	os.WriteFile(stateFile("recent"), []byte(strings.Join(a.recent, "\n")+"\n"), 0o644)
}

func (a *app) buildPlaces() {
	h := home()
	ud := userDirs()
	ps := []place{{label: "Home", path: h, icon: "⌂", section: "Places"}}
	for _, k := range []string{"DESKTOP", "DOCUMENTS", "DOWNLOAD", "MUSIC", "PICTURES", "VIDEOS"} {
		def := map[string]string{"DESKTOP": "Desktop", "DOCUMENTS": "Documents", "DOWNLOAD": "Downloads",
			"MUSIC": "Music", "PICTURES": "Pictures", "VIDEOS": "Videos"}[k]
		p := firstOf(ud[k], filepath.Join(h, def))
		if p != h && isDirPath(p) {
			ps = append(ps, place{label: filepath.Base(p), path: p, icon: "·", section: "Places"})
		}
	}
	ps = append(ps, place{label: "Root", path: "/", icon: "/", section: "Places"},
		place{label: "Trash", path: "trash:", icon: "♻", section: "Places"})
	ps = append(ps, loadBookmarks()...)
	for i := range a.devices {
		d := &a.devices[i]
		icon := "○"
		if d.mount != "" {
			icon = "●"
		}
		ps = append(ps, place{label: d.label, path: d.mount, icon: icon, dev: d, section: "Devices"})
	}
	a.places = ps
}

func (a *app) refreshDevices() {
	go func() {
		d := listDevices()
		a.post(func() { a.devices = d; a.buildPlaces() })
	}()
}

// ---------------------------------------------------------------- selection

// targets: the selection, or the item under the cursor.
func (a *app) targets(p *pane) []*entry {
	var out []*entry
	if len(p.sel) > 0 {
		for _, e := range p.all {
			if p.sel[e.path] {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if e := p.entry(); e != nil {
		return []*entry{e}
	}
	return nil
}

func paths(es []*entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.path
	}
	return out
}

func (a *app) move(p *pane, d int) {
	if len(p.view) == 0 {
		return
	}
	p.cur = min(max(p.cur+d, 0), len(p.view)-1)
	if p.visual >= 0 {
		lo, hi := min(p.visual, p.cur), max(p.visual, p.cur)
		p.sel = map[string]bool{}
		for i := lo; i <= hi; i++ {
			p.sel[p.view[i].path] = true
		}
	}
	a.pvScroll = 0
}

func (a *app) bodyH() int { return max(a.h-2, 1) }

// ---------------------------------------------------------------- drawing

func (a *app) draw() {
	g := newGrid(a.w, a.h)
	a.hits = a.hits[:0]
	t := a.tab()
	bh := a.bodyH()

	// top bar: tabs, then the path of the active pane
	x := 0
	if len(a.tabs) > 1 {
		for i, tb := range a.tabs {
			name := filepath.Base(tb.pane().dir)
			if tb.pane().kind == kTrash {
				name = "Trash"
			}
			st := dim()
			if i == a.ti {
				st = fg(3).with(aBold | aRev)
			}
			x0 := x
			x = g.text(x, 0, a.w, fmt.Sprintf(" %d %s ", i+1, fit(name, 14, false)), st)
			a.hits = append(a.hits, hit{x0: x0, y0: 0, x1: x, y1: 1, kind: "tab", idx: i})
			x = g.text(x, 0, a.w, " ", plain)
		}
	}
	p := a.pane()
	path := tilde(p.dir)
	switch p.kind {
	case kTrash:
		path = "♻ Trash"
	case kSearch:
		path = p.title
	}
	x = g.text(x, 0, a.w, fit(path, a.w-x-20, false), fg(3).with(aBold))
	if p.kind == kDir {
		if br := gitBranch(p.dir); br != "" {
			x = g.text(x, 0, a.w, "  "+br, fg(2))
		}
	}
	if p.filter != "" {
		g.text(x, 0, a.w, "  filter: "+p.filter, fg(6))
	}

	// body
	x = 0
	if a.cfg.places && a.w >= 70 {
		sw := min(max(a.w/6, 16), 24)
		a.drawPlaces(g, 0, 1, sw, bh)
		g.vline(sw, 1, 1+bh, dim())
		x = sw + 1
	}
	rest := a.w - x
	switch {
	case t.split:
		pw := rest / 2
		a.drawPane(g, t.panes[0], x, 1, pw, bh, t.active == 0)
		g.vline(x+pw, 1, 1+bh, dim())
		a.drawPane(g, t.panes[1], x+pw+1, 1, rest-pw-1, bh, t.active == 1)
		a.noSixel()
	case a.cfg.preview && rest >= 50:
		lw := rest / 2
		a.drawPane(g, p, x, 1, lw, bh, true)
		g.vline(x+lw, 1, 1+bh, dim())
		a.drawPreview(g, x+lw+1, 1, rest-lw-1, bh)
	default:
		a.drawPane(g, p, x, 1, rest, bh, true)
		a.noSixel()
	}

	switch {
	case a.over != nil:
		a.drawOverlay(g)
		a.noSixel()
	case a.menu != nil:
		a.drawMenu(g)
		a.noSixel()
	}
	a.drawStatus(g)
	a.flush(g)
}

func (a *app) drawPlaces(g *grid, x0, y0, w, h int) {
	y, sec := y0, ""
	cur := a.pane().dir
	if a.pane().kind == kTrash {
		cur = "trash:"
	}
	for i, pl := range a.places {
		if y >= y0+h {
			break
		}
		if pl.section != sec {
			if sec != "" {
				y++
			}
			sec = pl.section
			if y >= y0+h {
				break
			}
			g.text(x0+1, y, x0+w, pl.section, plain.with(aBold))
			y++
		}
		if y >= y0+h {
			break
		}
		st := plain
		if pl.path != "" && pl.path == cur {
			st = fg(3).with(aBold)
		}
		if pl.dev != nil && pl.dev.mount == "" {
			st = dim()
		}
		g.text(x0+1, y, x0+w, pl.icon+" "+fit(pl.label, w-4, false), st)
		a.hits = append(a.hits, hit{x0: x0, y0: y, x1: x0 + w, y1: y + 1, kind: "place", idx: i})
		y++
	}
}

func (a *app) drawPane(g *grid, p *pane, x0, y0, w, h int, active bool) {
	a.hits = append(a.hits, hit{x0: x0, y0: y0, x1: x0 + w, y1: y0 + h, kind: "pane", pane: p, row0: y0})
	if p.cur < p.top {
		p.top = p.cur
	}
	if p.cur >= p.top+h {
		p.top = p.cur - h + 1
	}
	if p.top > max(len(p.view)-h, 0) {
		p.top = max(len(p.view)-h, 0)
	}
	if len(p.view) == 0 {
		msg, st := "empty", dim().with(aItal)
		switch {
		case p.err != "":
			msg, st = p.err, fg(1)
		case p.loading:
			msg = "searching…"
		case p.filter != "":
			msg = "nothing matches " + p.filter
		}
		g.text(x0+2, y0, x0+w, fit(msg, w-2, false), st)
		return
	}
	ic := icons[a.cfg.icons]
	for row := 0; row < h; row++ {
		i := p.top + row
		if i >= len(p.view) {
			break
		}
		e := p.view[i]
		y := y0 + row
		k := kindOf(e)
		st := kindStyle(k)
		sel := p.sel[e.path]
		cursor := i == p.cur
		// the cursor row gets a background, not reverse video: the name keeps
		// its type colour and the row reads as one bar
		base := plain
		if cursor && active {
			base = plain.on(8)
			st = st.on(8).with(aBold)
		} else if cursor {
			base = plain.on(0)
			st = st.on(0)
		}
		g.fill(x0, y, x0+w, y+1, base)
		mark := " "
		if sel {
			mark = "▌"
		}
		mst := fg(3).with(aBold)
		if cursor && active {
			mst = mst.on(8)
		} else if cursor {
			mst = mst.on(0)
		}
		xx := g.text(x0, y, x0+w, mark, mst)
		if a.cfg.icons != "none" && ic[0] != "" {
			xx = g.text(xx, y, x0+w, ic[k]+" ", st)
		}
		// right-hand columns
		right := ""
		if a.cfg.details && w >= 30 {
			switch {
			case e.trash != nil:
				right = e.trash.when.Format("2006-01-02 15:04")
				if w >= 60 {
					right = fit(e.note, w/3, false) + "  " + right
				}
			case e.isDir():
				if e.count == 1 {
					right = "1 item"
				} else if e.count >= 0 {
					right = fmt.Sprintf("%d items", e.count)
				}
			case k == tBroken:
				right = "broken"
			default:
				right = human(e.size())
			}
			if w >= 52 && e.trash == nil {
				right = fmt.Sprintf("%9s  %s", right, fmt.Sprintf("%-12s", when(e.mtime())))
			}
			if e.note != "" && e.trash == nil && w >= 60 {
				right = fit(e.note, w/3, false) + "  " + right
			}
		}
		name := e.name
		if sel {
			st = st.with(aBold)
		}
		nameW := w - (xx - x0) - strWidth(right) - 2
		if e.isLink() && w >= 40 {
			tgt := " → " + e.link
			nm := fit(name, nameW, true)
			xx = g.text(xx, y, x0+w, nm, st)
			lst := dim()
			if cursor && active {
				lst = lst.on(8)
			} else if cursor {
				lst = lst.on(0)
			}
			g.text(xx, y, x0+w-strWidth(right)-2, fit(tgt, max(nameW-strWidth(nm), 0), false), lst)
		} else {
			g.text(xx, y, x0+w, fit(name, nameW, true), st)
		}
		if right != "" {
			rst := dim()
			if cursor && active {
				rst = rst.on(8)
			} else if cursor {
				rst = rst.on(0)
			}
			g.text(x0+w-strWidth(right)-1, y, x0+w, right, rst)
		}
	}
	if len(p.view) > h { // scrollbar
		th := max(h*h/len(p.view), 1)
		ty := y0 + p.top*(h-th)/max(len(p.view)-h, 1)
		for y := ty; y < ty+th && y < y0+h; y++ {
			g.set(x0+w-1, y, cell{"▐", 1, dim()})
		}
	}
}

func (a *app) noSixel() {
	a.sixPending = ""
	if a.sixShown != "" {
		a.invalidateBox()
		a.sixShown = ""
	}
}

func (a *app) invalidateBox() {
	if a.prev == nil {
		return
	}
	b := a.sixBox
	for y := b[1]; y < b[3] && y < a.prev.h; y++ {
		for x := b[0]; x < b[2] && x < a.prev.w; x++ {
			a.prev.c[y*a.prev.w+x].s = "\x00"
		}
	}
}

func (a *app) previewOf(e *entry) *preview {
	k := fmt.Sprintf("%s|%d|%d|%v", e.path, e.mtime().UnixNano(), e.size(), a.cfg.hidden)
	if pv, ok := a.pv[k]; ok {
		return pv
	}
	if len(a.pv) > 64 {
		a.pv = map[string]*preview{}
	}
	pv := &preview{loading: true}
	a.pv[k] = pv
	hidden := a.cfg.hidden
	go func() {
		res := buildPreview(e, hidden)
		a.post(func() { *pv = *res })
	}()
	return pv
}

func (a *app) drawPreview(g *grid, x0, y0, w, h int) {
	a.hits = append(a.hits, hit{x0: x0, y0: y0, x1: x0 + w, y1: y0 + h, kind: "preview"})
	e := a.pane().entry()
	if e == nil {
		a.noSixel()
		return
	}
	if a.pvFor != e.path {
		a.pvFor, a.pvScroll = e.path, 0
	}
	pv := a.previewOf(e)
	x0++
	w--
	if pv.loading {
		g.text(x0, y0, x0+w, "…", dim())
		a.noSixel()
		return
	}
	g.text(x0, y0, x0+w, fit(pv.head, w, false), dim().with(aItal))
	if pv.img != nil {
		if a.sixOn && a.t.cellW > 0 {
			bw, bh := w, h-2
			key := fmt.Sprintf("%p|%d|%d", pv.img, bw, bh)
			if pv.sixKey != key {
				pv.six, pv.sixKey = sixel(pv.img, bw*a.t.cellW, bh*a.t.cellH), key
			}
			box := [4]int{x0, y0 + 1, x0 + bw, y0 + 1 + bh}
			for y := box[1]; y < box[3]; y++ {
				for x := box[0]; x < box[2]; x++ {
					g.set(x, y, cell{" ", 1, plain.with(aHole)})
				}
			}
			if a.sixShown != key || a.sixBox != box {
				if a.sixShown != "" {
					a.invalidateBox()
				}
				a.sixBox, a.sixShown = box, key
				a.sixPending = pv.six
			}
			return
		}
		a.noSixel()
		g.text(x0, y0+2, x0+w, "(image preview needs a sixel terminal such as foot)", dim())
		return
	}
	a.noSixel()
	lines := pv.lines
	a.pvScroll = min(a.pvScroll, max(len(lines)-1, 0))
	for i := 0; i+1 < h && a.pvScroll+i < len(lines); i++ {
		l := lines[a.pvScroll+i]
		g.text(x0, y0+1+i, x0+w, l.s, l.st)
	}
}

func (a *app) drawStatus(g *grid) {
	y := a.h - 1
	g.fill(0, y, a.w, a.h, plain.on(0))
	if a.prompt != nil {
		pr := a.prompt
		x := g.text(0, y, a.w, pr.label, fg(3).with(aBold).on(0))
		s := string(pr.buf)
		// scroll the input so the cursor stays visible
		start := 0
		for strWidth(string(pr.buf[start:pr.pos])) > a.w-x-2 {
			start++
		}
		x = g.text(x, y, a.w, string(pr.buf[start:pr.pos]), plain.on(0))
		cur := " "
		if pr.pos < len(pr.buf) {
			cur = string(pr.buf[pr.pos])
		}
		x = g.text(x, y, a.w, cur, plain.with(aRev))
		if pr.pos+1 <= len(pr.buf) {
			g.text(x, y, a.w, string(pr.buf[min(pr.pos+1, len(pr.buf)):]), plain.on(0))
		}
		_ = s
		return
	}
	if a.msg != "" {
		st := fg(2).on(0)
		if a.msgErr {
			st = fg(1).with(aBold).on(0)
		}
		g.text(1, y, a.w, fit(a.msg, a.w-2, false), st)
		return
	}
	p := a.pane()
	left := fmt.Sprintf(" %d/%d", min(p.cur+1, len(p.view)), len(p.view))
	if n := len(p.sel); n > 0 {
		var sz int64
		for _, e := range a.targets(p) {
			sz += e.size()
		}
		left += fmt.Sprintf("  %d selected (%s)", n, human(sz))
	}
	if p.visual >= 0 {
		left += "  VISUAL"
	}
	if len(a.clip) > 0 {
		verb := "copy"
		if a.cut {
			verb = "move"
		}
		left += fmt.Sprintf("  ⎘ %d to %s", len(a.clip), verb)
	}
	if a.pending != "" {
		left += "  " + a.pending + "…"
	}
	if a.count > 0 {
		left += fmt.Sprintf("  %d", a.count)
	}
	for _, j := range a.jobs {
		t, d := j.total.Load(), j.done.Load()
		if t > 0 {
			left += fmt.Sprintf("  ⟳ %s %d%%", j.name, min(d*100/t, 100))
		} else {
			left += "  ⟳ " + j.name
		}
	}
	x := g.text(0, y, a.w, left, fg(7).on(0))
	right := ""
	if e := p.entry(); e != nil && e.fi != nil {
		right = modeString(e.fi.Mode()) + "  " + e.mtime().Format("2006-01-02 15:04")
		if !e.isDir() {
			right += "  " + human(e.size())
		}
	}
	if p.kind == kDir {
		if free, _ := freeSpace(p.dir); free > 0 {
			right += "  " + human(int64(free)) + " free"
		}
	}
	right += "  ? help "
	if strWidth(right) < a.w-x {
		g.text(a.w-strWidth(right), y, a.w, right, dim().on(0))
	}
}

func (a *app) drawMenu(g *grid) {
	m := a.menu
	m.shown = m.shown[:0]
	f := strings.ToLower(m.filter)
	for i, it := range m.items {
		if f == "" || strings.Contains(strings.ToLower(it.label+" "+it.hint), f) {
			m.shown = append(m.shown, i)
		}
	}
	m.cur = min(max(m.cur, 0), max(len(m.shown)-1, 0))
	bw := 30
	for _, it := range m.items {
		bw = max(bw, strWidth(it.label)+strWidth(it.hint)+10)
	}
	bw = min(bw, a.w-4)
	vis := min(len(m.shown), a.h-8)
	vis = max(vis, 1)
	bh := vis + 2
	foot := ""
	if m.filterable {
		foot = "type to filter"
	}
	var ks []string
	for k, d := range m.keys {
		ks = append(ks, k+" "+d)
	}
	sort.Strings(ks)
	if len(ks) > 0 {
		foot = strings.TrimPrefix(foot+" · "+strings.Join(ks, " · "), " · ")
	}
	if foot != "" || m.filter != "" {
		bh++
	}
	x0, y0 := (a.w-bw)/2, max((a.h-bh)/2, 1)
	g.box(x0, y0, x0+bw, y0+bh, m.title, fg(3), fg(3).with(aBold))
	if m.cur < m.top {
		m.top = m.cur
	}
	if m.cur >= m.top+vis {
		m.top = m.cur - vis + 1
	}
	for r := 0; r < vis && m.top+r < len(m.shown); r++ {
		i := m.shown[m.top+r]
		it := m.items[i]
		y := y0 + 1 + r
		st := it.st
		if st == (style{}) {
			st = plain
		}
		if m.top+r == m.cur {
			g.fill(x0+1, y, x0+bw-1, y+1, plain.on(8))
			st = st.on(8).with(aBold)
		}
		x := x0 + 2
		if it.hot != "" {
			hs := fg(3).with(aBold)
			if m.top+r == m.cur {
				hs = hs.on(8)
			}
			x = g.text(x, y, x0+bw-2, it.hot+"  ", hs)
		}
		x = g.text(x, y, x0+bw-2, fit(it.label, x0+bw-3-x-strWidth(it.hint), false), st)
		if it.hint != "" {
			hs := dim()
			if m.top+r == m.cur {
				hs = hs.on(8)
			}
			g.text(x0+bw-2-strWidth(it.hint), y, x0+bw-2, it.hint, hs)
		}
	}
	if len(m.shown) == 0 {
		g.text(x0+2, y0+1, x0+bw-2, "no matches", dim())
	}
	if foot != "" || m.filter != "" {
		s := foot
		if m.filter != "" {
			s = "/" + m.filter
		}
		g.text(x0+2, y0+bh-2, x0+bw-2, fit(s, bw-4, false), dim().with(aItal))
	}
}

func (a *app) drawOverlay(g *grid) {
	o := a.over
	// fit the box to its content: short panels stay small, long ones scroll
	bw, bh := min(a.w-4, 100), min(a.h-4, len(o.lines)+3)
	bh = max(bh, 6)
	x0, y0 := (a.w-bw)/2, max((a.h-bh)/2, 1)
	g.box(x0, y0, x0+bw, y0+bh, o.title, fg(3), fg(3).with(aBold))
	inner := bh - 3
	o.scroll = min(max(o.scroll, 0), max(len(o.lines)-inner, 0))
	for i := 0; i < inner && o.scroll+i < len(o.lines); i++ {
		l := o.lines[o.scroll+i]
		g.text(x0+2, y0+1+i, x0+bw-2, l.s, l.st)
	}
	ft := firstOf(o.footer, "j/k scroll · q close")
	g.text(x0+2, y0+bh-2, x0+bw-2, ft, dim().with(aItal))
}

func (a *app) flush(g *grid) {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	full := a.prev == nil || a.prev.w != g.w || a.prev.h != g.h
	if full {
		b.WriteString("\x1b[0m\x1b[2J")
	}
	cur := style{fg: -2}
	cx, cy := -1, -1
	for y := 0; y < g.h; y++ {
		for x := 0; x < g.w; x++ {
			c := g.c[y*g.w+x]
			if c.w == 0 {
				continue
			}
			if !full {
				pc := a.prev.c[y*g.w+x]
				if pc == c && (c.w == 1 || x+1 >= g.w || a.prev.c[y*g.w+x+1] == g.c[y*g.w+x+1]) {
					continue
				}
			}
			if x == g.w-1 && y == g.h-1 && c.w == 1 {
				// writing the last cell can scroll some terminals; it is the
				// status line's padding anyway
			}
			if cx != x || cy != y {
				fmt.Fprintf(&b, "\x1b[%d;%dH", y+1, x+1)
			}
			if c.st != cur {
				b.WriteString(sgr(c.st))
				cur = c.st
			}
			b.WriteString(c.s)
			cx, cy = x+int(c.w), y
		}
	}
	b.WriteString("\x1b[0m")
	if a.sixPending != "" {
		fmt.Fprintf(&b, "\x1b[%d;%dH", a.sixBox[1]+1, a.sixBox[0]+1)
		b.WriteString(a.sixPending)
		a.sixPending = ""
	}
	b.WriteString("\x1b[?2026l")
	a.t.f.WriteString(b.String())
	a.prev = g
}

// ---------------------------------------------------------------- running things

// runFg hands the terminal to a child (editor, shell, TUI app) and takes it back.
func (a *app) runFg(dir string, wait bool, name string, args ...string) error {
	a.t.paused.Store(true)
	time.Sleep(50 * time.Millisecond) // let the reader's poll expire
	a.t.restore()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.t.f, a.t.f, a.t.f
	cmd.Env = append(os.Environ(), "MUHFM_LEVEL=1")
	err := cmd.Run()
	if wait {
		a.t.f.WriteString("\n\x1b[2m── press Enter to return to muhfm ──\x1b[0m")
		b := make([]byte, 1)
		for {
			n, rerr := a.t.f.Read(b)
			if n == 0 || rerr != nil || b[0] == '\n' || b[0] == '\r' {
				break
			}
		}
	}
	a.t.raw()
	a.t.paused.Store(false)
	a.prev = nil
	a.sixShown = ""
	a.reloadAll()
	return err
}

func (a *app) editor(dir string, files ...string) {
	a.fail(a.runFg(dir, false, "sh", append([]string{"-c", a.cfg.editor + ` "$@"`, "sh"}, files...)...))
}

func (a *app) launch(ap *desktopApp, files []string) {
	dir := a.pane().dir
	for _, c := range execArgs(ap.exec, files) {
		if ap.terminal {
			a.fail(a.runFg(dir, false, c[0], c[1:]...))
		} else if err := spawn(dir, c...); err != nil {
			a.fail(err)
			return
		}
	}
	if !ap.terminal {
		a.say("opened with %s", ap.name)
	}
}

func (a *app) openFiles(files []string) {
	groups, order := map[string][]string{}, []string{}
	apps := map[string]*desktopApp{}
	for _, f := range files {
		m := mimeOf(f)
		cand := appsFor(m)
		id := ""
		if len(cand) > 0 {
			id = cand[0].id
			apps[id] = cand[0]
		} else if strings.HasPrefix(m, "text/") || m == "application/x-zerosize" || m == "application/x-shellscript" || m == "application/json" {
			id = "\x00editor"
		} else if have("xdg-open") {
			id = "\x00xdg:" + f
		} else {
			hint := "press o to choose one"
			if archiveKind(f) != "" {
				hint = "E extracts it, o picks an app"
			}
			a.fail(fmt.Errorf("no application for %s — %s", m, hint))
			continue
		}
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], f)
	}
	for _, id := range order {
		switch {
		case id == "\x00editor":
			a.editor(a.pane().dir, groups[id]...)
		case strings.HasPrefix(id, "\x00xdg:"):
			a.fail(spawn(a.pane().dir, "xdg-open", groups[id][0]))
		default:
			a.launch(apps[id], groups[id])
		}
	}
}

// ---------------------------------------------------------------- clipboard

func wayland() bool { return os.Getenv("WAYLAND_DISPLAY") != "" && have("wl-copy") }

func uriList(ps []string) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("file://" + (&url.URL{Path: p}).EscapedPath() + "\r\n")
	}
	return b.String()
}

func (a *app) sysCopyFiles(ps []string) {
	if !wayland() {
		return
	}
	cmd := exec.Command("wl-copy", "-t", "text/uri-list")
	cmd.Stdin = strings.NewReader(uriList(ps))
	cmd.Run()
}

func (a *app) sysCopyText(s string) {
	if wayland() {
		cmd := exec.Command("wl-copy")
		cmd.Stdin = strings.NewReader(s)
		if cmd.Run() == nil {
			return
		}
	}
	a.t.f.WriteString("\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\a")
}

func sysPasteFiles() []string {
	if !wayland() || !have("wl-paste") {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	types, _ := exec.CommandContext(ctx, "wl-paste", "-l").Output()
	if !strings.Contains(string(types), "text/uri-list") {
		return nil
	}
	b, err := exec.CommandContext(ctx, "wl-paste", "-n", "-t", "text/uri-list").Output()
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "file://") {
			if p := expand(l, "/"); exists(p) {
				out = append(out, p)
			}
		}
	}
	return out
}

func (a *app) yank(cut bool) {
	ts := a.targets(a.pane())
	if len(ts) == 0 {
		return
	}
	a.clip, a.cut = paths(ts), cut
	a.sysCopyFiles(a.clip)
	verb := "copy"
	if cut {
		verb = "move"
	}
	a.say("%d item(s) ready to %s — p to paste", len(ts), verb)
	a.pane().sel = map[string]bool{}
	a.pane().visual = -1
}

// ---------------------------------------------------------------- jobs and undo

func (a *app) newJob(name string) *job {
	ctx, cancel := context.WithCancel(context.Background())
	j := &job{name: name, ctx: ctx, cancel: cancel}
	a.jobs = append(a.jobs, j)
	return j
}

func (a *app) endJob(j *job) {
	j.cancel()
	for i, x := range a.jobs {
		if x == j {
			a.jobs = append(a.jobs[:i], a.jobs[i+1:]...)
			break
		}
	}
}

func (a *app) pushUndo(desc string, fn func() error) {
	a.undos = append(a.undos, undo{desc, fn})
	if len(a.undos) > 50 {
		a.undos = a.undos[1:]
	}
}

func (a *app) undo() {
	if len(a.undos) == 0 {
		a.say("nothing to undo")
		return
	}
	u := a.undos[len(a.undos)-1]
	a.undos = a.undos[:len(a.undos)-1]
	if err := u.fn(); err != nil {
		a.fail(fmt.Errorf("undo %s: %v", u.desc, err))
	} else {
		a.say("undone: %s", u.desc)
	}
	a.reloadAll()
}

func moveAny(j *job, src, dst string) error {
	err := os.Rename(src, dst)
	if errors.Is(err, syscall.EXDEV) {
		if err = copyTree(j, src, dst); err == nil {
			err = os.RemoveAll(src)
		}
	}
	return err
}

func (a *app) transferAsk(op string, srcs []string, dst string, after func()) {
	conflicts := 0
	for _, s := range srcs {
		t := filepath.Join(dst, filepath.Base(s))
		if exists(t) && !(op == "move" && t == s) {
			conflicts++
		}
	}
	if conflicts == 0 {
		a.transfer(op, srcs, dst, "rename", after)
		return
	}
	a.menu = &menu{title: fmt.Sprintf("%d item(s) already in %s", conflicts, tilde(dst)),
		items: []mitem{
			{hot: "k", label: "keep both — new copies get (1), (2)…"},
			{hot: "o", label: "overwrite — the old ones go to the trash"},
			{hot: "s", label: "skip those"},
			{hot: "c", label: "cancel"},
		},
		pick: func(i int, _ string) {
			if i < 3 {
				a.transfer(op, srcs, dst, []string{"rename", "overwrite", "skip"}[i], after)
			}
		}}
}

func (a *app) transfer(op string, srcs []string, dst, policy string, after func()) {
	j := a.newJob(map[string]string{"copy": "copying", "move": "moving", "link": "linking"}[op])
	go func() {
		if op == "copy" {
			var t int64
			for _, s := range srcs {
				n, _ := duSize(s)
				t += n
			}
			j.total.Store(t)
		}
		type pair struct{ src, dst string }
		var done []pair
		var trashed []*trashItem
		var errs []string
		for _, s := range srcs {
			if j.ctx.Err() != nil {
				errs = append(errs, "cancelled")
				break
			}
			t := filepath.Join(dst, filepath.Base(s))
			if op == "move" && t == s {
				continue
			}
			if op != "link" && isDirPath(s) && isInside(dst, s) {
				errs = append(errs, "a folder cannot go inside itself: "+filepath.Base(s))
				continue
			}
			if exists(t) {
				switch policy {
				case "skip":
					continue
				case "overwrite":
					it, err := trashPut(t)
					if err != nil {
						errs = append(errs, err.Error())
						continue
					}
					trashed = append(trashed, it)
				default:
					t = uniqueName(t)
				}
			}
			var err error
			switch op {
			case "copy":
				err = copyTree(j, s, t)
			case "link":
				err = os.Symlink(s, t)
			case "move":
				err = moveAny(j, s, t)
			}
			if err != nil {
				errs = append(errs, filepath.Base(s)+": "+err.Error())
				continue
			}
			done = append(done, pair{s, t})
		}
		a.post(func() {
			a.endJob(j)
			if len(done) > 0 {
				a.pushUndo(fmt.Sprintf("%s %d item(s)", op, len(done)), func() error {
					uj := &job{ctx: context.Background()}
					for i := len(done) - 1; i >= 0; i-- {
						d := done[i]
						if op == "move" {
							if err := moveAny(uj, d.dst, d.src); err != nil {
								return err
							}
						} else if _, err := trashPut(d.dst); err != nil {
							return err
						}
					}
					for _, it := range trashed {
						trashRestore(it)
					}
					return nil
				})
			}
			if len(errs) > 0 {
				a.fail(errors.New(strings.Join(errs, "; ")))
			} else {
				a.say("%s: %d item(s) → %s", op, len(done), tilde(dst))
			}
			if after != nil {
				after()
			}
			a.reloadAll()
		})
	}()
}

func (a *app) paste(link bool) {
	p := a.pane()
	if p.kind != kDir {
		a.fail(errors.New("open a folder to paste into"))
		return
	}
	srcs, cut := a.clip, a.cut
	if len(srcs) == 0 {
		srcs, cut = sysPasteFiles(), false
	}
	if len(srcs) == 0 {
		a.say("nothing to paste — y copies, x cuts")
		return
	}
	op := "copy"
	switch {
	case link:
		op = "link"
	case cut:
		op = "move"
	}
	a.transferAsk(op, srcs, p.dir, func() {
		if op == "move" {
			a.clip, a.cut = nil, false
		}
	})
}

func (a *app) toOther(op string) {
	o := a.other()
	if o == nil || o.kind != kDir {
		a.fail(errors.New("open a split view (|) with a folder on the other side"))
		return
	}
	ts := a.targets(a.pane())
	if len(ts) == 0 {
		return
	}
	a.transferAsk(op, paths(ts), o.dir, nil)
	a.pane().sel = map[string]bool{}
}

func (a *app) trash() {
	p := a.pane()
	ts := a.targets(p)
	if len(ts) == 0 {
		return
	}
	if p.kind == kTrash {
		a.deletePerm()
		return
	}
	var items []*trashItem
	var errs []string
	for _, e := range ts {
		it, err := trashPut(e.path)
		if err != nil {
			errs = append(errs, e.name+": "+err.Error())
			continue
		}
		items = append(items, it)
	}
	if len(items) > 0 {
		a.pushUndo(fmt.Sprintf("trash %d item(s)", len(items)), func() error {
			for _, it := range items {
				if _, err := trashRestore(it); err != nil {
					return err
				}
			}
			return nil
		})
	}
	p.sel, p.visual = map[string]bool{}, -1
	if len(errs) > 0 {
		a.fail(errors.New(strings.Join(errs, "; ") + " — D deletes permanently"))
	} else {
		a.say("%d item(s) moved to the trash — u to undo", len(items))
	}
	if p.kind == kSearch {
		var keep []*entry
		gone := map[string]bool{}
		for _, it := range items {
			gone[it.orig] = true
		}
		for _, e := range p.all {
			if !gone[e.path] {
				keep = append(keep, e)
			}
		}
		p.all = keep
	}
	a.reloadAll()
}

func (a *app) deletePerm() {
	p := a.pane()
	ts := a.targets(p)
	if len(ts) == 0 {
		return
	}
	what := ts[0].name
	if len(ts) > 1 {
		what = fmt.Sprintf("%d items", len(ts))
	}
	a.confirm("Delete "+what+" permanently? This cannot be undone.", func() {
		j := a.newJob("deleting")
		go func() {
			var errs []string
			for _, e := range ts {
				if err := os.RemoveAll(e.path); err != nil {
					errs = append(errs, err.Error())
				}
				if e.trash != nil {
					os.Remove(e.trash.info)
				}
			}
			a.post(func() {
				a.endJob(j)
				if len(errs) > 0 {
					a.fail(errors.New(strings.Join(errs, "; ")))
				} else {
					a.say("deleted %s", what)
				}
				p.sel = map[string]bool{}
				if p.kind == kSearch {
					var keep []*entry
					for _, e := range p.all {
						if exists(e.path) {
							keep = append(keep, e)
						}
					}
					p.all = keep
				}
				a.reloadAll()
			})
		}()
	})
}

func (a *app) restore() {
	p := a.pane()
	var restored []string
	var errs []string
	for _, e := range a.targets(p) {
		if e.trash == nil {
			continue
		}
		dst, err := trashRestore(e.trash)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		restored = append(restored, dst)
	}
	if len(restored) > 0 {
		a.pushUndo("restore", func() error {
			for _, r := range restored {
				if _, err := trashPut(r); err != nil {
					return err
				}
			}
			return nil
		})
		a.say("restored %d item(s) — %s", len(restored), tilde(filepath.Dir(restored[0])))
	}
	if len(errs) > 0 {
		a.fail(errors.New(strings.Join(errs, "; ")))
	}
	p.sel = map[string]bool{}
	a.reloadAll()
}

func (a *app) emptyTrash() {
	a.confirm("Empty the trash? This cannot be undone.", func() {
		for _, td := range trashDirs() {
			for _, sub := range []string{"files", "info"} {
				ns, _ := filepath.Glob(filepath.Join(td, sub, "*"))
				hs, _ := filepath.Glob(filepath.Join(td, sub, ".*"))
				for _, n := range append(ns, hs...) {
					os.RemoveAll(n)
				}
			}
			os.Remove(filepath.Join(td, "directorysizes"))
		}
		a.say("trash emptied")
		a.reloadAll()
	})
}

func (a *app) openTrash() {
	p := a.pane()
	p.stopSearch()
	if p.kind == kDir {
		p.back = append(p.back, p.dir)
	}
	p.kind, p.filter, p.sel, p.cur, p.top = kTrash, "", map[string]bool{}, 0, 0
	a.load(p, "")
}

// ---------------------------------------------------------------- renaming and creating

func (a *app) rename() {
	p := a.pane()
	e := p.entry()
	if e == nil {
		return
	}
	if p.kind == kTrash {
		a.restore()
		return
	}
	if len(p.sel) > 1 {
		a.bulkRename()
		return
	}
	base := filepath.Base(e.path)
	stem := base
	if !e.isDir() {
		stem, _ = splitExt(base)
	}
	a.ask("rename: ", base, utf8.RuneCountInString(stem), func(s string) {
		if s == "" || s == base {
			return
		}
		dst := expand(s, filepath.Dir(e.path))
		if fi, err := os.Lstat(dst); err == nil && !os.SameFile(fi, e.fi) {
			a.fail(fmt.Errorf("%s already exists", s))
			return
		}
		os.MkdirAll(filepath.Dir(dst), 0o755)
		if err := os.Rename(e.path, dst); err != nil {
			a.fail(err)
			return
		}
		src := e.path
		a.pushUndo("rename "+filepath.Base(dst), func() error { return os.Rename(dst, src) })
		if p.kind == kSearch {
			e.path = dst
			e.name = filepath.Join(filepath.Dir(e.name), filepath.Base(dst))
		}
		a.load(p, dst)
	})
}

func (a *app) bulkRename() {
	p := a.pane()
	ts := a.targets(p)
	if len(ts) == 0 {
		return
	}
	f, err := os.CreateTemp("", "muhfm-rename-*.txt")
	if err != nil {
		a.fail(err)
		return
	}
	defer os.Remove(f.Name())
	for _, e := range ts {
		fmt.Fprintln(f, filepath.Base(e.path))
	}
	f.Close()
	a.editor(p.dir, f.Name())
	b, err := os.ReadFile(f.Name())
	if err != nil {
		a.fail(err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != len(ts) {
		a.fail(fmt.Errorf("bulk rename: %d names for %d files — nothing changed", len(lines), len(ts)))
		return
	}
	type pair struct{ src, dst, tmp string }
	var ps []pair
	seen := map[string]bool{}
	moving := map[string]bool{}
	for _, e := range ts {
		moving[e.path] = true
	}
	for i, e := range ts {
		n := strings.TrimSpace(lines[i])
		if n == "" || strings.Contains(n, "/") {
			a.fail(fmt.Errorf("bulk rename: bad name on line %d — nothing changed", i+1))
			return
		}
		dst := filepath.Join(filepath.Dir(e.path), n)
		if seen[dst] {
			a.fail(fmt.Errorf("bulk rename: %s twice — nothing changed", n))
			return
		}
		seen[dst] = true
		if dst == e.path {
			continue
		}
		if exists(dst) && !moving[dst] {
			a.fail(fmt.Errorf("bulk rename: %s already exists — nothing changed", n))
			return
		}
		ps = append(ps, pair{e.path, dst, fmt.Sprintf("%s.muhfm-%d-%d", e.path, os.Getpid(), i)})
	}
	for _, x := range ps {
		if err := os.Rename(x.src, x.tmp); err != nil {
			a.fail(err)
			return
		}
	}
	for _, x := range ps {
		if err := os.Rename(x.tmp, x.dst); err != nil {
			a.fail(err)
		}
	}
	a.pushUndo(fmt.Sprintf("rename %d item(s)", len(ps)), func() error {
		for _, x := range ps {
			if err := os.Rename(x.dst, x.tmp); err != nil {
				return err
			}
		}
		for _, x := range ps {
			if err := os.Rename(x.tmp, x.src); err != nil {
				return err
			}
		}
		return nil
	})
	a.say("renamed %d item(s)", len(ps))
	p.sel = map[string]bool{}
	a.load(p, "")
}

func (a *app) create(s string, dir bool) {
	p := a.pane()
	if s == "" {
		return
	}
	dir = dir || strings.HasSuffix(s, "/")
	t := expand(s, p.dir)
	if exists(t) {
		a.fail(fmt.Errorf("%s already exists", s))
		return
	}
	var err error
	if dir {
		err = os.MkdirAll(t, 0o755)
	} else if err = os.MkdirAll(filepath.Dir(t), 0o755); err == nil {
		var f *os.File
		if f, err = os.OpenFile(t, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
			f.Close()
		}
	}
	if err != nil {
		a.fail(err)
		return
	}
	a.pushUndo("create "+filepath.Base(t), func() error {
		if os.Remove(t) == nil {
			return nil
		}
		_, err := trashPut(t)
		return err
	})
	// select the first new path component inside this folder
	sel := t
	if rel, err := filepath.Rel(p.dir, t); err == nil && !strings.HasPrefix(rel, "..") {
		sel = filepath.Join(p.dir, strings.Split(rel, "/")[0])
	}
	if p.kind == kDir {
		a.load(p, sel)
	}
}

func applyChmod(m fs.FileMode, spec string) (fs.FileMode, error) {
	if n, err := strconv.ParseUint(spec, 8, 32); err == nil {
		return m&^0o7777 | fs.FileMode(n)&0o777 | m&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky), nil
	}
	perm := m.Perm()
	for _, part := range strings.Split(spec, ",") {
		i := strings.IndexAny(part, "+-=")
		if i < 0 {
			return m, fmt.Errorf("bad mode %q (try 755 or u+x)", spec)
		}
		who, op, what := part[:i], part[i], part[i+1:]
		if who == "" || who == "a" {
			who = "ugo"
		}
		var bits fs.FileMode
		for _, c := range what {
			var b fs.FileMode
			switch c {
			case 'r':
				b = 4
			case 'w':
				b = 2
			case 'x':
				b = 1
			default:
				return m, fmt.Errorf("bad mode %q", spec)
			}
			for _, w := range who {
				switch w {
				case 'u':
					bits |= b << 6
				case 'g':
					bits |= b << 3
				case 'o':
					bits |= b
				}
			}
		}
		switch op {
		case '+':
			perm |= bits
		case '-':
			perm &^= bits
		case '=':
			var mask fs.FileMode
			for _, w := range who {
				mask |= map[rune]fs.FileMode{'u': 0o700, 'g': 0o070, 'o': 0o007}[w]
			}
			perm = perm&^mask | bits
		}
	}
	return m&^0o777 | perm, nil
}

func (a *app) chmod(spec string) {
	p := a.pane()
	n := 0
	for _, e := range a.targets(p) {
		if e.fi == nil {
			continue
		}
		old := e.info().Mode()
		var nm fs.FileMode
		var err error
		if spec == "toggle-x" {
			nm = old | (old&0o444)>>2
			if old&0o100 != 0 {
				nm = old &^ 0o111
			}
		} else if nm, err = applyChmod(old, spec); err != nil {
			a.fail(err)
			return
		}
		if err := os.Chmod(e.path, nm.Perm()|nm&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky)); err != nil {
			a.fail(err)
			return
		}
		path, prev := e.path, old
		a.pushUndo("chmod "+filepath.Base(path), func() error { return os.Chmod(path, prev.Perm()) })
		n++
	}
	if n > 0 {
		a.say("permissions changed on %d item(s)", n)
	}
	a.load(p, "")
}

// ---------------------------------------------------------------- archives (actions)

func (a *app) extractSel() {
	p := a.pane()
	var arcs []string
	for _, e := range a.targets(p) {
		if !e.isDir() && archiveKind(e.path) != "" {
			arcs = append(arcs, e.path)
		}
	}
	if len(arcs) == 0 {
		a.fail(errors.New("no archive selected"))
		return
	}
	j := a.newJob("extracting")
	go func() {
		var made, errs []string
		for _, arc := range arcs {
			parent := filepath.Dir(arc)
			stem, _ := splitExt(filepath.Base(arc))
			dst := uniqueName(filepath.Join(parent, stem))
			if err := extract(j, arc, dst); err != nil {
				errs = append(errs, filepath.Base(arc)+": "+err.Error())
				os.RemoveAll(dst)
				continue
			}
			// one top-level folder inside? don't wrap it in another one
			if f, err := os.Open(dst); err == nil {
				names, _ := f.Readdirnames(-1)
				f.Close()
				if len(names) == 1 && isDirPath(filepath.Join(dst, names[0])) && !exists(filepath.Join(parent, names[0])) {
					if os.Rename(filepath.Join(dst, names[0]), filepath.Join(parent, names[0])) == nil {
						os.Remove(dst)
						dst = filepath.Join(parent, names[0])
					}
				}
			}
			made = append(made, dst)
		}
		a.post(func() {
			a.endJob(j)
			if len(made) > 0 {
				a.pushUndo("extract", func() error {
					for _, m := range made {
						if _, err := trashPut(m); err != nil {
							return err
						}
					}
					return nil
				})
				a.say("extracted → %s", tilde(made[0]))
			}
			if len(errs) > 0 {
				a.fail(errors.New(strings.Join(errs, "; ")))
			}
			a.reloadAll()
		})
	}()
}

func commonDir(ps []string) string {
	if len(ps) == 0 {
		return "/"
	}
	d := filepath.Dir(ps[0])
	for _, p := range ps[1:] {
		for !isInside(p, d) {
			d = filepath.Dir(d)
		}
	}
	return d
}

func (a *app) compressSel() {
	p := a.pane()
	ts := paths(a.targets(p))
	if len(ts) == 0 {
		return
	}
	name := filepath.Base(p.dir)
	if len(ts) == 1 {
		name, _ = splitExt(filepath.Base(ts[0]))
	}
	def := name + ".zip"
	a.ask("archive (.zip .tar.gz .tar, others via bsdtar): ", def, utf8.RuneCountInString(name), func(s string) {
		if s == "" {
			return
		}
		base := commonDir(ts)
		out := expand(s, base)
		if exists(out) {
			a.fail(fmt.Errorf("%s already exists", s))
			return
		}
		j := a.newJob("compressing")
		var tot int64
		for _, t := range ts {
			n, _ := duSize(t)
			tot += n
		}
		j.total.Store(tot)
		go func() {
			err := compress(j, out, base, ts)
			a.post(func() {
				a.endJob(j)
				if err != nil {
					a.fail(err)
				} else {
					a.say("created %s", tilde(out))
					a.pushUndo("compress", func() error { _, err := trashPut(out); return err })
				}
				p.sel = map[string]bool{}
				a.reloadAll()
			})
		}()
	})
}

// ---------------------------------------------------------------- dialogs

func (a *app) ask(label, init string, pos int, done func(string)) *prompt {
	pr := &prompt{label: label, buf: []rune(init), done: done}
	pr.pos = len(pr.buf)
	if pos >= 0 && pos <= len(pr.buf) {
		pr.pos = pos
	}
	a.prompt = pr
	return pr
}

func (a *app) confirm(q string, yes func()) {
	a.menu = &menu{title: q, items: []mitem{{hot: "y", label: "Yes"}, {hot: "n", label: "No"}},
		cur: 1, pick: func(i int, _ string) {
			if i == 0 {
				yes()
			}
		}}
}

func (a *app) promptKey(k key) {
	pr := a.prompt
	changed := true
	switch k.k {
	case "esc", "ctrl+c", "ctrl+g":
		a.prompt = nil
		if pr.cancel != nil {
			pr.cancel()
		}
		return
	case "enter":
		a.prompt = nil
		pr.done(string(pr.buf))
		return
	case "backspace", "ctrl+h":
		if pr.pos > 0 {
			pr.buf = append(pr.buf[:pr.pos-1], pr.buf[pr.pos:]...)
			pr.pos--
		} else if len(pr.buf) == 0 {
			a.prompt = nil
			if pr.cancel != nil {
				pr.cancel()
			}
			return
		}
	case "delete", "ctrl+d":
		if pr.pos < len(pr.buf) {
			pr.buf = append(pr.buf[:pr.pos], pr.buf[pr.pos+1:]...)
		}
	case "left", "ctrl+b":
		pr.pos, changed = max(pr.pos-1, 0), false
	case "right", "ctrl+f":
		pr.pos, changed = min(pr.pos+1, len(pr.buf)), false
	case "home", "ctrl+a":
		pr.pos, changed = 0, false
	case "end", "ctrl+e":
		pr.pos, changed = len(pr.buf), false
	case "ctrl+u":
		pr.buf, pr.pos = pr.buf[pr.pos:], 0
	case "ctrl+k":
		pr.buf = pr.buf[:pr.pos]
	case "ctrl+w", "alt+backspace":
		i := pr.pos
		for i > 0 && pr.buf[i-1] == ' ' {
			i--
		}
		for i > 0 && pr.buf[i-1] != ' ' && pr.buf[i-1] != '/' {
			i--
		}
		if i == pr.pos && i > 0 {
			i--
		}
		pr.buf = append(pr.buf[:i], pr.buf[pr.pos:]...)
		pr.pos = i
	case "tab":
		a.complete(pr)
	case "paste":
		t := []rune(strings.ReplaceAll(strings.ReplaceAll(k.text, "\r", ""), "\n", " "))
		pr.buf = append(pr.buf[:pr.pos], append(t, pr.buf[pr.pos:]...)...)
		pr.pos += len(t)
	default:
		if k.r >= 0x20 && (utf8.RuneCountInString(k.k) == 1 || k.k == "space") {
			pr.buf = append(pr.buf[:pr.pos], append([]rune{k.r}, pr.buf[pr.pos:]...)...)
			pr.pos++
		} else {
			changed = false
		}
	}
	if changed && pr.change != nil {
		pr.change(string(pr.buf))
	}
}

// complete does shell-style path completion on the word before the cursor.
func (a *app) complete(pr *prompt) {
	s := string(pr.buf[:pr.pos])
	start := 0
	if pr.label == ":" || pr.label == "!" {
		if i := strings.LastIndex(s, " "); i >= 0 {
			start = i + 1
		}
	}
	tok := s[start:]
	dirPart, prefix := "", tok
	if i := strings.LastIndex(tok, "/"); i >= 0 {
		dirPart, prefix = tok[:i+1], tok[i+1:]
	}
	dir := a.pane().dir
	if dirPart != "" {
		dir = expand(dirPart, a.pane().dir)
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	names, _ := f.Readdirnames(-1)
	f.Close()
	var ms []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) && (strings.HasPrefix(prefix, ".") || !strings.HasPrefix(n, ".")) {
			ms = append(ms, n)
		}
	}
	if len(ms) == 0 {
		return
	}
	sort.Strings(ms)
	common := ms[0]
	for _, m := range ms[1:] {
		for !strings.HasPrefix(m, common) {
			common = common[:len(common)-1]
		}
	}
	add := common
	if len(ms) == 1 && isDirPath(filepath.Join(dir, common)) {
		add += "/"
	}
	newTok := dirPart + add
	rest := pr.buf[pr.pos:]
	nb := []rune(s[:start] + newTok)
	pr.pos = len(nb)
	pr.buf = append(nb, rest...)
	if len(ms) > 1 && common == prefix {
		show := ms
		if len(show) > 8 {
			show = append(show[:8:8], "…")
		}
		a.say("%s", strings.Join(show, "  "))
	}
}

func (a *app) menuKey(k key) {
	m := a.menu
	switch k.k {
	case "esc", "ctrl+c":
		a.menu = nil
		return
	case "down", "ctrl+n", "tab", "ctrl+j":
		m.cur++
		return
	case "up", "ctrl+p", "shift+tab", "ctrl+k":
		m.cur--
		return
	case "pgdn":
		m.cur += 10
		return
	case "pgup":
		m.cur -= 10
		return
	case "enter":
		a.menuPick(m, "enter")
		return
	case "backspace":
		if m.filter != "" {
			m.filter = m.filter[:len(m.filter)-1]
			m.cur = 0
		}
		return
	case "mouse":
		return
	}
	if !m.filterable {
		switch k.k {
		case "j":
			m.cur++
			return
		case "k":
			m.cur--
			return
		case "q", "h":
			a.menu = nil
			return
		case "l":
			a.menuPick(m, "enter")
			return
		}
	}
	for i, it := range m.items {
		if it.hot != "" && it.hot == k.k && !m.filterable {
			a.menu = nil
			m.pick(i, "enter")
			return
		}
	}
	if _, ok := m.keys[k.k]; ok {
		a.menuPick(m, k.k)
		return
	}
	if m.filterable && k.r >= 0x20 && (utf8.RuneCountInString(k.k) == 1 || k.k == "space") {
		m.filter += string(k.r)
		m.cur = 0
	}
}

func (a *app) menuPick(m *menu, k string) {
	if len(m.shown) == 0 || m.cur >= len(m.shown) {
		return
	}
	i := m.shown[m.cur]
	a.menu = nil
	m.pick(i, k)
}

func (a *app) overKey(k key) {
	o := a.over
	page := a.h - 8
	switch k.k {
	case "esc", "q", "?", "i", "f1", "enter":
		a.over = nil
	case "j", "down":
		o.scroll++
	case "k", "up":
		o.scroll--
	case "ctrl+d", "pgdn", "space":
		o.scroll += page
	case "ctrl+u", "pgup":
		o.scroll -= page
	case "g", "home":
		o.scroll = 0
	case "G", "end":
		o.scroll = len(o.lines)
	default:
		if fn := o.keys[k.k]; fn != nil {
			fn()
		}
	}
}

// ---------------------------------------------------------------- menus

func (a *app) jumpMenu() {
	type dest struct {
		path string
		pl   *place
	}
	var items []mitem
	var ds []dest
	for i := range a.places {
		pl := &a.places[i]
		hint := tilde(pl.path)
		if pl.dev != nil {
			hint = pl.dev.size + " " + pl.dev.fstype
			if pl.dev.mount == "" {
				hint += " · not mounted"
			}
		}
		items = append(items, mitem{label: pl.icon + " " + pl.label, hint: hint})
		ds = append(ds, dest{pl.path, pl})
	}
	var mk []string
	for k := range a.marks {
		mk = append(mk, k)
	}
	sort.Strings(mk)
	for _, k := range mk {
		items = append(items, mitem{label: "` " + k + "  " + filepath.Base(a.marks[k]), hint: tilde(a.marks[k])})
		ds = append(ds, dest{path: a.marks[k]})
	}
	for i, r := range a.recent {
		if i >= 15 {
			break
		}
		items = append(items, mitem{label: "◷ " + filepath.Base(r), hint: tilde(r), st: dim()})
		ds = append(ds, dest{path: r})
	}
	a.menu = &menu{title: "Go to", items: items, filterable: true, pick: func(i int, _ string) {
		if ds[i].pl != nil {
			a.goPlace(ds[i].pl)
		} else {
			a.cd(a.pane(), ds[i].path, true)
		}
	}}
}

func (a *app) recentMenu() {
	var items []mitem
	for _, r := range a.recent {
		items = append(items, mitem{label: filepath.Base(r), hint: tilde(r)})
	}
	if len(items) == 0 {
		a.say("no recent folders yet")
		return
	}
	a.menu = &menu{title: "Recent folders", items: items, filterable: true, pick: func(i int, _ string) {
		a.cd(a.pane(), a.recent[i], true)
	}}
}

func (a *app) goPlace(pl *place) {
	switch {
	case pl.path == "trash:":
		a.openTrash()
	case pl.dev != nil && pl.dev.mount == "":
		a.mount(pl.dev, true)
	default:
		a.cd(a.pane(), pl.path, true)
	}
}

func (a *app) mount(d *device, enter bool) {
	dev := d.dev
	a.say("mounting %s…", d.label)
	go func() {
		out, err := udisks("mount", "-b", dev, "--no-user-interaction")
		if err != nil && strings.Contains(err.Error(), "authoriz") {
			out, err = udisks("mount", "-b", dev)
		}
		a.post(func() {
			if err != nil {
				a.fail(err)
				return
			}
			mp := ""
			if i := strings.Index(out, " at "); i >= 0 {
				mp = strings.TrimSuffix(strings.TrimSpace(out[i+4:]), ".")
			}
			a.say("%s", out)
			a.refreshDevices()
			if enter && mp != "" {
				a.cd(a.pane(), mp, true)
			}
		})
	}()
}

func (a *app) deviceMenu() {
	a.devices = listDevices()
	a.buildPlaces()
	if len(a.devices) == 0 {
		a.say("no removable drives found")
		return
	}
	var items []mitem
	for _, d := range a.devices {
		state := "not mounted"
		if d.mount != "" {
			state = tilde(d.mount)
		}
		items = append(items, mitem{label: d.label + "  " + d.size + " " + d.fstype, hint: state})
	}
	a.menu = &menu{title: "Drives", items: items,
		keys: map[string]string{"u": "unmount", "e": "eject"},
		pick: func(i int, k string) {
			d := a.devices[i]
			switch {
			case k == "enter" && d.mount == "":
				a.mount(&d, true)
			case k == "enter":
				a.cd(a.pane(), d.mount, true)
			default:
				go func() {
					// leave the mount point first so it is not busy
					_, err := udisks("unmount", "-b", d.dev)
					if err == nil && k == "e" {
						_, err = udisks("power-off", "-b", d.dev)
					}
					a.post(func() {
						if err != nil {
							a.fail(err)
						} else {
							a.say("%s can be removed", d.label)
						}
						a.refreshDevices()
					})
				}()
				for _, t := range a.tabs {
					for _, p := range t.panes {
						if p != nil && d.mount != "" && isInside(p.dir, d.mount) {
							a.cd(p, home(), false)
						}
					}
				}
			}
		}}
}

func (a *app) sortMenu() {
	on := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	cur := func(s string) string {
		if a.cfg.sortBy == s {
			return "●"
		}
		return ""
	}
	a.menu = &menu{title: "Sort by", items: []mitem{
		{hot: "n", label: "name (natural: 2 before 10)", hint: cur("name")},
		{hot: "s", label: "size, largest first", hint: cur("size")},
		{hot: "t", label: "modified, newest first", hint: cur("time")},
		{hot: "e", label: "extension", hint: cur("ext")},
		{hot: "r", label: "reverse", hint: on(a.cfg.reverse)},
		{hot: "d", label: "folders first", hint: on(a.cfg.dirsFirst)},
	}, pick: func(i int, _ string) {
		switch i {
		case 0, 1, 2, 3:
			a.cfg.sortBy = []string{"name", "size", "time", "ext"}[i]
		case 4:
			a.cfg.reverse = !a.cfg.reverse
		case 5:
			a.cfg.dirsFirst = !a.cfg.dirsFirst
		}
		a.reloadAll()
	}}
}

func (a *app) openWithMenu() {
	p := a.pane()
	ts := a.targets(p)
	if len(ts) == 0 {
		return
	}
	files := paths(ts)
	m := mimeOf(files[0])
	cands := appsFor(m)
	seen := map[string]bool{}
	var list []*desktopApp
	for _, c := range cands {
		seen[c.id] = true
		list = append(list, c)
	}
	nRec := len(list)
	var rest []*desktopApp
	for id, ap := range loadApps() {
		if !seen[id] && !ap.hidden {
			rest = append(rest, ap)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return strings.ToLower(rest[i].name) < strings.ToLower(rest[j].name) })
	list = append(list, rest...)
	var items []mitem
	items = append(items, mitem{label: "$EDITOR in this terminal", hint: a.cfg.editor})
	for i, ap := range list {
		hint := ""
		switch {
		case i == 0 && nRec > 0:
			hint = "default"
		case i < nRec:
			hint = "for " + m
		}
		if ap.terminal {
			hint = strings.TrimSpace(hint + " · terminal")
		}
		st := plain
		if i >= nRec {
			st = dim()
		}
		items = append(items, mitem{label: ap.name, hint: hint, st: st})
	}
	items = append(items, mitem{label: "Custom command…", hint: "%f = file"})
	a.menu = &menu{title: "Open " + fit(filepath.Base(files[0]), 30, true) + " with", items: items, filterable: true,
		keys: map[string]string{"ctrl+d": "make default"},
		pick: func(i int, k string) {
			switch {
			case i == 0:
				a.editor(p.dir, files...)
			case i == len(items)-1:
				a.ask("command (%f = file): ", "", -1, func(s string) {
					if s == "" {
						return
					}
					q := make([]string, len(files))
					for i, f := range files {
						q[i] = shq(f)
					}
					c := strings.ReplaceAll(s, "%f", strings.Join(q, " "))
					if !strings.Contains(s, "%f") {
						c += " " + strings.Join(q, " ")
					}
					a.fail(spawn(p.dir, "sh", "-c", c))
				})
			default:
				ap := list[i-1]
				if k == "ctrl+d" {
					if err := setDefaultApp(m, ap.id); err != nil {
						a.fail(err)
						return
					}
					a.say("%s now opens with %s", m, ap.name)
				}
				a.launch(ap, files)
			}
		}}
}

func (a *app) contextMenu() {
	p := a.pane()
	e := p.entry()
	type act struct {
		label, key string
		fn         func()
	}
	var acts []act
	add := func(label, key string, fn func()) { acts = append(acts, act{label, key, fn}) }
	if p.kind == kTrash {
		add("Restore", "r", a.restore)
		add("Delete permanently", "D", a.deletePerm)
		add("Empty trash", ":emptytrash", a.emptyTrash)
		add("Properties", "i", a.info)
	} else {
		if e != nil {
			add("Open", "l", a.openEntry)
			add("Open with…", "o", a.openWithMenu)
			if !e.isDir() {
				add("Edit in $EDITOR", "e", func() { a.editor(p.dir, paths(a.targets(p))...) })
			}
			add("Copy", "y", func() { a.yank(false) })
			add("Cut", "x", func() { a.yank(true) })
		}
		if len(a.clip) > 0 || wayland() {
			add("Paste", "p", func() { a.paste(false) })
			add("Paste as link", "P", func() { a.paste(true) })
		}
		if e != nil {
			add("Rename", "r", a.rename)
			if len(p.sel) > 1 {
				add("Rename all in $EDITOR", "R", a.bulkRename)
			}
			add("Move to trash", "d", a.trash)
			add("Delete permanently", "D", a.deletePerm)
			if !e.isDir() && archiveKind(e.path) != "" {
				add("Extract here", "E", a.extractSel)
			}
			add("Compress…", "Z", a.compressSel)
			add("Copy path", "c", a.copyPath)
			if p.kind == kSearch {
				add("Show in folder", "gl", a.reveal)
			}
		}
		add("New file…", "a", func() { a.newItem(false) })
		add("New folder…", "A", func() { a.newItem(true) })
		add("Open terminal here", "T", a.terminalHere)
		add("Bookmark this folder", "b", a.bookmark)
		if e != nil {
			add("Properties", "i", a.info)
		}
	}
	items := make([]mitem, len(acts))
	for i, x := range acts {
		items[i] = mitem{label: x.label, hint: x.key}
	}
	title := "Actions"
	if e != nil {
		title = fit(e.name, 40, true)
	}
	a.menu = &menu{title: title, items: items, pick: func(i int, _ string) { acts[i].fn() }}
}

// ---------------------------------------------------------------- properties

func (a *app) info() {
	p := a.pane()
	ts := a.targets(p)
	if len(ts) == 0 {
		return
	}
	o := &overlay{title: "Properties"}
	row := func(k, v string) { o.lines = append(o.lines, sline{fmt.Sprintf("%-12s %s", k, v), plain}) }
	sizeRow := -1
	var dirs []string
	if len(ts) > 1 {
		o.title = fmt.Sprintf("Properties · %d items", len(ts))
		var total int64
		for _, e := range ts {
			if e.isDir() {
				dirs = append(dirs, e.path)
			} else {
				total += e.size()
			}
		}
		row("Items", strconv.Itoa(len(ts)))
		sizeRow = len(o.lines)
		row("Size", human(total)+"…")
		row("Location", tilde(commonDir(paths(ts))))
		if len(dirs) == 0 {
			o.lines[sizeRow].s = fmt.Sprintf("%-12s %s (%d bytes)", "Size", human(total), total)
		} else {
			a.du(o, sizeRow, paths(ts))
		}
		a.over = o
		return
	}
	e := ts[0]
	o.title = "Properties · " + fit(e.name, 40, true)
	row("Name", filepath.Base(e.path))
	m := mimeOf(e.path)
	row("Type", m)
	loc := filepath.Dir(e.path)
	if e.trash != nil {
		loc = filepath.Dir(e.trash.orig)
	}
	row("Location", tilde(loc))
	if e.isLink() {
		t := e.link
		if e.ti == nil {
			t += "  (broken)"
		}
		row("Points to", t)
	}
	if e.fi == nil {
		a.over = o
		return
	}
	fi := e.info()
	if e.isDir() {
		sizeRow = len(o.lines)
		row("Size", "counting…")
		a.du(o, sizeRow, []string{e.path})
	} else {
		row("Size", fmt.Sprintf("%s (%d bytes)", human(fi.Size()), fi.Size()))
	}
	row("Permissions", fmt.Sprintf("%s  %04o", modeString(fi.Mode()), fi.Mode().Perm()))
	u, g := ownerOf(fi)
	row("Owner", u+" : "+g)
	row("Modified", fi.ModTime().Format("2006-01-02 15:04:05"))
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		row("Accessed", time.Unix(st.Atim.Sec, st.Atim.Nsec).Format("2006-01-02 15:04:05"))
		row("Changed", time.Unix(st.Ctim.Sec, st.Ctim.Nsec).Format("2006-01-02 15:04:05"))
		row("Inode", fmt.Sprintf("%d  (%d links)", st.Ino, st.Nlink))
	}
	if e.trash != nil {
		row("Deleted", e.trash.when.Format("2006-01-02 15:04:05"))
	}
	mt := mountTop(e.path)
	free, total := freeSpace(e.path)
	row("On", fmt.Sprintf("%s  (%s free of %s)", tilde(mt), human(int64(free)), human(int64(total))))
	if !e.isDir() {
		if apps := appsFor(m); len(apps) > 0 {
			row("Opens with", apps[0].name)
		} else {
			row("Opens with", "nothing yet — press o to choose")
		}
		o.footer = "c checksums · o open with · q close"
		o.keys = map[string]func(){
			"c": func() { a.checksums(o, e.path) },
			"o": func() { a.over = nil; a.openWithMenu() },
		}
	}
	a.over = o
}

func (a *app) du(o *overlay, row int, ps []string) {
	go func() {
		var size, files, dirs int64
		for _, p := range ps {
			filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if d.IsDir() {
					dirs++
				} else {
					files++
					if i, err := d.Info(); err == nil && i.Mode().IsRegular() {
						size += i.Size()
					}
				}
				return nil
			})
		}
		a.post(func() {
			if row < len(o.lines) {
				o.lines[row].s = fmt.Sprintf("%-12s %s (%d bytes) · %d files, %d folders", "Size", human(size), size, files, max(dirs-int64(len(ps)), 0))
			}
		})
	}()
}

func (a *app) checksums(o *overlay, p string) {
	o.lines = append(o.lines, sline{"", plain}, sline{"computing checksums…", dim()})
	at := len(o.lines) - 1
	go func() {
		f, err := os.Open(p)
		var res []string
		if err == nil {
			hs := []hash.Hash{md5.New(), sha1.New(), sha256.New()}
			_, err = io.Copy(io.MultiWriter(hs[0], hs[1], hs[2]), f)
			f.Close()
			for i, n := range []string{"MD5", "SHA-1", "SHA-256"} {
				res = append(res, fmt.Sprintf("%-12s %x", n, hs[i].Sum(nil)))
			}
		}
		a.post(func() {
			if err != nil {
				o.lines[at] = sline{err.Error(), fg(1)}
				return
			}
			o.lines = append(o.lines[:at], sline{res[0], plain}, sline{res[1], plain}, sline{res[2], plain})
		})
	}()
}

// ---------------------------------------------------------------- search

func (a *app) search(q string, content bool) {
	p := a.pane()
	root := p.dir
	if p.kind == kSearch {
		root = p.origin
	}
	if p.kind == kDir {
		p.back = append(p.back, p.dir)
	}
	p.stopSearch()
	ctx, cancel := context.WithCancel(context.Background())
	p.kind, p.cancel, p.loading = kSearch, cancel, true
	p.all, p.view, p.cur, p.top, p.filter, p.sel, p.origin = nil, nil, 0, 0, "", map[string]bool{}, root
	verb := "find"
	if content {
		verb = "find text"
	}
	p.title = fmt.Sprintf("%s “%s” in %s", verb, q, tilde(root))
	smart := strings.ToLower(q) == q
	needle := q
	if smart {
		needle = strings.ToLower(q)
	}
	glob := !content && strings.ContainsAny(q, "*?[")
	hidden := a.cfg.hidden
	go func() {
		var batch []*entry
		last, n := time.Now(), 0
		send := func(final bool) {
			b := batch
			batch = nil
			a.post(func() {
				if ctx.Err() != nil && !final {
					return
				}
				if p.kind != kSearch || p.origin != root {
					return
				}
				p.all = append(p.all, b...)
				if final {
					p.loading = false
					p.title = fmt.Sprintf("%s — %d found", strings.TrimSuffix(p.title, "…"), len(p.all))
				}
				a.applyView(p, "")
			})
		}
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil || path == root {
				return nil
			}
			name := d.Name()
			if !hidden && strings.HasPrefix(name, ".") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() && (path == "/proc" || path == "/sys" || path == "/dev" || path == "/run") {
				return filepath.SkipDir
			}
			note, match := "", false
			if content {
				if d.Type().IsRegular() {
					note, match = grepFile(path, needle, smart)
				}
			} else {
				nm := name
				if smart {
					nm = strings.ToLower(nm)
				}
				if glob {
					match, _ = filepath.Match(needle, nm)
				} else {
					match = strings.Contains(nm, needle)
				}
			}
			if match {
				rel, _ := filepath.Rel(root, path)
				e := mkEntry(rel, path)
				e.note = note
				batch = append(batch, e)
				n++
			}
			if n >= 5000 {
				return filepath.SkipAll
			}
			if len(batch) > 0 && time.Since(last) > 150*time.Millisecond {
				last = time.Now()
				send(false)
			}
			return nil
		})
		send(true)
	}()
}

func grepFile(p, needle string, fold bool) (string, bool) {
	fi, err := os.Stat(p)
	if err != nil || fi.Size() > 16<<20 {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	head, _ := br.Peek(1024)
	if !isText(head) {
		return "", false
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for ln := 1; sc.Scan(); ln++ {
		l := sc.Text()
		hay := l
		if fold {
			hay = strings.ToLower(l)
		}
		if strings.Contains(hay, needle) {
			return fmt.Sprintf("%d: %s", ln, strings.TrimSpace(l)), true
		}
	}
	return "", false
}

func (a *app) reveal() {
	p := a.pane()
	if e := p.entry(); e != nil {
		a.cd(p, e.path, true)
		if !e.isDir() {
			return
		}
		a.cd(p, filepath.Dir(e.path), false)
		a.applyView(p, e.path)
	}
}

// ---------------------------------------------------------------- misc actions

func (a *app) openEntry() {
	p := a.pane()
	e := p.entry()
	if e == nil {
		return
	}
	if p.kind == kTrash {
		a.info()
		return
	}
	if e.isDir() {
		a.cd(p, e.path, true)
		return
	}
	files := []string{e.path}
	if p.sel[e.path] {
		files = paths(a.targets(p))
	}
	if a.pick {
		if len(p.sel) > 0 {
			files = paths(a.targets(p))
		}
		a.picked, a.quit = files, true
		return
	}
	a.openFiles(files)
}

func (a *app) parent() {
	p := a.pane()
	switch p.kind {
	case kSearch:
		p.stopSearch()
		a.cd(p, p.origin, false)
		return
	case kTrash:
		a.cd(p, p.dir, false)
		return
	}
	if p.dir != "/" {
		a.cd(p, filepath.Dir(p.dir), true)
	}
}

func (a *app) history(back bool) {
	p := a.pane()
	from, to := &p.back, &p.fwd
	if !back {
		from, to = to, from
	}
	for len(*from) > 0 {
		d := (*from)[len(*from)-1]
		*from = (*from)[:len(*from)-1]
		if isDirPath(d) {
			*to = append(*to, p.dir)
			a.cd(p, d, false)
			return
		}
	}
	a.say("no more history")
}

func (a *app) copyPath() {
	ts := a.targets(a.pane())
	if len(ts) == 0 {
		return
	}
	s := strings.Join(paths(ts), "\n")
	a.sysCopyText(s)
	a.say("copied %s", fit(strings.ReplaceAll(tilde(s), "\n", "  "), a.w-12, false))
}

func (a *app) copyName() {
	ts := a.targets(a.pane())
	var ns []string
	for _, e := range ts {
		ns = append(ns, filepath.Base(e.path))
	}
	if len(ns) > 0 {
		a.sysCopyText(strings.Join(ns, "\n"))
		a.say("copied %s", strings.Join(ns, "  "))
	}
}

func (a *app) terminalHere() {
	dir := a.pane().dir
	a.fail(spawn(dir, "sh", "-c", a.cfg.terminal))
	a.say("terminal opened in %s", tilde(dir))
}

func (a *app) shellHere() {
	sh := firstOf(os.Getenv("SHELL"), "sh")
	a.say("")
	a.fail(a.runFg(a.pane().dir, false, sh))
}

func (a *app) runCommand(cmdline string) {
	p := a.pane()
	var cur string
	if e := p.entry(); e != nil {
		cur = e.path
	}
	sel := paths(a.targets(p))
	q := make([]string, len(sel))
	for i, s := range sel {
		q[i] = shq(s)
	}
	c := strings.ReplaceAll(cmdline, "%f", shq(cur))
	c = strings.ReplaceAll(c, "%s", strings.Join(q, " "))
	c = strings.ReplaceAll(c, "%d", shq(p.dir))
	os.Setenv("f", cur)
	os.Setenv("fs", strings.Join(sel, "\n"))
	os.Setenv("d", p.dir)
	a.fail(a.runFg(p.dir, true, "sh", "-c", c))
}

func (a *app) newItem(dir bool) {
	label := "new file (end with / for a folder): "
	if dir {
		label = "new folder: "
	}
	a.ask(label, "", -1, func(s string) { a.create(s, dir) })
}

func (a *app) bookmark() {
	p := a.pane()
	if p.kind != kDir {
		return
	}
	added, err := toggleBookmark(p.dir)
	if err != nil {
		a.fail(err)
		return
	}
	if added {
		a.say("bookmarked %s (shared with GTK file pickers)", tilde(p.dir))
	} else {
		a.say("removed bookmark %s", tilde(p.dir))
	}
	a.buildPlaces()
}

func (a *app) filter() {
	p := a.pane()
	old := p.filter
	pr := a.ask("filter: ", p.filter, -1, func(s string) { p.filter = s; a.applyView(p, "") })
	pr.change = func(s string) { p.filter = s; a.applyView(p, "") }
	pr.cancel = func() { p.filter = old; a.applyView(p, "") }
}

func (a *app) selectGlob(on bool) {
	label := "select matching: "
	if !on {
		label = "unselect matching: "
	}
	a.ask(label, "*", -1, func(s string) {
		p := a.pane()
		n := 0
		for _, e := range p.view {
			if ok, _ := filepath.Match(s, e.name); ok || strings.Contains(strings.ToLower(e.name), strings.ToLower(s)) {
				if on {
					p.sel[e.path] = true
				} else {
					delete(p.sel, e.path)
				}
				n++
			}
		}
		a.say("%d matched", n)
	})
}

func (a *app) newTab() {
	p := a.pane()
	np := newPane(p.dir)
	t := &tab{panes: [2]*pane{np, nil}}
	a.load(np, "")
	a.tabs = append(a.tabs[:a.ti+1], append([]*tab{t}, a.tabs[a.ti+1:]...)...)
	a.ti++
}

func (a *app) closeTab() {
	if len(a.tabs) == 1 {
		a.say("last tab — q quits")
		return
	}
	for _, p := range a.tab().panes {
		if p != nil {
			p.stopSearch()
		}
	}
	a.tabs = append(a.tabs[:a.ti], a.tabs[a.ti+1:]...)
	a.ti = min(a.ti, len(a.tabs)-1)
}

func (a *app) toggleSplit() {
	t := a.tab()
	if t.panes[1] == nil {
		t.panes[1] = newPane(t.panes[0].dir)
		a.load(t.panes[1], "")
	}
	t.split = !t.split
	if !t.split {
		if t.active == 1 {
			t.panes[0], t.panes[1] = t.panes[1], t.panes[0]
		}
		t.active = 0
	} else {
		a.load(t.panes[1], "")
	}
}

func (a *app) setMark(k string) {
	p := a.pane()
	if p.kind != kDir || utf8.RuneCountInString(k) != 1 {
		return
	}
	a.marks[k] = p.dir
	a.saveState()
	a.say("mark %s → %s", k, tilde(p.dir))
}

func (a *app) jumpMark(k string) {
	d, ok := a.marks[k]
	if !ok {
		a.say("no mark %s — set one with m%s", k, k)
		return
	}
	a.cd(a.pane(), d, true)
}

func (a *app) help() {
	o := &overlay{title: "muhfm " + version + " — keys"}
	sec := ""
	for _, b := range a.bindList() {
		if b.desc == "" {
			continue
		}
		if b.section != sec {
			if sec != "" {
				o.lines = append(o.lines, sline{"", plain})
			}
			sec = b.section
			o.lines = append(o.lines, sline{strings.ToUpper(sec), fg(3).with(aBold)})
		}
		o.lines = append(o.lines, sline{fmt.Sprintf("  %-18s %s", prettyKeys(b.keys), b.desc), plain})
	}
	o.lines = append(o.lines, sline{"", plain},
		sline{"MOUSE", fg(3).with(aBold)},
		sline{"  click / double-click  select / open    right-click  actions    wheel  scroll", plain},
		sline{"", plain},
		sline{"COMMANDS  (:)", fg(3).with(aBold)},
		sline{"  cd mkdir touch rename chmod find grep sort set select unselect", plain},
		sline{"  extract compress trash emptytrash bookmark drives tab split sh q", plain},
		sline{"  :set hidden|preview|places|details|reverse|dirsfirst on|off", plain},
		sline{"  :set sort name|size|time|ext   :set icons none|nerd|emoji|ascii", plain})
	a.over = o
}

func prettyKeys(k string) string {
	var out []string
	for _, alt := range strings.Split(k, "|") {
		alt = strings.ReplaceAll(alt, " ", "")
		if alt == "bar" {
			alt = "|"
		}
		for from, to := range map[string]string{"down": "↓", "up": "↑", "left": "←", "right": "→"} {
			if alt == from {
				alt = to
			}
		}
		out = append(out, alt)
	}
	return strings.Join(out, " ")
}

func (a *app) setOpt(arg string) {
	f := strings.Fields(strings.ReplaceAll(arg, "=", " "))
	if len(f) == 0 {
		a.say("hidden=%v preview=%v places=%v details=%v sort=%s reverse=%v dirsfirst=%v icons=%s",
			a.cfg.hidden, a.cfg.preview, a.cfg.places, a.cfg.details, a.cfg.sortBy, a.cfg.reverse, a.cfg.dirsFirst, a.cfg.icons)
		return
	}
	v := ""
	if len(f) > 1 {
		v = f[1]
	}
	b := func(cur bool) bool {
		switch v {
		case "on", "true", "yes", "1":
			return true
		case "off", "false", "no", "0":
			return false
		}
		return !cur
	}
	switch f[0] {
	case "hidden":
		a.cfg.hidden = b(a.cfg.hidden)
	case "preview":
		a.cfg.preview = b(a.cfg.preview)
	case "places", "sidebar":
		a.cfg.places = b(a.cfg.places)
	case "details":
		a.cfg.details = b(a.cfg.details)
	case "reverse":
		a.cfg.reverse = b(a.cfg.reverse)
	case "dirsfirst", "dirs_first":
		a.cfg.dirsFirst = b(a.cfg.dirsFirst)
	case "sort":
		a.cfg.sortBy = v
	case "icons":
		if _, ok := icons[v]; ok || v == "none" {
			a.cfg.icons = v
		}
	case "sixel", "images":
		a.sixOn = b(a.sixOn)
	default:
		a.fail(fmt.Errorf("unknown option %s", f[0]))
		return
	}
	a.reloadAll()
}

func (a *app) command(s string) {
	p := a.pane()
	cmd, arg, _ := strings.Cut(strings.TrimSpace(s), " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "":
	case "cd":
		a.cd(p, expand(firstOf(arg, "~"), p.dir), true)
	case "mkdir":
		a.create(arg, true)
	case "touch", "new":
		a.create(arg, false)
	case "rename", "mv":
		if e := p.entry(); e != nil && arg != "" {
			a.ask("rename: ", arg, -1, nil).done = nil
			a.prompt = nil
			dst := expand(arg, filepath.Dir(e.path))
			if exists(dst) {
				a.fail(fmt.Errorf("%s already exists", arg))
				return
			}
			src := e.path
			if err := os.Rename(src, dst); err != nil {
				a.fail(err)
				return
			}
			a.pushUndo("rename", func() error { return os.Rename(dst, src) })
			a.load(p, dst)
		}
	case "chmod":
		a.chmod(arg)
	case "find":
		a.search(arg, false)
	case "grep":
		a.search(arg, true)
	case "sort":
		a.setOpt("sort " + arg)
	case "set":
		a.setOpt(arg)
	case "hidden":
		a.setOpt("hidden")
	case "select":
		for _, e := range p.view {
			if ok, _ := filepath.Match(arg, e.name); ok {
				p.sel[e.path] = true
			}
		}
	case "unselect":
		for _, e := range p.view {
			if ok, _ := filepath.Match(arg, e.name); ok {
				delete(p.sel, e.path)
			}
		}
	case "extract":
		a.extractSel()
	case "compress":
		if arg == "" {
			a.compressSel()
		} else {
			a.compressSel()
			if a.prompt != nil {
				a.prompt.buf, a.prompt.pos = []rune(arg), len([]rune(arg))
			}
		}
	case "trash":
		a.openTrash()
	case "emptytrash":
		a.emptyTrash()
	case "bookmark":
		a.bookmark()
	case "drives", "mount", "devices":
		a.deviceMenu()
	case "tab":
		a.newTab()
		if arg != "" {
			a.cd(a.pane(), expand(arg, p.dir), false)
		}
	case "split":
		a.toggleSplit()
	case "sh", "shell", "!":
		if arg == "" {
			a.shellHere()
		} else {
			a.runCommand(arg)
		}
	case "q", "quit":
		a.quit = true
	case "help":
		a.help()
	case "undo":
		a.undo()
	default:
		a.fail(fmt.Errorf("unknown command: %s (? lists them)", cmd))
	}
}

// ---------------------------------------------------------------- key table

type bind struct {
	keys, section, desc string
	fn                  func()
}

// bindList is the single source of truth: it drives dispatch, ? and --keys.
func (a *app) bindList() []bind {
	p := func() *pane { return a.pane() }
	page := func() int { return max(a.bodyH()-1, 1) }
	gohome := func(sub string) func() {
		return func() {
			d := home()
			if sub != "" {
				if v := userDirs()[strings.ToUpper(sub)]; v != "" {
					d = v
				} else {
					d = filepath.Join(home(), map[string]string{"DOWNLOAD": "Downloads", "DOCUMENTS": "Documents",
						"PICTURES": "Pictures", "MUSIC": "Music", "VIDEOS": "Videos", "DESKTOP": "Desktop"}[sub])
				}
			}
			a.cd(p(), d, true)
		}
	}
	bs := []bind{
		{"j|down", "move", "down", func() { a.move(p(), a.takeCount()) }},
		{"k|up", "move", "up", func() { a.move(p(), -a.takeCount()) }},
		{"h|left|backspace", "move", "parent folder (leaves search/trash)", a.parent},
		{"l|right|enter", "move", "open folder or file", a.openEntry},
		{"g g|home", "move", "first item", func() { a.move(p(), -len(p().view)) }},
		{"G|end", "move", "last item", func() { a.move(p(), len(p().view)) }},
		{"ctrl+d", "move", "half page down", func() { a.move(p(), page()/2) }},
		{"ctrl+u", "move", "half page up", func() { a.move(p(), -page()/2) }},
		{"pgdn|ctrl+f", "move", "page down", func() { a.move(p(), page()) }},
		{"pgup|ctrl+b", "move", "page up", func() { a.move(p(), -page()) }},
		{"[|alt+left", "move", "back", func() { a.history(true) }},
		{"]|alt+right", "move", "forward", func() { a.history(false) }},
		{"alt+up", "move", "", a.parent},

		{"'", "go", "go to: places, bookmarks, drives, marks, recent", a.jumpMenu},
		{"ctrl+l", "go", "type a location (Tab completes)", func() {
			a.ask("go to: ", tilde(p().dir)+"/", -1, func(s string) {
				if s != "" {
					a.cd(p(), expand(s, p().dir), true)
				}
			}).complete = true
		}},
		{"~|g h", "go", "home", gohome("")},
		{"g d", "go", "Downloads", gohome("DOWNLOAD")},
		{"g D", "go", "Documents", gohome("DOCUMENTS")},
		{"g p", "go", "Pictures", gohome("PICTURES")},
		{"g c", "go", "~/.config", func() { a.cd(p(), configHome(), true) }},
		{"g /", "go", "root /", func() { a.cd(p(), "/", true) }},
		{"g r", "go", "trash", a.openTrash},
		{"g m", "go", "drives: mount, unmount, eject", a.deviceMenu},
		{"H", "go", "recent folders", a.recentMenu},
		{"m", "go", "m<key> marks this folder", nil},
		{"`", "go", "`<key> jumps to a mark", nil},
		{"b", "go", "bookmark / unbookmark this folder", a.bookmark},

		{"/", "find", "filter this folder as you type", a.filter},
		{"ctrl+s|f", "find", "find files by name below here (glob ok)", func() {
			a.ask("find name: ", "", -1, func(s string) {
				if s != "" {
					a.search(s, false)
				}
			})
		}},
		{"ctrl+g|F", "find", "find files containing text below here", func() {
			a.ask("find text: ", "", -1, func(s string) {
				if s != "" {
					a.search(s, true)
				}
			})
		}},
		{"g l", "find", "show a search result in its folder", a.reveal},

		{"space", "select", "select / unselect, then down", func() {
			if e := p().entry(); e != nil {
				if p().sel[e.path] {
					delete(p().sel, e.path)
				} else {
					p().sel[e.path] = true
				}
				a.move(p(), 1)
			}
		}},
		{"v", "select", "visual: select a range by moving", func() {
			if p().visual >= 0 {
				p().visual = -1
			} else {
				p().visual = p().cur
				a.move(p(), 0)
			}
		}},
		{"ctrl+a", "select", "select all", func() {
			for _, e := range p().view {
				p().sel[e.path] = true
			}
		}},
		{"*", "select", "invert selection", func() {
			for _, e := range p().view {
				if p().sel[e.path] {
					delete(p().sel, e.path)
				} else {
					p().sel[e.path] = true
				}
			}
		}},
		{"+", "select", "select by pattern", func() { a.selectGlob(true) }},
		{"-", "select", "unselect by pattern", func() { a.selectGlob(false) }},
		{"esc", "select", "clear selection, filter, search", func() {
			q := p()
			switch {
			case q.visual >= 0:
				q.visual = -1
			case len(q.sel) > 0:
				q.sel = map[string]bool{}
			case q.filter != "":
				q.filter = ""
				a.applyView(q, "")
			case q.kind == kSearch && q.loading:
				q.stopSearch()
				a.say("search stopped")
			case len(a.clip) > 0:
				a.clip = nil
				a.say("clipboard cleared")
			}
		}},

		{"y", "files", "copy (also to the system clipboard)", func() { a.yank(false) }},
		{"x", "files", "cut", func() { a.yank(true) }},
		{"p", "files", "paste here (from muhfm or any app)", func() { a.paste(false) }},
		{"P", "files", "paste as symlink", func() { a.paste(true) }},
		{"f6", "files", "copy to the other split pane", func() { a.toOther("copy") }},
		{"f7", "files", "move to the other split pane", func() { a.toOther("move") }},
		{"d|delete", "files", "move to trash", a.trash},
		{"D|shift+delete", "files", "delete permanently (asks)", a.deletePerm},
		{"r|f2", "files", "rename (restore, in the trash)", a.rename},
		{"R", "files", "rename all selected in $EDITOR", a.bulkRename},
		{"a", "files", "new file (name/ makes a folder)", func() { a.newItem(false) }},
		{"A|f10", "files", "new folder", func() { a.newItem(true) }},
		{"X", "files", "toggle executable", func() { a.chmod("toggle-x") }},
		{"E", "files", "extract archive here", a.extractSel},
		{"Z", "files", "compress to .zip/.tar.gz…", a.compressSel},
		{"u|ctrl+z", "files", "undo", a.undo},
		{"c", "files", "copy path to clipboard", a.copyPath},
		{"C", "files", "copy name to clipboard", a.copyName},
		{"i|alt+enter", "files", "properties, folder size, checksums", a.info},

		{"o", "open", "open with… (ctrl+d there sets default)", a.openWithMenu},
		{"e", "open", "edit in $EDITOR", func() {
			if ts := a.targets(p()); len(ts) > 0 {
				a.editor(p().dir, paths(ts)...)
			}
		}},
		{"T|f4", "open", "open a terminal window here", a.terminalHere},
		{"s", "open", "shell here (exit to return)", a.shellHere},
		{"!", "open", "run a command (%f file, %s selection)", func() {
			a.ask("!", "", -1, func(s string) {
				if s != "" {
					a.runCommand(s)
				}
			}).complete = true
		}},
		{":", "open", "command line (see below)", func() {
			a.ask(":", "", -1, a.command).complete = true
		}},
		{"ctrl+space|\\", "open", "actions menu (also right-click)", a.contextMenu},

		{".", "view", "show / hide hidden files", func() { a.setOpt("hidden") }},
		{",", "view", "sort…", a.sortMenu},
		{"=", "view", "details columns on / off", func() { a.setOpt("details") }},
		{"w|f11", "view", "preview pane on / off", func() { a.setOpt("preview") }},
		{"B|f9", "view", "places sidebar on / off", func() { a.setOpt("places") }},
		{"bar|f3", "view", "split view on / off", a.toggleSplit},
		{"tab", "view", "other pane (or next tab)", func() {
			t := a.tab()
			if t.split {
				t.active = 1 - t.active
			} else if len(a.tabs) > 1 {
				a.ti = (a.ti + 1) % len(a.tabs)
			}
		}},
		{"J", "view", "scroll preview down", func() { a.pvScroll += 5 }},
		{"K", "view", "scroll preview up", func() { a.pvScroll = max(a.pvScroll-5, 0) }},
		{"I", "view", "cycle icons: none, nerd, emoji, ascii", func() {
			order := []string{"none", "nerd", "emoji", "ascii"}
			for i, s := range order {
				if s == a.cfg.icons {
					a.cfg.icons = order[(i+1)%len(order)]
					break
				}
			}
			a.say("icons: %s", a.cfg.icons)
		}},
		{"ctrl+r|f5", "view", "reload", func() { a.pv = map[string]*preview{}; a.reloadAll(); a.refreshDevices() }},

		{"ctrl+t", "tabs", "new tab", a.newTab},
		{"ctrl+w", "tabs", "close tab", a.closeTab},
		{"g t", "tabs", "next tab", func() { a.ti = (a.ti + 1) % len(a.tabs) }},
		{"g T", "tabs", "previous tab", func() { a.ti = (a.ti + len(a.tabs) - 1) % len(a.tabs) }},
		{"alt+1", "tabs", "alt+1…9 jumps to a tab", nil},

		{"?|f1", "app", "this help", a.help},
		{"q", "app", "quit", func() { a.quit = true }},
		{"ctrl+c", "app", "cancel running jobs, else quit", func() {
			if len(a.jobs) > 0 {
				for _, j := range a.jobs {
					j.cancel()
				}
				a.say("cancelling…")
				return
			}
			a.quit = true
		}},
	}
	for i := 1; i <= 9; i++ {
		n := i
		bs = append(bs, bind{keys: fmt.Sprintf("alt+%d", i), section: "tabs", fn: func() {
			if n <= len(a.tabs) {
				a.ti = n - 1
			}
		}})
	}
	return bs
}

func (a *app) setupBinds() {
	a.binds, a.prefixes = map[string]func(){}, map[string]bool{"m": true, "`": true}
	for _, b := range a.bindList() {
		if b.fn == nil {
			continue
		}
		for _, k := range strings.Split(b.keys, "|") {
			if k == "bar" {
				k = "|"
			}
			a.binds[k] = b.fn
			if f, _, ok := strings.Cut(k, " "); ok {
				a.prefixes[f] = true
			}
		}
	}
}

func (a *app) handle(k key) {
	if k.k == "mouse" {
		a.mouse(k)
		return
	}
	if k.k != "paste" || a.prompt == nil {
		a.msg = ""
	}
	if a.prompt != nil {
		a.promptKey(k)
		return
	}
	if a.menu != nil {
		a.menuKey(k)
		return
	}
	if a.over != nil {
		a.overKey(k)
		return
	}
	if k.k == "paste" { // a pasted path jumps there
		if t := strings.TrimSpace(strings.SplitN(k.text, "\n", 2)[0]); t != "" {
			if d := expand(t, a.pane().dir); exists(d) {
				a.cd(a.pane(), d, true)
			}
		}
		return
	}
	if a.pending != "" {
		pre := a.pending
		a.pending = ""
		switch pre {
		case "m":
			a.setMark(k.k)
			return
		case "`":
			a.jumpMark(k.k)
			return
		}
		if fn := a.binds[pre+" "+k.k]; fn != nil {
			fn()
		}
		a.count = 0
		return
	}
	if len(k.k) == 1 && k.k[0] >= '0' && k.k[0] <= '9' && (k.k != "0" || a.count > 0) {
		a.count = min(a.count*10+int(k.k[0]-'0'), 9999)
		return
	}
	if a.prefixes[k.k] {
		a.pending = k.k
		return
	}
	if fn := a.binds[k.k]; fn != nil {
		fn()
	}
	a.count = 0
}

func (a *app) mouse(k key) {
	if k.mrel {
		return
	}
	b := k.mb &^ (4 | 8 | 16)
	if a.menu != nil {
		switch b {
		case 64:
			a.menu.cur--
		case 65:
			a.menu.cur++
		default:
			a.menu = nil
		}
		return
	}
	if a.over != nil {
		switch b {
		case 64:
			a.over.scroll -= 3
		case 65:
			a.over.scroll += 3
		default:
			a.over = nil
		}
		return
	}
	if a.prompt != nil {
		return
	}
	for _, h := range a.hits {
		if k.mx < h.x0 || k.mx >= h.x1 || k.my < h.y0 || k.my >= h.y1 {
			continue
		}
		switch h.kind {
		case "tab":
			if b == 0 {
				a.ti = h.idx
			}
		case "place":
			if b == 0 && h.idx < len(a.places) {
				a.goPlace(&a.places[h.idx])
			}
		case "preview":
			if b == 64 {
				a.pvScroll = max(a.pvScroll-3, 0)
			} else if b == 65 {
				a.pvScroll += 3
			}
		case "pane":
			t := a.tab()
			for i, q := range t.panes {
				if q == h.pane && (i == 0 || t.split) {
					t.active = i
				}
			}
			p := h.pane
			switch b {
			case 64:
				a.move(p, -3)
			case 65:
				a.move(p, 3)
			case 0, 2:
				idx := p.top + k.my - h.row0
				if idx >= len(p.view) {
					if b == 2 {
						p.cur = len(p.view)
						a.contextMenu()
					}
					return
				}
				dbl := idx == a.lastIdx && time.Since(a.lastClk) < 400*time.Millisecond
				p.cur = idx
				a.lastIdx, a.lastClk = idx, time.Now()
				if b == 2 {
					a.contextMenu()
				} else if dbl {
					a.lastIdx = -1
					a.openEntry()
				}
			}
		}
		return
	}
}

// ---------------------------------------------------------------- loop

func (a *app) tick(n int) {
	t := a.tab()
	for i, p := range t.panes {
		if p == nil || (i == 1 && !t.split) || p.kind != kDir {
			continue
		}
		fi, err := os.Stat(p.dir)
		if err != nil { // the folder is gone: climb to one that exists
			d := p.dir
			for !isDirPath(d) && d != "/" {
				d = filepath.Dir(d)
			}
			a.cd(p, d, false)
			a.say("folder disappeared; now in %s", tilde(d))
			continue
		}
		if !fi.ModTime().Equal(p.stamp) {
			a.load(p, "")
		}
	}
	if n%10 == 0 {
		a.refreshDevices()
	}
}

func usage() {
	fmt.Print(`muhfm — a terminal file manager in one file

usage: muhfm [options] [folder | file | file://uri]

  a file argument opens its folder with the file selected ("show in folder")

  --keys              print every key binding
  --pick              choose files: Enter prints the selection to stdout
  --choosefiles=F     like --pick, but write the selection to F
  --choosedir=F       on quit, write the current folder to F (cd on quit)
  --window            open in a new terminal when not started from one
  --set-default       install muhfm.desktop and make it the default for folders
  --desktop-file      print the desktop entry
  --version

config: ~/.config/muhfm/config   (key = value)
  hidden preview places details reverse dirs_first   true|false
  sort name|size|time|ext   icons none|nerd|emoji|ascii   sixel auto|yes|no
  terminal <cmd>   editor <cmd>
`)
}

func printKeys(w io.Writer) {
	a := &app{}
	sec := ""
	for _, b := range a.bindList() {
		if b.desc == "" {
			continue
		}
		if b.section != sec {
			sec = b.section
			fmt.Fprintf(w, "\n## %s\n", sec)
		}
		fmt.Fprintf(w, "  %-20s %s\n", prettyKeys(b.keys), b.desc)
	}
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "muhfm"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func desktopFile(self string) string {
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=muhfm
GenericName=File Manager
Comment=Browse and manage files
Exec=%s --window %%U
Icon=system-file-manager
Terminal=false
Categories=System;FileTools;FileManager;Utility;
MimeType=inode/directory;
Keywords=files;folder;explorer;directory;
StartupNotify=false
`, self)
}

func setDefault() error {
	dir := filepath.Join(dataHome(), "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f := filepath.Join(dir, "muhfm.desktop")
	if err := os.WriteFile(f, []byte(desktopFile(selfPath())), 0o644); err != nil {
		return err
	}
	if err := setDefaultApp("inode/directory", "muhfm.desktop"); err != nil {
		return err
	}
	if have("update-desktop-database") {
		exec.Command("update-desktop-database", dir).Run()
	}
	fmt.Printf("installed %s\nfolders now open in muhfm (%s)\n", f, filepath.Join(configHome(), "mimeapps.list"))
	return nil
}

func main() {
	var start string
	var pick, window bool
	var choose, chdir string
	args := os.Args[1:]
	var passthrough []string
	for i := 0; i < len(args); i++ {
		s := args[i]
		val := func() string {
			if _, v, ok := strings.Cut(s, "="); ok {
				return v
			}
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case s == "-h" || s == "--help":
			usage()
			return
		case s == "-v" || s == "--version":
			fmt.Println("muhfm", version)
			return
		case s == "--keys":
			printKeys(os.Stdout)
			return
		case s == "--desktop-file":
			fmt.Print(desktopFile(selfPath()))
			return
		case s == "--set-default":
			if err := setDefault(); err != nil {
				fmt.Fprintln(os.Stderr, "muhfm:", err)
				os.Exit(1)
			}
			return
		case s == "--window":
			window = true
			continue
		case s == "--pick":
			pick = true
		case strings.HasPrefix(s, "--choosefiles") || strings.HasPrefix(s, "--choosefile"):
			choose, pick = val(), true
		case strings.HasPrefix(s, "--choosedir"):
			chdir = val()
		case s == "--":
		case strings.HasPrefix(s, "-") && s != "-":
			fmt.Fprintf(os.Stderr, "muhfm: unknown option %s (try --help)\n", s)
			os.Exit(2)
		default:
			if start == "" {
				start = s
			}
		}
		passthrough = append(passthrough, args[i])
	}

	if window && !isatty(0) {
		cfg := loadConfig()
		all := append([]string{selfPath()}, passthrough...)
		if start != "" && !containsStr(passthrough, start) {
			all = append(all, start)
		}
		if err := spawn(home(), append([]string{"sh", "-c", cfg.terminal + ` -e "$@"`, "sh"}, all...)...); err != nil {
			fmt.Fprintln(os.Stderr, "muhfm:", err)
			os.Exit(1)
		}
		return
	}

	cwd, _ := os.Getwd()
	if cwd == "" {
		cwd = home()
	}
	sel := ""
	if start != "" {
		start = expand(start, cwd)
		if fi, err := os.Stat(start); err == nil && !fi.IsDir() {
			sel, start = start, filepath.Dir(start)
		} else if err != nil {
			for !isDirPath(start) && start != "/" {
				start = filepath.Dir(start)
			}
		}
	} else {
		start = cwd
	}

	t, err := openTerm()
	if err != nil {
		fmt.Fprintln(os.Stderr, "muhfm: needs a terminal:", err)
		os.Exit(1)
	}
	a := &app{t: t, cfg: loadConfig(), ev: make(chan func(), 512), pv: map[string]*preview{},
		lastIdx: -1, pick: pick, choose: choose, chdir: chdir}
	switch a.cfg.sixel {
	case "yes", "on", "true":
		a.sixOn = true
	case "auto":
		tn := os.Getenv("TERM")
		a.sixOn = strings.HasPrefix(tn, "foot") || strings.Contains(tn, "mlterm") ||
			os.Getenv("WEZTERM_EXECUTABLE") != "" || os.Getenv("MUHFM_SIXEL") == "1"
	}
	a.loadState()
	p := newPane(start)
	a.tabs = []*tab{{panes: [2]*pane{p, nil}}}
	a.load(p, sel)
	a.addRecent(start)
	a.buildPlaces()
	a.setupBinds()

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	t.raw()
	defer func() {
		if r := recover(); r != nil {
			t.restore()
			panic(r)
		}
	}()
	a.w, a.h = t.size()
	a.refreshDevices()
	in := make(chan []byte, 64)
	go t.readLoop(in)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	var rest []byte
	n := 0
	a.draw()
	for !a.quit {
		select {
		case b := <-in:
			ks, r := parseKeys(append(rest, b...))
			rest = r
			for _, k := range ks {
				a.handle(k)
				if a.quit {
					break
				}
			}
		case f := <-a.ev:
			f()
		drain:
			for {
				select {
				case f := <-a.ev:
					f()
				default:
					break drain
				}
			}
		case s := <-sigs:
			switch s {
			case syscall.SIGWINCH:
				a.w, a.h = t.size()
				a.prev, a.sixShown = nil, ""
			case syscall.SIGTERM, syscall.SIGHUP:
				a.quit = true
			}
		case <-tick.C:
			n++
			if len(rest) > 0 { // a lone, unfinished escape sequence
				ks, _ := parseKeys(rest[:1])
				for _, k := range ks {
					a.handle(k)
				}
				rest = rest[1:]
			}
			a.tick(n)
		}
		if !a.quit {
			a.draw()
		}
	}
	for _, j := range a.jobs {
		j.cancel()
	}
	a.saveState()
	t.restore()
	if a.chdir != "" {
		os.WriteFile(a.chdir, []byte(a.pane().dir+"\n"), 0o644)
	}
	if a.pick && len(a.picked) > 0 {
		out := strings.Join(a.picked, "\n") + "\n"
		if a.choose != "" {
			os.WriteFile(a.choose, []byte(out), 0o644)
		} else {
			os.Stdout.WriteString(out)
		}
	}
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
