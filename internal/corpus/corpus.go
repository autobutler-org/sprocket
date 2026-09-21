// Package corpus reads the generated media test corpus and its golden probe
// results. See testdata/corpus/README.md for what the corpus holds and how the
// goldens are produced.
package corpus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
)

// Golden is the expected probe result committed alongside a corpus file.
// Rotation is clockwise degrees, one of 0, 90, 180, and 270; Width and Height
// are the stored dimensions, before rotation. AudioCodec is empty when the file
// has no audio track.
type Golden struct {
	Duration   float64 `json:"duration"`
	Width      int     `json:"width"`
	Height     int     `json:"height"`
	VideoCodec string  `json:"video_codec"`
	AudioCodec string  `json:"audio_codec"`
	Bitrate    int     `json:"bitrate"`
	Framerate  float64 `json:"framerate"`
	Rotation   int     `json:"rotation"`
}

// notMedia are the extensions of the corpus directory's bookkeeping, as opposed
// to its media files.
var notMedia = []string{".json", ".md", ".sh"}

// Dir returns the corpus directory. It is resolved from this file's own path, so
// it does not depend on the working directory of the test that calls it.
func Dir() string {
	const thisFileToRepoRoot = "../.."
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), thisFileToRepoRoot, "testdata", "corpus")
}

// Files returns the names of the media files in the corpus, sorted.
func Files() ([]string, error) {
	entries, err := os.ReadDir(Dir())
	if err != nil {
		return nil, fmt.Errorf("read corpus dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || slices.Contains(notMedia, filepath.Ext(entry.Name())) {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// Open opens the named corpus media file and loads its golden probe result. The
// caller closes the file.
func Open(name string) (*os.File, Golden, error) {
	var golden Golden

	raw, err := os.ReadFile(filepath.Join(Dir(), goldenName(name)))
	if err != nil {
		return nil, golden, fmt.Errorf("read golden for %s: %w", name, err)
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		return nil, golden, fmt.Errorf("parse golden for %s: %w", name, err)
	}

	file, err := os.Open(filepath.Join(Dir(), name))
	if err != nil {
		return nil, golden, fmt.Errorf("open corpus file: %w", err)
	}
	return file, golden, nil
}

// goldenName maps a media file name to the name of its golden JSON.
func goldenName(media string) string {
	return media[:len(media)-len(filepath.Ext(media))] + ".json"
}
