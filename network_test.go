package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestListenersRecoverAfterNetworkUnavailable(t *testing.T) {
	var attempts atomic.Int32
	n := newRelayListeners(&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })}, 0, nil, nil, func([]string) {})
	n.listen = func(network, address string) (net.Listener, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("interface not ready")
		}
		return net.Listen(network, address)
	}
	n.reconcile([]string{"127.0.0.1"})
	if len(n.listeners) != 0 {
		t.Fatal("failed bind published")
	}
	n.reconcile([]string{"127.0.0.1"})
	listener := n.listeners["127.0.0.1"]
	if listener == nil {
		t.Fatal("failed bind not retried")
	}
	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 204 {
		t.Fatal(response.Status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.run(ctx, func() []string { return nil }, time.Hour)
}

func TestListenerExitAndAddressRemovalRecover(t *testing.T) {
	var desired atomic.Value
	desired.Store([]string{"127.0.0.1"})
	opened := make(chan net.Listener, 10)
	observed := make(chan int, 100)
	n := newRelayListeners(&http.Server{}, 0, nil, nil, nil)
	n.publish = func([]string) { observed <- len(n.listeners) }
	n.listen = func(network, address string) (net.Listener, error) {
		listener, err := net.Listen(network, address)
		if err == nil {
			opened <- listener
		}
		return listener, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		n.run(ctx, func() []string { return desired.Load().([]string) }, 5*time.Millisecond)
		close(done)
	}()
	next := func() net.Listener {
		t.Helper()
		select {
		case listener := <-opened:
			return listener
		case <-time.After(time.Second):
			t.Fatal("listener did not recover")
			return nil
		}
	}
	first := next()
	first.Close()
	second := next()
	if first == second {
		t.Fatal("closed listener reused")
	}
	for len(observed) > 0 {
		<-observed
	}
	desired.Store([]string{})
	deadline := time.After(time.Second)
removed:
	for {
		select {
		case count := <-observed:
			if count == 0 {
				break removed
			}
		case <-deadline:
			t.Fatal("removed address was not withdrawn")
		}
	}
	// Cancellation joins the owner before inspecting its private maps.
	cancel()
	<-done
	if len(n.listeners) != 0 {
		t.Fatal("removed interface retained")
	}
}

func TestLiveTLSAddressChangeKeepsPairing(t *testing.T) {
	_, pin, identity, err := newRelayTLSIdentity(t.TempDir(), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err = identity.setNames([]string{"127.0.0.1", "192.168.1.77"}); err != nil {
		t.Fatal(err)
	}
	cert, err := identity.getCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.VerifyHostname("192.168.1.77") != nil || tlsFingerprint(leaf) != pin {
		t.Fatal("address change lost hostname or paired identity")
	}
}

func TestAddressSnapshotsAreIndependent(t *testing.T) {
	h := &Hub{clients: map[*Client]bool{}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				h.setAddresses([]string{"http://192.168.1.77:8787"})
				snapshot := h.addresses()
				snapshot[0] = "changed"
			}
		}()
	}
	wg.Wait()
	if h.addresses()[0] != "http://192.168.1.77:8787" {
		t.Fatal("published snapshot was mutated")
	}
}

func TestServeClientSendsHelloBeforeLargeThreadSnapshot(t *testing.T) {
	h := &Hub{
		clients:  map[*Client]bool{},
		sessions: map[string]*Session{},
		execs:    map[string]*execRun{},
		addrs:    []string{"http://192.168.1.10:8787", "http://blakberrisigma-2.local:8787"},
	}
	// Simulate an ~900 KB initial Codex thread snapshot.
	threads := make([]ThreadInfo, 400)
	pad := strings.Repeat("a", 2000)
	for i := range threads {
		threads[i] = ThreadInfo{ID: fmt.Sprintf("t-%d", i), Title: pad}
	}
	h.listJSON = mustJSON(map[string]any{"t": "threads", "threads": threads, "groups": []any{}})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		serveClient(r.Context(), h, conn, r.URL.Query().Get("events") == "1")
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Normal UI client must receive hello strictly first, then threads.
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(4 << 20)
	_, firstRaw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		T         string   `json:"t"`
		V         int      `json:"v"`
		Heartbeat bool     `json:"heartbeat"`
		Addrs     []string `json:"addrs"`
	}
	if err := json.Unmarshal(firstRaw, &first); err != nil {
		t.Fatal(err)
	}
	if first.T != "hello" || first.V != 3 || !first.Heartbeat {
		t.Fatalf("expected hello frame first, got %s", firstRaw)
	}
	if !slices.Equal(first.Addrs, h.addrs) {
		t.Fatalf("unexpected hello.addrs: %v", first.Addrs)
	}
	_, secondRaw, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var second struct {
		T       string       `json:"t"`
		Threads []ThreadInfo `json:"threads"`
	}
	if err := json.Unmarshal(secondRaw, &second); err != nil {
		t.Fatal(err)
	}
	if second.T != "threads" || len(second.Threads) != len(threads) {
		t.Fatalf("expected full threads snapshot second, got type=%q count=%d", second.T, len(second.Threads))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	// events=1 client must receive hello first and never receive the threads snapshot.
	evConn, _, err := websocket.Dial(ctx, wsURL+"?events=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, evFirstRaw, err := evConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var evFirst struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(evFirstRaw, &evFirst); err != nil || evFirst.T != "hello" {
		t.Fatalf("expected hello first for events=1 client, got %s", evFirstRaw)
	}
	h.setAddresses([]string{"http://192.168.1.7:8787", "http://m5p.local:8787"})
	_, evSecondRaw, err := evConn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var evSecond struct {
		T     string   `json:"t"`
		Addrs []string `json:"addrs"`
	}
	if err := json.Unmarshal(evSecondRaw, &evSecond); err != nil || evSecond.T != "network" {
		t.Fatalf("expected network update second for events=1 client, got %s", evSecondRaw)
	}
	if !slices.Equal(evSecond.Addrs, []string{"http://192.168.1.7:8787", "http://m5p.local:8787"}) {
		t.Fatalf("unexpected network.addrs: %v", evSecond.Addrs)
	}
	_ = evConn.Close(websocket.StatusNormalClosure, "")
}

func TestServeClientSerializesHelloWithConcurrentNetworkUpdates(t *testing.T) {
	h := &Hub{
		clients:  map[*Client]bool{},
		sessions: map[string]*Session{},
		execs:    map[string]*execRun{},
		addrs:    []string{"http://192.168.1.1:8787"},
		listJSON: mustJSON(map[string]any{"t": "threads", "threads": []ThreadInfo{{ID: "1", Title: "One"}}, "groups": []any{}}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		serveClient(r.Context(), h, conn, false)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	parseSeq := func(addrs []string) int {
		if len(addrs) != 1 {
			return -1
		}
		var n int
		if _, err := fmt.Sscanf(addrs[0], "http://192.168.1.%d:8787", &n); err != nil {
			return -1
		}
		return n
	}

	const finalSeq = 40
	var updaterWG sync.WaitGroup
	updaterWG.Add(1)
	go func() {
		defer updaterWG.Done()
		for i := 2; i <= finalSeq; i++ {
			h.setAddresses([]string{fmt.Sprintf("http://192.168.1.%d:8787", i)})
			time.Sleep(time.Millisecond)
		}
	}()

	var clientsWG sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 12; i++ {
		clientsWG.Add(1)
		go func(idx int) {
			defer clientsWG.Done()
			time.Sleep(time.Duration(idx*2) * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, wsURL, nil)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close(websocket.StatusNormalClosure, "")
			_, raw, err := conn.Read(ctx)
			if err != nil {
				errCh <- err
				return
			}
			var msg struct {
				T     string   `json:"t"`
				Addrs []string `json:"addrs"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				errCh <- err
				return
			}
			if msg.T != "hello" {
				errCh <- fmt.Errorf("client %d: frame 1 was %q, want hello", idx, msg.T)
				return
			}
			lastSeq := parseSeq(msg.Addrs)
			if lastSeq < 1 {
				errCh <- fmt.Errorf("client %d: invalid hello.addrs %v", idx, msg.Addrs)
				return
			}
			sawThreads := false
			for lastSeq < finalSeq || !sawThreads {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					errCh <- fmt.Errorf("client %d read (lastSeq=%d sawThreads=%v): %w", idx, lastSeq, sawThreads, err)
					return
				}
				var next struct {
					T     string   `json:"t"`
					Addrs []string `json:"addrs"`
				}
				if err := json.Unmarshal(raw, &next); err != nil {
					errCh <- err
					return
				}
				switch next.T {
				case "threads":
					sawThreads = true
				case "network":
					seq := parseSeq(next.Addrs)
					if seq <= lastSeq {
						errCh <- fmt.Errorf("client %d: out-of-order network seq %d after %d", idx, seq, lastSeq)
						return
					}
					lastSeq = seq
				}
			}
		}(i)
	}
	updaterWG.Wait()
	clientsWG.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestReconcileAdvertisesStableHostnamesWithoutBindingThem(t *testing.T) {
	dir := t.TempDir()
	tlsCfg, pin, identity, err := newRelayTLSIdentity(dir, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	var published []string
	var boundHosts []string
	var lanActive bool
	var lanIPs []net.IP

	n := newRelayListeners(&http.Server{}, 8787, tlsCfg, identity, func(addrs []string) {
		published = append([]string(nil), addrs...)
	})
	n.hostnames = func(hasLAN, hasTailscale bool) relayHostnames {
		var out relayHostnames
		if hasLAN {
			out.Local = "blakberrisigma-2.local"
		}
		if hasTailscale {
			out.MagicDNS = "blakberrisigma.tail1000c2.ts.net"
		}
		return out
	}
	n.onLANState = func(active bool, ips []net.IP) {
		lanActive = active
		lanIPs = ips
	}
	n.listen = func(network, address string) (net.Listener, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if net.ParseIP(host) == nil {
			t.Fatalf("attempted to net.Listen on non-IP hostname %q", host)
		}
		boundHosts = append(boundHosts, host)
		return net.Listen("tcp", "127.0.0.1:0")
	}

	// Loopback only: no external routes advertised, mDNS inactive.
	n.reconcile([]string{"127.0.0.1"})
	if len(published) != 0 || lanActive || len(lanIPs) != 0 {
		t.Fatalf("loopback published external routes: addrs=%v lanActive=%v ips=%v", published, lanActive, lanIPs)
	}

	// LAN + Tailscale: advertises IPv4s, .local and MagicDNS over HTTPS, and updates TLS SANs.
	n.reconcile([]string{"127.0.0.1", "192.168.1.10", "100.125.53.124"})
	want := []string{
		"https://192.168.1.10:8787",
		"https://blakberrisigma-2.local:8787",
		"https://100.125.53.124:8787",
		"https://blakberrisigma.tail1000c2.ts.net:8787",
	}
	if !slices.Equal(published, want) {
		t.Fatalf("published = %v, want %v", published, want)
	}
	if !lanActive || len(lanIPs) != 1 || lanIPs[0].String() != "192.168.1.10" {
		t.Fatalf("unexpected LAN state: active=%v ips=%v", lanActive, lanIPs)
	}
	cert, err := identity.getCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"192.168.1.10", "100.125.53.124", "blakberrisigma-2.local", "blakberrisigma.tail1000c2.ts.net"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Fatalf("TLS cert missing SAN for %s: %v", host, err)
		}
	}
	if tlsFingerprint(leaf) != pin {
		t.Fatal("adding stable hostnames to TLS SANs changed the paired pin")
	}

	// Tailscale only: drops .local and disables LAN mDNS, keeps MagicDNS.
	n.reconcile([]string{"127.0.0.1", "100.125.53.124"})
	wantTS := []string{
		"https://100.125.53.124:8787",
		"https://blakberrisigma.tail1000c2.ts.net:8787",
	}
	if !slices.Equal(published, wantTS) {
		t.Fatalf("tailscale-only published = %v, want %v", published, wantTS)
	}
	if lanActive || len(lanIPs) != 0 {
		t.Fatalf("LAN mDNS should pause on Tailscale-only bind: active=%v ips=%v", lanActive, lanIPs)
	}

	// Verify net.Listen only ever saw IP literals.
	for _, host := range boundHosts {
		if net.ParseIP(host) == nil {
			t.Fatalf("non-IP passed to listen: %q", host)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.run(ctx, func() []string { return nil }, time.Hour)
}

func TestSanitizeLocalHostLabelAndTailscaleMagicDNS(t *testing.T) {
	labelCases := []struct {
		in   string
		want string
	}{
		{"blakberrisigma-2", "blakberrisigma-2"},
		{"M5P.local.", "m5p"},
		{"Siraj's MacBook Pro", "sirajs-macbook-pro"},
		{"my_host.home.arpa", "my-host"},
		{"192.168.1.10", ""},
		{"192.168.1.10.local", ""},
		{"::1", ""},
		{"localhost", ""},
		{"localhost.local", ""},
		{"---", ""},
		{strings.Repeat("a", 80), strings.Repeat("a", 63)},
	}
	for _, tc := range labelCases {
		if got := sanitizeLocalHostLabel(tc.in); got != tc.want {
			t.Errorf("sanitizeLocalHostLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	tsCases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "running with MagicDNS",
			json: `{"BackendState":"Running","Self":{"DNSName":"BlakBerriSigma.tail1000c2.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`,
			want: "blakberrisigma.tail1000c2.ts.net",
		},
		{
			name: "running without CurrentTailnet field",
			json: `{"BackendState":"Running","Self":{"DNSName":"m5p.tail1000c2.ts.net."}}`,
			want: "m5p.tail1000c2.ts.net",
		},
		{
			name: "stopped",
			json: `{"BackendState":"Stopped","Self":{"DNSName":"m5p.tail1000c2.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`,
			want: "",
		},
		{
			name: "MagicDNS disabled",
			json: `{"BackendState":"Running","Self":{"DNSName":"m5p.tail1000c2.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":false}}`,
			want: "",
		},
		{
			name: "single label rejected",
			json: `{"BackendState":"Running","Self":{"DNSName":"m5p."}}`,
			want: "",
		},
		{
			name: "local domain rejected",
			json: `{"BackendState":"Running","Self":{"DNSName":"m5p.local."}}`,
			want: "",
		},
		{
			name: "IP address rejected",
			json: `{"BackendState":"Running","Self":{"DNSName":"100.67.66.88"}}`,
			want: "",
		},
		{
			name: "invalid JSON",
			json: `{broken`,
			want: "",
		},
	}
	for _, tc := range tsCases {
		if got := parseTailscaleMagicDNS([]byte(tc.json)); got != tc.want {
			t.Errorf("%s: parseTailscaleMagicDNS = %q, want %q", tc.name, got, tc.want)
		}
	}
}
