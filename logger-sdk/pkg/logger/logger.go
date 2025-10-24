package logger

import (
	"log/slog"
	"os"
	"strings"
)

const (
	// EnvVarLogLevel is the environment variable name for setting the log level.
	EnvVarLogLevel = "LOG_LEVEL"
)

// SetDefault initializes the structured logger with the appropriate log level and sets it as the default logger.
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//
// Derives log level from the LOG_LEVEL environment variable.
func SetDefault(module, version string) {
	SetDefaultWithLevel(module, version, os.Getenv(EnvVarLogLevel))
}

// SetDefaultWithLevel initializes the structured logger with the specified log level
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
func SetDefaultWithLevel(module, version, level string) {
	var lev slog.Level

	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lev = slog.LevelDebug
	case "warn", "warning":
		lev = slog.LevelWarn
	case "error":
		lev = slog.LevelError
	default:
		lev = slog.LevelInfo
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     lev,
		AddSource: true,
	})).With("module", module, "version", version)

	slog.SetDefault(logger)
}
