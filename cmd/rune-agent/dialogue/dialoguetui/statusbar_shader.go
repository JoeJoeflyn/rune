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
	"slices"
	"sync"
	"time"

	"github.com/unstablebuild/rune-go-sdk/term"
	"github.com/unstablebuild/rune-go-sdk/tui"
	"unstable.build/rune/internal/component/shader"
	"unstable.build/rune/internal/component/shader/glslshader"
	"unstable.build/rune/internal/component/shader/timeshader"
)

const (
	// DefaultStatusBarShaderFPS is the cadence a status-bar effect is
	// redrawn at when the config names none.
	DefaultStatusBarShaderFPS = 30
	// DefaultStatusBarShaderLoop is how long one visual loop of a
	// status-bar effect lasts when the config names none.
	DefaultStatusBarShaderLoop = 1200 * time.Millisecond
	// A turn has no known length, so the animation is built long
	// enough to outlast any of them and looped within that span.
	statusBarShaderDuration = 24 * time.Hour
	// The statuses carry their meaning in colour, and the stock pulse
	// washes a dark one most of the way to white so it reads as a
	// different status.
	statusBarPulseIntensity = 0.25
)

// statusBarLoopFrames is one visual loop at the bar's cadence.
func statusBarLoopFrames(fps int, loop time.Duration) int {
	return int(float64(fps) * loop.Seconds())
}

// StatusBarShaderNames lists the effects status_bar.shader accepts.
// The frame shaders are deliberately absent: they only paint
// box-drawing characters, of which a status bar has none. Incendium
// costs too much to run for the length of every turn, and embers,
// flames and risingChars garble a single row of status text rather
// than dress it.
func StatusBarShaderNames() []string {
	return []string{
		"blaze", "burn", "fade", "grayFade", "inferno", "noise",
		"pulse", "shine", "trippy",
	}
}

// buildStatusBarShader resolves a name from StatusBarShaderNames into
// an effect that keeps painting for as long as the turn runs, either by
// looping its animation every loop or by running its own clock.
func buildStatusBarShader(
	name string, defAttr term.Attributes, fps int, loop time.Duration,
) (shader.Shader, bool) {
	fpsf := float64(fps)
	var inner shader.Shader
	// An effect that runs its clock off the frame index alone rather
	// than against total is already continuous, and Loop would replay
	// the whole statusBarShaderDuration inside one loop window and run
	// it thousands of times too fast.
	var continuous bool
	switch name {
	case "blaze":
		blazeParams := glslshader.DefaultBlazeParams()
		blazeParams.PaintForeground = true
		inner, continuous = glslshader.Blaze(blazeParams, fpsf), true
	case "burn":
		burnParams := shader.DefaultBurnParams()
		burnParams.PaintForeground = true
		inner = shader.Burn(burnParams, defAttr)
	case "fade":
		inner = shader.Fade(defAttr)
	case "grayFade":
		inner = shader.GrayFade(shader.DefaultGrayFadeParams(), defAttr)
	case "inferno":
		infernoParams := glslshader.DefaultInfernoParams()
		infernoParams.PaintForeground = true
		inner, continuous = glslshader.Inferno(infernoParams, fpsf), true
	case "noise":
		noiseParams := glslshader.DefaultNoiseParams()
		noiseParams.PaintForeground = true
		inner, continuous = glslshader.Noise(noiseParams, fpsf), true
	case "pulse":
		params := shader.DefaultPulseParams()
		params.PeriodFrames = statusBarLoopFrames(fps, loop)
		params.Intensity = statusBarPulseIntensity
		inner, continuous = shader.Pulse(params, defAttr), true
	case "shine":
		inner = glslshader.Shine(glslshader.DefaultShineParams(), defAttr)
	case "trippy":
		trippyParams := glslshader.DefaultTrippyParams()
		trippyParams.PaintForeground = true
		inner, continuous = glslshader.Trippy(trippyParams, fpsf), true
	default:
		return nil, false
	}
	if continuous {
		return inner, true
	}
	return timeshader.Loop(inner, int(statusBarShaderDuration/loop)), true
}

// ValidStatusBarShader reports whether name selects a known effect.
// An empty name leaves the bar unshaded.
func ValidStatusBarShader(name string) bool {
	return name == "" || slices.Contains(StatusBarShaderNames(), name)
}

// shadedBar runs a shader over the status bar while a turn is active.
// It is a tui.Component so the dialogue can swap it in for the bar
// without the layout knowing an effect is running.
type shadedBar struct {
	mu            sync.Mutex
	root          tui.Component
	name          string
	fps           int
	loop          time.Duration
	defAttr       term.Attributes
	shader        *shader.Component
	width, height int
}

// Draw satisfies tui.Component.
func (s *shadedBar) Draw(w term.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shader != nil {
		s.shader.Draw(w)
		return
	}
	s.root.Draw(w)
}

// Resize satisfies tui.Component.
func (s *shadedBar) Resize(width, height int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.width, s.height = width, height
	if s.shader != nil {
		s.shader.Resize(width, height)
		return
	}
	s.root.Resize(width, height)
}

// setRunning starts or stops the effect. It reports whether the state
// changed, so the caller can repaint only on the transition rather than
// on every status update.
func (s *shadedBar) setRunning(running bool, interrupter term.Interrupter) bool {
	s.mu.Lock()
	if s.name == "" || interrupter == nil || running == (s.shader != nil) {
		s.mu.Unlock()
		return false
	}
	var closing *shader.Component
	if running {
		fps, loop := s.fps, s.loop
		if fps <= 0 {
			fps = DefaultStatusBarShaderFPS
		}
		if loop <= 0 {
			loop = DefaultStatusBarShaderLoop
		}
		sh, ok := buildStatusBarShader(s.name, s.defAttr, fps, loop)
		if !ok {
			s.mu.Unlock()
			return false
		}
		s.shader = shader.New(s.root, sh, interrupter,
			fps, statusBarShaderDuration)
		s.shader.Resize(s.width, s.height)
	} else {
		closing = s.shader
		s.shader = nil
	}
	s.mu.Unlock()
	if closing != nil {
		_ = closing.Close()
	}
	return true
}
