// Copyright © 2026 Michael Shields
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package model

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
)

// logHandler is an slog.Handler that formats each record as one line —
// "15:04:05 LEVEL message key=value …", dropping the redundant time= and
// level= attributes slog would otherwise add — appends it to the owning
// Model's log ring, and notifies OnChange. It is immutable except through
// WithAttrs/WithGroup, each of which returns a new handler, so it is safe
// for concurrent use from any goroutine (the only state it touches besides
// its own fields is behind Model's mutex).
type logHandler struct {
	m      *Model
	level  slog.Level
	prefix string   // dotted group prefix from WithGroup, e.g. "g1.g2."
	attrs  []string // already-formatted "key=value" pieces from WithAttrs
}

// LogHandler returns an slog.Handler the app can install alongside its
// file handler: it both keeps a ring of recent lines for the log panel and
// notifies OnChange so the binding repaints it. level is the minimum level
// that reaches the ring; nil means slog.LevelInfo.
func (m *Model) LogHandler(level slog.Leveler) slog.Handler {
	lvl := slog.LevelInfo
	if level != nil {
		lvl = level.Level()
	}
	return &logHandler{m: m, level: lvl}
}

// Enabled implements slog.Handler.
func (h *logHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle implements slog.Handler.
func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(r.Level.String())
	b.WriteByte(' ')
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		b.WriteByte(' ')
		b.WriteString(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, h.prefix, a)
		return true
	})

	h.m.appendLog(b.String())
	return nil
}

// WithAttrs implements slog.Handler by flattening attrs (resolving any
// group values and the current group prefix) into formatted pieces stored
// alongside any already accumulated from an earlier WithAttrs call.
func (h *logHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	nh := *h
	nh.attrs = append(append([]string{}, h.attrs...), flatten(h.prefix, attrs)...)
	return &nh
}

// WithGroup implements slog.Handler.
func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	nh := *h
	nh.prefix = h.prefix + name + "."
	return &nh
}

// flatten renders attrs as "key=value" pieces, recursing into any
// slog.Group-valued attr with its key folded into the dotted prefix.
func flatten(prefix string, attrs []slog.Attr) []string {
	var b strings.Builder
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		b.Reset()
		if !writeAttr(&b, prefix, a) {
			continue
		}
		out = append(out, strings.TrimPrefix(b.String(), " "))
	}
	return out
}

// writeAttr appends " key=value" for a, resolving group-valued attrs
// recursively with their key folded into prefix. It reports whether
// anything was written (a group with no leaf attrs writes nothing).
func writeAttr(b *strings.Builder, prefix string, a slog.Attr) bool {
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		wrote := false
		for _, sub := range a.Value.Group() {
			if writeAttr(b, prefix+a.Key+".", sub) {
				wrote = true
			}
		}
		return wrote
	}
	b.WriteByte(' ')
	b.WriteString(prefix)
	b.WriteString(a.Key)
	b.WriteByte('=')
	b.WriteString(formatAttrValue(a.Value))
	return true
}

// formatAttrValue renders v as a bare token, or a Go-quoted string if it
// contains a space or a double quote.
func formatAttrValue(v slog.Value) string {
	s := v.String()
	if s == "" || strings.ContainsAny(s, " \"") {
		return strconv.Quote(s)
	}
	return s
}

// appendLog adds line to the ring, evicting the oldest line if it is now
// over maxLog, and notifies OnChange. Safe for concurrent use.
func (m *Model) appendLog(line string) {
	m.mu.Lock()
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > m.maxLog {
		m.logLines = m.logLines[len(m.logLines)-m.maxLog:]
	}
	onChange := m.onChange
	m.mu.Unlock()

	if onChange != nil {
		onChange(Changes{Log: true})
	}
}
