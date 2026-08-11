package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
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
		return nil, err
	}
	return m, nil
}

func savePluginManifest(path string, m pluginManifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
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
