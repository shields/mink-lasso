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
	"strings"
	"sync"
	"testing"

	"msrl.dev/mink-lasso/internal/engine"
)

func TestLogHandlerBasic(t *testing.T) {
	t.Parallel()
	var changes int
	var mu sync.Mutex
	m := New(Options{OnChange: func(c Changes) {
		mu.Lock()
		defer mu.Unlock()
		if !c.Log {
			t.Errorf("OnChange(%+v), want Log set", c)
		}
		changes++
	}})

	logger := slog.New(m.LogHandler(slog.LevelInfo))
	logger.Info("hello", "key", "value")

	lines := m.LogLines()
	if len(lines) != 1 {
		t.Fatalf("LogLines() = %v, want 1 line", lines)
	}
	line := lines[0]
	if !strings.HasSuffix(line, " INFO hello key=value") {
		t.Errorf("line = %q, want suffix ' INFO hello key=value'", line)
	}
	if strings.Contains(line, "time=") || strings.Contains(line, "level=") {
		t.Errorf("line = %q, should not contain raw time= or level=", line)
	}
	// "15:04:05 " prefix.
	if len(line) < 9 || line[2] != ':' || line[5] != ':' || line[8] != ' ' {
		t.Errorf("line = %q, want a leading HH:MM:SS timestamp", line)
	}

	mu.Lock()
	defer mu.Unlock()
	if changes != 1 {
		t.Errorf("OnChange called %d times, want 1", changes)
	}
}

func TestLogHandlerBelowLevelDropped(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelWarn))
	logger.Info("should not appear")
	logger.Warn("should appear")

	lines := m.LogLines()
	if len(lines) != 1 {
		t.Fatalf("LogLines() = %v, want 1 line", lines)
	}
	if !strings.Contains(lines[0], "should appear") {
		t.Errorf("lines[0] = %q", lines[0])
	}
}

func TestLogHandlerNilLevelDefaultsInfo(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(nil))
	logger.Debug("hidden")
	logger.Info("shown")

	lines := m.LogLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "shown") {
		t.Errorf("LogLines() = %v, want only the Info line", lines)
	}
}

func TestLogHandlerWithAttrsAndGroup(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	base := m.LogHandler(slog.LevelInfo)
	logger := slog.New(base).With("a", "1").WithGroup("g").With("b", "2")
	logger.Info("msg", "c", "3")

	lines := m.LogLines()
	if len(lines) != 1 {
		t.Fatalf("LogLines() = %v, want 1 line", lines)
	}
	line := lines[0]
	if !strings.Contains(line, "a=1") {
		t.Errorf("line = %q, want a=1 (from outer With)", line)
	}
	if !strings.Contains(line, "g.b=2") {
		t.Errorf("line = %q, want g.b=2 (from WithGroup then With)", line)
	}
	if !strings.Contains(line, "g.c=3") {
		t.Errorf("line = %q, want g.c=3 (record attr under the active group)", line)
	}
}

func TestLogHandlerGroupedAttrValue(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelInfo))
	logger.Info("msg", slog.Group("req", slog.String("id", "abc"), slog.Int("n", 5)))

	line := m.LogLines()[0]
	if !strings.Contains(line, "req.id=abc") || !strings.Contains(line, "req.n=5") {
		t.Errorf("line = %q, want req.id=abc and req.n=5", line)
	}
}

func TestLogHandlerQuotesValuesNeedingIt(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelInfo))
	logger.Info("msg", "s", "has space", "e", "")

	line := m.LogLines()[0]
	if !strings.Contains(line, `s="has space"`) {
		t.Errorf("line = %q, want quoted value with a space", line)
	}
	if !strings.Contains(line, `e=""`) {
		t.Errorf("line = %q, want an empty value quoted", line)
	}
}

func TestLogHandlerWithAttrsEmptyGroupWritesNothing(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelInfo)).With("empty", slog.GroupValue(), "a", "1")
	logger.Info("msg")

	line := m.LogLines()[0]
	if !strings.HasSuffix(line, "msg a=1") {
		t.Errorf("line = %q, want the empty group to contribute nothing", line)
	}
}

func TestLogHandlerWithAttrsEmptyReturnsSame(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	h := m.LogHandler(slog.LevelInfo)
	if h.WithAttrs(nil) != h {
		t.Error("WithAttrs(nil) returned a different handler")
	}
	if h.WithGroup("") != h {
		t.Error("WithGroup(\"\") returned a different handler")
	}
}

func TestLogHandlerRingEviction(t *testing.T) {
	t.Parallel()
	m := New(Options{MaxLog: 3})
	logger := slog.New(m.LogHandler(slog.LevelInfo))
	logger.Info("one")
	logger.Info("two")
	logger.Info("three")
	logger.Info("four")

	lines := m.LogLines()
	if len(lines) != 3 {
		t.Fatalf("LogLines() = %v, want 3 lines", lines)
	}
	if strings.Contains(lines[0], "one") {
		t.Errorf("lines[0] = %q, want the oldest line evicted", lines[0])
	}
	if !strings.Contains(lines[2], "four") {
		t.Errorf("lines[2] = %q, want the newest line last", lines[2])
	}
}

func TestLogHandlerConcurrent(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelInfo))

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			logger.Info("concurrent", "i", i)
			_ = m.LogLines()
		})
	}
	wg.Wait()

	if got := len(m.LogLines()); got != 20 {
		t.Errorf("LogLines() len = %d, want 20", got)
	}
}

// TestLogHandlerRacesWithApply directly exercises the interaction the
// Model doc comment calls out by name: "LogHandler and Apply cannot race
// (verify with -race tests)". TestLogHandlerConcurrent only ever races
// LogHandler against itself; this races it against Apply, which is
// otherwise only ever called serially in the rest of the suite.
func TestLogHandlerRacesWithApply(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	logger := slog.New(m.LogHandler(slog.LevelInfo))

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			logger.Info("concurrent", "i", i)
		}
	})
	wg.Go(func() {
		for range 200 {
			m.Apply(engine.WatchState{Dir: "/w"})
		}
	})
	wg.Wait()

	if got := len(m.LogLines()); got != 200 {
		t.Errorf("LogLines() len = %d, want 200", got)
	}
}

func TestLogHandlerEnabledAndHandleDirect(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	h := m.LogHandler(slog.LevelWarn)
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Enabled(Info) = true, want false below LevelWarn")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled(Error) = false, want true")
	}
}
