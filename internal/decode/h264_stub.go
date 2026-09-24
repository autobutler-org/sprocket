//go:build !h264

package decode

import (
	"fmt"
	"image"
)

// h264Keyframe is the H.264 decoder left out of the build. The decoder is
// behind the h264 build tag; see the package documentation for why.
func h264Keyframe(_ []byte, _ int, _ []byte) (image.Image, error) {
	return nil, fmt.Errorf("%w: h264 decoding is compiled out; build with -tags h264",
		ErrUnsupportedCodec)
}
