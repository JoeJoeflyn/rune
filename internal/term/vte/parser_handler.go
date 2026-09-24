// Copyright (C) 2017-2026 The Rune Authors
// SPDX-License-Identifier: GPL-3.0-or-later
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at
// your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package vte

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/blue/logging"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/clipboard"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/browser"
	"unstable.build/rune/internal/term/vte/vteparser"
	"unstable.build/rune/internal/term/vte/vtescreen"
)

var _ vteparser.Handler = (*parserHandler)(nil)

const (
	pkgVersion          = 1
	selectionRegisterID = "srid"
)

type parserHandler struct {
	pty       workspaceapi.Pty
	clipboard clipboard.Register
	tm        browser.TabManager
	bell      func()
	sync      struct {
		mu      sync.Locker
		buf     screenBuffer
		primBuf *vtescreen.PrimaryBuffer
		altBuf  *vtescreen.AltBuffer
	}
	tabs              tabstops
	maxScrollLength   int
	minWidth          int
	useTitleAsTabname bool

	needsAttentionAttr        term.Attributes
	needsAttention            bool
	uri                       workspaceapi.URI
	width, height             int
	inFocus                   bool
	cursorStyle               term.CursorStyle
	cursorHidden              bool
	title                     string
	titles                    []string
	modeCursorKeys            bool
	modeInsert                bool
	modeOrigin                bool
	modeBlinkingCursor        bool
	modeLineFeedNewLine       bool
	modeShowCursor            bool
	modeReportMouseClicks     bool
	modeReportCellMouseMotion bool
	modeReportAllMouseMotion  bool
	modeReportFocusInOut      bool
	modeWrap                  bool
	modeUtf8Mouse             bool
	modeSgrMouse              bool
	modeAlternateScroll       bool
	modeUrgencyHints          bool
	modeBracketedPaste        bool
	modeSyncUpdate            bool

	shouldWrap     bool
	useAlt         bool
	usedAlt        bool
	currentCharset vteparser.CharsetIndex

	// clusterBuf is reused scratch for the mergeContinuation probe.
	clusterBuf []byte
	// glyphBuf is reused scratch for the batched non-ASCII write.
	glyphBuf []vtescreen.Glyph
}

// use a common api for alternate and primary buffers
// used to simplify critical path calls and avoid extra branches
type screenBuffer interface {
	SetCursorAtScreen(c term.Coordinates)
	CursorAtScreen() term.Coordinates
	CursorAtScroll() term.Coordinates
	Insert(c rune, width int, charset vteparser.CharsetIndex)
	Write(c rune, width int, charset vteparser.CharsetIndex)
	// WriteRun writes leading printable-ASCII bytes at the cursor in one
	// pass and reports how many were written; 0 demands the per-character
	// Write path. See AltBuffer.WriteRun.
	WriteRun(run []byte, charset vteparser.CharsetIndex) int
	// WriteGlyphRun writes decoded glyphs at the cursor in one pass and
	// reports how many were written; 0 demands the per-glyph Write path.
	// See AltBuffer.WriteGlyphRun and PrimaryBuffer.WriteGlyphRun.
	WriteGlyphRun(glyphs []vtescreen.Glyph, charset vteparser.CharsetIndex) int
	Delete(count int)
	ResetCells(start, end int)
	ResetLines(start, end int)
	BottomScrollableRegion() int
	TopScrollableRegion() int
	SetScrollableRegion(top, bottom int, end bool)
	Columns(line int) int
	Rows() int
	CellAt(pos term.Coordinates) *term.Cell
	PrevCellAtCursor() *term.Cell
	// AdvanceColumns reports how far the cursor moves after writing a
	// glyph of the given display width. The primary buffer tracks visual
	// columns and advances by the full width; the alternate buffer stores
	// one cell per grapheme and advances by one.
	AdvanceColumns(width int) int
	CursorAttributes() term.Attributes
	SetCursorAttributes(attr term.Attributes)
	SetHiddenCursor(hidden bool)
}

func newParserHandler(
	mu sync.Locker, pty workspaceapi.Pty,
	tm browser.TabManager,
	clipboard clipboard.Register,
	bell func(),
	uri workspaceapi.URI,
	needsAttentionAttr term.Attributes,
	useTitleAsTabname bool,
	maxScrollLength int,
	minWidth int,
) *parserHandler {
	ret := new(parserHandler)
	ret.init(mu, pty, tm, clipboard, bell, uri,
		needsAttentionAttr, useTitleAsTabname, maxScrollLength, minWidth)
	return ret
}

func (t *parserHandler) init(
	mu sync.Locker, pty workspaceapi.Pty,
	tm browser.TabManager,
	clipboard clipboard.Register,
	bell func(),
	uri workspaceapi.URI,
	needsAttentionAttr term.Attributes,
	useTitleAsTabname bool,
	maxScrollLength int,
	minWidth int,
) {
	t.maxScrollLength = maxScrollLength
	t.minWidth = minWidth
	t.sync.altBuf = vtescreen.NewAltBuffer()
	t.sync.primBuf = vtescreen.NewPrimaryBuffer(minWidth, maxScrollLength)
	t.sync.buf = t.sync.primBuf
	t.sync.mu = mu
	t.pty = pty
	t.clipboard = clipboard
	t.tm = tm
	t.uri = uri
	t.title = uri.Name()
	t.bell = bell
	t.needsAttentionAttr = needsAttentionAttr
	t.useTitleAsTabname = useTitleAsTabname

	// assume we are in focus when initialized
	t.inFocus = true
	t.modeAlternateScroll = true
	t.modeUrgencyHints = true
	t.modeShowCursor = true
	t.modeWrap = true
}

func (t *parserHandler) Resize(width, height int) {
	if width < 0 || height < 0 {
		return
	}

	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()
	t.resizeLocked(width, height)
}

// resizeLocked performs the parser-side resize work. Callers must
// hold t.sync.mu.
func (t *parserHandler) resizeLocked(width, height int) {
	if width < 0 || height < 0 {
		return
	}
	t.sync.primBuf.Resize(width, height)
	t.sync.altBuf.Resize(width, height)
	t.width = width
	t.height = height
	t.tabs.resize(width)
}

// OSC to set window title.
func (t *parserHandler) SetTitle(title string) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if !t.useTitleAsTabname {
		return
	}

	t.updateTabName(title)
}

// Set the cursor style.
func (t *parserHandler) SetCursorStyle(style vteparser.CursorStyle) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setCursorShape(style.Shape)
	if t.cursorHidden {
		return
	}

	if !style.Blinking {
		// already set by SetCursorShape
		return
	}

	switch style.Shape {
	case vteparser.CursorShapeBlock:
		t.cursorStyle = term.CursorStyleBlinkingBlock
	case vteparser.CursorShapeUnderline:
		t.cursorStyle = term.CursorStyleBlinkingUnderline
	case vteparser.CursorShapeBeam:
		t.cursorStyle = term.CursorStyleBlinkingBar
	case vteparser.CursorShapeHollowBlock:
		/* unsupported by tcell */
		t.cursorStyle = term.CursorStyleBlinkingBlock
	case vteparser.CursorShapeDefault:
		// DECSCUSR 0 with the (non-spec) blinking bit set: keep the
		// terminal default so the host can render the configured shape.
		t.cursorStyle = term.CursorStyleDefault
	}
}

// Set the cursor shape.
func (t *parserHandler) SetCursorShape(shape vteparser.CursorShape) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setCursorShape(shape)
}

// A character to be displayed.
func (t *parserHandler) Input(c rune) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.input(c)
}

func (t *parserHandler) input(c rune) {
	width := runeWidth(c)

	// Every grapheme continuation codepoint sorts above 0x0300.
	if c >= 0x0300 && !t.shouldWrap && t.mergeContinuation(c, width) {
		return
	}

	if t.shouldWrap {
		t.wrapLine()
	}

	advance := t.sync.buf.AdvanceColumns(width)

	// A wide glyph must not straddle the right margin: wrap first so it
	// lands whole on the next row. Only the primary buffer advances >1.
	if advance > 1 && !t.modeInsert &&
		t.sync.buf.CursorAtScreen().X+advance > t.width {
		t.wrapLine()
	}

	if t.modeInsert {
		t.sync.buf.Insert(c, width, t.currentCharset)
	} else {
		t.sync.buf.Write(c, width, t.currentCharset)
	}

	pos := t.sync.buf.CursorAtScreen()
	pos.X += advance

	if pos.X < t.width {
		t.setCursorAtScreen(pos)
	} else {
		// implementations that use DECAWM usually expect the next call to Input
		// to push the cursor down to the next line; i.e. there could be
		// carriageReturn or other commands that could be send before the next
		// call to Input if program wanted to manage the wrap around process manually.
		t.shouldWrap = true
	}
}

// InputRun displays a run of printable UTF-8 text, preserving Input's
// per-codepoint semantics.
func (t *parserHandler) InputRun(run []byte) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	for len(run) > 0 {
		if run[0] >= utf8.RuneSelf {
			run = run[t.inputGlyphRun(run):]
			continue
		}
		run = run[t.inputASCIIRun(run):]
	}
}

// inputGlyphRun writes the leading non-ASCII codepoints of run, which
// must start with one, and reports how many bytes it consumed. Only
// codepoints that cannot continue a grapheme cluster and that fit whole
// before the right margin join the batch; anything else ends it so the
// per-codepoint path keeps its semantics.
func (t *parserHandler) inputGlyphRun(run []byte) int {
	if t.shouldWrap || t.modeInsert {
		return t.inputFirstRune(run)
	}
	buf := t.sync.buf
	room := t.width - buf.CursorAtScreen().X
	glyphs := t.glyphBuf[:0]
	size, columns, last := 0, 0, 0
	for size < len(run) && run[size] >= utf8.RuneSelf {
		c, n := utf8.DecodeRune(run[size:])
		if c == utf8.RuneError && n <= 1 {
			break
		}
		width := runeWidth(c)
		if mayContinueAnyCluster(c, width) {
			break
		}
		last = buf.AdvanceColumns(width)
		if last > room-columns {
			break
		}
		glyphs = append(glyphs, vtescreen.Glyph{Ch: c, Width: uint8(width)})
		columns += last
		size += n
	}
	t.glyphBuf = glyphs

	if len(glyphs) == 0 || buf.WriteGlyphRun(glyphs, t.currentCharset) == 0 {
		return t.inputFirstRune(run)
	}

	pos := buf.CursorAtScreen()
	pos.X += columns
	if pos.X >= t.width {
		// input leaves the cursor on the glyph that filled the margin
		// so wrapLine still finds the marked cell.
		pos.X -= last
		t.shouldWrap = true
	}
	t.setCursorAtScreen(pos)
	return size
}

func (t *parserHandler) inputFirstRune(run []byte) int {
	c, n := utf8.DecodeRune(run)
	t.input(c)
	return n
}

// inputASCIIRun writes the leading ASCII bytes of run, which must start
// with one, and reports how many bytes it consumed.
func (t *parserHandler) inputASCIIRun(run []byte) int {
	if t.shouldWrap {
		t.wrapLine()
	}
	if t.modeInsert {
		t.input(rune(run[0]))
		return 1
	}
	buf := t.sync.buf
	pos := buf.CursorAtScreen()
	n := min(len(run), t.width-pos.X)
	if n > 0 {
		n = asciiRunLen(run[:n])
	}
	if n <= 0 {
		t.input(rune(run[0]))
		return 1
	}
	w := buf.WriteRun(run[:n], t.currentCharset)
	if w == 0 {
		t.input(rune(run[0]))
		return 1
	}
	pos.X += w
	if pos.X < t.width {
		t.setCursorAtScreen(pos)
	} else {
		// wrapLine relies on the cursor sitting on the marked cell.
		pos.X = t.width - 1
		t.setCursorAtScreen(pos)
		t.shouldWrap = true
	}
	return w
}

// asciiRunLen returns the length of run's leading ASCII bytes.
func asciiRunLen(run []byte) int {
	// The mask is identical in every byte position, so byte order is
	// irrelevant and a native read avoids a bswap on big-endian.
	const highBits = 0x8080808080808080
	i := 0
	for ; i+8 <= len(run); i += 8 {
		if binary.NativeEndian.Uint64(run[i:])&highBits != 0 {
			break
		}
	}
	for ; i < len(run); i++ {
		if run[i] >= utf8.RuneSelf {
			break
		}
	}
	return i
}

// mergeContinuation folds c into the preceding cell when c extends that
// cell's grapheme cluster (base+c still steps as a single cluster),
// leaving the cursor in place. It reports whether the merge happened;
// callers fall back to a normal write when it did not.
func (t *parserHandler) mergeContinuation(c rune, width int) bool {
	if !mayContinueAnyCluster(c, width) {
		return false
	}
	prev := t.sync.buf.PrevCellAtCursor()
	if prev == nil || prev.Ch == 0 || prev.Ch == '\n' {
		return false
	}
	combining := prev.CombiningRunes()
	last := prev.Ch
	if n := len(combining); n > 0 {
		last = combining[n-1]
	}
	if !mayExtendCluster(last, c, width) {
		return false
	}
	buf := utf8.AppendRune(t.clusterBuf[:0], prev.Ch)
	for _, r := range combining {
		buf = utf8.AppendRune(buf, r)
	}
	buf = utf8.AppendRune(buf, c)
	t.clusterBuf = buf
	if _, rest, _, _ := uniseg.FirstGraphemeCluster(buf, -1); len(rest) != 0 {
		return false
	}
	prev.SetCombining(append(combining, c))
	return true
}

// mayContinueAnyCluster is the half of mayExtendCluster that depends
// only on c, so it can run before the costly lookup of the preceding
// cell. isSpacingMark searches a Unicode table, so it goes last.
func mayContinueAnyCluster(c rune, width int) bool {
	return width == 0 || c == '\n' || isEmojiModifier(c) ||
		isRegionalIndicator(c) || isHangul(c) || maybePictographic(c) ||
		isSpacingMark(c)
}

// mayExtendCluster reports whether c could continue a grapheme cluster
// whose last codepoint is last. It must never reject a pair that the
// segmentation probe it gates would merge.
func mayExtendCluster(last, c rune, width int) bool {
	if width == 0 || isEmojiModifier(c) || isSpacingMark(c) {
		return true
	}
	switch {
	case last == zeroWidthJoiner: // GB11
		return true
	case last == '\r': // GB3
		return c == '\n'
	case isRegionalIndicator(last): // GB12/GB13
		return isRegionalIndicator(c)
	case isHangul(last): // GB6/GB7/GB8
		return isHangul(c)
	}
	return false
}

const zeroWidthJoiner = 0x200D

// maybePictographic is a coarse superset of Extended_Pictographic; it
// only guards GB11, which also requires a preceding ZWJ.
func maybePictographic(c rune) bool {
	switch {
	case c == 0x00A9 || c == 0x00AE:
		return true
	case c >= 0x2000 && c <= 0x33FF:
		return true
	case c >= 0x1F000 && c <= 0x1FFFD:
		return true
	}
	return false
}

// isEmojiModifier reports whether c is a skin-tone modifier, the only
// non-zero-width codepoint range with Grapheme_Cluster_Break=Extend.
func isEmojiModifier(c rune) bool { return c >= 0x1F3FB && c <= 0x1F3FF }

// isSpacingMark covers Grapheme_Cluster_Break=SpacingMark, which is the
// spacing combining marks plus the two Thai/Lao vowel signs that Unicode
// classifies as letters.
func isSpacingMark(c rune) bool {
	return c == 0x0E33 || c == 0x0EB3 || unicode.Is(unicode.Mc, c)
}

func isRegionalIndicator(c rune) bool { return c >= 0x1F1E6 && c <= 0x1F1FF }

// isHangul covers every codepoint with a Hangul
// Grapheme_Cluster_Break value.
func isHangul(c rune) bool {
	return (c >= 0x1100 && c <= 0x11FF) ||
		(c >= 0xA960 && c <= 0xA97F) ||
		(c >= 0xAC00 && c <= 0xD7FF)
}

// Set cursor to position.
func (t *parserHandler) Goto(line int, col int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()
	t.goTo(line, col)
}

// Set cursor to specific row.
func (t *parserHandler) GotoLine(line int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.goTo(line, t.sync.buf.CursorAtScreen().X)
}

// Set cursor to specific column.
func (t *parserHandler) GotoCol(col int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setColumn(col)
}

// Insert blank characters in current line starting from cursor.
func (t *parserHandler) InsertBlank(count int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pos := t.sync.buf.CursorAtScreen()
	width := t.width
	count = int(math.Min(float64(count), float64(width-pos.X)))

	for i := 0; i < count; i++ {
		t.sync.buf.Insert(' ', 1, t.currentCharset)
	}
}

// Move cursor up `rows`.
func (t *parserHandler) MoveUp(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.moveRows(-rows)
}

// Move cursor down `rows`.
func (t *parserHandler) MoveDown(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.moveRows(rows)
}

// IdentifyTerminal identifies the terminal implementatino.
func (t *parserHandler) IdentifyTerminal(secondary bool) {
	var err error
	if secondary {
		_, err = fmt.Fprintf(t.pty.Master, "\x1b[>0;%d;1c", pkgVersion)
	} else {
		_, err = t.pty.Master.Write([]byte("\x1b[?6c"))
	}
	if err != nil {
		t.log(log.ErrorLevel, "identify terminal: write to master: %v", err)
	}
}

// Report device status.
func (t *parserHandler) DeviceStatus(status int) {
	var err error
	switch status {
	case 5:
		_, err = t.pty.Master.Write([]byte("\x1b[0n"))
	case 6:
		t.sync.mu.Lock()
		pos := t.sync.buf.CursorAtScreen()
		if t.modeOrigin {
			pos.Y -= min(pos.Y, t.sync.buf.TopScrollableRegion())
		}
		t.sync.mu.Unlock()
		text := fmt.Sprintf("\x1b[%d;%dR", pos.Y+1, pos.X+1)
		_, err = t.pty.Master.Write([]byte(text))
	default:
		t.log(log.WarnLevel, "unknown device status query: %d", status)
	}
	if err != nil {
		t.log(log.ErrorLevel, "device status: write to master: %v", err)
	}
}

// Move cursor forward `cols`.
func (t *parserHandler) MoveForward(cols int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setColumn(t.sync.buf.CursorAtScreen().X + cols)
}

// Move cursor backward `cols`.
func (t *parserHandler) MoveBackward(cols int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setColumn(t.sync.buf.CursorAtScreen().X - cols)
}

// Move cursor down `rows` and set to column 1.
func (t *parserHandler) MoveDownAndCR(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.moveRows(rows)
	t.setColumn(0)
}

// Move cursor up `rows` and set to column 1.
func (t *parserHandler) MoveUpAndCR(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.moveRows(-rows)
	t.setColumn(0)
}

// Put a tab.
func (t *parserHandler) PutTab() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if t.shouldWrap {
		t.wrapLine()
		return
	}

	pos := t.sync.buf.CursorAtScroll()
	if pos.X+1 >= t.width {
		return
	}

	var cursor vtescreen.CursorState
	if t.useAlt {
		cursor = t.sync.altBuf.Cursor()
	} else {
		cursor = t.sync.primBuf.Cursor()
	}
	c := '\t'
	if charset, ok := cursor.Charsets[t.currentCharset]; ok {
		c = charset.Map(c)
	}

	// overwrite cell at current position, if it's an empty cell
	cell := t.sync.buf.CellAt(pos)
	if cell != nil && cell.Ch == vtescreen.DefaultChar {
		cell.Ch = c
	}

	// move cursor until next tab stop
	for {
		pos := t.sync.buf.CursorAtScreen()
		pos.X++
		if pos.X == t.width {
			break
		}

		t.sync.buf.SetCursorAtScreen(pos)

		if t.tabs.get(pos.X) {
			break
		}
	}
}

// Backspace `count` characters.
func (t *parserHandler) Backspace() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	x := t.sync.buf.CursorAtScreen().X
	if x <= 0 {
		return
	}
	t.setColumn(x - 1)
}

// Carriage return.
func (t *parserHandler) CarriageReturn() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.carriageReturn()
	t.shouldWrap = false
}

// Linefeed
func (t *parserHandler) Linefeed() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.index()
}

// Ring the bell.
func (t *parserHandler) Bell() {
	t.sync.mu.Lock()
	if !t.inFocus && t.modeUrgencyHints {
		t.setNeedsAttention()
	}
	t.sync.mu.Unlock()

	// bell is caller-supplied (Config.scheduleBell in production,
	// which forwards to ScheduleNextTick) and must run without
	// holding the lock: callers commonly implement ScheduleNextTick
	// as a synchronous call under their own lock to model a
	// single-threaded event loop, and calling back into that lock
	// while still holding t.sync.mu would invert lock order against
	// any other parserHandler method invoked while the caller's lock
	// is held (e.g. Draw/SetTitle).
	t.bell()
}

// Substitute char under the cursor.
func (t *parserHandler) Substitute() {
	t.log(log.WarnLevel, "unimplemented substitute")
}

// Set current position as a tabstop.
func (t *parserHandler) SetHorizontalTabstop() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.tabs.set(t.sync.buf.CursorAtScroll().X, true)
}

// Clear tab stops.
func (t *parserHandler) ClearTabs(mode vteparser.TabulationClearMode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	switch mode {
	case vteparser.TabulationClearModeCurrent:
		t.tabs.set(t.sync.buf.CursorAtScroll().X, false)
	case vteparser.TabulationClearModeAll:
		t.tabs.clearAll()
	default:
		t.log(log.WarnLevel, "unknown clear tabs mode: %v", mode)
	}
}

// Scroll up `rows` rows.
func (t *parserHandler) ScrollUp(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.scrollUp(rows)
}

// Scroll down `rows` rows.
func (t *parserHandler) ScrollDown(rows int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.scrollDown(rows)
}

// Insert `count` blank lines.
func (t *parserHandler) InsertBlankLines(count int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	start := t.sync.buf.CursorAtScreen().Y
	if start >= t.sync.buf.TopScrollableRegion() && start < t.sync.buf.BottomScrollableRegion() {
		if t.useAlt {
			t.scrollDownAltRelative(start, count)
		} else if bottom := t.sync.primBuf.BottomScrollableRegion(); bottom < t.height {
			// Inside a region the lines pushed past the bottom margin
			// are dropped; outside one the primary buffer keeps them by
			// growing history instead.
			screenTop := t.sync.primBuf.Rows() - t.height
			t.sync.primBuf.ScrollDown(screenTop+start, screenTop+bottom, min(count, bottom-start))
		} else {
			t.sync.primBuf.InsertLinesCursor(count)
		}
	}
}

// Delete `count` lines.
func (t *parserHandler) DeleteLines(count int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if count <= 0 {
		return
	}

	start := t.sync.buf.CursorAtScreen().Y
	if start >= t.sync.buf.TopScrollableRegion() &&
		start < t.sync.buf.BottomScrollableRegion() {
		count = min(count, t.height-start)
		if t.useAlt {
			t.scrollUpAltRelative(start, count)
		} else if bottom := t.sync.primBuf.BottomScrollableRegion(); bottom < t.height {
			screenTop := t.sync.primBuf.Rows() - t.height
			t.sync.primBuf.ScrollUp(screenTop+start, screenTop+bottom, min(count, bottom-start))
		} else {
			end := start + count - 1
			t.sync.primBuf.ScrollUp(start, end, count)
		}
	}
}

// Erase `count` chars in the current line following the cursor.
//
// Erase means resetting to the default state (default colors, no content,
// no mode flags).
func (t *parserHandler) EraseChars(count int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pos := t.sync.buf.CursorAtScroll()
	start := pos.X
	end := min(start+count, t.endOfLine(pos.Y))

	t.sync.buf.ResetCells(start, end)
}

// Delete `count` chars.
//
// Deleting a character is like the delete key on the keyboard - everything
// to the right of the deleted things is shifted left.
func (t *parserHandler) DeleteChars(count int) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.sync.buf.Delete(count)
}

// Move backward `count` tabs.
func (t *parserHandler) MoveBackwardTabs(count int) {
	t.log(log.WarnLevel, "unimplemented move backward %d tabs", count)
}

// Move forward `count` tabs.
func (t *parserHandler) MoveForwardTabs(count int) {
	t.log(log.WarnLevel, "unimplemented move forward %d tabs", count)
}

// Save the current cursor position.
func (t *parserHandler) SaveCursorPosition() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if t.useAlt {
		t.sync.altBuf.SaveCursor()
	} else {
		t.sync.primBuf.SaveCursor()
	}
}

// Restore cursor position.
func (t *parserHandler) RestoreCursorPosition() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if t.useAlt {
		t.sync.altBuf.RestoreCursor()
	} else {
		t.sync.primBuf.RestoreCursor()
	}
}

// Clear the current line.
func (t *parserHandler) ClearLine(mode vteparser.LineClearMode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pos := t.sync.buf.CursorAtScroll()
	var start, end int
	switch mode {
	case vteparser.LineClearModeRight:
		if t.shouldWrap {
			return
		}
		start = pos.X
		end = t.endOfLine(pos.Y)
	case vteparser.LineClearModeLeft:
		end = pos.X + 1
	case vteparser.LineClearModeAll:
		end = t.endOfLine(pos.Y)
	default:
		t.log(log.WarnLevel, "unknown clear line mode: %v", mode)
		return
	}

	t.sync.buf.ResetCells(start, end)
}

// Clear the screen.
func (t *parserHandler) ClearScreen(mode vteparser.ClearMode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pos := t.sync.buf.CursorAtScroll()
	columns := t.endOfLine(pos.Y)
	lines := t.maxRows()

	switch mode {
	case vteparser.ClearModeBelow:
		t.sync.buf.ResetCells(pos.X, columns)
		if pos.Y+1 < lines {
			t.sync.buf.ResetLines(pos.Y+1, lines)
		}

	case vteparser.ClearModeAbove:
		if pos.Y > 1 {
			t.sync.buf.ResetLines(0, pos.Y)
		}
		start := 0
		end := min(pos.X+1, columns)
		t.sync.buf.ResetCells(start, end)

	case vteparser.ClearModeAll:
		if t.useAlt {
			t.resetBufLines(t.sync.buf)
		} else {
			t.sync.primBuf.Clear()
		}

	case vteparser.ClearModeSaved:
		if !t.useAlt {
			t.sync.primBuf.ClearHistory()
		}
	default:
		t.log(log.WarnLevel, "unknown clear screen mode: %v", mode)
	}
}

// Reset parserHandler state.
func (t *parserHandler) ResetState() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pty := t.pty
	clipboard := t.clipboard
	tm := t.tm
	uri := t.uri
	needsAttentionAttr := t.needsAttentionAttr
	bell := t.bell
	useTitleAsTabname := t.useTitleAsTabname
	maxScrollLength := t.maxScrollLength
	minWidth := t.minWidth
	mu := t.sync.mu
	width := t.width
	height := t.height
	*t = parserHandler{}
	t.init(mu, pty, tm, clipboard, bell, uri,
		needsAttentionAttr, useTitleAsTabname, maxScrollLength, minWidth)

	// resize
	t.sync.altBuf.Resize(width, height)
	t.sync.primBuf.Resize(width, height)
	t.width = width
	t.height = height
	t.tabs.resize(width)
}

// Reverse Index.
//
// Move the active position to the same horizontal position on the
// preceding line. If the active position is at the top margin, a scroll
// down is performed; above it the cursor stops at the first line
// (kitty screen_reverse_index, screen.c:2435).
func (t *parserHandler) ReverseIndex() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	pos := t.sync.buf.CursorAtScreen()
	switch {
	case pos.Y == t.sync.buf.TopScrollableRegion():
		t.scrollDown(1)
	case pos.Y > 0:
		pos.Y--
		t.setCursorAtScreen(pos)
	}
	t.shouldWrap = false
}

// Set a parserHandler attribute.
func (t *parserHandler) TerminalAttribute(pattr vteparser.Attr) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	attr := t.sync.buf.CursorAttributes()

	switch pattr.Type {
	case vteparser.ResetAttr:
		attr.Fg = 0
		attr.Bg = 0
		attr.Attrs = 0
	case vteparser.BoldAttr:
		attr.Attrs |= term.AttrBold
	case vteparser.DimAttr:
		attr.Attrs |= term.AttrDim
	case vteparser.ItalicAttr:
		attr.Attrs |= term.AttrItalic
	case vteparser.UnderlineAttr:
		attr.Attrs |= term.AttrUnderline
	case vteparser.BlinkSlowAttr, vteparser.BlinkFastAttr:
		attr.Attrs |= term.AttrBlink
	case vteparser.ReverseAttr:
		attr.Attrs |= term.AttrReverse
	case vteparser.HiddenAttr:
		t.sync.buf.SetHiddenCursor(true)
		return
	case vteparser.StrikeAttr:
		attr.Attrs |= term.AttrStrikeThrough
	case vteparser.CancelBoldAttr:
		attr.Attrs &^= term.AttrBold
	case vteparser.CancelBoldDimAttr:
		attr.Attrs &^= term.AttrBold
		attr.Attrs &^= term.AttrDim
	case vteparser.CancelItalicAttr:
		attr.Attrs &^= term.AttrItalic
	case vteparser.CancelUnderlineAttr:
		attr.Attrs &^= term.AttrUnderline
	case vteparser.CancelBlinkAttr:
		attr.Attrs &^= term.AttrBlink
	case vteparser.CancelReverseAttr:
		attr.Attrs &^= term.AttrReverse
	case vteparser.CancelHiddenAttr:
		t.sync.buf.SetHiddenCursor(false)
		return
	case vteparser.CancelStrikeAttr:
		attr.Attrs &^= term.AttrStrikeThrough
	case vteparser.ForegroundAttr:
		attr.Fg = pattr.Color
	case vteparser.BackgroundAttr:
		attr.Bg = pattr.Color
	case vteparser.DoubleUnderlineAttr, vteparser.UndercurlAttr,
		vteparser.DottedUnderlineAttr, vteparser.DashedUnderlineAttr,
		vteparser.UnderlineColorAttr:
		/* ignored */
		return
	}

	t.sync.buf.SetCursorAttributes(attr)
}

// Report private mode
func (t *parserHandler) ReportPrivateMode(mode vteparser.PrivateMode) {
	// no need to sync since writes only occur on pty parsing goroutine

	var modeVar *bool
	switch mode {
	case vteparser.PrivateModeCursorKeys:
		modeVar = &t.modeCursorKeys
	case vteparser.PrivateModeColumnMode:
		/* not supported for reporting */
	case vteparser.PrivateModeOrigin:
		modeVar = &t.modeOrigin
	case vteparser.PrivateModeScreen:
		/* DECSCNM not supported */
	case vteparser.PrivateModeLineWrap:
		modeVar = &t.modeWrap
	case vteparser.PrivateModeBlinkingCursor:
		modeVar = &t.modeBlinkingCursor
	case vteparser.PrivateModeShowCursor:
		modeVar = &t.modeShowCursor
	case vteparser.PrivateModeReportMouseClicks:
		modeVar = &t.modeReportMouseClicks
	case vteparser.PrivateModeReportCellMouseMotion:
		modeVar = &t.modeReportCellMouseMotion
	case vteparser.PrivateModeReportAllMouseMotion:
		modeVar = &t.modeReportAllMouseMotion
	case vteparser.PrivateModeReportFocusInOut:
		modeVar = &t.modeReportFocusInOut
	case vteparser.PrivateModeUtf8Mouse:
		modeVar = &t.modeUtf8Mouse
	case vteparser.PrivateModeSgrMouse:
		modeVar = &t.modeSgrMouse
	case vteparser.PrivateModeAlternateScroll:
		modeVar = &t.modeAlternateScroll
	case vteparser.PrivateModeUrgencyHints:
		modeVar = &t.modeUrgencyHints
	case vteparser.PrivateModeSwapScreenAndSetRestoreCursor:
		modeVar = &t.useAlt
	case vteparser.PrivateModeBracketedPaste:
		modeVar = &t.modeBracketedPaste
	case vteparser.PrivateModeSyncUpdate:
		modeVar = &t.modeSyncUpdate
	default:
		t.log(log.WarnLevel, "Set unkown private mode: %v", mode)
	}
	t.reportMode("\x1b[?%d;%d$y", int(mode), modeVar)
}

// Set private mode.
func (t *parserHandler) SetPrivateMode(mode vteparser.PrivateMode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	switch mode {
	case vteparser.PrivateModeCursorKeys:
		t.modeCursorKeys = true
	case vteparser.PrivateModeColumnMode:
		t.deccolm()
	case vteparser.PrivateModeOrigin:
		t.modeOrigin = true
		t.goTo(0, 0)
	case vteparser.PrivateModeScreen:
		/* DECSCNM not supported */
	case vteparser.PrivateModeLineWrap:
		t.modeWrap = true
	case vteparser.PrivateModeBlinkingCursor:
		t.modeBlinkingCursor = true
	case vteparser.PrivateModeShowCursor:
		t.modeShowCursor = true
	case vteparser.PrivateModeReportMouseClicks:
		t.modeReportMouseClicks = true
	case vteparser.PrivateModeReportCellMouseMotion:
		t.modeReportCellMouseMotion = true
	case vteparser.PrivateModeReportAllMouseMotion:
		t.modeReportAllMouseMotion = true
	case vteparser.PrivateModeReportFocusInOut:
		t.modeReportFocusInOut = true
	case vteparser.PrivateModeUtf8Mouse:
		t.modeUtf8Mouse = true
	case vteparser.PrivateModeSgrMouse:
		t.modeSgrMouse = true
	case vteparser.PrivateModeAlternateScroll:
		t.modeAlternateScroll = true
	case vteparser.PrivateModeUrgencyHints:
		t.modeUrgencyHints = true
	case vteparser.PrivateModeSwapScreenAndSetRestoreCursor:
		if !t.useAlt {
			t.swapAlt()
		}
	case vteparser.PrivateModeBracketedPaste:
		t.modeBracketedPaste = true
	case vteparser.PrivateModeSyncUpdate:
		t.modeSyncUpdate = true
	default:
		t.log(log.WarnLevel, "Set unkown private mode: %v", mode)
		return
	}
}

// Unset private mode.
func (t *parserHandler) UnsetPrivateMode(mode vteparser.PrivateMode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	switch mode {
	case vteparser.PrivateModeCursorKeys:
		t.modeCursorKeys = false
	case vteparser.PrivateModeColumnMode:
		t.deccolm()
	case vteparser.PrivateModeOrigin:
		t.modeOrigin = false
		t.goTo(0, 0)
	case vteparser.PrivateModeScreen:
		/* DECSCNM not supported */
	case vteparser.PrivateModeLineWrap:
		t.modeWrap = false
	case vteparser.PrivateModeBlinkingCursor:
		t.modeBlinkingCursor = false
	case vteparser.PrivateModeShowCursor:
		t.modeShowCursor = false
	case vteparser.PrivateModeReportMouseClicks:
		t.modeReportMouseClicks = false
	case vteparser.PrivateModeReportCellMouseMotion:
		t.modeReportCellMouseMotion = false
	case vteparser.PrivateModeReportAllMouseMotion:
		t.modeReportAllMouseMotion = false
	case vteparser.PrivateModeReportFocusInOut:
		t.modeReportFocusInOut = false
	case vteparser.PrivateModeUtf8Mouse:
		t.modeUtf8Mouse = false
	case vteparser.PrivateModeSgrMouse:
		t.modeSgrMouse = false
	case vteparser.PrivateModeAlternateScroll:
		t.modeAlternateScroll = false
	case vteparser.PrivateModeUrgencyHints:
		t.modeUrgencyHints = false
	case vteparser.PrivateModeSwapScreenAndSetRestoreCursor:
		if t.useAlt {
			t.swapAlt()
		}
	case vteparser.PrivateModeBracketedPaste:
		t.modeBracketedPaste = false
	case vteparser.PrivateModeSyncUpdate:
		t.modeSyncUpdate = false
	default:
		t.log(log.WarnLevel, "Unset unkown private mode: %v", mode)
		return
	}
}

// Report mode
func (t *parserHandler) ReportMode(mode vteparser.Mode) {
	// no need to sync since writes only occur on pty parsing goroutine

	var modeVar *bool
	switch mode {
	case vteparser.ModeInsert:
		modeVar = &t.modeInsert
	case vteparser.ModeLineFeedNewLine:
		modeVar = &t.modeLineFeedNewLine
	default:
		t.log(log.DebugLevel, "Report unkown public mode: %v", mode)
	}

	t.reportMode("\x1b[%d;%d$y", int(mode), modeVar)
}

// Set mode.
func (t *parserHandler) SetMode(mode vteparser.Mode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	switch mode {
	case vteparser.ModeInsert:
		t.modeInsert = true
	case vteparser.ModeLineFeedNewLine:
		t.modeLineFeedNewLine = true
	default:
		t.log(log.WarnLevel, "Set unkown public mode: %v", mode)
		return
	}
}

// Unset mode.
func (t *parserHandler) UnsetMode(mode vteparser.Mode) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	switch mode {
	case vteparser.ModeInsert:
		t.modeInsert = false
	case vteparser.ModeLineFeedNewLine:
		t.modeLineFeedNewLine = false
	default:
		t.log(log.WarnLevel, "Unset unkown public mode: %v", mode)
		return
	}
}

// DECSTBM - Set the parserHandler scrolling region.
func (t *parserHandler) SetScrollingRegion(top, bottom int, end bool) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.setScrollingRegion(top, bottom, end)
}

// DECKPAM - Set the keypad to applications mode (ESCape instead of digits).
func (t *parserHandler) SetKeypadApplicationMode() {
	t.log(log.DebugLevel, "unsupported call to SetKeypadApplicationMode")
}

// DECKPNM - Set the keypad to numeric mode (digits instead of ESCape seq).
func (t *parserHandler) UnsetKeypadApplicationMode() {
	t.log(log.DebugLevel, "unsupported call to UnsetKeypadApplicationMode")
}

// Set one of the graphic character sets, G0 to G3, as the active charset.
//
// 'Invoke' one of G0 to G3 in the GL area. Also referred to as shift in,
// shift out and locking shift depending on the set being activated.
func (t *parserHandler) SetActiveCharset(index vteparser.CharsetIndex) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.currentCharset = index
}

// Assign a graphic character set to G0, G1, G2 or G3.
//
// 'Designate' a graphic character set as one of G0 to G3 so that it can
// later be 'invoked' by `SetActiveCharset`.
func (t *parserHandler) ConfigureCharset(
	index vteparser.CharsetIndex, charset vteparser.StandardCharset,
) {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	var buf *vtescreen.AltBuffer
	if t.useAlt {
		buf = t.sync.altBuf
	} else {
		buf = &t.sync.primBuf.AltBuffer
	}
	buf.ConfigureCharset(index, charset)
}

// Store data into the clipboard.
func (t *parserHandler) ClipboardStore(register int, data []byte) {
	d := base64.NewDecoder(base64.StdEncoding, bytes.NewReader(data))
	decodedData, err := io.ReadAll(d)
	if err != nil {
		t.log(log.WarnLevel, "decode base64 data "+
			"for ClipboardStore (len=%d): %v", len(data), err)
		return
	}

	clipData := clipboard.Data{Text: string(decodedData)}
	switch register {
	case int('c'):
		err = t.clipboard.Copy(clipboard.DefaultRegisterID, clipData)
	case int('p') | int('s'):
		err = t.clipboard.Copy(selectionRegisterID, clipData)
	default:
		t.log(log.WarnLevel, "unknown register ID upon ClipboardStore: %c", rune(register))
	}
	if err != nil {
		t.log(log.ErrorLevel, "copy to clipboard: %v", err)
	}
}

// Load data from the clipboard.
func (t *parserHandler) ClipboardLoad(register int, terminator string) {
	var data clipboard.Data
	var err error
	switch register {
	case int('c'):
		data, err = t.clipboard.Paste(clipboard.DefaultRegisterID)
	case int('p') | int('s'):
		data, err = t.clipboard.Paste(selectionRegisterID)
	default:
		t.log(log.WarnLevel, "unknown register ID upon ClipboardLoad: %c", rune(register))
		return
	}
	if err != nil {
		t.log(log.ErrorLevel, "load data from clipboard "+
			"for ClipboardLoad: %v", err)
		t.sync.mu.Unlock()
		return
	}

	var buf bytes.Buffer
	w := base64.NewEncoder(base64.StdEncoding, &buf)
	if _, err := w.Write([]byte(data.Text)); err != nil {
		t.log(log.WarnLevel, "encode data in base64 "+
			"for ClipboardLoad: %v", err)
		return
	}
	if err := w.Close(); err != nil {
		t.log(log.WarnLevel, "encode data in base64 "+
			"for ClipboardLoad: %v", err)
		return
	}

	encodedCmd := fmt.Sprintf("\x1b]52;%c;%s%s", rune(register), buf.String(), terminator)
	if _, err := t.pty.Master.Write([]byte(encodedCmd)); err != nil {
		t.log(log.WarnLevel, "write encoded clipboard data to pty "+
			"for ClipboardLoad: %v", err)
		return
	}
}

// Run the decaln routine.
func (t *parserHandler) Decaln() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	// keep the screenBuffer ifc small and Decaln
	// is not critical path
	var buf *vtescreen.AltBuffer
	if t.useAlt {
		buf = t.sync.altBuf
	} else {
		buf = &t.sync.primBuf.AltBuffer
	}
	rows := buf.Cells.Rows()
	buf.ResetLinesWith(max(0, rows-t.height), rows, 'E')
	t.resetScrollingRegion()
}

// Push a title onto the stack.
func (t *parserHandler) PushTitle() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	t.titles = append(t.titles, t.title)
}

// Pop the last title from the stack.
func (t *parserHandler) PopTitle() {
	t.sync.mu.Lock()
	defer t.sync.mu.Unlock()

	if len(t.titles) > 0 {
		e := t.titles[len(t.titles)-1]
		t.titles = t.titles[:len(t.titles)-1]
		if !t.useTitleAsTabname {
			return
		}
		t.updateTabName(e)
	}
}

// Report text area size in pixels.
func (t *parserHandler) TextAreaSizePixels() {
	t.log(log.DebugLevel, "unsupported call to TextAreaSizePixels")
}

// Report text area size in characters.
func (t *parserHandler) TextAreaSizeChars() {
	t.sync.mu.Lock()
	data := fmt.Sprintf("\x1b[8;%d;%dt", t.height, t.width)
	t.sync.mu.Unlock()

	if _, err := t.pty.Master.Write([]byte(data)); err != nil {
		t.log(log.WarnLevel, "write text area size in chars: %v", err)
		return
	}
}

// Set hyperlink.
func (t *parserHandler) SetHyperlink(link *vteparser.Hyperlink) {
	t.log(log.DebugLevel, "unsupported call to SetHyperlink")
}

// ReportKeyboardMode reports current keyboard mode.
func (t *parserHandler) ReportKeyboardMode() {
	t.log(log.DebugLevel, "unsupported call to ReportKeyboardMode")
}

// PushKeyboardMode pushes the keyboard mode into the keyboard mode stack.
func (t *parserHandler) PushKeyboardMode(mode vteparser.KeyboardMode) {
	t.log(log.DebugLevel, "unsupported call to PushKeyboardMode: "+
		"kitty keyboard handling not supported yet")
}

// PopKeyboardModes pops the given amount of keyboard modes
// from the keyboard mode stack.
func (t *parserHandler) PopKeyboardModes(count int) {
	t.log(log.DebugLevel, "unsupported call to PopKeyboardModes: "+
		"kitty keyboard handling not supported yet")
}

// SetKeyboardMode sets the [`keyboard mode`] using the given [`behavior`].
func (t *parserHandler) SetKeyboardMode(
	mode vteparser.KeyboardMode, behavior vteparser.KeyboardModesApplyBehavior,
) {
	t.log(log.DebugLevel, "unsupported call to SetKeyboardMode: "+
		"kitty keyboard handling not supported yet")
}

// SetModifyOtherKeys sets XTerm's [`ModifyOtherKeys`] option.
func (t *parserHandler) SetModifyOtherKeys(mode vteparser.ModifyOtherKeysMode) {
	t.log(log.DebugLevel, "unsupported call to SetModifyOtherKeys")
}

// ReportModifyOtherKeys report XTerm's [`ModifyOtherKeys`] state.
func (t *parserHandler) ReportModifyOtherKeys() {
	t.log(log.DebugLevel, "unsupported call to ReportModifyOtherKeys")
}

func (t *parserHandler) log(level log.Level, line string, params ...any) {
	if !log.IsLevelEnabled(level) {
		return
	}
	log.WithField(logging.KeyClass, "vte.parserHandler").
		Logf(level, line, params...)
}

func (t *parserHandler) reportMode(template string, mode int, modeVar *bool) {
	var rep int
	if modeVar == nil {
		rep = 0
	} else if *modeVar {
		rep = 1
	} else {
		rep = 2
	}

	_, err := fmt.Fprintf(t.pty.Master, template, mode, rep)
	if err != nil {
		t.log(log.WarnLevel, "report mode with template %q: %v", template, err)
	}
}

func (t *parserHandler) swapAlt() {
	if !t.useAlt {
		// Set alt screen cursor to the current primary screen cursor.
		t.sync.altBuf.SetCursor(t.sync.primBuf.CloneCursor())

		// Drop information about the primary screens saved cursor.
		t.sync.primBuf.SetSavedCursor(t.sync.primBuf.CloneCursor())

		// Reset alternate screen contents.
		t.resetBufLines(t.sync.altBuf)

		t.sync.buf = t.sync.altBuf
		t.useAlt = true
		t.usedAlt = true

	} else {
		t.sync.buf = t.sync.primBuf
		t.useAlt = false
	}
}

func (t *parserHandler) deccolm() {
	t.resetBufLines(t.sync.buf)
	t.resetScrollingRegion()
}

func (t *parserHandler) resetBufLines(buf screenBuffer) {
	// use rows rather than height if height is not yet == rows
	// to be resilient against constant resizes
	buf.ResetLines(0, t.maxRows())
}

func (t *parserHandler) carriageReturn() {
	t.setCursorAtScreen(term.Coordinates{
		X: 0,
		Y: t.sync.buf.CursorAtScreen().Y,
	})
}

func (t *parserHandler) scrollDown(rows int) bool {
	if t.useAlt {
		return t.scrollDownAltRelative(t.sync.buf.TopScrollableRegion(), rows)
	}

	buf := t.sync.primBuf
	t.shouldWrap = false

	// The primary buffer keeps history in the same matrix, so the
	// region rows have to be offset by the screen's first row.
	screenTop := buf.Rows() - t.height
	start := screenTop + buf.TopScrollableRegion()
	end := screenTop + buf.BottomScrollableRegion()
	count := max(0, min(rows, end-start))
	if count > 0 {
		buf.ScrollDown(start, end, count)
		return true
	}
	return false
}

func (t *parserHandler) scrollUp(count int) bool {
	if t.useAlt {
		return t.scrollUpAltRelative(t.sync.buf.TopScrollableRegion(), count)
	}
	buf := t.sync.primBuf
	t.shouldWrap = false

	top, bottom := buf.TopScrollableRegion(), buf.BottomScrollableRegion()
	if top > 0 {
		// Lines leaving a region that does not start at the top of the
		// screen are discarded rather than saved (kitty screen.c:2408,
		// :2427).
		count = max(0, min(count, bottom-top))
		if count <= 0 {
			return false
		}
		screenTop := buf.Rows() - t.height
		buf.ScrollUp(screenTop+top, screenTop+bottom, count)
		return true
	}

	rows := buf.Rows()
	if bottom < t.height {
		rows = bottom
	}
	count = max(0, min(count, rows))
	if count <= 0 {
		return false
	}
	buf.ScrollUpHistory(count)
	if bottom < t.height {
		// ScrollUpHistory moved the whole screen up to save the top
		// rows; rotate the rows below the bottom margin back down over
		// the blanks it appended.
		r := buf.Rows()
		buf.ScrollDown(r-(t.height-bottom)-count, r, count)
	}
	return true
}

func (t *parserHandler) scrollUpAltRelative(start int, count int) bool {
	if t.sync.buf != t.sync.altBuf {
		panic("called scroll up relative on non alternate buffer")
	}
	count = min(count, t.sync.buf.BottomScrollableRegion()-t.sync.buf.TopScrollableRegion())
	end := t.sync.altBuf.BottomScrollableRegion()
	ok := count != 0
	if ok {
		t.sync.altBuf.ScrollUp(start, end, count)
	}
	return ok
}

func (t *parserHandler) scrollDownAltRelative(start int, count int) bool {
	if t.sync.buf != t.sync.altBuf {
		panic("called scroll down relative on non alternate buffer")
	}
	count = min(
		count,
		t.sync.buf.BottomScrollableRegion()-t.sync.buf.TopScrollableRegion(),
	)
	count = min(
		count,
		t.sync.buf.BottomScrollableRegion()-start,
	)

	end := t.sync.altBuf.BottomScrollableRegion()
	ok := count != 0
	if ok {
		t.sync.altBuf.ScrollDown(start, end, count)
	}
	return ok
}

// moveRows moves the cursor by delta rows. A cursor inside the margins
// stops at them and one outside stops at the screen edge (DEC STD 070;
// xterm CursorUp and CursorDown).
func (t *parserHandler) moveRows(delta int) {
	buf := t.sync.buf
	pos := buf.CursorAtScreen()
	top, bottom := 0, t.height-1
	if delta > 0 && pos.Y < buf.BottomScrollableRegion() {
		bottom = buf.BottomScrollableRegion() - 1
	}
	if delta < 0 && pos.Y >= buf.TopScrollableRegion() {
		top = buf.TopScrollableRegion()
	}
	pos.Y = max(top, min(pos.Y+delta, bottom))
	t.setCursorAtScreen(pos)
	t.shouldWrap = false
}

// setColumn moves the cursor to col on its row.
func (t *parserHandler) setColumn(col int) {
	pos := t.sync.buf.CursorAtScreen()
	pos.X = max(0, min(col, t.width-1))
	t.setCursorAtScreen(pos)
	t.shouldWrap = false
}

func (t *parserHandler) setCursorShape(shape vteparser.CursorShape) {
	t.cursorHidden = shape == vteparser.CursorShapeHidden
	if t.cursorHidden {
		return
	}
	switch shape {
	case vteparser.CursorShapeBlock:
		t.cursorStyle = term.CursorStyleSteadyBlock
	case vteparser.CursorShapeUnderline:
		t.cursorStyle = term.CursorStyleSteadyUnderline
	case vteparser.CursorShapeBeam:
		t.cursorStyle = term.CursorStyleSteadyBar
	case vteparser.CursorShapeHollowBlock:
		/* unsupported by tcell */
		t.cursorStyle = term.CursorStyleSteadyBlock
	case vteparser.CursorShapeDefault:
		t.cursorStyle = term.CursorStyleDefault
	}
}

// setScrollingRegion implements DECSTBM. top and bottom are 1-based;
// bottom is kept as is since it doubles as the exclusive end of the
// 0-based region. A region of fewer than two rows is ignored, as in
// kitty and xterm, so that DECSTBM never traps the cursor on one line.
func (t *parserHandler) setScrollingRegion(top, bottom int, end bool) {
	top = min(top-1, t.height)
	if end || bottom > t.height {
		bottom = t.height
	}
	if bottom-top < 2 {
		return
	}
	t.sync.buf.SetScrollableRegion(top, bottom, false)
	t.goTo(0, 0)
}

// resetScrollingRegion opens the margins to the whole screen and homes
// the cursor, as the sequences that reset the margins as a side effect
// do.
func (t *parserHandler) resetScrollingRegion() {
	t.sync.buf.SetScrollableRegion(0, 0, true)
	t.goTo(0, 0)
}

func (t *parserHandler) wrapLine() {
	if !t.modeWrap {
		return
	}

	if !t.useAlt {
		t.sync.primBuf.MarkWrapAtCursor()
	}

	t.carriageReturn()
	t.index()
}

// index moves the cursor down a line. Only a cursor on the bottom
// margin scrolls the region; below it the cursor stops at the last
// line (kitty screen_index, screen.c:2404).
func (t *parserHandler) index() {
	buf := t.sync.buf
	pos := buf.CursorAtScreen()
	switch {
	case pos.Y == buf.BottomScrollableRegion()-1:
		t.scrollUp(1)
	case pos.Y < t.height-1:
		pos.Y++
		t.setCursorAtScreen(pos)
	}
	t.shouldWrap = false
}

func (t *parserHandler) setCursorAtScreen(pos term.Coordinates) {
	t.sync.buf.SetCursorAtScreen(pos)
}

func (t *parserHandler) endOfLine(y int) int {
	return max(t.sync.buf.Columns(y), t.width)
}

func (t *parserHandler) maxRows() int {
	return max(t.height, t.sync.buf.Rows())
}

func (t *parserHandler) clearNeedsAttention() {
	t.needsAttention = false
	err := t.tm.SetTabName(t.uri, t.title, term.Attributes{})
	// doesn't necessarily need to be a tab vte
	if err != nil {
		t.log(log.TraceLevel, "set tab name uri=%q: %v", t.uri, err)
	}
}

func (t *parserHandler) setNeedsAttention() {
	t.needsAttention = true
	err := t.tm.SetTabName(t.uri, t.title, t.needsAttentionAttr)
	// doesn't necessarily need to be a tab vte
	if err != nil {
		t.log(log.TraceLevel, "set tab name: %v", err)
	}
}

func (t *parserHandler) onFocusChange(inFocus bool) (cmd string, ok bool) {
	if inFocus && t.needsAttention {
		t.clearNeedsAttention()
	}
	t.inFocus = inFocus

	t.log(log.TraceLevel, "OnFocusChange(inFocus=%t, reportFocusMode=%t)",
		inFocus, t.modeReportFocusInOut)
	if !t.modeReportFocusInOut {
		return
	}

	if inFocus {
		cmd = "I"
	} else {
		cmd = "O"
	}
	ok = true
	return
}

func (t *parserHandler) updateTabName(title string) {
	t.title = title
	if t.needsAttention {
		t.setNeedsAttention()
	} else {
		t.clearNeedsAttention()
	}
}

func (t *parserHandler) usedAlternate() bool {
	return t.usedAlt
}

// goTo implements CUP: line and col are relative to the origin, which
// DECOM moves to the top margin and confines to the region.
func (t *parserHandler) goTo(line int, col int) {
	top, bottom := 0, t.height-1
	if t.modeOrigin {
		top = t.sync.buf.TopScrollableRegion()
		bottom = t.sync.buf.BottomScrollableRegion() - 1
	}
	t.setCursorAtScreen(term.Coordinates{
		Y: max(top, min(line+top, bottom)),
		X: max(0, min(col, t.width-1)),
	})
	t.shouldWrap = false
}
