package main

import (
	"fmt"
	"time"

	slogbetterstack "github.com/samber/slog-betterstack"

	"log/slog"
)

func main() {
	logger := slog.New(slogbetterstack.Option{Level: slog.LevelDebug, Token: "xxxx"}.NewBetterstackHandler())
	logger = logger.With("release", "v1.0.0")

	logger.
		With(
			slog.Group("user",
				slog.String("id", "user-123"),
				slog.Time("created_at", time.Now()),
			),
		).
		With("error", fmt.Errorf("an error")).
		Error("a message", slog.Int("count", 1))

	time.Sleep(1 * time.Second)
}
