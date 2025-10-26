// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package logger

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

const (
	// EnvVarLogLevel is the environment variable name for setting the log level.
	EnvVarLogLevel = "LOG_LEVEL"
)

// NewStructuredLogger creates a new structured logger with the specified log level
// Defined module name and version are included in the logger's context.
// AddSource is enabled for debug level logging only.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
//
// Returns:
//   - *slog.Logger: A pointer to the configured slog.Logger instance.
func NewStructuredLogger(module, version, level string) *slog.Logger {
	lev := ParseLogLevel(level)
	addSource := lev <= slog.LevelDebug

	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     lev,
		AddSource: addSource,
	})).With("module", module, "version", version)
}

// NewLogLogger creates a new standard library log.Logger that writes logs
// using the slog package with the specified log level.
// Parameters:
//   - level: The log level as a slog.Level.
//
// Returns:
//   - *log.Logger: A pointer to the configured log.Logger instance.
func NewLogLogger(level slog.Level, withSource bool) *log.Logger {
	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level:     level,
		AddSource: withSource,
	})

	return slog.NewLogLogger(handler, level)
}

// SetDefaultStructuredLogger initializes the structured logger with the
// appropriate log level and sets it as the default logger.
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//
// Derives log level from the LOG_LEVEL environment variable.
func SetDefaultStructuredLogger(module, version string) {
	SetDefaultStructuredLoggerWithLevel(module, version, os.Getenv(EnvVarLogLevel))
}

// SetDefaultStructuredLoggerWithLevel initializes the structured logger with the specified log level
// Defined module name and version are included in the logger's context.
// Parameters:
//   - module: The name of the module/application using the logger.
//   - version: The version of the module/application (e.g., "v1.0.0").
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
func SetDefaultStructuredLoggerWithLevel(module, version, level string) {
	slog.SetDefault(NewStructuredLogger(module, version, level))
}

// ParseLogLevel converts a string representation of a log level into a slog.Level.
// Parameters:
//   - level: The log level as a string (e.g., "debug", "info", "warn", "error").
//
// Returns:
//   - slog.Level corresponding to the input string. Defaults to slog.LevelInfo for unrecognized strings.
func ParseLogLevel(level string) slog.Level {
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

	return lev
}
