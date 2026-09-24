package mpegts

import "fmt"

// adtsSampleRates is the sampling_frequency_index table ADTS and the
// AudioSpecificConfig share. ISO/IEC 14496-3 1.6.3.4.
var adtsSampleRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// aacFrameSamples is how many samples one AAC raw data block decodes to.
const aacFrameSamples = 1024

// adtsHeader is one ADTS frame header. ISO/IEC 14496-3 1.A.2.2.
type adtsHeader struct {
	size     int // the header's own bytes: 7, or 9 with a CRC
	frame    int // the whole frame, header included
	profile  int // the audio object type minus one
	rate     int // sampling_frequency_index
	channels int // channel_configuration
	blocks   int // raw data blocks in the frame
}

// parseADTS decodes an ADTS header from at least its first seven bytes.
func parseADTS(b []byte) (adtsHeader, error) {
	if len(b) < 7 || b[0] != 0xff || b[1]&0xf6 != 0xf0 {
		return adtsHeader{}, fmt.Errorf("%w: no ADTS sync word", ErrMalformed)
	}
	h := adtsHeader{
		size:     7,
		profile:  int(b[2] >> 6),
		rate:     int(b[2] >> 2 & 0x0f),
		channels: int(b[2]&1)<<2 | int(b[3]>>6),
		frame:    int(b[3]&0x03)<<11 | int(b[4])<<3 | int(b[5]>>5),
		blocks:   int(b[6]&0x03) + 1,
	}
	if b[1]&0x01 == 0 {
		h.size = 9
	}
	if h.rate >= len(adtsSampleRates) {
		return adtsHeader{}, fmt.Errorf("%w: ADTS sampling frequency index %d", ErrMalformed, h.rate)
	}
	if h.frame <= h.size {
		return adtsHeader{}, fmt.Errorf("%w: an ADTS frame of %d bytes", ErrMalformed, h.frame)
	}
	return h, nil
}

// audioSpecificConfig is the two byte AudioSpecificConfig an ADTS header
// implies: the object type, the rate index, and the channel configuration, and
// three zero flags. It is what an mp4a sample entry's esds carries.
// ISO/IEC 14496-3 1.6.2.1.
func (h adtsHeader) audioSpecificConfig() []byte {
	v := (h.profile+1)<<11 | h.rate<<7 | h.channels<<3
	return []byte{byte(v >> 8), byte(v)}
}

// mpegAudioLayer reads the layer out of an MPEG audio frame header: 1, 2, or 3,
// or 0 when the bytes are not a frame header. ISO/IEC 11172-3 2.4.2.3.
func mpegAudioLayer(b []byte) int {
	if len(b) < 2 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return 0
	}
	layer := int(b[1] >> 1 & 0x03)
	if layer == 0 {
		return 0
	}
	return 4 - layer
}
