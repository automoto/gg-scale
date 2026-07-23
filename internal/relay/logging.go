package relay

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pion/logging"
)

// slogLoggerFactory adapts pion's LoggerFactory to slog so the TURN server's
// internal events (bind failures, malformed packets, allocation errors) land in
// the process log instead of being silently dropped — pion logs nothing unless
// a factory is wired. Scope tags the emitting pion subsystem.
type slogLoggerFactory struct{ base *slog.Logger }

func newSlogLoggerFactory(base *slog.Logger) logging.LoggerFactory {
	if base == nil {
		base = slog.Default()
	}
	return &slogLoggerFactory{base: base}
}

func (f *slogLoggerFactory) NewLogger(scope string) logging.LeveledLogger {
	return &slogLeveledLogger{l: f.base.With("pion_scope", scope)}
}

// slogLeveledLogger maps pion's leveled logger onto slog. pion Trace/Debug both
// map to slog Debug; Warn maps to slog Warn.
type slogLeveledLogger struct{ l *slog.Logger }

func (s *slogLeveledLogger) log(level slog.Level, msg string) {
	s.l.LogAttrs(context.Background(), level, msg)
}

func (s *slogLeveledLogger) Trace(msg string) { s.log(slog.LevelDebug, msg) }
func (s *slogLeveledLogger) Tracef(format string, a ...any) {
	s.log(slog.LevelDebug, fmt.Sprintf(format, a...))
}
func (s *slogLeveledLogger) Debug(msg string) { s.log(slog.LevelDebug, msg) }
func (s *slogLeveledLogger) Debugf(format string, a ...any) {
	s.log(slog.LevelDebug, fmt.Sprintf(format, a...))
}
func (s *slogLeveledLogger) Info(msg string) { s.log(slog.LevelInfo, msg) }
func (s *slogLeveledLogger) Infof(format string, a ...any) {
	s.log(slog.LevelInfo, fmt.Sprintf(format, a...))
}
func (s *slogLeveledLogger) Warn(msg string) { s.log(slog.LevelWarn, msg) }
func (s *slogLeveledLogger) Warnf(format string, a ...any) {
	s.log(slog.LevelWarn, fmt.Sprintf(format, a...))
}
func (s *slogLeveledLogger) Error(msg string) { s.log(slog.LevelError, msg) }
func (s *slogLeveledLogger) Errorf(format string, a ...any) {
	s.log(slog.LevelError, fmt.Sprintf(format, a...))
}
