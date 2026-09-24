//go:build !hevc

package decode_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// TestKeyframeHEVCCompiledOut is the other half of hevc_test.go: without the
// build tag an HEVC sample is a codec this build has no decoder for, and the
// error says which tag puts one in.
func TestKeyframeHEVCCompiledOut(t *testing.T) {
	sample := syncSample(t, "hevc-aac-8bit.mov")

	_, err := decode.Keyframe("hevc", sample.Config, sample.NALLengthSize, sample.Data)
	if !errors.Is(err, decode.ErrUnsupportedCodec) {
		t.Fatalf("error is %v, want ErrUnsupportedCodec", err)
	}
	if !strings.Contains(err.Error(), "-tags hevc") {
		t.Errorf("error is %q, want it to name the hevc build tag", err)
	}
}
