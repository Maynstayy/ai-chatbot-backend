// Command api runs the standalone AI chatbot backend.
package main

import (
	"log/slog"
	"os"

	"github.com/Maynstayy/ai-chatbot-backend/internal/app"
	"github.com/Maynstayy/ai-chatbot-backend/internal/config"
)

func main() {
	a, err := app.New(config.Load())
	if err != nil {
		slog.Error("startup failed", "err", err.Error())
		os.Exit(1)
	}
	if err := a.Run(); err != nil {
		slog.Error("server error", "err", err.Error())
		os.Exit(1)
	}
}
