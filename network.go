package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"time"
)

type listenerExit struct {
	address  string
	listener net.Listener
}

// Only the reconciliation loop owns these maps. HTTP workers consume immutable
// address snapshots through Hub.mu; certificate handshakes use relayTLS.mu.
type relayListeners struct {
	server    *http.Server
	port      int
	tls       *tls.Config
	identity  *relayTLS
	listeners map[string]net.Listener
	failures  map[string]string
	exits     chan listenerExit
	stopped   chan struct{}
	publish   func([]string)
	listen    func(string, string) (net.Listener, error)
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
	if n.identity != nil {
		if err := n.identity.setNames(append(append([]string(nil), names...), "localhost", "127.0.0.1", "::1")); err != nil {
			log.Printf("network certificate: %v", err)
			return
		}
	}
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
	var available []string
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
		if !isLoopbackAddress(address) {
			available = append(available, n.scheme(address)+"://"+net.JoinHostPort(address, fmt.Sprint(n.port)))
		}
	}
	n.publish(available)
}

func (n *relayListeners) available() []string {
	addresses := []string{}
	for address := range n.listeners {
		if !isLoopbackAddress(address) {
			addresses = append(addresses, n.scheme(address)+"://"+net.JoinHostPort(address, fmt.Sprint(n.port)))
		}
	}
	slices.Sort(addresses)
	return addresses
}

func isLoopbackAddress(address string) bool {
	ip := net.ParseIP(address)
	return ip != nil && ip.IsLoopback()
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
			n.publish(n.available())
		case <-ticker.C:
			n.reconcile(resolve())
		}
	}
}

func (h *Hub) addresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string{}, h.addrs...)
}
func (h *Hub) setAddresses(addresses []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if slices.Equal(h.addrs, addresses) {
		return
	}
	h.addrs = append([]string{}, addresses...)
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
