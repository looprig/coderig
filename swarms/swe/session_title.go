package swe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ciram-co/looprig/pkg/content"
	"github.com/ciram-co/looprig/pkg/llm"
	"github.com/ciram-co/looprig/pkg/persistence"
)

const (
	// titleSystemPrompt is the fixed instruction for the Economy title model.
	titleSystemPrompt = "You name a coding session. Given the user's first request and the assistant's reply, respond with ONLY a concise, specific title of 3-8 words on a single line — no quotes, no trailing punctuation, no preamble."
	// titleGenTimeout bounds the best-effort title generation so a stuck provider never
	// holds the background worker open indefinitely.
	titleGenTimeout = 20 * time.Second
	// titlePromptExcerptRunes bounds each excerpt (user request, assistant reply) fed to the
	// title model, so a huge turn cannot bloat the title request.
	titlePromptExcerptRunes = 1500
	// titleOutputRunes bounds the one-line title before the manifest's own length cap.
	titleOutputRunes = 80
)

// titleSink is the narrow metadata-writer seam the coordinator depends on (the real
// *persistence.SessionMetaStore satisfies it), so the title logic can be unit-tested against
// a real manifest without any engine.
type titleSink interface {
	SetTitle(title string, source persistence.TitleSource, now time.Time) (persistence.SessionMeta, error)
}

// titleCoordinator produces a best-effort session title at the persisted-agent boundary —
// never inside core session logic. It stores an immediate first-user-message fallback so a
// crashed first turn is still listable, then, only if an Economy model is configured,
// generates ONE improved title after the first terminal primary-loop turn. It never blocks a
// turn, session creation, shutdown, or /clear: generation runs on an independent, bounded
// background context and the coordinator is never waited on by Close.
type titleCoordinator struct {
	meta      titleSink
	client    llm.LLM
	titleSpec func(system string) llm.ModelSpec // nil → no Economy model → no generation
	now       func() time.Time
	timeout   time.Duration

	mu        sync.Mutex
	observed  bool   // first user input handled (only the first matters)
	userText  string // first accepted user text ("" for image-only/empty input)
	generated bool   // one generation maximum
	wg        sync.WaitGroup
}

// newTitleCoordinator builds a coordinator writing through meta, using client + titleSpec for
// the Economy call. A nil titleSpec disables generation (only the fallback is written). now
// defaults to time.Now.
func newTitleCoordinator(meta titleSink, client llm.LLM, titleSpec func(system string) llm.ModelSpec, now func() time.Time) *titleCoordinator {
	if now == nil {
		now = time.Now
	}
	return &titleCoordinator{meta: meta, client: client, titleSpec: titleSpec, now: now, timeout: titleGenTimeout}
}

// observeUserInput handles the FIRST accepted user input: it extracts the first non-empty
// text and stores it as the immediate fallback title (first_user_message). Image-only or
// empty input stores no title (the manifest keeps TitleSourceNone). Subsequent inputs are
// ignored — only the first turn names the session.
func (c *titleCoordinator) observeUserInput(blocks []content.Block) {
	c.mu.Lock()
	if c.observed {
		c.mu.Unlock()
		return
	}
	c.observed = true
	text := firstNonEmptyText(blocks)
	c.userText = text
	c.mu.Unlock()

	if text == "" {
		return // image-only / empty → retain TitleSourceNone
	}
	fallback := oneLineTitle(text, titleOutputRunes)
	if fallback == "" {
		return
	}
	if _, err := c.meta.SetTitle(fallback, persistence.TitleSourceFirstUserMessage, c.now()); err != nil {
		slog.Warn("swe: session title fallback write failed", "err", err)
	}
}

// observeTerminalResponse handles the FIRST terminal primary-loop response: when an Economy
// model is configured and a user text was captured, it launches ONE best-effort background
// generation (independent bounded context) that replaces the fallback with a generated
// title. At most one generation ever runs; it returns immediately and never blocks the
// caller.
func (c *titleCoordinator) observeTerminalResponse(responseText string) {
	c.mu.Lock()
	if c.generated || c.titleSpec == nil || c.userText == "" {
		c.mu.Unlock()
		return
	}
	c.generated = true
	userText := c.userText
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
		defer cancel()

		title, err := c.generate(ctx, userText, responseText)
		if err != nil || title == "" {
			slog.Debug("swe: session title generation skipped, keeping fallback", "err", err)
			return
		}
		if _, err := c.meta.SetTitle(title, persistence.TitleSourceGenerated, c.now()); err != nil {
			slog.Warn("swe: session title write failed", "err", err)
		}
	}()
}

// generate makes the one-shot Economy call: a fixed system instruction, the bounded request +
// reply excerpts as a single user message, and NO tools. It drains the stream into one
// sanitized line.
func (c *titleCoordinator) generate(ctx context.Context, userText, responseText string) (string, error) {
	prompt := "User request:\n" + boundRunes(userText, titlePromptExcerptRunes) +
		"\n\nAssistant reply:\n" + boundRunes(responseText, titlePromptExcerptRunes)

	req := llm.Request{
		Model: c.titleSpec(titleSystemPrompt),
		Messages: content.AgenticMessages{
			&content.UserMessage{Message: content.Message{
				Role:   content.RoleUser,
				Blocks: []content.Block{&content.TextBlock{Text: prompt}},
			}},
		},
		// Tools is intentionally empty: a title call must never invoke tools.
	}

	stream, err := c.client.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	var sb strings.Builder
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if tc, ok := chunk.(*content.TextChunk); ok {
			sb.WriteString(tc.Text)
		}
	}
	return oneLineTitle(sb.String(), titleOutputRunes), nil
}

// wait blocks until any in-flight generation worker finishes. It is for tests only —
// production never waits on the title worker (Close must not block on it).
func (c *titleCoordinator) wait() { c.wg.Wait() }

// firstNonEmptyText returns the first text block's text whose trimmed value is non-empty, or
// "" when there is none (e.g. an image-only message).
func firstNonEmptyText(blocks []content.Block) string {
	for _, b := range blocks {
		tb, ok := b.(*content.TextBlock)
		if !ok {
			continue
		}
		if strings.TrimSpace(tb.Text) != "" {
			return tb.Text
		}
	}
	return ""
}

// aiMessageText concatenates the text of every *content.TextBlock in m, ignoring non-text
// blocks. A nil message yields the empty string. It is the package's final-text projection,
// flattening a turn's terminal AIMessage into a plain string for the title prompt.
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

// oneLineTitle reduces s to a single sanitized, bounded line suitable for a manifest title:
// it takes the first line, replaces control runes with spaces, collapses whitespace, strips
// surrounding quotes a model may add, and bounds the rune length.
func oneLineTitle(s string, limit int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.Trim(out, `"'`)
	return boundRunes(strings.TrimSpace(out), limit)
}

// boundRunes truncates s to at most limit runes.
func boundRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
