//go:build !hevc

package decode

import "fmt"

// hevcKeyframe is the HEVC decoder left out of the build. The decoder is
// behind the hevc build tag; see the package documentation for why.
func hevcKeyframe(_ []byte, _ int, _ []byte) (Picture, error) {
	return Picture{}, fmt.Errorf("%w: hevc decoding is compiled out; build with -tags hevc",
		ErrUnsupportedCodec)
}
