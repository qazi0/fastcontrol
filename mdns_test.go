package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMDNSTXTFingerprintAndSanitization(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef"
	sum := sha256.Sum256([]byte(token))
	wantFP := hex.EncodeToString(sum[:])[:12]

	if got := mdnsTokenFingerprint(" " + token + "\n"); got != wantFP {
		t.Fatalf("mdnsTokenFingerprint = %q, want %q", got, wantFP)
	}
	if len(wantFP) != 12 {
		t.Fatalf("fingerprint length = %d, want 12", len(wantFP))
	}

	cfg := mdnsServiceConfig{
		Instance: "-Siraj's Mac\x00",
		Host:     "BlakBerriSigma-2.local",
		Port:     8787,
		TLS:      true,
		Token:    token,
	}
	txt := cfg.txtRecords()
	wantTXT := []string{
		"fp=" + wantFP,
		"host=blakberrisigma-2",
		"tls=1",
		"v=3",
	}
	if !slices.Equal(txt, wantTXT) {
		t.Fatalf("txtRecords = %v, want %v", txt, wantTXT)
	}
	for _, entry := range txt {
		if strings.Contains(entry, token) {
			t.Fatalf("raw token leaked into TXT record: %q", entry)
		}
	}

	httpCfg := cfg
	httpCfg.TLS = false
	if !slices.Contains(httpCfg.txtRecords(), "tls=0") {
		t.Fatalf("expected tls=0 for HTTP mode, got %v", httpCfg.txtRecords())
	}

	args := cfg.dnsSDArgs()
	if len(args) < 9 || args[0] != "-R" || args[1] != "Siraj's Mac" || args[2] != "_fastcontrol._tcp" || args[3] != "local" || args[4] != "8787" {
		t.Fatalf("unexpected dns-sd args: %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, token) {
			t.Fatalf("raw token leaked into dns-sd args: %q", a)
		}
	}

	// Long UTF-8 instance name must truncate cleanly to <= 63 bytes on rune boundary.
	longName := strings.Repeat("é", 40) // 80 bytes
	sanitized := sanitizeMDNSInstance(longName, "m5p")
	if len(sanitized) > 63 || !utf8.ValidString(sanitized) {
		t.Fatalf("invalid UTF-8 truncation: len=%d valid=%v", len(sanitized), utf8.ValidString(sanitized))
	}
}

type parsedDNSRecord struct {
	Labels []string
	Type   uint16
	Class  uint16
	TTL    uint32
	RData  []byte
}

func parseTestDNSResponse(t *testing.T, pkt []byte) (id uint16, flags uint16, answers, additionals []parsedDNSRecord) {
	t.Helper()
	if len(pkt) < 12 {
		t.Fatalf("short DNS packet: %d bytes", len(pkt))
	}
	id = binary.BigEndian.Uint16(pkt[0:2])
	flags = binary.BigEndian.Uint16(pkt[2:4])
	anCount := int(binary.BigEndian.Uint16(pkt[6:8]))
	arCount := int(binary.BigEndian.Uint16(pkt[10:12]))
	off := 12
	readRR := func() parsedDNSRecord {
		labels, next, err := decodeDNSName(pkt, off)
		if err != nil {
			t.Fatalf("decodeDNSName failed at %d: %v", off, err)
		}
		if next+10 > len(pkt) {
			t.Fatalf("truncated RR header at %d", next)
		}
		typ := binary.BigEndian.Uint16(pkt[next : next+2])
		cls := binary.BigEndian.Uint16(pkt[next+2 : next+4])
		ttl := binary.BigEndian.Uint32(pkt[next+4 : next+8])
		rdLen := int(binary.BigEndian.Uint16(pkt[next+8 : next+10]))
		start := next + 10
		if start+rdLen > len(pkt) {
			t.Fatalf("truncated RData at %d (need %d)", start, rdLen)
		}
		off = start + rdLen
		return parsedDNSRecord{Labels: labels, Type: typ, Class: cls, TTL: ttl, RData: pkt[start:off]}
	}
	for i := 0; i < anCount; i++ {
		answers = append(answers, readRR())
	}
	for i := 0; i < arCount; i++ {
		additionals = append(additionals, readRR())
	}
	return id, flags, answers, additionals
}

func buildTestMDNSQuery(id uint16, labels []string, qtype, qclass uint16) []byte {
	buf := make([]byte, 12)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], 0) // QR=0 standard query
	binary.BigEndian.PutUint16(buf[4:6], 1) // QDCOUNT=1
	buf = append(buf, encodeDNSName(labels)...)
	var tail [4]byte
	binary.BigEndian.PutUint16(tail[0:2], qtype)
	binary.BigEndian.PutUint16(tail[2:4], qclass)
	buf = append(buf, tail[:]...)
	return buf
}

func TestMDNSWireProtocolAndLiveResponder(t *testing.T) {
	token := "abcdef0123456789abcdef0123456789abcdef0123456789"
	wantFP := mdnsTokenFingerprint(token)

	sinkConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer sinkConn.Close()

	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}

	sinkAddr := sinkConn.LocalAddr().(*net.UDPAddr)
	serverAddr := serverConn.LocalAddr().(*net.UDPAddr)

	cfg := mdnsServiceConfig{
		Instance: "blakberrisigma",
		Host:     "blakberrisigma-2",
		Port:     8787,
		TLS:      true,
		Token:    token,
	}
	responder := newPureMDNSResponderWithConn(cfg, []net.IP{net.IPv4(192, 168, 1, 10)}, serverConn, sinkAddr)

	readPacket := func(conn *net.UDPConn, timeout time.Duration) []byte {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		buf := make([]byte, 4096)
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("expected UDP packet: %v", err)
		}
		return buf[:n]
	}

	// 1. Initial announcement arrives on multicast destination.
	ann := readPacket(sinkConn, 2*time.Second)
	if strings.Contains(string(ann), token) {
		t.Fatal("raw token leaked into announcement packet")
	}
	_, flags, answers, _ := parseTestDNSResponse(t, ann)
	if flags != 0x8400 {
		t.Fatalf("unexpected announcement flags: 0x%x", flags)
	}
	sawPTR, sawSRV, sawTXT, sawA := false, false, false, false
	for _, rr := range answers {
		switch rr.Type {
		case dnsTypePTR:
			if equalDNSLabels(rr.Labels, mdnsServiceTypeLabels) {
				target, _, err := decodeDNSName(rr.RData, 0)
				if err != nil || !equalDNSLabels(target, []string{"blakberrisigma", "_fastcontrol", "_tcp", "local"}) {
					t.Fatalf("bad PTR target: %v %v", target, err)
				}
				sawPTR = true
			}
		case dnsTypeSRV:
			port := binary.BigEndian.Uint16(rr.RData[4:6])
			target, _, err := decodeDNSName(rr.RData, 6)
			if err != nil || port != 8787 || !equalDNSLabels(target, []string{"blakberrisigma-2", "local"}) {
				t.Fatalf("bad SRV: port=%d target=%v err=%v", port, target, err)
			}
			sawSRV = true
		case dnsTypeTXT:
			entries, err := decodeTXTRData(rr.RData)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"fp=" + wantFP, "host=blakberrisigma-2", "tls=1", "v=3"}
			if !slices.Equal(entries, want) {
				t.Fatalf("TXT entries = %v, want %v", entries, want)
			}
			sawTXT = true
		case dnsTypeA:
			if len(rr.RData) == 4 && net.IP(rr.RData).Equal(net.IPv4(192, 168, 1, 10)) {
				sawA = true
			}
		}
	}
	if !sawPTR || !sawSRV || !sawTXT || !sawA {
		t.Fatalf("incomplete announcement: ptr=%v srv=%v txt=%v a=%v", sawPTR, sawSRV, sawTXT, sawA)
	}

	// 2. Query _fastcontrol._tcp.local. from an ephemeral port (legacy unicast).
	querier, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer querier.Close()

	query := buildTestMDNSQuery(0x4242, []string{"_FASTCONTROL", "_TCP", "LOCAL"}, dnsTypePTR, dnsClassIN)
	if _, err := querier.WriteToUDP(query, serverAddr); err != nil {
		t.Fatal(err)
	}
	resp := readPacket(querier, 2*time.Second)
	if strings.Contains(string(resp), token) {
		t.Fatal("raw token leaked into query response")
	}
	respID, _, qAnswers, qAdditionals := parseTestDNSResponse(t, resp)
	if respID != 0x4242 {
		t.Fatalf("legacy unicast did not preserve query ID: got 0x%x", respID)
	}
	if len(qAnswers) != 1 || qAnswers[0].Type != dnsTypePTR || qAnswers[0].TTL > mdnsLegacyMaxTTL {
		t.Fatalf("unexpected PTR query answer: %+v", qAnswers)
	}
	if len(qAdditionals) != 3 {
		t.Fatalf("expected SRV+TXT+A additionals, got %d", len(qAdditionals))
	}

	// 3. Malformed packets (pointer loop, truncated, QR=1) must not panic or reply.
	pointerLoop := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xC0, 0x0C, 0, 12, 0, 1}
	_, _ = querier.WriteToUDP(pointerLoop, serverAddr)
	unrelated := buildTestMDNSQuery(0x1111, []string{"_http", "_tcp", "local"}, dnsTypePTR, dnsClassIN)
	_, _ = querier.WriteToUDP(unrelated, serverAddr)
	_ = querier.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	tmp := make([]byte, 256)
	if n, _, err := querier.ReadFromUDP(tmp); err == nil {
		t.Fatalf("expected no reply for malformed/unrelated queries, got %d bytes", n)
	}

	// 4. Updating LAN IPs sends an updated announcement with the new IPv4 address.
	responder.UpdateIPs([]net.IP{net.IPv4(192, 168, 1, 7)})
	deadline := time.Now().Add(2 * time.Second)
	sawNewIP := false
	for !sawNewIP && time.Now().Before(deadline) {
		pkt := readPacket(sinkConn, time.Until(deadline))
		_, _, rrs, _ := parseTestDNSResponse(t, pkt)
		for _, rr := range rrs {
			if rr.Type == dnsTypeA && len(rr.RData) == 4 && net.IP(rr.RData).Equal(net.IPv4(192, 168, 1, 7)) {
				sawNewIP = true
			}
		}
	}
	if !sawNewIP {
		t.Fatal("IP update did not trigger updated A record announcement")
	}

	// 5. Close sends goodbye (TTL=0 for PTR/SRV/TXT, never TTL=0 for host A) and exits cleanly.
	responder.Close()
	responder.Close() // idempotent

	goodbyeDeadline := time.Now().Add(2 * time.Second)
	sawGoodbye := false
	for !sawGoodbye && time.Now().Before(goodbyeDeadline) {
		pkt := readPacket(sinkConn, time.Until(goodbyeDeadline))
		_, _, rrs, _ := parseTestDNSResponse(t, pkt)
		for _, rr := range rrs {
			if rr.Type == dnsTypeA && rr.TTL == 0 {
				t.Fatal("goodbye must not flush host A record")
			}
			if rr.Type == dnsTypePTR && rr.TTL == 0 && equalDNSLabels(rr.Labels, mdnsServiceTypeLabels) {
				sawGoodbye = true
			}
		}
	}
	if !sawGoodbye {
		t.Fatal("Close did not send PTR TTL=0 goodbye packet")
	}
}

type fakeMDNSBackend struct {
	mu      sync.Mutex
	updates [][]net.IP
	closed  bool
}

func (f *fakeMDNSBackend) UpdateIPs(ips []net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, ips)
}

func (f *fakeMDNSBackend) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

func TestMDNSAdvertiserLifecycle(t *testing.T) {
	var starts int
	var last *fakeMDNSBackend
	adv := newMDNSAdvertiser(mdnsServiceConfig{Instance: "Mac", Host: "m5p", Port: 8787, Token: "secret-token-value-12345678901234567890"})
	adv.startBackend = func(cfg mdnsServiceConfig, ips []net.IP) mdnsBackend {
		starts++
		last = &fakeMDNSBackend{}
		return last
	}

	adv.SetLANState(false, nil)
	if starts != 0 {
		t.Fatal("started backend with inactive LAN")
	}
	adv.SetLANState(true, []net.IP{net.IPv4(192, 168, 1, 10)})
	if starts != 1 || last == nil || last.closed {
		t.Fatal("backend did not start on active LAN")
	}
	adv.SetLANState(true, []net.IP{net.IPv4(192, 168, 1, 7)})
	if starts != 1 || len(last.updates) != 1 || !last.updates[0][0].Equal(net.IPv4(192, 168, 1, 7)) {
		t.Fatalf("expected UpdateIPs on IP change, starts=%d updates=%v", starts, last.updates)
	}
	first := last
	adv.SetLANState(false, nil)
	if !first.closed {
		t.Fatal("backend was not closed when LAN went inactive")
	}
	adv.SetLANState(true, []net.IP{net.IPv4(192, 168, 1, 11)})
	if starts != 2 || last == first || last.closed {
		t.Fatal("backend did not restart when LAN returned")
	}
	adv.Close()
	if !last.closed {
		t.Fatal("advertiser Close did not close active backend")
	}
	adv.SetLANState(true, []net.IP{net.IPv4(192, 168, 1, 12)})
	if starts != 2 {
		t.Fatal("closed advertiser restarted backend")
	}
}
