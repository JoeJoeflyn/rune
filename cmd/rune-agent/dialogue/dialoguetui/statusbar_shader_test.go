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
	"testing"

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
			sh, ok := buildStatusBarShader(name, term.Attributes{})
			assert.True(t, ok)
			assert.NotNil(t, sh)
			assert.True(t, ValidStatusBarShader(name))
		})
	}
}

func TestStatusBarShaderRejectsUnknownNames(t *testing.T) {
	_, ok := buildStatusBarShader("radarFrame", term.Attributes{})
	assert.False(t, ok, "frame shaders have nothing to paint on a bar")
	assert.False(t, ValidStatusBarShader("radarFrame"))
	assert.True(t, ValidStatusBarShader(""), "an empty name disables the effect")
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
