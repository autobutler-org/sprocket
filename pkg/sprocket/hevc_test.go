//go:build hevc

package sprocket_test

// hevcCompiled reports whether this build has an HEVC decoder. The thumbnail
// tests use it to tell a real failure from the documented fallback: without the
// tag an HEVC corpus file returns ErrUnsupportedCodec and there is no frame to
// assert anything about.
const hevcCompiled = true
