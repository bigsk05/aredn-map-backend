package ping

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/net/icmp"
)

// Socket kinds: unprivileged is the rootless SOCK_DGRAM icmp socket (udp4 on macOS, ping sockets
// on Linux); privileged is raw ip4:icmp and needs root or CAP_NET_RAW.
const (
	dgramNetwork = "udp4"
	rawNetwork   = "ip4:icmp"
)

// pingGroupRangePath is a variable so tests can feed unprivHint a synthetic sysctl file.
var (
	pingGroupRangePath = "/proc/sys/net/ipv4/ping_group_range"

	errEPERM  = syscall.EPERM
	errEACCES = syscall.EACCES
)

// noPermission reports whether err is a socket-layer permission denial.
func noPermission(err error) bool {
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, errEPERM) ||
		errors.Is(err, errEACCES)
}

// openICMPSocket opens and closes the socket kind behind privileged; a permission test only.
func openICMPSocket(privileged bool) error {
	network := dgramNetwork
	if privileged {
		network = rawNetwork
	}
	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return err
	}
	return conn.Close()
}

// readPingGroupRange returns the Linux ping_group_range sysctl contents, "" when unreadable.
func readPingGroupRange(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// groupAllowedRange matches gid against "lowgid highgid"; a failed parse sets known=false.
func groupAllowedRange(spec string, gid int) (allowed, known bool) {
	fields := strings.Fields(spec)
	if len(fields) < 2 {
		return false, false
	}
	low, err1 := strconv.Atoi(fields[0])
	high, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return false, false
	}
	return gid >= low && gid <= high, true
}

// unprivHint is the socket-denial explanation for the log, never for Result.Err.
func unprivHint(goos string, privileged bool, err error) string {
	gid := os.Getegid()
	switch {
	case !noPermission(err):
		return fmt.Sprintf("socket open failed for %s (not a permissions problem)", socketKind(privileged))
	case goos == "linux":
		spec := readPingGroupRange(pingGroupRangePath)
		allowed, known := groupAllowedRange(spec, gid)
		if privileged {
			return fmt.Sprintf("raw icmp needs root or CAP_NET_RAW; unprivileged icmp needs "+
				"/proc/sys/net/ipv4/ping_group_range to include gid %d (current %q, includes=%t/%s)",
				gid, orUnknown(spec, known), allowed, boolWord(allowed))
		}
		return fmt.Sprintf("datagram icmp denied: set /proc/sys/net/ipv4/ping_group_range to "+
			"\"0 2147483647\" so gid %d may ping (current %q, includes=%t)", gid, spec, allowed)
	case goos == "darwin":
		if privileged {
			return "macOS denies ip4:icmp to non-root users; use mode=unprivileged, which " +
				"drives the built-in SOCK_DGRAM ICMP socket, or run as root"
		}
		return "macOS datagram icmp sockets are available to every user by default; a denial here " +
			"usually means a sandbox (App Nap / seatbelt) around the process"
	default:
		return fmt.Sprintf("%s sockets need elevated privileges on %s", socketKind(privileged), goos)
	}
}

func socketKind(privileged bool) string {
	if privileged {
		return rawNetwork
	}
	return dgramNetwork
}

func orUnknown(spec string, known bool) string {
	if strings.TrimSpace(spec) == "" {
		return "unreadable"
	}
	if !known {
		return "unparsable"
	}
	return spec
}

func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
