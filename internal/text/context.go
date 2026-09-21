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
)

type ctxKey int

var barsKey ctxKey

func withAuxiliaryBars(ctx context.Context) context.Context {
	return context.WithValue(ctx, barsKey, true)
}

// BarsFromContext reports whether the context was prepared by
// withAuxiliaryBars. text.Editor implementations call it to decide
// whether to wrap the returned text.Handler with status / icons /
// aux bars.
func BarsFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(barsKey).(bool)
	return v
}
