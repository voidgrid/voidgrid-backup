// Package logbuf keeps the most recent log lines of a process in memory so the
// UI can show them. It wraps a slog.Handler: everything still goes to the
// wrapped handler (stderr, `docker logs`) unchanged.
package logbuf

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxFieldLen = 2000

// secretKey matches attribute names whose values must not be kept: the
// buffer is shown in a web page, and these include registration tokens
// and setup tokens.
var secretKey = regexp.MustCompile(`(?i)code|token|secret|password|passwd|key`)

// Entry is one captured log line.
type Entry struct {
	Seq     uint64
	Time    time.Time
	Level   slog.Level
	Message string
	Attrs   string // key=value pairs, space separated
}

// Buffer is a fixed-size ring of the newest entries.
type Buffer struct {
	mu   sync.Mutex
	ring []Entry
	next uint64 // sequence number of the next entry; also the count ever added
}

// New returns a buffer holding the last capacity entries.
func New(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 1
	}
	return &Buffer{ring: make([]Entry, capacity)}
}

func (b *Buffer) add(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e.Seq = b.next
	b.ring[b.next%uint64(len(b.ring))] = e
	b.next++
}

// Entries returns up to limit of the newest entries at or above min, oldest
// first.
func (b *Buffer) Entries(min slog.Level, limit int) []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Entry
	n := uint64(len(b.ring))
	start := uint64(0)
	if b.next > n {
		start = b.next - n
	}
	for seq := b.next; seq > start && (limit <= 0 || len(out) < limit); seq-- {
		e := b.ring[(seq-1)%n]
		if e.Level >= min {
			out = append(out, e)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Handler returns a handler that records into b and then hands every record
// to inner.
func (b *Buffer) Handler(inner slog.Handler) slog.Handler {
	return &handler{b: b, inner: inner}
}

type handler struct {
	b     *Buffer
	inner slog.Handler
	attrs string // attributes added by WithAttrs, already formatted
	group string // dotted prefix from WithGroup
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.inner.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&sb, h.group, a, true)
		return true
	})
	h.b.add(Entry{Time: r.Time, Level: r.Level, Message: clip(r.Message), Attrs: clip(sb.String())})
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&sb, h.group, a, true)
	}
	return &handler{b: h.b, inner: h.inner.WithAttrs(as), attrs: sb.String(), group: h.group}
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &handler{b: h.b, inner: h.inner.WithGroup(name), attrs: h.attrs, group: h.group + name + "."}
}

func appendAttr(sb *strings.Builder, prefix string, a slog.Attr, redact bool) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			appendAttr(sb, p, g, redact)
		}
		return
	}
	if sb.Len() > 0 {
		sb.WriteByte(' ')
	}
	key := prefix + a.Key
	sb.WriteString(key)
	sb.WriteByte('=')
	v := a.Value.String()
	if redact && secretKey.MatchString(a.Key) {
		v = "[hidden]"
	}
	if v == "" || strings.ContainsAny(v, " \t\n\"=") {
		v = strconv.Quote(v)
	}
	sb.WriteString(v)
}

func clip(s string) string {
	if len(s) > maxFieldLen {
		return s[:maxFieldLen] + "…"
	}
	return s
}

// NewStderrHandler writes records the way Go's default log output does,
// "2006/01/02 15:04:05 INFO message key=value", so wrapping it with
// Buffer.Handler leaves what `docker logs` shows as it was. It is used instead
// of slog's own default handler because that one writes through the log
// package, which slog points back at the new default handler: wrapping it loops.
func NewStderrHandler(w io.Writer, level slog.Leveler) slog.Handler {
	return &lineHandler{mu: new(sync.Mutex), w: w, level: level}
}

type lineHandler struct {
	mu    *sync.Mutex // shared by handlers derived with WithAttrs/WithGroup
	w     io.Writer
	level slog.Leveler
	attrs string
	group string
}

func (h *lineHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *lineHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&sb, h.group, a, false)
		return true
	})
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	line := t.Format("2006/01/02 15:04:05") + " " + r.Level.String() + " " + r.Message
	if sb.Len() > 0 {
		line += " " + sb.String()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, line+"\n")
	return err
}

func (h *lineHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&sb, h.group, a, false)
	}
	return &lineHandler{mu: h.mu, w: h.w, level: h.level, attrs: sb.String(), group: h.group}
}

func (h *lineHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &lineHandler{mu: h.mu, w: h.w, level: h.level, attrs: h.attrs, group: h.group + name + "."}
}
