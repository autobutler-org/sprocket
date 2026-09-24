//go:build h264 || hevc

package decode_test

import (
	"image"
	"math/bits"
	"testing"

	"github.com/autobutler-org/sprocket/internal/decode"
)

// fuzzNALLengthSize is the prefix width every corpus file uses, and the one the
// fuzz targets hold fixed so that the input they vary is the bitstream.
const fuzzNALLengthSize = 4

// keyframe decodes a corpus file's first sync sample.
func keyframe(t testing.TB, name string) image.Image {
	t.Helper()

	sample := syncSample(t, name)
	picture, err := decode.Keyframe(sample.Codec, sample.Config, sample.NALLengthSize, sample.Data)
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return picture.Image
}

// bitWriter builds an RBSP out of the fields the tests need to state exactly. It
// keeps one byte per bit and packs at the end, which is slow and obvious.
type bitWriter struct {
	bits []byte
}

// u appends an n bit unsigned integer, most significant bit first.
func (w *bitWriter) u(n int, value uint32) {
	for i := n - 1; i >= 0; i-- {
		w.bits = append(w.bits, byte(value>>uint(i)&1))
	}
}

// ue appends an unsigned Exp-Golomb coded field.
func (w *bitWriter) ue(value uint32) {
	size := bits.Len32(value + 1)
	w.u(2*size-1, value+1)
}

// bytes packs the written bits, padding the last byte with zeros.
func (w *bitWriter) bytes() []byte {
	out := make([]byte, (len(w.bits)+7)/8)
	for i, bit := range w.bits {
		out[i/8] |= bit << (7 - i%8)
	}
	return out
}
