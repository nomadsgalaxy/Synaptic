// handlers_admin_network.go — `GET /admin/network` for the dashboard's
// "Local LAN" agent-endpoint auto-detection.
//
// SD Core knows the host's IP addresses; the dashboard is asking instead
// of making the user type "what's my LAN IP again?" every time they share
// the brain with another device on the network.
//
// Endpoint:
//
//   GET /admin/network
//   Authorization: Bearer <SD_API_TOKEN>
//
//   → {
//       "hostname":      "anthony-pc",
//       "primary_ipv4":  "192.168.1.42",
//       "primary_ipv6":  "fd12:...",  // empty when no global IPv6 found
//       "interfaces":    [...],       // each {name, addrs, up, loopback, primary}
//       "listen_port":   9911,
//       "cloudflared_hostname": "cognito.example.com"  // best-effort, "" when absent
//     }
//
// "Primary IPv4" rule: first non-loopback, "up" interface that has an
// RFC1918 private IPv4. If none found, the first non-link-local global
// IPv4. We avoid the route-table heuristic because cloudflared / VPN
// tunnels can rewrite it. Link-local 169.254.x.x is skipped — Windows
// auto-assigns those when DHCP fails and they aren't useful for
// "another device on this LAN."
//
// Selection is factored into selectPrimaryIPv4 / selectPrimaryIPv6 so
// tests can drive it with synthetic interface fixtures.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// netIface is the wire shape per interface.
type netIface struct {
	Name     string   `json:"name"`
	Addrs    []string `json:"addrs"`
	Up       bool     `json:"up"`
	Loopback bool     `json:"loopback"`
	Primary  bool     `json:"primary"`
}

// netInfo is the full response payload.
type netInfo struct {
	Hostname            string     `json:"hostname"`
	PrimaryIPv4         string     `json:"primary_ipv4"`
	PrimaryIPv6         string     `json:"primary_ipv6"`
	Interfaces          []netIface `json:"interfaces"`
	ListenPort          int        `json:"listen_port"`
	CloudflaredHostname string     `json:"cloudflared_hostname,omitempty"`
}

// ifaceProbe is the small struct the selection helpers consume — abstracts
// over net.Interface so tests can fabricate interface lists without
// touching the OS.
type ifaceProbe struct {
	name     string
	up       bool
	loopback bool
	addrs    []*net.IPNet
}

// selectPrimaryIPv4 walks the probes and returns (ip, ifaceName) for the
// best LAN IP. Empty strings when nothing matches.
//
// Preference (in order):
//  1. RFC1918 private (10/8, 172.16/12, 192.168/16) on a non-loopback up iface
//  2. First global non-link-local IPv4 on a non-loopback up iface
//  3. "" (caller falls back to localhost)
func selectPrimaryIPv4(probes []ifaceProbe) (ip, ifaceName string) {
	bestPrivate, bestPrivateIface := "", ""
	bestGlobal, bestGlobalIface := "", ""
	for _, p := range probes {
		if !p.up || p.loopback {
			continue
		}
		for _, a := range p.addrs {
			ip4 := a.IP.To4()
			if ip4 == nil {
				continue
			}
			// Skip link-local 169.254.0.0/16 (Windows DHCP-failure addrs).
			if ip4[0] == 169 && ip4[1] == 254 {
				continue
			}
			isPrivate := (ip4[0] == 10) ||
				(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
				(ip4[0] == 192 && ip4[1] == 168)
			if isPrivate && bestPrivate == "" {
				bestPrivate = ip4.String()
				bestPrivateIface = p.name
			} else if !isPrivate && bestGlobal == "" {
				bestGlobal = ip4.String()
				bestGlobalIface = p.name
			}
		}
	}
	if bestPrivate != "" {
		return bestPrivate, bestPrivateIface
	}
	return bestGlobal, bestGlobalIface
}

// selectPrimaryIPv6 returns the first global-unicast IPv6 on a non-loopback,
// up interface. Skips link-local (fe80::/10) and loopback (::1).
func selectPrimaryIPv6(probes []ifaceProbe) string {
	for _, p := range probes {
		if !p.up || p.loopback {
			continue
		}
		for _, a := range p.addrs {
			if a.IP.To4() != nil {
				continue
			}
			if a.IP.IsLinkLocalUnicast() || a.IP.IsLoopback() {
				continue
			}
			if !a.IP.IsGlobalUnicast() {
				continue
			}
			return a.IP.String()
		}
	}
	return ""
}

// gatherInterfaceProbes converts net.Interfaces() output into the testable
// shape selectPrimary* expects. Errors on any individual interface are
// swallowed (we still want to surface other interfaces' info).
func gatherInterfaceProbes() []ifaceProbe {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]ifaceProbe, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		probe := ifaceProbe{
			name:     ifc.Name,
			up:       ifc.Flags&net.FlagUp != 0,
			loopback: ifc.Flags&net.FlagLoopback != 0,
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				probe.addrs = append(probe.addrs, ipn)
			}
		}
		out = append(out, probe)
	}
	return out
}

// listenPortFromEnv reads SD_CORE_LISTEN ("0.0.0.0:9911") and returns the
// port portion, or 9911 as a sensible default.
func listenPortFromEnv() int {
	listen := envOr("SD_CORE_LISTEN", "0.0.0.0:9911")
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		if p, err := strconv.Atoi(listen[i+1:]); err == nil {
			return p
		}
	}
	return 9911
}

func (s *Server) getAdminNetwork(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w, "GET", "OPTIONS")
		return
	}
	probes := gatherInterfaceProbes()
	primaryV4, primaryV4Iface := selectPrimaryIPv4(probes)
	primaryV6 := selectPrimaryIPv6(probes)

	// Build the wire-form interface list. Mark the chosen interface as
	// primary so the dashboard can highlight it.
	wireIfaces := make([]netIface, 0, len(probes))
	for _, p := range probes {
		ni := netIface{
			Name:     p.name,
			Up:       p.up,
			Loopback: p.loopback,
			Primary:  p.name == primaryV4Iface,
		}
		ni.Addrs = make([]string, 0, len(p.addrs))
		for _, a := range p.addrs {
			ni.Addrs = append(ni.Addrs, a.String())
		}
		wireIfaces = append(wireIfaces, ni)
	}

	hostname, _ := os.Hostname()
	info := netInfo{
		Hostname:            hostname,
		PrimaryIPv4:         primaryV4,
		PrimaryIPv6:         primaryV6,
		Interfaces:          wireIfaces,
		ListenPort:          listenPortFromEnv(),
		CloudflaredHostname: detectCloudflaredHostname(listenPortFromEnv()),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

// detectCloudflaredHostname is a best-effort scan of the user's cloudflared
// config for an ingress entry that points at our listen port. Returns ""
// on any failure — the field is decorative; nothing depends on it being
// present. We prefer the LocalSystem service config (path 1) over the
// user-profile one because the running tunnel reads the former.
func detectCloudflaredHostname(listenPort int) string {
	// Try the LocalSystem service config first (the running cloudflared
	// service reads this one), then the user-profile copy. On non-Windows
	// hosts these paths just don't exist and we silently return "".
	candidates := []string{
		`C:\Windows\System32\config\systemprofile\.cloudflared\config.yml`,
		os.Getenv("USERPROFILE") + `\.cloudflared\config.yml`,
		os.Getenv("HOME") + "/.cloudflared/config.yml",
	}
	for _, p := range candidates {
		if p == "" || p == `\.cloudflared\config.yml` || p == "/.cloudflared/config.yml" {
			continue
		}
		if h := scanCloudflaredConfig(p, listenPort); h != "" {
			return h
		}
	}
	return ""
}

// scanCloudflaredConfig reads a YAML file line-by-line looking for the
// first `service: http://localhost:<port>` entry and returns the hostname
// from the most recent `hostname:` line above it. Doesn't attempt full
// YAML parsing — the format is constrained and a regex-free line scan is
// robust enough for the common case. Errors → "".
func scanCloudflaredConfig(path string, listenPort int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	wantSvc := "http://localhost:" + strconv.Itoa(listenPort)
	wantSvc127 := "http://127.0.0.1:" + strconv.Itoa(listenPort)
	lastHostname := ""
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if strings.HasPrefix(t, "- hostname:") {
			lastHostname = strings.TrimSpace(strings.TrimPrefix(t, "- hostname:"))
		} else if strings.HasPrefix(t, "hostname:") {
			lastHostname = strings.TrimSpace(strings.TrimPrefix(t, "hostname:"))
		} else if strings.HasPrefix(t, "service:") {
			svc := strings.TrimSpace(strings.TrimPrefix(t, "service:"))
			// Accept localhost or 127.0.0.1 spellings.
			if svc == wantSvc || svc == wantSvc127 {
				return lastHostname
			}
		}
	}
	return ""
}
