//go:build !hevc

package sprocket_test

// hevcCompiled is the other half of hevc_test.go: this build has no HEVC
// decoder, so an HEVC corpus file is the unsupported-codec fallback.
const hevcCompiled = false
