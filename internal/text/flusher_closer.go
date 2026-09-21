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

package text

import (
	"context"
	"time"

	"github.com/ernestrc/go-multierror"
	log "github.com/sirupsen/logrus"
	"github.com/unstablebuild/rune-go-sdk/api/textapi"
	"github.com/unstablebuild/rune-go-sdk/api/workspaceapi"
	"github.com/unstablebuild/rune-go-sdk/term"
	"unstable.build/rune/internal/cell"
	"unstable.build/rune/internal/debug"
	"unstable.build/rune/internal/workspace"
)

var _ workspace.FlusherCloser = (*editorFlusherCloser)(nil)

// used to intercept calls to Close and Flush to dispatch
// corresponding events to subscribers.
type editorFlusherCloser struct {
	parent    *Component
	fc        workspace.FlusherCloser
	h         Handler
	uri       workspaceapi.URI
	buf       *cell.Buffer
	commands  []textapi.CommandManual
	lastFlush int
	reloading bool
}

func (c *editorFlusherCloser) OnWillEdit(
	ctx context.Context, start, end term.Coordinates, str string,
) {
}

func (c *editorFlusherCloser) OnDidEdit(
	ctx context.Context, from, to term.Coordinates, old string,
) {
	if c.reloading {
		// A reload replaced the whole buffer. If the file shrank, the caret can
		// be left on a row that no longer exists; the handler's SetCursorAtScroll
		// clamps it back into bounds. Doing this here (synchronously, as the
		// buffer content is swapped) avoids a window where a stale caret could
		// drive a Columns(row) access past the end of the buffer.
		if c.h != nil {
			c.h.SetCursorAtScroll(c.h.CursorAtScroll())
		}
		return
	}
	c.parent.setDirtyFileAttr(c.uri, c.buf, c.lastFlush)
}

func (e *editorFlusherCloser) ForceFlush(ctx context.Context) (<-chan error, error) {
	inner, err := e.fc.ForceFlush(ctx)
	if err != nil {
		return nil, err
	}
	return e.wrapAndDispatch(inner, false, false), nil
}

func (e *editorFlusherCloser) LastFlush() time.Time {
	return e.fc.LastFlush()
}

func (e *editorFlusherCloser) Flush(ctx context.Context) (<-chan error, error) {
	inner, err := e.fc.Flush(ctx)
	if err != nil {
		return nil, err
	}
	return e.wrapAndDispatch(inner, false, false), nil
}

func (e *editorFlusherCloser) Reload(ctx context.Context) (<-chan error, error) {
	e.reloading = true
	inner, err := e.fc.Reload(ctx)
	if err != nil {
		e.reloading = false
		return nil, err
	}
	return e.wrapAndDispatch(inner, true, true), nil
}

func (e *editorFlusherCloser) wrapAndDispatch(
	inner <-chan error, skipOnErr, isReload bool,
) <-chan error {
	out := make(chan error, 1)
	go debug.CapturePanicReport(func() {
		err := <-inner
		doDispatch := err == nil || !skipOnErr
		if !doDispatch {
			if isReload {
				e.parent.config.ScheduleNextTick(func() {
					e.reloading = false
				})
			}
			out <- err
			close(out)
			return
		}
		e.parent.config.ScheduleNextTick(func() {
			_ = e.dispatchFlush()
			if isReload {
				e.reloading = false
			}
		})
		out <- err
		close(out)
	})
	return out
}

func (e *editorFlusherCloser) dispatchFlush() error {
	e.lastFlush = e.buf.Version()
	e.parent.log(log.TraceLevel, "flushed, new snapshot is at %d", e.lastFlush)
	return e.parent.dispatchFlush(e.uri, e.h)
}

func (e *editorFlusherCloser) Close() error {
	ev := textapi.Event{
		Type:     textapi.EventTypeClose,
		URI:      e.uri,
		Resource: e.h,
	}
	e.parent.DispatchEvent(ev)
	ret := e.fc.Close()
	for _, cmd := range e.commands {
		err := e.parent.fileRegistry.UnsubscribeCommandForFile(e.uri, cmd.Name)
		if err != nil {
			ret = multierror.Append(ret, err)
		}
	}
	return ret
}
