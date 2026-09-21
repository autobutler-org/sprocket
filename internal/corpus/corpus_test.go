package corpus_test

import (
	"slices"
	"testing"

	"github.com/autobutler-org/sprocket/internal/corpus"
)

func TestFilesLoadAndLookSane(t *testing.T) {
	names, err := corpus.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("Files returned nothing; the corpus should be committed")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file, golden, err := corpus.Open(name)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer file.Close()

			info, err := file.Stat()
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if info.Size() == 0 {
				t.Error("media file is empty")
			}

			if golden.Duration <= 0 {
				t.Errorf("duration = %v, want > 0", golden.Duration)
			}
			if golden.Width <= 0 || golden.Height <= 0 {
				t.Errorf("dimensions = %dx%d, want both > 0", golden.Width, golden.Height)
			}
			if golden.VideoCodec == "" {
				t.Error("video codec is empty")
			}
			if golden.Bitrate <= 0 {
				t.Errorf("bitrate = %d, want > 0", golden.Bitrate)
			}
			if golden.Framerate <= 0 {
				t.Errorf("framerate = %v, want > 0", golden.Framerate)
			}
			if !slices.Contains([]int{0, 90, 180, 270}, golden.Rotation) {
				t.Errorf("rotation = %d, want one of 0, 90, 180, 270", golden.Rotation)
			}
		})
	}
}
