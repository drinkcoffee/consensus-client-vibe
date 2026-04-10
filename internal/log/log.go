package log

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Logger is the package-level logger used throughout the node.
var Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()

// Init configures the package-level Logger based on level and format strings.
// level: trace | debug | info | warn | error
// format: json | pretty
func Init(level, format string) {
	var w io.Writer = os.Stderr
	if strings.ToLower(format) == "pretty" {
		w = zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: time.RFC3339,
		}
	}

	lvl, err := zerolog.ParseLevel(strings.ToLower(level))
	if err != nil {
		lvl = zerolog.InfoLevel
	}

	Logger = zerolog.New(w).Level(lvl).With().Timestamp().Logger()
}

// With returns a child logger with the given component name attached.
func With(component string) zerolog.Logger {
	return Logger.With().Str("component", component).Logger()
}
