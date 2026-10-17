// Package meta holds the build identity: app name, version and user agent.
package meta

const (
	AppName = "aredn-map"
	Version = "0.1.0"

	// UserAgent is what remote AREDN nodes see on every request.
	UserAgent = AppName + "/" + Version
)
