package ping

import (
	"context"
	"fmt"
	"net"
	"strings"

	"go.uber.org/zap"
)

// Mode keys accepted in Config.Mode and reported by Pinger.Mode.
const (
	// ModeAuto tries each socket kind at construction and keeps the first that opens.
	ModeAuto = "auto"
	// ModeUnprivileged forces the SOCK_DGRAM ICMP datagram socket.
	ModeUnprivileged = "unprivileged"
	// ModePrivileged forces the raw ip4:icmp socket.
	ModePrivileged = "privileged"
)

// meshCheckName is what the startup self-check resolves to separate DNS from ICMP failure.
const meshCheckName = "localnode.local.mesh"

// ErrInvalidMode is returned by New for an unknown Config.Mode.
var ErrInvalidMode = fmt.Errorf("ping: unknown icmp mode (want auto, unprivileged or privileged)")

// socketProbe opens one ICMP socket of the given kind and closes it again.
type socketProbe func(privileged bool) error

// lookupFn is the DNS seam used for resolution.
type lookupFn func(ctx context.Context, host string) ([]net.IPAddr, error)

// defaultLookup is the production resolver.
var defaultLookup lookupFn = net.DefaultResolver.LookupIPAddr

// normalizeMode maps a configured value onto a known mode key, "" giving auto.
func normalizeMode(mode string) string {
	switch strings.TrimSpace(strings.ToLower(mode)) {
	case "", ModeAuto:
		return ModeAuto
	case ModeUnprivileged:
		return ModeUnprivileged
	case ModePrivileged:
		return ModePrivileged
	default:
		return ""
	}
}

// decision records what the socket probe found, for logging the effective mode.
type decision struct {
	mode      string
	reason    string
	hint      string
	available bool
	warn      bool
}

// resolveMode turns Config.Mode into the effective mode: auto tries unprivileged then
// privileged and keeps the first socket that opens; a forced mode is never downgraded.
func resolveMode(requested string, probe socketProbe, goos string) (decision, error) {
	mode := normalizeMode(requested)
	if mode == "" {
		return decision{}, fmt.Errorf("%w: %q", ErrInvalidMode, requested)
	}

	switch mode {
	case ModeUnprivileged:
		if err := probe(false); err != nil {
			return decision{
				mode:   ModeUnprivileged,
				reason: fmt.Sprintf("forced by config; %s socket check failed: %v", dgramNetwork, err),
				hint:   unprivHint(goos, false, err),
				warn:   true,
			}, nil
		}
		return decision{
			mode:      ModeUnprivileged,
			reason:    fmt.Sprintf("forced by config; %s socket opened", dgramNetwork),
			available: true,
		}, nil

	case ModePrivileged:
		if err := probe(true); err != nil {
			return decision{
				mode:   ModePrivileged,
				reason: fmt.Sprintf("forced by config; %s socket check failed: %v", rawNetwork, err),
				hint:   unprivHint(goos, true, err),
				warn:   true,
			}, nil
		}
		return decision{
			mode:      ModePrivileged,
			reason:    fmt.Sprintf("forced by config; %s socket opened", rawNetwork),
			available: true,
		}, nil
	}

	// auto
	dgramErr := probe(false)
	if dgramErr == nil {
		return decision{
			mode:      ModeUnprivileged,
			reason:    fmt.Sprintf("auto: %s datagram icmp socket opened, no raw socket needed", dgramNetwork),
			available: true,
		}, nil
	}

	rawErr := probe(true)
	if rawErr == nil {
		return decision{
			mode: ModePrivileged,
			reason: fmt.Sprintf("auto: %s denied (%v), fell back to %s raw icmp socket",
				dgramNetwork, dgramErr, rawNetwork),
			available: true,
		}, nil
	}

	// Nothing opens: stay unprivileged so each host still yields a readable error.
	return decision{
		mode: ModeUnprivileged,
		reason: fmt.Sprintf("auto: neither socket kind could be opened (%s: %v; %s: %v); "+
			"probes will fail until privileges change", dgramNetwork, dgramErr, rawNetwork, rawErr),
		hint: unprivHint(goos, false, dgramErr) + " | " + unprivHint(goos, true, rawErr),
		warn: true,
	}, nil
}

// checkMeshResolution resolves a mesh hostname through the probe's own path.
func (p *Pinger) checkMeshResolution(ctx context.Context, name string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()

	ip, err := p.resolve(cctx, name)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

// reportMeshResolution logs the result of the startup self-check.
func (p *Pinger) reportMeshResolution(ctx context.Context, name string) {
	ip, err := p.checkMeshResolution(ctx, name)
	if err != nil {
		p.log.Warn("mesh hostname resolution unavailable",
			zap.String("name", name),
			zap.String("mode", p.Mode()),
			zap.Error(err),
			zap.String("hint", "the mesh names come from the AREDN tunnel client's dnsmasq; "+
				"check /etc/resolver/local.mesh and that the tunnel is up"),
		)
		return
	}
	p.log.Info("mesh hostname resolution ok",
		zap.String("name", name),
		zap.String("addr", ip),
		zap.String("mode", p.Mode()),
	)
}
