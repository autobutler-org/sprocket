//go:build !h264

package decode_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// TestKeyframeH264CompiledOut is the other half of h264_test.go: without the
// build tag an H.264 sample is a codec this build has no decoder for, and the
// error says which tag puts one in.
func TestKeyframeH264CompiledOut(t *testing.T) {
	sample := syncSample(t, "h264-aac.mp4")

	_, err := decode.Keyframe("h264", sample.Config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrUnsupportedCodec) {
		t.Fatalf("error is %v, want ErrUnsupportedCodec", err)
	}
	if !strings.Contains(err.Error(), "-tags h264") {
		t.Errorf("error is %q, want it to name the h264 build tag", err)
	}
}
