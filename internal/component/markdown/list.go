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

package markdown

import (
	"fmt"

	"github.com/unstablebuild/rune-go-sdk/term"

	"unstable.build/rune/internal/component"
)

const listIndent = 2

type listItem struct {
	content textRun
	isTask  bool
	checked bool
	nested  *listBlock
}

type listBlock struct {
	ordered bool
	start   int
	items   []listItem
	// loose marks a list whose source separates items with blank
	// lines; CommonMark renders those items as paragraphs, so they get
	// a blank line between them.
	loose bool
	cfg   *Config
	w     int // width from last Height call
}

var _ block = (*listBlock)(nil)

func newListBlock(
	ordered bool, start int, items []listItem, cfg *Config,
) *listBlock {
	return &listBlock{
		ordered: ordered,
		start:   start,
		items:   items,
		cfg:     cfg,
	}
}

func (l *listBlock) Height(width int) int {
	return l.heightAtIndent(width, 0)
}

func (l *listBlock) Resize(width, _ int) {
	l.w = width
	for _, item := range l.items {
		if item.nested != nil {
			item.nested.Resize(width, 0)
		}
	}
}

func (l *listBlock) Draw(w term.Writer) {
	contentHeight := l.heightAtIndent(l.w, 0)
	if l.cfg.Paragraph.Bg != term.ColorDefault && contentHeight > 1 {
		bgAttr := term.Attributes{Bg: l.cfg.Paragraph.Bg}
		for y := range contentHeight - 1 {
			for x := range l.w {
				w.UnionAttributes(term.Coordinates{X: x, Y: y}, bgAttr)
			}
		}
	}
	l.drawAtIndent(w, 0, 0)
}

func (l *listBlock) Dimensions() (width, height int) {
	return l.dimensionsAtIndent(0)
}

func (l *listBlock) SpanAt(x, y int) (text, url string, ok bool) {
	return l.spanAtIndent(x, y, 0)
}

func (l *listBlock) CharAt(x, y int) (rune, bool) {
	return l.charAtIndent(x, y, 0)
}

func (l *listBlock) heightAtIndent(width, indent int) int {
	if width <= indent+listIndent {
		return 0
	}

	h := 0
	effectiveWidth := width - indent - listIndent

	for i, item := range l.items {
		lines := countWrappedLines(item.content, effectiveWidth)
		h += lines

		if item.nested != nil {
			h += l.nestedGap()
			h += item.nested.heightAtIndent(width, indent+listIndent)
		}
		h += l.gapAfter(i)
	}

	// Only add spacing at top level, not for nested lists
	if indent == 0 {
		h++
	}
	return h
}

func (l *listBlock) drawAtIndent(w term.Writer, y, indent int) int {
	if l.w <= indent+listIndent {
		return 0
	}

	startY := y
	effectiveWidth := l.w - indent - listIndent

	for i, item := range l.items {
		bullet := l.getBullet(i, item)
		component.WriteText(w, indent, y, l.w, bullet, l.cfg.Paragraph)

		lines := wrapTextRun(item.content, effectiveWidth)
		for lineIdx, line := range lines {
			x := indent + listIndent
			for _, sp := range line {
				attr := resolveStyle(sp.style, l.cfg)
				x = component.WriteText(w, x, y+lineIdx, l.w, sp.text, attr)
			}
		}
		y += len(lines)
		if len(lines) == 0 {
			y++
		}

		if item.nested != nil {
			y += l.nestedGap()
			y += item.nested.drawAtIndent(w, y, indent+listIndent)
		}
		y += l.gapAfter(i)
	}

	drawnHeight := y - startY
	// Only count spacing at top level
	if indent == 0 {
		drawnHeight++
	}
	return drawnHeight
}

func (l *listBlock) dimensionsAtIndent(indent int) (width, height int) {
	maxWidth := 0
	totalHeight := 0

	for i, item := range l.items {
		itemWidth := indent + listIndent + item.content.Width()
		if itemWidth > maxWidth {
			maxWidth = itemWidth
		}
		totalHeight++

		if item.nested != nil {
			totalHeight += l.nestedGap()
			nestedW, nestedH := item.nested.dimensionsAtIndent(indent + listIndent)
			if nestedW > maxWidth {
				maxWidth = nestedW
			}
			totalHeight += nestedH
		}
		totalHeight += l.gapAfter(i)
	}

	// Only add spacing at top level
	if indent == 0 {
		totalHeight++
	}
	return maxWidth, totalHeight
}

// gapAfter reports the blank rows that follow the item at index i.
func (l *listBlock) gapAfter(i int) int {
	if l.loose && i < len(l.items)-1 {
		return 1
	}
	return 0
}

// nestedGap reports the blank rows between an item's own text and the
// list nested under it. A loose list wraps item text in a paragraph,
// so the sub-list reads as a block of its own.
func (l *listBlock) nestedGap() int {
	if l.loose {
		return 1
	}
	return 0
}

func (l *listBlock) getBullet(index int, item listItem) string {
	if item.isTask {
		if item.checked {
			return string(l.cfg.TaskListChecked) + " "
		}
		return string(l.cfg.TaskListUnchecked) + " "
	}

	if l.ordered {
		return fmt.Sprintf("%d.", l.start+index)
	}

	return string(l.cfg.ListBullet) + " "
}

func (l *listBlock) spanAtIndent(x, y, indent int) (text, url string, ok bool) {
	if l.w <= indent+listIndent {
		return
	}

	effectiveWidth := l.w - indent - listIndent
	currentY := 0

	for i, item := range l.items {
		lines := wrapTextRun(item.content, effectiveWidth)
		lineCount := len(lines)
		if lineCount == 0 {
			lineCount = 1
		}

		if y >= currentY && y < currentY+lineCount {
			lineIdx := y - currentY
			if lineIdx < len(lines) {
				adjustedX := x - indent - listIndent
				if adjustedX >= 0 {
					return spanAtInLine(lines[lineIdx], adjustedX)
				}
			}
			return
		}
		currentY += lineCount

		if item.nested != nil {
			nestedHeight := item.nested.heightAtIndent(l.w, indent+listIndent)
			currentY += l.nestedGap()
			if y >= currentY && y < currentY+nestedHeight {
				return item.nested.spanAtIndent(x, y-currentY, indent+listIndent)
			}
			currentY += nestedHeight
		}
		currentY += l.gapAfter(i)
	}
	return
}

func (l *listBlock) charAtIndent(x, y, indent int) (rune, bool) {
	if l.w <= indent+listIndent {
		return 0, false
	}

	effectiveWidth := l.w - indent - listIndent
	currentY := 0

	for i, item := range l.items {
		lines := wrapTextRun(item.content, effectiveWidth)
		lineCount := len(lines)
		if lineCount == 0 {
			lineCount = 1
		}

		if y >= currentY && y < currentY+lineCount {
			lineIdx := y - currentY
			if lineIdx == 0 && x >= indent && x < indent+listIndent {
				bullet := textRun{{text: l.getBullet(i, item)}}
				return charAtInLine(bullet, x-indent)
			}
			if lineIdx < len(lines) {
				adjustedX := x - indent - listIndent
				if adjustedX >= 0 {
					return charAtInLine(lines[lineIdx], adjustedX)
				}
			}
			return 0, false
		}
		currentY += lineCount

		if item.nested != nil {
			nestedHeight := item.nested.heightAtIndent(l.w, indent+listIndent)
			currentY += l.nestedGap()
			if y >= currentY && y < currentY+nestedHeight {
				return item.nested.charAtIndent(x, y-currentY, indent+listIndent)
			}
			currentY += nestedHeight
		}
		currentY += l.gapAfter(i)
	}
	return 0, false
}
