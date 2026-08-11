package main

import (
	"os"
	"path/filepath"
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
