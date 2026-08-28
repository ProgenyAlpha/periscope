package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func TestSyncFS_FreshDirectory(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{
		"themes/arctic.toml":   &fstest.MapFile{Data: []byte("theme v1")},
		"widgets/session.html": &fstest.MapFile{Data: []byte("widget v1")},
	}

	result, err := syncFS(dir, source, ".")
	if err != nil {
		t.Fatalf("syncFS: %v", err)
	}
	if result.written != 2 {
		t.Errorf("written = %d, want 2", result.written)
	}
	if result.adopted != 0 || result.unchanged != 0 || len(result.preserved) != 0 {
		t.Errorf("unexpected extra activity: %+v", result)
	}

	for rel, f := range source {
		got := mustReadFile(t, filepath.Join(dir, rel))
		if string(got) != string(f.Data) {
			t.Errorf("%s content = %q, want %q", rel, got, f.Data)
		}
	}

	manifest, err := loadPluginManifest(filepath.Join(dir, pluginManifestName))
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}
	for rel, f := range source {
		if manifest[rel] != hashBytes(f.Data) {
			t.Errorf("manifest[%s] = %s, want hash of embedded content", rel, manifest[rel])
		}
	}
	if _, ok := manifest[pluginManifestName]; ok {
		t.Errorf("manifest must not track itself")
	}
}

func TestSyncFS_OwnedFileUpdatedWhenEmbeddedChanges(t *testing.T) {
	dir := t.TempDir()
	v1 := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("v1")}}
	if _, err := syncFS(dir, v1, "."); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	v2 := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("v2")}}
	result, err := syncFS(dir, v2, ".")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if result.written != 1 {
		t.Errorf("written = %d, want 1", result.written)
	}
	got := mustReadFile(t, filepath.Join(dir, "widgets/foo.html"))
	if string(got) != "v2" {
		t.Errorf("content = %q, want v2", got)
	}
	manifest, _ := loadPluginManifest(filepath.Join(dir, pluginManifestName))
	if manifest["widgets/foo.html"] != hashBytes([]byte("v2")) {
		t.Errorf("manifest hash not updated to v2")
	}
}

func TestSyncFS_UserEditedFileNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	v1 := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("v1")}}
	if _, err := syncFS(dir, v1, "."); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	path := filepath.Join(dir, "widgets/foo.html")
	if err := os.WriteFile(path, []byte("user edit"), 0644); err != nil {
		t.Fatalf("hand-edit: %v", err)
	}

	v2 := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("v2")}}
	result, err := syncFS(dir, v2, ".")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got := mustReadFile(t, path)
	if string(got) != "user edit" {
		t.Errorf("content = %q, want user edit to survive", got)
	}
	if len(result.preserved) != 1 || result.preserved[0] != "widgets/foo.html" {
		t.Errorf("preserved = %v, want [widgets/foo.html]", result.preserved)
	}
	if result.written != 0 {
		t.Errorf("written = %d, want 0", result.written)
	}
}

func TestSyncFS_UntrackedMatchingFileAdopted(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("same content")}}

	if err := os.MkdirAll(filepath.Join(dir, "widgets"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "widgets/foo.html")
	if err := os.WriteFile(path, []byte("same content"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	result, err := syncFS(dir, source, ".")
	if err != nil {
		t.Fatalf("syncFS: %v", err)
	}
	if result.adopted != 1 {
		t.Errorf("adopted = %d, want 1", result.adopted)
	}
	if result.written != 0 {
		t.Errorf("written = %d, want 0 (must not rewrite the file)", result.written)
	}

	got := mustReadFile(t, path)
	if string(got) != "same content" {
		t.Errorf("content changed: %q", got)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !stat.ModTime().Equal(past) {
		t.Errorf("mtime changed to %v, want unchanged %v (file was rewritten)", stat.ModTime(), past)
	}

	manifest, _ := loadPluginManifest(filepath.Join(dir, pluginManifestName))
	if manifest["widgets/foo.html"] != hashBytes([]byte("same content")) {
		t.Errorf("adopted file not recorded in manifest")
	}
}

func TestSyncFS_UntrackedDivergingFileLeftAlone(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("embedded content")}}

	if err := os.MkdirAll(filepath.Join(dir, "widgets"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "widgets/foo.html")
	if err := os.WriteFile(path, []byte("pre-existing user content"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	result, err := syncFS(dir, source, ".")
	if err != nil {
		t.Fatalf("syncFS: %v", err)
	}
	if len(result.preserved) != 1 || result.preserved[0] != "widgets/foo.html" {
		t.Errorf("preserved = %v, want [widgets/foo.html]", result.preserved)
	}
	if result.written != 0 || result.adopted != 0 {
		t.Errorf("written/adopted should be 0, got written=%d adopted=%d", result.written, result.adopted)
	}

	got := mustReadFile(t, path)
	if string(got) != "pre-existing user content" {
		t.Errorf("content changed: %q", got)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !stat.ModTime().Equal(past) {
		t.Errorf("mtime changed to %v, want unchanged %v (file was rewritten)", stat.ModTime(), past)
	}

	manifest, _ := loadPluginManifest(filepath.Join(dir, pluginManifestName))
	if _, tracked := manifest["widgets/foo.html"]; tracked {
		t.Errorf("diverging untracked file must stay untracked, got manifest entry")
	}
}

func TestSyncFS_UserAddedFileNeverDeleted(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{"widgets/foo.html": &fstest.MapFile{Data: []byte("v1")}}
	if _, err := syncFS(dir, source, "."); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	extraPath := filepath.Join(dir, "widgets", "user-added.html")
	if err := os.WriteFile(extraPath, []byte("mine, not periscope's"), 0644); err != nil {
		t.Fatalf("write extra file: %v", err)
	}

	if _, err := syncFS(dir, source, "."); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	got := mustReadFile(t, extraPath)
	if string(got) != "mine, not periscope's" {
		t.Errorf("user-added file changed: %q", got)
	}
}

func TestSyncFS_SecondRunIsNoOp(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{
		"themes/arctic.toml":   &fstest.MapFile{Data: []byte("theme v1")},
		"widgets/session.html": &fstest.MapFile{Data: []byte("widget v1")},
	}
	if _, err := syncFS(dir, source, "."); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	result, err := syncFS(dir, source, ".")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if result.written != 0 || result.adopted != 0 || len(result.preserved) != 0 {
		t.Errorf("second run should be a no-op, got %+v", result)
	}
	if result.unchanged != len(source) {
		t.Errorf("unchanged = %d, want %d", result.unchanged, len(source))
	}
}

// --- manifest durability ---
//
// The manifest is what tells a later sync "this file is still ours, overwrite
// it" from "the user edited this, leave it". Losing or tearing it costs the
// user every future widget and theme update, so the write has to be atomic and
// the recovery from an already-torn file has to be a rebuild, not a hard stop.

// bigManifest builds a manifest large enough that a truncate-then-write is
// visibly non-instant to a concurrent reader.
func bigManifest(tag string) pluginManifest {
	m := pluginManifest{}
	for i := 0; i < 3000; i++ {
		rel := fmt.Sprintf("widgets/%s-%04d.html", tag, i)
		m[rel] = hashBytes([]byte(rel))
	}
	return m
}

func TestSavePluginManifest_ReadersNeverSeeAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, pluginManifestName)

	alpha, beta := bigManifest("alpha"), bigManifest("beta")
	if err := savePluginManifest(path, alpha); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	var (
		wg    sync.WaitGroup
		stop  = make(chan struct{})
		reads atomic.Int64
		torn  atomic.Int64
		first atomic.Value // string: what the first torn read looked like
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(path)
				if err != nil {
					// The manifest must never even briefly vanish.
					torn.Add(1)
					first.CompareAndSwap(nil, "read error: "+err.Error())
					continue
				}
				reads.Add(1)
				m := pluginManifest{}
				if err := json.Unmarshal(data, &m); err != nil {
					torn.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("unparseable %d-byte read: %v", len(data), err))
					continue
				}
				if len(m) != len(alpha) {
					torn.Add(1)
					first.CompareAndSwap(nil, fmt.Sprintf("short manifest: %d entries, want %d", len(m), len(alpha)))
				}
			}
		}()
	}

	for i := 0; i < 40; i++ {
		m := alpha
		if i%2 == 1 {
			m = beta
		}
		if err := savePluginManifest(path, m); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("rewrite %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if reads.Load() == 0 {
		t.Fatal("no concurrent reads happened; the test proved nothing")
	}
	if n := torn.Load(); n > 0 {
		t.Errorf("%d of %d reads observed a partial manifest (first: %v); the write is not atomic",
			n, reads.Load()+n, first.Load())
	}
}

// dirNames lists the plain names in dir, so a stray temp file is visible.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestSavePluginManifest_LeavesNoTempFile(t *testing.T) {
	m := pluginManifest{"widgets/foo.html": hashBytes([]byte("v1"))}

	t.Run("after success", func(t *testing.T) {
		dir := t.TempDir()
		if err := savePluginManifest(filepath.Join(dir, pluginManifestName), m); err != nil {
			t.Fatalf("savePluginManifest: %v", err)
		}
		got := dirNames(t, dir)
		if len(got) != 1 || got[0] != pluginManifestName {
			t.Errorf("dir contains %v, want only %s", got, pluginManifestName)
		}
	})

	t.Run("after failure", func(t *testing.T) {
		dir := t.TempDir()
		// A directory where the manifest belongs: the final rename cannot
		// succeed, so this exercises the error path after a temp file exists.
		path := filepath.Join(dir, pluginManifestName)
		if err := os.MkdirAll(filepath.Join(path, "blocker"), 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := savePluginManifest(path, m); err == nil {
			t.Fatal("savePluginManifest onto a directory: want error, got nil")
		}
		got := dirNames(t, dir)
		if len(got) != 1 || got[0] != pluginManifestName {
			t.Errorf("dir contains %v after a failed write, want only %s (a temp file leaked)",
				got, pluginManifestName)
		}
	})
}

func TestSavePluginManifest_PreservesFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file modes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, pluginManifestName)

	if err := savePluginManifest(path, pluginManifest{"a": hashBytes([]byte("a"))}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := savePluginManifest(path, pluginManifest{"a": hashBytes([]byte("a2"))}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0600 {
		t.Errorf("mode = %v after rewrite, want 0600 preserved", st.Mode().Perm())
	}
}

func TestSyncFS_CorruptManifestIsRebuilt(t *testing.T) {
	dir := t.TempDir()
	v1 := fstest.MapFS{
		"themes/arctic.toml":   &fstest.MapFile{Data: []byte("theme v1")},
		"widgets/session.html": &fstest.MapFile{Data: []byte("widget v1")},
	}
	if _, err := syncFS(dir, v1, "."); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	// A crash mid-write leaves a truncated manifest behind.
	manifestPath := filepath.Join(dir, pluginManifestName)
	full := mustReadFile(t, manifestPath)
	if err := os.WriteFile(manifestPath, full[:len(full)/2], 0644); err != nil {
		t.Fatalf("truncate manifest: %v", err)
	}

	// Sync must recover rather than abort, and must re-adopt the files that
	// still match what periscope ships.
	result, err := syncFS(dir, v1, ".")
	if err != nil {
		t.Fatalf("sync over a corrupt manifest: %v (it must rebuild, not fail)", err)
	}
	if result.adopted != 2 {
		t.Errorf("adopted = %d, want 2 (both untouched files re-adopted)", result.adopted)
	}
	if len(result.preserved) != 0 {
		t.Errorf("preserved = %v, want none: no file was user-edited", result.preserved)
	}

	rebuilt, err := loadPluginManifest(manifestPath)
	if err != nil {
		t.Fatalf("manifest still unparseable after sync: %v", err)
	}
	if len(rebuilt) != 2 {
		t.Errorf("rebuilt manifest has %d entries, want 2", len(rebuilt))
	}

	// The real damage a corrupt manifest does is permanent: updates stop
	// arriving forever. Prove they arrive again.
	v2 := fstest.MapFS{
		"themes/arctic.toml":   &fstest.MapFile{Data: []byte("theme v2")},
		"widgets/session.html": &fstest.MapFile{Data: []byte("widget v2")},
	}
	result, err = syncFS(dir, v2, ".")
	if err != nil {
		t.Fatalf("sync after rebuild: %v", err)
	}
	if result.written != 2 {
		t.Errorf("written = %d, want 2; updates are still blocked (preserved=%v)",
			result.written, result.preserved)
	}
	if got := string(mustReadFile(t, filepath.Join(dir, "themes/arctic.toml"))); got != "theme v2" {
		t.Errorf("theme content = %q, want theme v2", got)
	}
}

func TestSyncFS_UnchangedManifestIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	source := fstest.MapFS{
		"themes/arctic.toml":   &fstest.MapFile{Data: []byte("theme v1")},
		"widgets/session.html": &fstest.MapFile{Data: []byte("widget v1")},
	}
	if _, err := syncFS(dir, source, "."); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	manifestPath := filepath.Join(dir, pluginManifestName)
	before := mustReadFile(t, manifestPath)
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(manifestPath, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if _, err := syncFS(dir, source, "."); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	st, err := os.Stat(manifestPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.ModTime().Equal(past) {
		t.Errorf("manifest mtime changed to %v, want %v: an unchanged manifest must not be rewritten",
			st.ModTime(), past)
	}
	if got := mustReadFile(t, manifestPath); string(got) != string(before) {
		t.Errorf("manifest content changed across a no-op sync")
	}
}
