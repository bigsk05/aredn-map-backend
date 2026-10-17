// Package logger builds the zap logger, writing through lumberjack into a logs directory.
package logger

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"aredn-map/internal/infra/config"
)

// New returns a production logger; console output is mirrored when logging.console is true.
func New(cfg config.Logging) (*zap.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "ts"
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeDuration = zapcore.StringDurationEncoder

	var sinks []zapcore.WriteSyncer
	if cfg.Console {
		sinks = append(sinks, zapcore.Lock(os.Stdout))
	}
	if cfg.Dir != "" || cfg.File != "" {
		if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
			return nil, err
		}
		sinks = append(sinks, zapcore.AddSync(&lumberjack.Logger{
			Filename:   filepath.Join(cfg.Dir, cfg.File),
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   cfg.Compress,
		}))
	}
	if len(sinks) == 0 {
		sinks = append(sinks, zapcore.Lock(os.Stdout))
	}

	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(encCfg),
		zapcore.NewMultiWriteSyncer(sinks...),
		level,
	)
	return zap.New(core, zap.AddCaller(), zap.ErrorOutput(zapcore.Lock(os.Stderr))), nil
}

func parseLevel(s string) (zapcore.Level, error) {
	var l zapcore.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, err
	}
	return l, nil
}
