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

package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"msrl.dev/mink-lasso/internal/masso"
	"msrl.dev/mink-lasso/internal/watch"
)

// takeQueued removes and returns every event queued on e's dispatcher,
// which a never-Run engine leaves undelivered.
func takeQueued(t *testing.T, e *Engine) []TransferEvent {
	t.Helper()
	e.dispatcher.mu.Lock()
	queued := e.dispatcher.queue
	e.dispatcher.queue = nil
	e.dispatcher.mu.Unlock()
	out := make([]TransferEvent, 0, len(queued))
	for _, ev := range queued {
		out = append(out, asTransferEvent(t, ev))
	}
	return out
}

func TestToUploadDir(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("D", 128)
	tests := []struct {
		dir  string
		sep  rune
		want string
	}{
		{"", '/', ""},
		{"JOBS", '/', "JOBS"},
		{"JOBS/SUB", '/', `JOBS\SUB`},
		{`JOBS\SUB`, '\\', `JOBS\SUB`},
		{long + "/" + strings.Repeat("E", 126), '/', long + `\` + strings.Repeat("E", 126)},
	}
	for _, tt := range tests {
		got, err := toUploadDir(tt.dir, tt.sep)
		if err != nil || got != tt.want {
			t.Errorf("toUploadDir(%q, %q) = (%q, %v), want (%q, nil)", tt.dir, tt.sep, got, err, tt.want)
		}
	}
}

func TestToUploadDirRejects(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("D", 128)
	tests := []struct {
		dir    string
		sep    rune
		reason string
	}{
		{`A\B`, '/', "a backslash inside a folder name"},
		{`JOBS/A\B`, '/', "a backslash inside a nested folder name"},
		{"JO:BS", '/', "a colon"},
		{"JO:BS", '\\', "a colon, Windows separator"},
		{long + "/" + long, '/', "longer than masso.MaxUploadDir"},
	}
	for _, tt := range tests {
		got, err := toUploadDir(tt.dir, tt.sep)
		if !errors.Is(err, masso.ErrBadUploadDir) {
			t.Errorf("toUploadDir(%q, %q) [%s] = (%q, %v), want ErrBadUploadDir", tt.dir, tt.sep, tt.reason, got, err)
		}
	}
}

func TestUploadDirNative(t *testing.T) {
	t.Parallel()
	got, err := uploadDir(filepath.Join("JOBS", "SUB"))
	if err != nil || got != `JOBS\SUB` {
		t.Errorf(`uploadDir(JOBS/SUB) = (%q, %v), want ("JOBS\\SUB", nil)`, got, err)
	}
	if got, err := uploadDir(""); err != nil || got != "" {
		t.Errorf(`uploadDir("") = (%q, %v), want ("", nil)`, got, err)
	}
}

func TestUploadDirBackslashInFolderName(t *testing.T) {
	t.Parallel()
	got, err := uploadDir(`A\B`)
	if runtime.GOOS == "windows" {
		// A backslash is Windows' own separator: two folders.
		if err != nil || got != `A\B` {
			t.Errorf(`uploadDir("A\\B") = (%q, %v), want ("A\\B", nil)`, got, err)
		}
		return
	}
	if !errors.Is(err, masso.ErrBadUploadDir) {
		t.Errorf(`uploadDir("A\\B") = (%q, %v), want ErrBadUploadDir`, got, err)
	}
}

func TestValidateUpload(t *testing.T) {
	t.Parallel()
	if err := validateUpload(filepath.Join("JOBS", "SUB"), "PART.NC"); err != nil {
		t.Errorf("validateUpload(JOBS/SUB, PART.NC) = %v, want nil", err)
	}
	if err := validateUpload("", "PART.NC"); err != nil {
		t.Errorf("validateUpload(root, PART.NC) = %v, want nil", err)
	}
	if err := validateUpload("JOBS", "THIS-NAME-IS-TOO-LONG.NC"); !errors.Is(err, masso.ErrBadFileName) {
		t.Errorf("validateUpload with a bad name = %v, want ErrBadFileName", err)
	}
	if err := validateUpload("JO:BS", "PART.NC"); !errors.Is(err, masso.ErrBadUploadDir) {
		t.Errorf("validateUpload with a bad folder = %v, want ErrBadUploadDir", err)
	}
}

func TestOpenForSendBadDir(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	it := &item{name: filepath.Join("JO:BS", "A.NC"), dir: "JO:BS", base: "A.NC", path: "/dev/null"}
	if _, _, err := e.scheduler.openForSend(it); !errors.Is(err, masso.ErrBadUploadDir) {
		t.Errorf("openForSend error = %v, want ErrBadUploadDir", err)
	}
}

func TestReadyKeysByRelativePath(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	sub := filepath.Join("JOBS", "SUB")
	key := filepath.Join(sub, "PART.NC")

	e.scheduler.ready("/watch", watch.File{Name: "PART.NC", Dir: sub, Path: "/watch/JOBS/SUB/PART.NC", Size: 1})
	e.scheduler.ready("/watch", watch.File{Name: "PART.NC", Path: "/watch/PART.NC", Size: 2})

	evs := takeQueued(t, e)
	if len(evs) != 2 || evs[0].Name != key || evs[0].State != Pending || evs[1].Name != "PART.NC" {
		t.Errorf("events = %+v, want %q then PART.NC, both Pending", evs, key)
	}

	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if n := len(e.scheduler.items); n != 2 {
		t.Fatalf("queue has %d items, want 2 (one per relative path)", n)
	}
	it := e.scheduler.items[key]
	if it == nil {
		t.Fatalf("no item for %q", key)
	}
	if it.root != "/watch" || it.dir != sub || it.base != "PART.NC" {
		t.Errorf("item = (root %q, dir %q, base %q), want (/watch, %q, PART.NC)", it.root, it.dir, it.base, sub)
	}
}

func TestRetryByRelativePath(t *testing.T) {
	t.Parallel()
	e := newUnitTestEngine(t, nil)
	key := filepath.Join("JOBS", "BAD.NC")

	e.scheduler.rejected("/watch", watch.Rejected{Name: "BAD.NC", Dir: "JOBS", Path: "/watch/JOBS/BAD.NC", Reason: "bad"})
	if evs := takeQueued(t, e); len(evs) != 1 || evs[0].Name != key || evs[0].State != Rejected {
		t.Fatalf("events = %+v, want %q Rejected", evs, key)
	}

	e.scheduler.retry("BAD.NC")
	if evs := takeQueued(t, e); len(evs) != 0 {
		t.Errorf("events after Retry by base name = %+v, want none", evs)
	}
	e.scheduler.retry(key)
	if evs := takeQueued(t, e); len(evs) != 1 || evs[0].Name != key || evs[0].State != Pending {
		t.Errorf("events after Retry = %+v, want %q Pending", evs, key)
	}
}

// uploadRecorder is a fakeClient whose Upload records what it was asked to
// send, then runs during (if set) before succeeding.
type uploadRecorder struct {
	fakeClient

	mu     sync.Mutex
	dir    string
	name   string
	data   []byte
	during func()
}

func (c *uploadRecorder) Upload(
	_ context.Context, dir, name string, r io.ReaderAt, size int64, _ func(int64, int64),
) error {
	data, err := io.ReadAll(io.NewSectionReader(r, 0, size))
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.dir, c.name, c.data = dir, name, data
	during := c.during
	c.mu.Unlock()
	if during != nil {
		during()
	}
	return nil
}

func (c *uploadRecorder) got() (dir, name string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dir, c.name, c.data
}

// subdirSendEngine returns an Engine (never Run) with an uploadRecorder and
// the real filesystem, and the item for JOBS/SUB/PART.NC in a new watch
// folder (it.root), queued as the watcher would report it.
func subdirSendEngine(t *testing.T) (e *Engine, client *uploadRecorder, it *item) {
	t.Helper()
	root := t.TempDir()
	sub := filepath.Join("JOBS", "SUB")
	if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := writeFile(t, filepath.Join(root, sub), "PART.NC", []byte("new"))

	e = newUnitTestEngine(t, os.Open)
	client = &uploadRecorder{}
	e.client = client
	e.scheduler.ready(root, watch.File{Name: "PART.NC", Dir: sub, Path: path, Size: 3})
	takeQueued(t, e)
	return e, client, e.scheduler.items[filepath.Join(sub, "PART.NC")]
}

func TestSendOneUploadsIntoSubfolderAndArchivesNested(t *testing.T) {
	t.Parallel()
	e, client, it := subdirSendEngine(t)
	root := it.root
	nestedSent := filepath.Join(root, "sent", "JOBS", "SUB")
	if err := os.MkdirAll(nestedSent, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeFile(t, nestedSent, "PART.NC", []byte("old"))

	e.scheduler.sendOne(context.Background(), it)

	if dir, name, data := client.got(); dir != `JOBS\SUB` || name != "PART.NC" || string(data) != "new" {
		t.Errorf("Upload(%q, %q) sent %q, want (JOBS\\SUB, PART.NC) sending %q", dir, name, data, "new")
	}
	if it.state != Sent {
		t.Errorf("state = %v, want Sent", it.state)
	}
	if data, err := os.ReadFile(filepath.Join(nestedSent, "PART.NC")); err != nil || string(data) != "new" {
		t.Errorf("sent/JOBS/SUB/PART.NC = (%q, %v), want the sent file", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "JOBS", "SUB", "PART.NC")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("JOBS/SUB/PART.NC still in the watch folder (Stat error %v), want moved", err)
	}
	entries, err := os.ReadDir(nestedSent)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("sent/JOBS/SUB has %d entries, want the sent file plus the timestamped backup", len(entries))
	}
}

func TestSendOneOrphanArchivesUnderItsOwnRoot(t *testing.T) {
	t.Parallel()
	e, client, it := subdirSendEngine(t)
	root := it.root
	newRoot := t.TempDir()
	e.scheduler.mu.Lock()
	it.sending = true
	e.scheduler.mu.Unlock()
	client.during = func() {
		if err := e.SetWatchDir(newRoot); err != nil {
			t.Errorf("SetWatchDir: %v", err)
		}
		e.scheduler.mu.Lock()
		orphaned := it.orphaned
		e.scheduler.mu.Unlock()
		if !orphaned {
			t.Error("in-flight item not marked orphaned by SetWatchDir")
		}
	}

	e.scheduler.sendOne(context.Background(), it)

	if _, err := os.Stat(filepath.Join(root, "sent", "JOBS", "SUB", "PART.NC")); err != nil {
		t.Errorf("orphaned file not archived under its own watch folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(newRoot, "sent")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("new watch folder has a sent folder (Stat error %v), want none", err)
	}
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	if len(e.scheduler.items) != 0 {
		t.Error("orphaned item still queued after its send, want dropped")
	}
}

func TestNewFolderFileReplacesOrphan(t *testing.T) {
	t.Parallel()
	e, client, it := subdirSendEngine(t)
	root := it.root
	sub := filepath.Join("JOBS", "SUB")
	key := filepath.Join(sub, "PART.NC")
	newRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(newRoot, sub), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	newPath := writeFile(t, filepath.Join(newRoot, sub), "PART.NC", []byte("other"))
	e.scheduler.mu.Lock()
	it.sending = true
	e.scheduler.mu.Unlock()
	client.during = func() {
		if err := e.SetWatchDir(newRoot); err != nil {
			t.Errorf("SetWatchDir: %v", err)
		}
		e.scheduler.ready(newRoot, watch.File{Name: "PART.NC", Dir: sub, Path: newPath, Size: 5})
	}

	e.scheduler.sendOne(context.Background(), it)

	if _, err := os.Stat(filepath.Join(root, "sent", "JOBS", "SUB", "PART.NC")); err != nil {
		t.Errorf("orphaned file not archived under its own watch folder: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Errorf("new folder's file moved (Stat error %v), want left for its own send", err)
	}
	if evs := takeQueued(t, e); len(evs) == 0 || evs[len(evs)-1].Name != key || evs[len(evs)-1].State != Pending {
		t.Errorf("events = %+v, want the replacement's Pending last (a superseded orphan must not overwrite its row)", evs)
	}
	e.scheduler.mu.Lock()
	defer e.scheduler.mu.Unlock()
	got := e.scheduler.items[key]
	if len(e.scheduler.items) != 1 || got == nil || got == it {
		t.Fatalf("queue = %v, want one new item for %q", e.scheduler.items, key)
	}
	if got.root != newRoot || got.path != newPath || got.state != Pending || got.sending || got.resendAfter {
		t.Errorf("item = (root %q, path %q, state %v, sending %v, resendAfter %v), want (%q, %q, Pending, false, false)",
			got.root, got.path, got.state, got.sending, got.resendAfter, newRoot, newPath)
	}
}

func TestRetryOfInvalidFileRejectsAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dir, name string
		want      error
	}{
		{"30\u00b0", "A.NC", masso.ErrBadUploadDir},
		{"", "THIS-NAME-IS-TOO-LONG.NC", masso.ErrBadFileName},
	} {
		t.Run(tc.want.Error(), func(t *testing.T) {
			t.Parallel()
			e := newUnitTestEngine(t, os.Open)
			key := filepath.Join(tc.dir, tc.name)
			e.scheduler.rejected("/watch", watch.Rejected{
				Name: tc.name, Dir: tc.dir, Path: filepath.Join("/watch", key), Reason: "bad",
			})
			e.scheduler.retry(key)
			e.scheduler.mu.Lock()
			it := e.scheduler.items[key]
			it.sending = true
			e.scheduler.mu.Unlock()
			takeQueued(t, e)

			e.scheduler.sendOne(context.Background(), it)

			evs := takeQueued(t, e)
			if len(evs) != 1 || evs[0].Name != key || evs[0].State != Rejected ||
				!strings.Contains(evs[0].Message, tc.want.Error()) {
				t.Errorf("events = %+v, want %q Rejected for %v", evs, key, tc.want)
			}
			e.scheduler.mu.Lock()
			defer e.scheduler.mu.Unlock()
			if it.sending || !it.nextAttempt.IsZero() {
				t.Errorf("item sending %v, nextAttempt %v, want false and zero", it.sending, it.nextAttempt)
			}
		})
	}
}

func TestArchiveSentUsesSendSource(t *testing.T) {
	t.Parallel()
	src := sendSource{
		root: "/watch", dir: filepath.Join("JOBS", "SUB"), base: "PART.NC",
		path: filepath.Join("/watch", "JOBS", "SUB", "PART.NC"),
	}

	var mkdirs, renames []string
	e := archiveTestEngine(t, Options{
		MkdirAll: func(p string, _ os.FileMode) error { mkdirs = append(mkdirs, p); return nil },
		Stat: func(path string) (os.FileInfo, error) {
			if path == src.path {
				return fakeFileInfo{}, nil // unchanged since it was sent
			}
			return nil, os.ErrNotExist // the sent/ destination doesn't exist yet
		},
		Rename: func(o, n string) error { renames = append(renames, o+" -> "+n); return nil },
	})
	it := &item{name: "stale", root: "/other", dir: "OTHER", base: "OTHER.NC", path: "/other/OTHER/OTHER.NC"}

	e.scheduler.archiveSent(context.Background(), it, src)

	wantDir := filepath.Join("/watch", "sent", "JOBS", "SUB")
	if len(mkdirs) != 1 || mkdirs[0] != wantDir {
		t.Errorf("MkdirAll calls = %q, want [%q]", mkdirs, wantDir)
	}
	wantRename := src.path + " -> " + filepath.Join(wantDir, "PART.NC")
	if len(renames) != 1 || renames[0] != wantRename {
		t.Errorf("Rename calls = %q, want [%q]", renames, wantRename)
	}
}
