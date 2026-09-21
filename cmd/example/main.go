package main

import (
	"log/slog"
	"os"

	"github.com/jamesaorson/template-golang/internal/example"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	var config example.Config
	if err := example.LoadConfig(&config); err != nil {
		logger.Error("error loading config", slog.String("error", err.Error()))
		panic(err)
	}
	logger.Info("loaded config", slog.Any("config", config))
}
