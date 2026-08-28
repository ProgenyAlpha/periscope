package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

const pluginManifestName = ".periscope-manifest.json"

// syncResult tallies what a plugin sync did.
type syncResult struct {
	written   int      // files created or updated to the embedded version
	adopted   int      // untracked files that matched the embedded version and started being tracked
	unchanged int      // periscope-owned files already matching the embedded version
	preserved []string // relative paths left alone because they diverge from what periscope would write
}

type pluginManifest map[string]string

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// errCorruptManifest marks a manifest file that exists but does not parse —
// the residue of a crash mid-write. It is kept distinct from an I/O error so
// syncFS can rebuild past it without also swallowing a genuinely unreadable
// directory (EACCES, EIO), which the user does need to hear about.
var errCorruptManifest = errors.New("plugin manifest is not valid JSON")

func loadPluginManifest(path string) (pluginManifest, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return pluginManifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := pluginManifest{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w (%s): %v", errCorruptManifest, path, err)
	}
	return m, nil
}

// savePluginManifest writes the manifest atomically: a temp file in the same
// directory, then a rename over the target.
//
// A torn manifest is not a small loss. It is the only record of which files
// periscope wrote, so without it every shipped file looks like one the user
// might have edited, and sync turns conservative forever: widget and theme
// updates quietly stop arriving. os.WriteFile truncates before it writes, so a
// crash — or merely a concurrent reader — in that window was enough to cause
// it. writeFileAtomic (sidecarwrite.go) already does the temp+fsync+rename
// dance and preserves an existing file's mode, so this reuses it rather than
// growing a second copy; the MkdirAll mirrors writeSettingsFile
// (claudesettings.go), since a manifest whose parent does not exist yet must
// not fail the sync that just created the plugin tree.
//
// The temp file writeFileAtomic creates is named "..periscope-manifest.json.tmpNNN".
// It cannot be mistaken for a plugin: syncFS walks the *embedded* source tree,
// not pluginDir, and the /plugins listing in server.go only reads the
// themes/, widgets/, ... subdirectories, never the plugin root where this
// lives. The name is dot-prefixed and does not end in .json regardless.
//
// An unchanged manifest is not rewritten at all. Re-serializing identical
// bytes buys nothing and only widens the window in which a crash could damage
// the file, and `periscope sync` on an up-to-date install is the common case.
func savePluginManifest(path string, m pluginManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	return writeFileAtomic(path, data, 0644)
}

// syncPlugins updates periscope-owned files in pluginDir from the embedded
// defaults while leaving user-edited files alone. See pluginManifest: a file
// is periscope-owned only while its on-disk hash matches the hash periscope
// last wrote for it.
func syncPlugins(pluginDir string) (syncResult, error) {
	return syncFS(pluginDir, defaultPlugins, "defaults")
}

func syncFS(pluginDir string, source fs.FS, root string) (syncResult, error) {
	manifestPath := filepath.Join(pluginDir, pluginManifestName)
	manifest, err := loadPluginManifest(manifestPath)
	if errors.Is(err, errCorruptManifest) {
		// Refusing to sync would be the worse failure. The manifest is
		// unparseable on every subsequent run too, so `periscope sync` and
		// `periscope init` would abort here forever and the user would never
		// receive another update — with nothing but a JSON error to explain
		// it. Start from empty and let the walk below rebuild: every file
		// still matching what we ship is re-adopted and tracked again, and
		// anything that diverges is preserved exactly as it would be on a
		// first-ever sync. Nothing is overwritten on the strength of a
		// manifest we could not read.
		slog.Warn("plugin manifest unreadable, rebuilding it", "path", manifestPath, "err", err)
		manifest, err = pluginManifest{}, nil
	}
	if err != nil {
		return syncResult{}, err
	}

	var result syncResult
	walkErr := fs.WalkDir(source, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dest := filepath.Join(pluginDir, rel)

		embedded, err := fs.ReadFile(source, path)
		if err != nil {
			return err
		}
		embeddedHash := hashBytes(embedded)

		onDisk, err := os.ReadFile(dest)
		switch {
		case os.IsNotExist(err):
			if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(dest, embedded, 0644); err != nil {
				return err
			}
			manifest[rel] = embeddedHash
			result.written++
			return nil
		case err != nil:
			return err
		}

		diskHash := hashBytes(onDisk)
		knownHash, tracked := manifest[rel]

		if !tracked {
			// No provenance for this file. Only start tracking it if its
			// content already matches what periscope would write — never
			// overwrite a file periscope has no history for.
			if diskHash == embeddedHash {
				manifest[rel] = diskHash
				result.adopted++
			} else {
				result.preserved = append(result.preserved, rel)
			}
			return nil
		}

		if diskHash != knownHash {
			// User touched a periscope-owned file since the last sync.
			result.preserved = append(result.preserved, rel)
			return nil
		}

		if diskHash == embeddedHash {
			result.unchanged++
			return nil
		}

		if err := os.WriteFile(dest, embedded, 0644); err != nil {
			return err
		}
		manifest[rel] = embeddedHash
		result.written++
		return nil
	})
	if walkErr != nil {
		return syncResult{}, walkErr
	}

	if err := savePluginManifest(manifestPath, manifest); err != nil {
		return syncResult{}, err
	}
	return result, nil
}
