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

package agentshell

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/unstablebuild/rune-go-sdk/api/llmapi"
	"github.com/unstablebuild/rune-go-sdk/api/storageapi"
	"github.com/unstablebuild/rune-go-sdk/iterator"

	"unstable.build/rune/cmd/rune-agent/dialogue/dialoguemanager"
	"unstable.build/rune/cmd/rune-agent/llm/llmtest"
)

type compactAliasService struct {
	*llmtest.Service
	target llmapi.ModelEntry
}

func (s *compactAliasService) GetModel(
	ctx context.Context, model llmapi.ModelEntry,
) (llmapi.ModelEntry, error) {
	if model.Provider == "" && model.Name == "compact" {
		return s.target, nil
	}
	return s.Service.GetModel(ctx, model)
}

type compactDialogueStore struct {
	dialogue dialoguemanager.Dialogue
}

func (s *compactDialogueStore) Health(context.Context) error { return nil }
func (s *compactDialogueStore) Create(_ context.Context, d dialoguemanager.Dialogue) error {
	s.dialogue = d
	return nil
}
func (s *compactDialogueStore) Get(_ context.Context, id string) (dialoguemanager.Dialogue, error) {
	if id != s.dialogue.ID {
		return dialoguemanager.Dialogue{}, storageapi.ErrNotFound
	}
	return s.dialogue, nil
}
func (s *compactDialogueStore) Delete(context.Context, string) error { return nil }
func (s *compactDialogueStore) AppendMessages(
	context.Context, dialoguemanager.Dialogue, []llmapi.Message, llmapi.DialogueUsage,
) error {
	return nil
}
func (s *compactDialogueStore) ArchiveAndReplace(
	_ context.Context, params dialoguemanager.ArchiveAndReplaceParams,
) error {
	s.dialogue.Messages = params.Messages
	return nil
}
func (s *compactDialogueStore) List(context.Context) (
	iterator.Iterator[dialoguemanager.DialogueHeader], error,
) {
	return iterator.FromSlice([]dialoguemanager.DialogueHeader{s.dialogue.Header()}), nil
}

func TestCompactConversationModelSelection(t *testing.T) {
	chatModel := llmapi.ModelEntry{Provider: "openai", Name: "chat", ContextWindow: 100_000}
	compactModel := llmapi.ModelEntry{Provider: "anthropic", Name: "summary", ContextWindow: 100_000}

	for _, tc := range []struct {
		name      string
		args      []string
		wantModel llmapi.ModelEntry
	}{
		{"compact alias by default", []string{"compact", "rolling-fox"}, compactModel},
		{"explicit model", []string{"compact", "rolling-fox", "openai/chat"}, chatModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := llmtest.New(
				[]llmapi.ModelEntry{chatModel, compactModel},
				llmtest.Response{Chunks: []string{"summary"}},
			)
			svc := &compactAliasService{Service: backend, target: compactModel}
			store := &compactDialogueStore{dialogue: dialoguemanager.Dialogue{
				ID: "rolling-fox",
				Messages: []llmapi.Message{
					{Role: llmapi.RoleSystem, Content: "system"},
					{Role: llmapi.RoleUser, Content: "question"},
				},
			}}
			s := &shell{
				llmSvc:       svc,
				defaultModel: "openai/chat",
				store:        store,
			}

			_, err := s.handleChats(t.Context(), tc.args)
			require.NoError(t, err)
			require.Len(t, backend.Requests(), 1)
			assert.Equal(t, tc.wantModel, backend.Requests()[0].Model)
		})
	}
}

func TestCompleteChatsCompactModel(t *testing.T) {
	s := &shell{llmSvc: llmtest.New([]llmapi.ModelEntry{
		{Provider: "openai", Name: "gpt-5"},
		{Provider: "codex", Name: "gpt-5"},
	})}

	it, err := s.Complete(t.Context(), "chats", []string{"compact", "rolling-fox", ""})
	require.NoError(t, err)
	got, err := iterator.ToSlice(t.Context(), it)
	require.NoError(t, err)
	assert.Equal(t, []string{"openai/gpt-5", "codex/gpt-5"}, got)
}
