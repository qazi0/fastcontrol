package main

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
