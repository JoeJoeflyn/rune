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

package extension

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/llmapi"
	"unstable.build/rune/cmd/rune-agent/agent"
	"unstable.build/rune/cmd/rune-agent/agent/skills"
	"unstable.build/rune/cmd/rune-agent/dialogue/dialoguetui"
	"unstable.build/rune/cmd/rune-agent/llm/llmtest"
	"unstable.build/rune/internal/llm/anthropic"
)

func newStatusBarSyncComponent() syncComponent {
	comp := dialoguetui.NewComponent(dialoguetui.ComponentConfig{
		StatusBar: dialoguetui.StatusBarConfig{Enabled: true},
	})
	return syncComponent{
		mu:   new(sync.Mutex),
		comp: comp,
		h:    &aiEditorHandler{n: stubNotifications{}},
	}
}

// Reopening a conversation must fill the context gauge from the
// replayed history; until the first completion reports usage the bar
// would otherwise read empty, which is indistinguishable from a chat
// that has not started yet.
func TestSeedContextTokens(t *testing.T) {
	entry := llmapi.ModelEntry{
		Provider: "test", Name: "test-model", ContextWindow: 200_000,
	}
	msgs := []llmapi.Message{
		{Role: llmapi.RoleUser, Content: "hello"},
		{Role: llmapi.RoleAssistant, Content: "hi"},
	}

	svc := llmtest.New([]llmapi.ModelEntry{entry})
	svc.CountTokensFn = func(_ llmapi.ModelEntry, m []llmapi.Message) (int, error) {
		assert.Equal(t, msgs, m)
		return 1234, nil
	}

	s := newStatusBarSyncComponent()
	t.Cleanup(func() { _ = s.comp.Close() })
	s.seedContextTokens(svc, entry, msgs)

	got := s.comp.StatusBarState()
	assert.Equal(t, 1234, got.ContextTokens)
	assert.Equal(t, 200_000, got.ContextWindow)
}

func TestSeedContextTokensSkipped(t *testing.T) {
	entry := llmapi.ModelEntry{Provider: "test", Name: "test-model"}

	tests := []struct {
		name  string
		msgs  []llmapi.Message
		count int
		err   error
	}{
		{name: "no history", msgs: nil, count: 99},
		{
			name: "count fails", msgs: []llmapi.Message{{Content: "x"}},
			err: errors.New("boom"),
		},
		{
			name: "count returns nothing",
			msgs: []llmapi.Message{{Content: "x"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := llmtest.New([]llmapi.ModelEntry{entry})
			svc.CountTokensFn = func(llmapi.ModelEntry, []llmapi.Message) (int, error) {
				return tt.count, tt.err
			}

			s := newStatusBarSyncComponent()
			t.Cleanup(func() { _ = s.comp.Close() })
			s.seedContextTokens(svc, entry, tt.msgs)

			assert.Zero(t, s.comp.StatusBarState().ContextTokens)
		})
	}
}

// A usage report from a live turn is authoritative; a late estimate
// must not overwrite it.
func TestSeedContextTokensDoesNotOverwriteReportedUsage(t *testing.T) {
	entry := llmapi.ModelEntry{Provider: "test", Name: "test-model"}
	svc := llmtest.New([]llmapi.ModelEntry{entry})
	svc.CountTokensFn = func(llmapi.ModelEntry, []llmapi.Message) (int, error) {
		return 1234, nil
	}

	s := newStatusBarSyncComponent()
	t.Cleanup(func() { _ = s.comp.Close() })
	s.setStatusBarState(func(st *dialoguetui.StatusBarState) {
		st.ContextTokens = 77
	})
	s.seedContextTokens(svc, entry, []llmapi.Message{{Content: "x"}})

	assert.Equal(t, 77, s.comp.StatusBarState().ContextTokens)
}

// Context occupancy accumulates over a conversation, so starting a turn
// must not blank it. Clearing it left the gauge reading 0 until the
// first completion reported usage back, which is how a fresh chat
// reads.
func TestBeginTurnKeepsContextOccupancy(t *testing.T) {
	s := newStatusBarSyncComponent()
	t.Cleanup(func() { _ = s.comp.Close() })

	s.setStatusBarState(func(st *dialoguetui.StatusBarState) {
		st.ContextTokens = 120_000
		st.ContextWindow = 1_000_000
		st.Usage = llmapi.DialogueUsage{TokensSent: 5_000, TokensCached: 4_000}
	})

	start := time.Now()
	s.beginTurn(start)

	got := s.comp.StatusBarState()
	assert.Equal(t, 120_000, got.ContextTokens,
		"what the conversation already occupies carries into the new turn")
	assert.Equal(t, 1_000_000, got.ContextWindow)
	assert.Zero(t, got.Usage.TokensSent, "per-turn usage starts fresh")
	assert.True(t, got.Active)
	assert.Equal(t, phaseSending, got.Phase)
	assert.Equal(t, start, got.TurnStart)
}

// An unset session effort must name the level the provider applies,
// not leave the bar reading "default".
func TestSyncStatusBarModelResolvesProviderDefaultEffort(t *testing.T) {
	tests := []struct {
		name    string
		entry   llmapi.ModelEntry
		session llmapi.ReasoningEffort
		want    string
	}{
		{
			name: "provider default named",
			entry: llmapi.ModelEntry{
				Provider: anthropic.LLMProvider, Name: anthropic.ClaudeOpus5,
				ContextWindow: 1_000_000,
			},
			want: "high",
		},
		{
			name: "session override wins",
			entry: llmapi.ModelEntry{
				Provider: anthropic.LLMProvider, Name: anthropic.ClaudeOpus5,
			},
			session: llmapi.ReasoningEffortLow,
			want:    "low",
		},
		{
			name: "no published default",
			entry: llmapi.ModelEntry{
				Provider: "whatever", Name: "some-model",
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newStatusBarSyncComponent()
			t.Cleanup(func() { _ = s.comp.Close() })

			svc := llmtest.New([]llmapi.ModelEntry{tt.entry})
			fs := nopFileSystem{}
			ag := agent.NewAgent(svc, agent.NewRegistry(),
				skills.NewRegistry(fs, dirURI(""), nil, nil),
				newMemDialogueStore(), agent.NoMemory(),
				agent.Config{Model: tt.entry})
			ag.SetEffort(tt.session)

			a := &commandAdapter{agent: ag, statusBarFn: s.setStatusBarState}
			a.syncStatusBarModel()

			got := s.comp.StatusBarState()
			require.Equal(t, tt.entry.Name, got.Model)
			assert.Equal(t, tt.want, got.Effort)
		})
	}
}
