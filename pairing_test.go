package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPairingQRRetainsIdentityAndAlternateRoutes(t *testing.T) {
	info := pairingInfo{Host: "My Mac", URLs: []string{"https://100.64.0.2:8787", "https://192.168.1.20:8787"}, Pin: strings.Repeat("a", 64)}
	link, err := info.link(strings.Repeat("b", 48))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "fastcontrol" || u.Host != "pair" || q.Get("url") != "https://192.168.1.20:8787" || q.Get("pin") != info.Pin || q.Get("t") != strings.Repeat("b", 48) {
		t.Fatal("pairing data changed")
	}
	var alternatives []string
	if err = json.Unmarshal([]byte(q.Get("alts")), &alternatives); err != nil || len(alternatives) != 1 || alternatives[0] != "https://100.64.0.2:8787" {
		t.Fatal("lost alternate route", err)
	}
	if _, err = (pairingInfo{URLs: []string{"http://127.0.0.1:8787"}, Pin: info.Pin}).link("key"); err == nil {
		t.Fatal("secure pairing downgraded to HTTP")
	}
}

func TestPairingCommandUsesRunningServiceWithoutPrintingToken(t *testing.T) {
	token := strings.Repeat("b", 48)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pairing" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong pairing request")
		}
		json.NewEncoder(w).Encode(pairingInfo{Host: "Mac", URLs: []string{"http://192.168.1.20:8787"}})
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	_, portText, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portText)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := printPairing(ctx, port, token, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), token) || strings.Contains(output.String(), "fastcontrol://") || !strings.Contains(output.String(), "█") {
		t.Fatal("expected QR without a raw credential link")
	}
}
