package example_test

import (
	"testing"

	"github.com/jamesaorson/template-golang/internal/example"
)

func validateConfig(t *testing.T, config example.Config) {
	t.Helper()
	if config.Port == 0 {
		t.Error("Port is not set")
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	var config example.Config
	if err := example.LoadConfig(&config); err != nil {
		t.Error("Failed to LoadConfig")
	}

	validateConfig(t, config)
}
