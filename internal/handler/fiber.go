// Package handler assembles the Fiber app: middleware, routes, WebSocket push, static bundle.
package handler

import (
	"errors"
	"fmt"

	fiberzap "github.com/gofiber/contrib/v3/zap"
	"github.com/gofiber/fiber/v3"
	fibercompress "github.com/gofiber/fiber/v3/middleware/compress"
	"github.com/gofiber/fiber/v3/middleware/cors"
	fiberrecover "github.com/gofiber/fiber/v3/middleware/recover"
	fiberrequestid "github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/gofiber/fiber/v3/middleware/static"
	"github.com/gofiber/utils/v2"
	gojson "go.gh.ink/json"
	"go.uber.org/zap"

	"aredn-map/internal/controller"
	"aredn-map/internal/meta"
)

// New builds the app without binding a port, which is what the tests drive.
func New(d controller.Deps) *fiber.App {
	app := fiber.New(fiber.Config{
		AppName:      meta.AppName,
		ReadTimeout:  d.Cfg.Server.ReadTimeout,
		WriteTimeout: d.Cfg.Server.WriteTimeout,
		// One JSON encoder for both responses and push frames.
		JSONEncoder:  gojson.Marshal,
		JSONDecoder:  gojson.Unmarshal,
		ErrorHandler: handleError(d),
	})

	app.Use(fiberrecover.New())
	app.Use(fiberrequestid.New(fiberrequestid.Config{
		Header:    fiber.HeaderXRequestID,
		Generator: utils.UUIDv4,
	}))
	app.Use(fiberzap.New(fiberzap.Config{
		Logger:   d.Log,
		SkipURIs: []string{"/api/health"},
		Fields:   []string{"latency", "status", "method", "url", "requestId", "ua"},
	}))
	app.Use(cors.New(cors.Config{AllowOrigins: d.Cfg.Server.AllowOrigins}))
	bundle := findBundle(d)
	if bundle != "" {
		app.Use(fibercompress.New())
	}

	// The API is registered before the bundle, whose static handler answers every unknown path.
	Register(app.Group("/api"), d)

	if bundle != "" {
		app.Use("/", static.New(bundle, static.Config{
			IndexNames: []string{"index.html"},
			MaxAge:     60,
			// Unknown paths belong to the single-page frontend.
			NotFoundHandler: func(c fiber.Ctx) error {
				return c.SendFile(bundle + "/index.html")
			},
		}))
	}

	Register(app.Group("/api"), d)
	return app
}

// Run builds the app and starts listening; the returned app shuts the listener down.
func Run(d controller.Deps) *fiber.App {
	app := New(d)
	addr := fmt.Sprintf("%s:%d", d.Cfg.Server.Host, d.Cfg.Server.Port)
	go func() {
		d.Log.Info("listening", zap.String("addr", addr))
		if err := app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}); err != nil {
			d.Log.Fatal("http listen failed", zap.Error(err))
		}
	}()
	return app
}

// handleError is the last resort for anything a handler returned unwrapped.
func handleError(d controller.Deps) fiber.ErrorHandler {
	return func(c fiber.Ctx, err error) error {
		code := fiber.StatusInternalServerError
		var e *fiber.Error
		if errors.As(err, &e) {
			code = e.Code
		}
		if code >= fiber.StatusInternalServerError {
			d.Log.Error("request failed", zap.Error(err), zap.String("path", string(c.Request().URI().Path())))
		}
		return c.Status(code).JSON(fiber.Map{"error": err.Error()})
	}
}
