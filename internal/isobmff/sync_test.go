package isobmff

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/autobutler-org/sprocket/internal/corpus"
)

// withSyncSamples rewrites a corpus track's sync sample table to name the given
// zero-based sample indexes. Every corpus file holds one keyframe, at sample
// zero, so a table with more than one has to be planted.
func withSyncSamples(t *testing.T, name string, indexes ...uint32) *File {
	t.Helper()

	file, _, _ := openCorpus(t, name)
	table := make([]byte, 0, 4*len(indexes))
	for _, index := range indexes {
		table = append(table, put32(index+1)...) // stss is one-based
	}
	file.VideoTrack().tables.stss = table
	return file
}

// movieTimeOf returns the earliest movie time that maps to a media tick, which
// is what a test needs to ask for a time exactly halfway between two keyframes.
// It searches rather than inverting the edit list, because the tick a duration
// maps to is truncated and the inverse of a truncation is a range.
func movieTimeOf(t *testing.T, file *File, ticks uint64) time.Duration {
	t.Helper()

	track := file.VideoTrack()
	limit := int(ticksToDuration(ticks, track.Timescale) + time.Second)
	at := time.Duration(sort.Search(limit, func(i int) bool {
		return track.mediaTicks(time.Duration(i), file.Timescale) >= ticks
	}))
	if got := track.mediaTicks(at, file.Timescale); got != ticks {
		t.Fatalf("the earliest movie time at or past %d media ticks is %v, which is %d ticks", ticks, at, got)
	}
	return at
}

func TestReadNearestSyncSamplePicksTheCloserKeyframe(t *testing.T) {
	// Two keyframes a second apart, planted: every corpus file ships with one.
	const second = 24 // sample index, at 24 fps

	file := withSyncSamples(t, "h264-aac.mp4", 0, second)
	track := file.VideoTrack()
	first, err := track.tableSample(0)
	if err != nil {
		t.Fatalf("locate the first keyframe: %v", err)
	}
	last, err := track.tableSample(second)
	if err != nil {
		t.Fatalf("locate the second keyframe: %v", err)
	}
	midpoint := (compositionTime(first) + compositionTime(last)) / 2

	for _, tc := range []struct {
		name string
		at   time.Duration
		want uint32
	}{
		{name: "before both", at: 0, want: 0},
		{name: "nearer the first", at: movieTimeOf(t, file, midpoint-1), want: 0},
		{name: "a tie goes to the earlier one", at: movieTimeOf(t, file, midpoint), want: 0},
		{name: "nearer the second", at: movieTimeOf(t, file, midpoint+1), want: second},
		{name: "on the second", at: time.Second, want: second},
		{name: "past the end", at: time.Hour, want: second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample, err := file.ReadNearestSyncSample(tc.at)
			if err != nil {
				t.Fatalf("read nearest sync sample: %v", err)
			}
			if sample.Index != tc.want {
				t.Errorf("index = %d at %v, want %d", sample.Index, tc.at, tc.want)
			}
			if len(sample.Data) == 0 {
				t.Error("sample is empty")
			}
		})
	}
}

func TestReadNearestSyncSampleFallsForwardToTheFirstKeyframe(t *testing.T) {
	// A track whose first keyframe is not its first sample has nothing at or
	// before time zero, and the nearest keyframe is the one ahead.
	const keyframe = 24
	file := withSyncSamples(t, "h264-aac.mp4", keyframe)

	if _, err := file.ReadSyncSample(0); !errors.Is(err, ErrNoSyncSample) {
		t.Fatalf("ReadSyncSample at 0 = %v, want ErrNoSyncSample", err)
	}
	sample, err := file.ReadNearestSyncSample(0)
	if err != nil {
		t.Fatalf("read nearest sync sample: %v", err)
	}
	if sample.Index != keyframe {
		t.Errorf("index = %d, want %d", sample.Index, keyframe)
	}
}

func TestReadNearestSyncSampleMatchesReadSyncSampleOnTheCorpus(t *testing.T) {
	// Every corpus file holds exactly one keyframe, so nearest and at-or-before
	// have to agree everywhere, fragmented file included.
	names, err := corpus.ISOBMFFFiles()
	if err != nil {
		t.Fatalf("list corpus: %v", err)
	}
	for _, name := range names {
		for _, at := range []time.Duration{0, time.Second, time.Hour} {
			t.Run(name+"@"+at.String(), func(t *testing.T) {
				file, _, _ := openCorpus(t, name)

				want, err := file.ReadSyncSample(at)
				if err != nil {
					t.Fatalf("read sync sample: %v", err)
				}
				got, err := file.ReadNearestSyncSample(at)
				if err != nil {
					t.Fatalf("read nearest sync sample: %v", err)
				}
				if got.Index != want.Index || got.Time != want.Time {
					t.Errorf("nearest = sample %d at %v, want sample %d at %v",
						got.Index, got.Time, want.Index, want.Time)
				}
			})
		}
	}
}

func TestReadNearestSyncSampleReportsAMissingKeyframe(t *testing.T) {
	audioOnly := &File{Tracks: []*Track{{Handler: "soun"}}}
	if _, err := audioOnly.ReadNearestSyncSample(0); !errors.Is(err, ErrNoVideoTrack) {
		t.Errorf("no video track = %v, want ErrNoVideoTrack", err)
	}
	noSamples := &File{Tracks: []*Track{{Handler: "vide", Timescale: 1000}}}
	if _, err := noSamples.ReadNearestSyncSample(0); !errors.Is(err, ErrNoSyncSample) {
		t.Errorf("no samples = %v, want ErrNoSyncSample", err)
	}
}
