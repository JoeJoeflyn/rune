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

package dialoguetui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/term"
)

// Every name the config accepts must resolve, or a valid config would
// silently leave the bar unshaded.
func TestStatusBarShaderNamesAllResolve(t *testing.T) {
	require.NotEmpty(t, StatusBarShaderNames())
	for _, name := range StatusBarShaderNames() {
		t.Run(name, func(t *testing.T) {
			sh, ok := buildStatusBarShader(name, term.Attributes{},
				DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
			assert.True(t, ok)
			assert.NotNil(t, sh)
			assert.True(t, ValidStatusBarShader(name))
		})
	}
}

func TestStatusBarShaderRejectsUnknownNames(t *testing.T) {
	_, ok := buildStatusBarShader("radarFrame", term.Attributes{},
		DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
	assert.False(t, ok, "frame shaders have nothing to paint on a bar")
	assert.False(t, ValidStatusBarShader("radarFrame"))
	for _, name := range []string{"incendium", "embers", "flames", "risingChars"} {
		assert.False(t, ValidStatusBarShader(name), name)
	}
	assert.True(t, ValidStatusBarShader(""), "an empty name disables the effect")
}

// barSnapshot renders the shipped bar mid-turn into a cell matrix. The
// snapshot is frozen, so anything that varies between shaded copies of
// it came from the effect rather than from the spinner.
func barSnapshot(t *testing.T, width int) [][]term.Cell {
	t.Helper()
	bar := shippedStatusBar(t)
	bar.SetState(func(s *StatusBarState) { s.Active = true })
	bar.Resize(width, 1)
	rec := newCellRecorder()
	bar.Draw(rec)
	row := make([]term.Cell, width)
	for x := range width {
		row[x] = rec.cells[term.Coordinates{X: x}]
	}
	return [][]term.Cell{row}
}

func cloneCells(src [][]term.Cell) [][]term.Cell {
	out := make([][]term.Cell, len(src))
	for y, row := range src {
		out[y] = make([]term.Cell, len(row))
		copy(out[y], row)
	}
	return out
}

func cellsSignature(cells [][]term.Cell) string {
	var b strings.Builder
	for _, row := range cells {
		for _, c := range row {
			fmt.Fprintf(&b, "%d/%d/%d,", c.Ch, int(c.Fg), int(c.Bg))
		}
	}
	return b.String()
}

// An effect that builds but paints the same cells on every frame is
// indistinguishable from no effect at all. timeshader.Loop advances the
// inner shader in strides of statusBarShaderDuration/statusBarShaderLoop,
// so an effect keyed off absolute frame counts rather than against total
// can alias to a single phase and silently stop animating.
func TestStatusBarShaderNamesAllAnimate(t *testing.T) {
	defAttr := term.Attributes{
		Fg: DefaultStatusBarForeground,
		Bg: DefaultStatusBarBackground,
	}
	// The frame budget shader.Component runs these effects against.
	total := int(statusBarShaderDuration /
		(time.Second / DefaultStatusBarShaderFPS))
	loopFrames := statusBarLoopFrames(
		DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
	// The grid is kept narrow because the simulation effects cost time
	// proportional to it, and a single column already exposes a phase
	// that never advances.
	snapshot := barSnapshot(t, 8)
	for _, name := range StatusBarShaderNames() {
		t.Run(name, func(t *testing.T) {
			sh, ok := buildStatusBarShader(name, defAttr,
				DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
			require.True(t, ok)
			seen := make(map[string]struct{})
			// Effects that open on a still phase need most of the loop
			// to show movement, while the simulation effects cost time
			// proportional to the frame index. Stopping at the first
			// change serves both: only an effect that never moves pays
			// for the whole sweep.
			for frame := range loopFrames {
				cells := cloneCells(snapshot)
				sh.Shade(frame, total, cells)
				seen[cellsSignature(cells)] = struct{}{}
				if len(seen) > 1 {
					break
				}
			}
			assert.Greater(t, len(seen), 1,
				"effect paints identical cells on every frame of a loop")
		})
	}
}

// blaze, inferno, noise and trippy run their clock off the frame index
// alone and never read total. timeshader.Loop replays the whole
// statusBarShaderDuration inside one loop window, which steps their
// clock by thousands of seconds per drawn frame and makes them a blur,
// so they have to reach the bar unwrapped. A shader that ignores total
// paints the same cells whatever frame budget it is handed.
func TestStatusBarContinuousShadersIgnoreTotal(t *testing.T) {
	defAttr := term.Attributes{
		Fg: DefaultStatusBarForeground,
		Bg: DefaultStatusBarBackground,
	}
	total := int(statusBarShaderDuration /
		(time.Second / DefaultStatusBarShaderFPS))
	// Past one loop window, so a Loop wrapper would fold the frame
	// against a different framesPerLoop for each budget.
	frame := statusBarLoopFrames(
		DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop) + 4
	snapshot := barSnapshot(t, 8)
	for _, name := range []string{"blaze", "inferno", "noise", "trippy"} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, budget := range []int{total, 2 * total} {
				sh, ok := buildStatusBarShader(name, defAttr,
					DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
				require.True(t, ok)
				cells := cloneCells(snapshot)
				sh.Shade(frame, budget, cells)
				got = append(got, cellsSignature(cells))
			}
			assert.Equal(t, got[0], got[1],
				"effect is time-continuous and must not be wrapped in Loop")
		})
	}
}

// The cadence knobs reach the effect rather than staying pinned to the
// shipped constants. A zero knob keeps the shipped value, which is what
// a config naming neither key leaves behind.
func TestShadedBarHonoursConfiguredFPS(t *testing.T) {
	for _, tc := range []struct {
		name string
		fps  int
		want int
	}{
		{name: "configured", fps: 12, want: 12},
		{name: "unset falls back", fps: 0, want: DefaultStatusBarShaderFPS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bar := shippedStatusBar(t)
			bar.Resize(40, 1)
			s := &shadedBar{root: bar, name: "pulse", fps: tc.fps}
			s.Resize(40, 1)

			require.True(t, s.setRunning(true, term.NopInterrupter()))
			require.NotNil(t, s.shader)
			// Total is the frame budget the cadence buys over
			// statusBarShaderDuration, so it reads the fps back out.
			assert.Equal(t,
				int(statusBarShaderDuration/(time.Second/time.Duration(tc.want))),
				s.shader.Total())
			require.True(t, s.setRunning(false, term.NopInterrupter()))
		})
	}
}

// The loop knob sets how much of a looped effect's animation is replayed
// The bar's statuses carry their meaning in colour, so pulse must only
// brighten them. At the stock intensity its peak washed a navy status
// most of the way to white and read as a different colour.
func TestStatusBarPulseIsSubtle(t *testing.T) {
	navy := term.NewRGBColor(0, 0, 128)
	sh, ok := buildStatusBarShader("pulse", term.Attributes{},
		DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop)
	require.True(t, ok)

	cells := [][]term.Cell{{{Ch: 'x', Width: 1, Fg: navy, Bg: navy}}}
	peak := statusBarLoopFrames(
		DefaultStatusBarShaderFPS, DefaultStatusBarShaderLoop) / 2
	sh.Shade(peak, 1, cells)

	r, _, b := cells[0][0].Bg.RGB()
	assert.Greater(t, r, int32(0), "the pulse must still be visible")
	// Under a third of the way to white; the stock 0.6 lands on 153.
	assert.LessOrEqual(t, r, int32(80), "the pulse must stay subtle")
	assert.Greater(t, b, r, "navy must stay blue at the peak")
}

// The loop knob sets how much of a looped effect's animation is replayed
// per window, so widening it must change what the effect paints.
func TestStatusBarShaderLoopChangesLoopedEffects(t *testing.T) {
	total := int(statusBarShaderDuration /
		(time.Second / DefaultStatusBarShaderFPS))
	snapshot := barSnapshot(t, 8)
	var got []string
	for _, loop := range []time.Duration{
		DefaultStatusBarShaderLoop, 10 * DefaultStatusBarShaderLoop,
	} {
		sh, ok := buildStatusBarShader("shine", term.Attributes{},
			DefaultStatusBarShaderFPS, loop)
		require.True(t, ok)
		cells := cloneCells(snapshot)
		sh.Shade(4, total, cells)
		got = append(got, cellsSignature(cells))
	}
	assert.NotEqual(t, got[0], got[1])
}

// The effect only runs while a turn does, and stopping it must put the
// bar back exactly as it was.
func TestShadedBarRunsOnlyWhileActive(t *testing.T) {
	bar := shippedStatusBar(t)
	bar.Resize(40, 1)
	s := &shadedBar{root: bar, name: "pulse"}
	s.Resize(40, 1)

	plain := newCellRecorder()
	s.Draw(plain)

	require.True(t, s.setRunning(true, term.NopInterrupter()))
	assert.NotNil(t, s.shader)
	assert.False(t, s.setRunning(true, term.NopInterrupter()),
		"starting a running effect must not report a transition")

	require.True(t, s.setRunning(false, term.NopInterrupter()))
	assert.Nil(t, s.shader)

	restored := newCellRecorder()
	s.Draw(restored)
	assert.Equal(t, plain.row(40), restored.row(40))
}

// A bar with no effect configured, or with no interrupter to drive one,
// must draw straight through rather than allocating a shader.
func TestShadedBarWithoutShaderDrawsRoot(t *testing.T) {
	bar := shippedStatusBar(t)
	s := &shadedBar{root: bar}
	s.Resize(40, 1)
	assert.False(t, s.setRunning(true, term.NopInterrupter()))
	assert.Nil(t, s.shader)

	withName := &shadedBar{root: bar, name: "pulse"}
	withName.Resize(40, 1)
	assert.False(t, withName.setRunning(true, nil))
	assert.Nil(t, withName.shader)
}
