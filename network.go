package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var _, tailscaleCGNAT, _ = net.ParseCIDR("100.64.0.0/10")

type listenerExit struct {
	address  string
	listener net.Listener
}

// relayHostnames carries advertised DNS hostnames (never bound with net.Listen).
type relayHostnames struct {
	Local    string // e.g. "blakberrisigma-2.local"
	MagicDNS string // e.g. "blakberrisigma.tail1000c2.ts.net"
}

// Only the reconciliation loop owns these maps. HTTP workers consume immutable
// address snapshots through Hub.mu; certificate handshakes use relayTLS.mu.
type relayListeners struct {
	server     *http.Server
	port       int
	tls        *tls.Config
	identity   *relayTLS
	listeners  map[string]net.Listener
	failures   map[string]string
	exits      chan listenerExit
	stopped    chan struct{}
	publish    func([]string)
	listen     func(string, string) (net.Listener, error)
	hostnames  func(hasLAN, hasTailscale bool) relayHostnames
	lastHosts  relayHostnames
	onLANState func(active bool, ips []net.IP)
}

func newRelayListeners(server *http.Server, port int, config *tls.Config, identity *relayTLS, publish func([]string)) *relayListeners {
	return &relayListeners{server: server, port: port, tls: config, identity: identity, listeners: map[string]net.Listener{}, failures: map[string]string{}, exits: make(chan listenerExit), stopped: make(chan struct{}), publish: publish, listen: net.Listen}
}

func (n *relayListeners) reconcile(addresses []string) {
	wanted := map[string]bool{}
	var names []string
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address != "" && !wanted[address] {
			wanted[address] = true
			names = append(names, address)
		}
	}
	slices.Sort(names)

	wantedLAN, wantedTS := listenerKinds(names)
	var hosts relayHostnames
	if n.hostnames != nil && (wantedLAN || wantedTS) {
		hosts = sanitizeRelayHostnames(n.hostnames(wantedLAN, wantedTS))
	}

	if n.identity != nil {
		if err := n.identity.setNames(relayCertNames(names, hosts)); err != nil {
			log.Printf("network certificate: %v", err)
			return
		}
	}
	n.lastHosts = hosts
	for address, listener := range n.listeners {
		if !wanted[address] {
			delete(n.listeners, address)
			_ = listener.Close()
		}
	}
	for address := range n.failures {
		if !wanted[address] {
			delete(n.failures, address)
		}
	}
	for _, address := range names {
		listener := n.listeners[address]
		if listener == nil {
			var err error
			listener, err = n.listen("tcp", net.JoinHostPort(address, fmt.Sprint(n.port)))
			if err != nil {
				if n.failures[address] != err.Error() {
					log.Printf("listen %s (will retry): %v", address, err)
				}
				n.failures[address] = err.Error()
				continue
			}
			delete(n.failures, address)
			if n.tls != nil && !isLoopbackAddress(address) {
				listener = tls.NewListener(listener, n.tls)
			}
			n.listeners[address] = listener
			log.Printf("listening on %s://%s", n.scheme(address), listener.Addr())
			go func(address string, listener net.Listener) {
				err := n.server.Serve(listener)
				if err != nil && err != http.ErrServerClosed && !strings.Contains(err.Error(), "closed network connection") {
					log.Printf("listener %s: %v", address, err)
				}
				select {
				case n.exits <- listenerExit{address, listener}:
				case <-n.stopped:
				}
			}(address, listener)
		}
	}
	n.publishState()
}

func (n *relayListeners) publishState() {
	if n.publish != nil {
		n.publish(n.available())
	}
	if n.onLANState != nil {
		active, ips := n.lanState()
		n.onLANState(active, ips)
	}
}

func (n *relayListeners) available() []string {
	var lanAddrs []string
	var tsAddrs []string
	var activeNames []string
	for address := range n.listeners {
		activeNames = append(activeNames, address)
		if isLoopbackAddress(address) {
			continue
		}
		u := n.scheme(address) + "://" + net.JoinHostPort(address, fmt.Sprint(n.port))
		if isTailscaleAddress(address) {
			tsAddrs = append(tsAddrs, u)
		} else {
			lanAddrs = append(lanAddrs, u)
		}
	}
	slices.Sort(lanAddrs)
	slices.Sort(tsAddrs)
	hasLAN, hasTS := listenerKinds(activeNames)
	scheme := "http"
	if n.tls != nil {
		scheme = "https"
	}
	port := fmt.Sprint(n.port)
	addresses := make([]string, 0, len(lanAddrs)+len(tsAddrs)+2)
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			addresses = append(addresses, u)
		}
	}
	for _, u := range lanAddrs {
		add(u)
	}
	if hasLAN && n.lastHosts.Local != "" {
		add(scheme + "://" + net.JoinHostPort(n.lastHosts.Local, port))
	}
	for _, u := range tsAddrs {
		add(u)
	}
	if hasTS && n.lastHosts.MagicDNS != "" {
		add(scheme + "://" + net.JoinHostPort(n.lastHosts.MagicDNS, port))
	}
	return addresses
}

func (n *relayListeners) lanState() (bool, []net.IP) {
	active := false
	hasWildcard := false
	seen := map[string]bool{}
	var ips []net.IP
	addIP := func(ip net.IP) {
		v4 := ip.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() || tailscaleCGNAT.Contains(v4) {
			return
		}
		key := v4.String()
		if !seen[key] {
			seen[key] = true
			ips = append(ips, append(net.IP(nil), v4...))
		}
	}
	for address := range n.listeners {
		if isLoopbackAddress(address) || isTailscaleAddress(address) {
			continue
		}
		active = true
		if isWildcardAddress(address) {
			hasWildcard = true
			continue
		}
		if ip := net.ParseIP(address); ip != nil {
			addIP(ip)
		}
	}
	if hasWildcard && len(ips) == 0 {
		auto, _ := autoAddrs()
		for _, a := range auto {
			if ip := net.ParseIP(a); ip != nil {
				addIP(ip)
			}
		}
	}
	slices.SortFunc(ips, func(a, b net.IP) int { return strings.Compare(a.String(), b.String()) })
	return active, ips
}

func isLoopbackAddress(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.IsLoopback()
}

func isWildcardAddress(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.IsUnspecified()
}

func isTailscaleAddress(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.To4() != nil && tailscaleCGNAT.Contains(ip.To4())
}

func listenerKinds(addresses []string) (hasLAN, hasTailscale bool) {
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" || isLoopbackAddress(address) {
			continue
		}
		if isWildcardAddress(address) {
			hasLAN = true
			hasTailscale = true
		} else if isTailscaleAddress(address) {
			hasTailscale = true
		} else {
			hasLAN = true
		}
	}
	return hasLAN, hasTailscale
}

func relayCertNames(bindAddrs []string, hosts relayHostnames) []string {
	raw := make([]string, 0, len(bindAddrs)+5)
	for _, a := range bindAddrs {
		if a = strings.TrimSpace(a); a != "" {
			raw = append(raw, a)
		}
	}
	raw = append(raw, "localhost", "127.0.0.1", "::1")
	hosts = sanitizeRelayHostnames(hosts)
	if hosts.Local != "" {
		raw = append(raw, hosts.Local)
	}
	if hosts.MagicDNS != "" {
		raw = append(raw, hosts.MagicDNS)
	}
	return normalizeCertNames(raw)
}

func sanitizeRelayHostnames(hosts relayHostnames) relayHostnames {
	var out relayHostnames
	if label := sanitizeLocalHostLabel(hosts.Local); label != "" {
		out.Local = label + ".local"
	}
	magic := strings.ToLower(strings.TrimRight(strings.TrimSpace(hosts.MagicDNS), "."))
	if isValidDNSName(magic) && !strings.HasSuffix(magic, ".local") && magic != "localhost" {
		out.MagicDNS = magic
	}
	return out
}

// sanitizeLocalHostLabel converts a LocalHostName or system hostname into a
// lowercase RFC 1123 single DNS label suitable for <label>.local.
func sanitizeLocalHostLabel(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, ".")
	if s == "" || net.ParseIP(s) != nil {
		return ""
	}
	if len(s) > 6 && strings.EqualFold(s[len(s)-6:], ".local") {
		s = strings.TrimRight(s[:len(s)-6], ".")
	}
	if s == "" || net.ParseIP(s) != nil {
		return ""
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ':
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	label := strings.Trim(b.String(), "-")
	if len(label) > 63 {
		label = strings.TrimRight(label[:63], "-")
	}
	if label == "" || label == "localhost" {
		return ""
	}
	return label
}

func isValidDNSName(name string) bool {
	if len(name) == 0 || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

func localHostLabel() string {
	if runtime.GOOS == "darwin" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cmd := exec.CommandContext(ctx, "/usr/sbin/scutil", "--get", "LocalHostName")
		prepareProcess(cmd)
		out, err := cmd.Output()
		cancel()
		if err == nil {
			if label := sanitizeLocalHostLabel(string(out)); label != "" {
				return label
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		if label := sanitizeLocalHostLabel(h); label != "" {
			return label
		}
	}
	return ""
}

func parseTailscaleMagicDNS(data []byte) string {
	var status struct {
		BackendState string `json:"BackendState"`
		Self         *struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
		CurrentTailnet *struct {
			MagicDNSEnabled *bool `json:"MagicDNSEnabled"`
		} `json:"CurrentTailnet"`
	}
	if json.Unmarshal(data, &status) != nil {
		return ""
	}
	if status.BackendState != "Running" || status.Self == nil {
		return ""
	}
	if status.CurrentTailnet != nil && status.CurrentTailnet.MagicDNSEnabled != nil && !*status.CurrentTailnet.MagicDNSEnabled {
		return ""
	}
	name := strings.ToLower(strings.TrimRight(strings.TrimSpace(status.Self.DNSName), "."))
	if !isValidDNSName(name) || strings.HasSuffix(name, ".local") || name == "localhost" {
		return ""
	}
	return name
}

func tailscaleBinary() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
		}
	case "linux":
		candidates = []string{"/usr/bin/tailscale", "/usr/local/bin/tailscale"}
	case "windows":
		candidates = []string{
			filepath.Join(os.Getenv("ProgramFiles"), "Tailscale", "tailscale.exe"),
			filepath.Join(os.Getenv("ProgramFiles(x86)"), "Tailscale", "tailscale.exe"),
		}
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func discoverTailscaleMagicDNS() string {
	bin := tailscaleBinary()
	if bin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "status", "--json")
	prepareProcess(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseTailscaleMagicDNS(out)
}

func newSystemHostnameResolver() func(hasLAN, hasTailscale bool) relayHostnames {
	var mu sync.Mutex
	var cached relayHostnames
	var lastLAN, lastTS bool
	var fetchedAt time.Time
	return func(hasLAN, hasTailscale bool) relayHostnames {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if !fetchedAt.IsZero() && hasLAN == lastLAN && hasTailscale == lastTS && now.Sub(fetchedAt) < 30*time.Second {
			return cached
		}
		var next relayHostnames
		if hasLAN {
			if label := localHostLabel(); label != "" {
				next.Local = label + ".local"
			}
		}
		if hasTailscale {
			next.MagicDNS = discoverTailscaleMagicDNS()
		}
		cached, lastLAN, lastTS, fetchedAt = next, hasLAN, hasTailscale, now
		return cached
	}
}

func (n *relayListeners) scheme(address string) string {
	if n.tls != nil && !isLoopbackAddress(address) {
		return "https"
	}
	return "http"
}

func (n *relayListeners) run(ctx context.Context, resolve func() []string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	defer close(n.stopped)
	defer func() {
		for _, listener := range n.listeners {
			_ = listener.Close()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case exit := <-n.exits:
			if n.listeners[exit.address] == exit.listener {
				delete(n.listeners, exit.address)
			}
			n.publishState()
		case <-ticker.C:
			n.reconcile(resolve())
		}
	}
}

func (h *Hub) addresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.addrs))
	copy(out, h.addrs)
	return out
}

func (h *Hub) setAddresses(addresses []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Equal(h.addrs, addresses) {
		return
	}
	h.addrs = make([]string, len(addresses))
	copy(h.addrs, addresses)
	message := mustJSON(map[string]any{"t": "network", "addrs": h.addrs})
	for client := range h.clients {
		client.push(message)
	}
}

func physicalLANInterface(name string) bool {
	name = strings.ToLower(name)
	if runtime.GOOS == "windows" {
		return strings.HasPrefix(name, "ethernet") || strings.HasPrefix(name, "wi-fi") || strings.HasPrefix(name, "wlan")
	}
	return strings.HasPrefix(name, "en") || strings.HasPrefix(name, "eth") || strings.HasPrefix(name, "wl")
}
