//go:build !h264

package sprocket_test

// h264Compiled is the other half of h264_test.go: this build has no H.264
// decoder, so an H.264 corpus file is the unsupported-codec fallback.
const h264Compiled = false
