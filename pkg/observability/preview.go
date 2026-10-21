// Copyright 2026 Teradata
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package observability

import "sync/atomic"

// DefaultPreviewMaxChars bounds each text preview (prompt, completion, tool
// arguments and results) recorded on spans, in runes.
const DefaultPreviewMaxChars = 32768

var previewMaxChars atomic.Int64 //nolint:gochecknoglobals

func init() { previewMaxChars.Store(DefaultPreviewMaxChars) }

// SetPreviewMaxChars sets the preview bound for the process. Values <= 0
// restore DefaultPreviewMaxChars. Call it once at startup, before agents run.
func SetPreviewMaxChars(n int) {
	if n <= 0 {
		n = DefaultPreviewMaxChars
	}
	previewMaxChars.Store(int64(n))
}

// PreviewMaxChars returns the current preview bound in runes.
func PreviewMaxChars() int { return int(previewMaxChars.Load()) }

// TruncatePreview returns up to PreviewMaxChars runes of s, appending "…"
// when it cuts.
func TruncatePreview(s string) string {
	limit := PreviewMaxChars()
	if len(s) <= limit { // byte length bounds rune count
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
