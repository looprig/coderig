package coderig

import (
	"strings"

	"github.com/looprig/core/content"
)

// aiMessageText concatenates the text of every *content.TextBlock in m, ignoring non-text
// blocks. A nil message yields the empty string. It is the package's final-text projection,
// flattening a turn's terminal AIMessage into a plain string (used by the eval harness and the
// acceptance tests to read a turn's reply).
func aiMessageText(m *content.AIMessage) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range m.Blocks {
		if tb, ok := blk.(*content.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}
