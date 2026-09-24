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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
)

// TestListCopyAndLinks covers hit-testing a drawn list: copying it
// gives back the text as drawn, and a click lands on the link drawn
// under the pointer, not on the marker, the blank past it or a gap.
func TestListCopyAndLinks(t *testing.T) {
	type probe struct {
		x, y int
		url  string // "" expects no link
	}
	tests := []struct {
		name   string
		src    string
		width  int
		copied string
		links  []probe
	}{
		{name: "bullets", src: "- one\n- two", width: 12,
			copied: "• one\n• two\n"},
		{name: "task boxes", src: "- [ ] todo\n- [x] done", width: 12,
			copied: "☐ todo\n☑ done\n"},
		{name: "wrapped item keeps the space it broke at", src: "- one two", width: 7,
			copied: "• one \ntwo\n"},
		{name: "tight sub-list", src: "- one\n  - sub", width: 12,
			copied: "• one\n• sub\n"},
		{name: "link in an item", src: "- [one](u1) two", width: 12,
			copied: "• one two\n",
			links:  []probe{{x: 0, y: 0}, {x: 2, y: 0, url: "u1"}, {x: 5, y: 0}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md, err := New(tt.src)
			require.NoError(t, err)
			md.Resize(tt.width, 40)
			assert.Equal(t, tt.copied, md.TextRange(
				term.Coordinates{}, term.Coordinates{Y: md.totalHeight}))
			for _, p := range tt.links {
				link := md.LinkAt(p.x, p.y)
				if p.url == "" {
					assert.Nil(t, link, "link at (%d, %d)", p.x, p.y)
					continue
				}
				if assert.NotNil(t, link, "link at (%d, %d)", p.x, p.y) {
					assert.Equal(t, p.url, link.URL, "link at (%d, %d)", p.x, p.y)
				}
			}
		})
	}
}
