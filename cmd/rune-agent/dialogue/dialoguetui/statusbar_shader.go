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
	statusBarShaderFPS = 30
	// A turn has no known length, so the animation is built long
	// enough to outlast any of them and looped within that span.
	statusBarShaderDuration = 24 * time.Hour
	statusBarShaderLoop     = 1200 * time.Millisecond
)

// StatusBarShaderNames lists the effects status_bar.shader accepts.
// The frame shaders are deliberately absent: they only paint
// box-drawing characters, of which a status bar has none.
func StatusBarShaderNames() []string {
	return []string{
		"blaze", "burn", "embers", "fade", "flames", "grayFade",
		"incendium", "inferno", "noise", "pulse", "risingChars",
		"shine", "trippy",
	}
}

// buildStatusBarShader resolves a name from StatusBarShaderNames into
// an effect looping for as long as the turn runs.
func buildStatusBarShader(
	name string, defAttr term.Attributes,
) (shader.Shader, bool) {
	const fps = float64(statusBarShaderFPS)
	var inner shader.Shader
	switch name {
	case "blaze":
		inner = glslshader.Blaze(glslshader.DefaultBlazeParams(), fps)
	case "burn":
		inner = shader.Burn(shader.DefaultBurnParams(), defAttr)
	case "embers":
		inner = glslshader.Embers(
			glslshader.DefaultEmbersParams(), defAttr, fps)
	case "fade":
		inner = shader.Fade(defAttr)
	case "flames":
		inner = glslshader.Flames(
			glslshader.DefaultFlamesParams(), defAttr, fps)
	case "grayFade":
		inner = shader.GrayFade(shader.DefaultGrayFadeParams(), defAttr)
	case "incendium":
		inner = glslshader.Incendium(
			glslshader.DefaultIncendiumParams(), defAttr, fps)
	case "inferno":
		inner = glslshader.Inferno(glslshader.DefaultInfernoParams(), fps)
	case "noise":
		inner = glslshader.Noise(glslshader.DefaultNoiseParams(), fps)
	case "pulse":
		inner = shader.Pulse(shader.DefaultPulseParams(), defAttr)
	case "risingChars":
		inner = glslshader.RisingChars(
			glslshader.DefaultRisingCharsParams(), defAttr)
	case "shine":
		inner = glslshader.Shine(glslshader.DefaultShineParams(), defAttr)
	case "trippy":
		inner = glslshader.Trippy(glslshader.DefaultTrippyParams(), fps)
	default:
		return nil, false
	}
	return timeshader.Loop(
		inner, int(statusBarShaderDuration/statusBarShaderLoop)), true
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
		sh, ok := buildStatusBarShader(s.name, s.defAttr)
		if !ok {
			s.mu.Unlock()
			return false
		}
		s.shader = shader.New(s.root, sh, interrupter,
			statusBarShaderFPS, statusBarShaderDuration)
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
