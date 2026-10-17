package handler

import (
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"aredn-map/internal/controller"
)

// findBundle returns server.static_dir when a built bundle is there. The API does
// not look for the frontend next door: the path is the operator's, or empty.
func findBundle(d controller.Deps) string {
	dir := d.Cfg.Server.StaticDir
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		d.Log.Warn("static_dir is not resolvable", zap.String("static_dir", dir), zap.Error(err))
		return ""
	}
	if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
		d.Log.Warn("frontend bundle not found; serving API only", zap.String("static_dir", abs))
		return ""
	}
	d.Log.Info("serving frontend", zap.String("dir", abs))
	return abs
}
