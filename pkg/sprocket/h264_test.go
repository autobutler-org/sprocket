//go:build h264

package sprocket_test

// h264Compiled reports whether this build has an H.264 decoder. The thumbnail
// tests use it to tell a real failure from the documented fallback: without the
// tag an H.264 corpus file returns ErrUnsupportedCodec and there is no frame to
// assert anything about.
const h264Compiled = true
