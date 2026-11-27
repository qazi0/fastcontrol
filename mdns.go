package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	mdnsServiceType   = "_fastcontrol._tcp"
	mdnsDomain        = "local"
	mdnsMulticastPort = 5353
	mdnsDefaultTTL    = 120
	mdnsLegacyMaxTTL  = 10
	mdnsProtocolVer   = "3"

	dnsTypeA      uint16 = 1
	dnsTypePTR    uint16 = 12
	dnsTypeTXT    uint16 = 16
	dnsTypeSRV    uint16 = 33
	dnsTypeANY    uint16 = 255
	dnsClassIN    uint16 = 1
	dnsClassFlush uint16 = 0x8001
)

var (
	mdnsServicesEnumLabels = []string{"_services", "_dns-sd", "_udp", "local"}
	mdnsServiceTypeLabels  = []string{"_fastcontrol", "_tcp", "local"}
	mdnsMulticastIPv4      = net.IPv4(224, 0, 0, 251)
)

// mdnsServiceConfig describes the _fastcontrol._tcp DNS-SD service advertised on LAN.
type mdnsServiceConfig struct {
	Instance string
	Host     string
	Port     int
	TLS      bool
	Token    string
}

// mdnsTokenFingerprint returns the first 12 lowercase hex characters of
// SHA-256(token). Raw tokens are never exposed in mDNS TXT records.
func mdnsTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])[:12]
}

func sanitizeMDNSInstance(raw, fallbackHost string) string {
	s := strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	s = strings.TrimSpace(strings.TrimLeft(s, "-"))
	if s == "" {
		s = sanitizeLocalHostLabel(fallbackHost)
	}
	if s == "" {
		s = "fastcontrol"
	}
	for len(s) > 63 {
		_, size := utf8.DecodeLastRuneInString(s)
		if size <= 0 {
			s = s[:63]
			break
		}
		s = s[:len(s)-size]
	}
	return s
}

func (cfg mdnsServiceConfig) normalized() mdnsServiceConfig {
	host := sanitizeLocalHostLabel(cfg.Host)
	if host == "" {
		host = sanitizeLocalHostLabel(cfg.Instance)
	}
	if host == "" {
		host = "fastcontrol"
	}
	instance := sanitizeMDNSInstance(cfg.Instance, host)
	return mdnsServiceConfig{
		Instance: instance,
		Host:     host,
		Port:     cfg.Port,
		TLS:      cfg.TLS,
		Token:    cfg.Token,
	}
}

func (cfg mdnsServiceConfig) txtRecords() []string {
	norm := cfg.normalized()
	tlsVal := "0"
	if norm.TLS {
		tlsVal = "1"
	}
	return []string{
		"fp=" + mdnsTokenFingerprint(norm.Token),
		"host=" + norm.Host,
		"tls=" + tlsVal,
		"v=" + mdnsProtocolVer,
	}
}

func (cfg mdnsServiceConfig) dnsSDArgs() []string {
	norm := cfg.normalized()
	args := []string{"-R", norm.Instance, mdnsServiceType, mdnsDomain, strconv.Itoa(norm.Port)}
	return append(args, norm.txtRecords()...)
}

type mdnsBackend interface {
	UpdateIPs(ips []net.IP)
	Close()
}

// mdnsAdvertiser manages LAN mDNS/Bonjour service advertisement and reacts to
// interface transitions cleanly without crashing in sandboxed environments.
type mdnsAdvertiser struct {
	mu           sync.Mutex
	cfg          mdnsServiceConfig
	backend      mdnsBackend
	startBackend func(cfg mdnsServiceConfig, ips []net.IP) mdnsBackend
	closed       bool
}

func newMDNSAdvertiser(cfg mdnsServiceConfig) *mdnsAdvertiser {
	return &mdnsAdvertiser{
		cfg:          cfg.normalized(),
		startBackend: startPlatformMDNSBackend,
	}
}

// SetLANState activates, updates or pauses mDNS advertisement based on whether
// the relay has active non-loopback LAN listeners.
func (a *mdnsAdvertiser) SetLANState(active bool, ips []net.IP) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.cfg.Port <= 0 || a.cfg.Port > 65535 {
		return
	}
	if !active {
		if a.backend != nil {
			a.backend.Close()
			a.backend = nil
		}
		return
	}
	cleaned := normalizeIPv4List(ips)
	if a.backend == nil {
		if a.startBackend != nil {
			a.backend = a.startBackend(a.cfg, cleaned)
		}
		return
	}
	a.backend.UpdateIPs(cleaned)
}

func (a *mdnsAdvertiser) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	b := a.backend
	a.backend = nil
	a.mu.Unlock()
	if b != nil {
		b.Close()
	}
}

func normalizeIPv4List(ips []net.IP) []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil || v4.IsLoopback() || v4.IsUnspecified() {
			continue
		}
		key := v4.String()
		if !seen[key] {
			seen[key] = true
			out = append(out, append(net.IP(nil), v4...))
		}
	}
	slices.SortFunc(out, func(a, b net.IP) int { return strings.Compare(a.String(), b.String()) })
	return out
}

func equalIPv4Lists(a, b []net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

func startPlatformMDNSBackend(cfg mdnsServiceConfig, ips []net.IP) mdnsBackend {
	if runtime.GOOS == "darwin" {
		if bin, err := exec.LookPath("/usr/bin/dns-sd"); err == nil {
			return newCommandMDNSBackend(bin, cfg.dnsSDArgs(), "dns-sd")
		}
		if bin, err := exec.LookPath("dns-sd"); err == nil {
			return newCommandMDNSBackend(bin, cfg.dnsSDArgs(), "dns-sd")
		}
	}
	responder, err := startPureMDNSResponder(cfg, ips)
	if err != nil {
		log.Printf("mdns: advertisement unavailable (%v)", err)
		return nil
	}
	return responder
}

// commandMDNSBackend supervises a native service publisher such as macOS dns-sd.
type commandMDNSBackend struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newCommandMDNSBackend(binary string, args []string, label string) *commandMDNSBackend {
	ctx, cancel := context.WithCancel(context.Background())
	b := &commandMDNSBackend{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(b.done)
		backoff := time.Second
		loggedFail := false
		for {
			started := time.Now()
			cmd := exec.CommandContext(ctx, binary, args...)
			prepareProcess(cmd)
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			cmd.Cancel = func() error {
				if cmd.Process != nil {
					terminateProcess(cmd.Process)
				}
				return nil
			}
			cmd.WaitDelay = 2 * time.Second
			err := cmd.Run()
			if ctx.Err() != nil {
				return
			}
			if time.Since(started) > 10*time.Second {
				backoff = time.Second
				loggedFail = false
			}
			if !loggedFail {
				log.Printf("mdns (%s): exited (%v), retrying", label, err)
				loggedFail = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	return b
}

func (b *commandMDNSBackend) UpdateIPs([]net.IP) {
	// Native mDNSResponder automatically tracks interface and IP changes.
}

func (b *commandMDNSBackend) Close() {
	b.cancel()
	<-b.done
}

// pureMDNSResponder implements a minimal, zero-dependency RFC 6762/6763
// responder for advertising _fastcontrol._tcp.local. on LANs (Linux/Windows
// and fallback when native Bonjour CLI is unavailable).
type pureMDNSResponder struct {
	cfg          mdnsServiceConfig
	conn         net.PacketConn
	multicastDst *net.UDPAddr

	mu       sync.Mutex
	ips      []net.IP
	closed   bool
	announce chan struct{}
	stop     chan struct{}
	wg       sync.WaitGroup
}

func startPureMDNSResponder(cfg mdnsServiceConfig, ips []net.IP) (*pureMDNSResponder, error) {
	group := &net.UDPAddr{IP: mdnsMulticastIPv4, Port: mdnsMulticastPort}
	conn, err := openMDNSMulticastConn(group)
	if err != nil {
		return nil, err
	}
	return newPureMDNSResponderWithConn(cfg, ips, conn, group), nil
}

func openMDNSMulticastConn(group *net.UDPAddr) (*net.UDPConn, error) {
	// Prefer an active multicast-capable physical LAN interface when available.
	if ifaces, err := net.Interfaces(); err == nil {
		for i := range ifaces {
			ifc := &ifaces[i]
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagMulticast == 0 {
				continue
			}
			if !physicalLANInterface(ifc.Name) {
				continue
			}
			if conn, err := net.ListenMulticastUDP("udp4", ifc, group); err == nil {
				return conn, nil
			}
		}
	}
	return net.ListenMulticastUDP("udp4", nil, group)
}

func newPureMDNSResponderWithConn(cfg mdnsServiceConfig, ips []net.IP, conn net.PacketConn, multicastDst *net.UDPAddr) *pureMDNSResponder {
	r := &pureMDNSResponder{
		cfg:          cfg.normalized(),
		conn:         conn,
		multicastDst: multicastDst,
		ips:          normalizeIPv4List(ips),
		announce:     make(chan struct{}, 1),
		stop:         make(chan struct{}),
	}
	r.wg.Add(2)
	go r.readLoop()
	go r.announceLoop()
	r.triggerAnnounce()
	return r
}

func (r *pureMDNSResponder) triggerAnnounce() {
	select {
	case r.announce <- struct{}{}:
	default:
	}
}

func (r *pureMDNSResponder) UpdateIPs(ips []net.IP) {
	cleaned := normalizeIPv4List(ips)
	r.mu.Lock()
	if r.closed || equalIPv4Lists(r.ips, cleaned) {
		r.mu.Unlock()
		return
	}
	r.ips = cleaned
	r.mu.Unlock()
	r.triggerAnnounce()
}

func (r *pureMDNSResponder) currentIPs() []net.IP {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]net.IP, len(r.ips))
	for i, ip := range r.ips {
		out[i] = append(net.IP(nil), ip...)
	}
	return out
}

func (r *pureMDNSResponder) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	r.mu.Unlock()
	close(r.stop)
	if pkt := r.buildGoodbyePacket(); len(pkt) > 0 && r.multicastDst != nil {
		_, _ = r.conn.WriteTo(pkt, r.multicastDst)
	}
	_ = r.conn.Close()
	r.wg.Wait()
}

func (r *pureMDNSResponder) announceLoop() {
	defer r.wg.Done()
	var repeatTimer *time.Timer
	var repeatCh <-chan time.Time
	stopRepeat := func() {
		if repeatTimer != nil {
			if !repeatTimer.Stop() {
				select {
				case <-repeatTimer.C:
				default:
				}
			}
			repeatTimer = nil
			repeatCh = nil
		}
	}
	defer stopRepeat()
	for {
		select {
		case <-r.stop:
			return
		case <-r.announce:
			stopRepeat()
			if r.multicastDst == nil {
				continue
			}
			if pkt := r.buildAnnouncementPacket(r.currentIPs()); len(pkt) > 0 {
				_, _ = r.conn.WriteTo(pkt, r.multicastDst)
			}
			repeatTimer = time.NewTimer(time.Second)
			repeatCh = repeatTimer.C
		case <-repeatCh:
			repeatTimer = nil
			repeatCh = nil
			if r.multicastDst == nil {
				continue
			}
			if pkt := r.buildAnnouncementPacket(r.currentIPs()); len(pkt) > 0 {
				_, _ = r.conn.WriteTo(pkt, r.multicastDst)
			}
		}
	}
}

func (r *pureMDNSResponder) readLoop() {
	defer r.wg.Done()
	buf := make([]byte, 4096)
	for {
		n, src, err := r.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if n < 12 {
			continue
		}
		id, questions, ok := parseMDNSQuery(buf[:n])
		if !ok || len(questions) == 0 {
			continue
		}
		udpSrc, _ := src.(*net.UDPAddr)
		legacyUnicast := udpSrc != nil && udpSrc.Port != mdnsMulticastPort
		resp, sendUnicast := r.buildQueryResponse(id, questions, r.currentIPs(), legacyUnicast)
		if len(resp) == 0 {
			continue
		}
		if (legacyUnicast || sendUnicast) && src != nil {
			_, _ = r.conn.WriteTo(resp, src)
		} else if r.multicastDst != nil {
			_, _ = r.conn.WriteTo(resp, r.multicastDst)
		}
	}
}

// ---- RFC 6762 / 6763 DNS wire encoding and decoding ----

type mdnsQuestion struct {
	Labels  []string
	QType   uint16
	QClass  uint16
	Unicast bool
}

type dnsRecord struct {
	Labels []string
	Type   uint16
	Class  uint16
	TTL    uint32
	RData  []byte
}

func (r *pureMDNSResponder) instanceLabels() []string {
	return []string{r.cfg.Instance, "_fastcontrol", "_tcp", "local"}
}

func (r *pureMDNSResponder) hostLabels() []string {
	return []string{r.cfg.Host, "local"}
}

func (r *pureMDNSResponder) serviceEnumPTR(ttl uint32) dnsRecord {
	return dnsRecord{
		Labels: mdnsServicesEnumLabels,
		Type:   dnsTypePTR,
		Class:  dnsClassIN,
		TTL:    ttl,
		RData:  encodeDNSName(mdnsServiceTypeLabels),
	}
}

func (r *pureMDNSResponder) servicePTR(ttl uint32) dnsRecord {
	return dnsRecord{
		Labels: mdnsServiceTypeLabels,
		Type:   dnsTypePTR,
		Class:  dnsClassIN,
		TTL:    ttl,
		RData:  encodeDNSName(r.instanceLabels()),
	}
}

func (r *pureMDNSResponder) serviceSRV(ttl uint32, flush bool) dnsRecord {
	class := dnsClassIN
	if flush {
		class = dnsClassFlush
	}
	target := encodeDNSName(r.hostLabels())
	rdata := make([]byte, 6+len(target))
	binary.BigEndian.PutUint16(rdata[4:6], uint16(r.cfg.Port))
	copy(rdata[6:], target)
	return dnsRecord{
		Labels: r.instanceLabels(),
		Type:   dnsTypeSRV,
		Class:  class,
		TTL:    ttl,
		RData:  rdata,
	}
}

func (r *pureMDNSResponder) serviceTXT(ttl uint32, flush bool) dnsRecord {
	class := dnsClassIN
	if flush {
		class = dnsClassFlush
	}
	return dnsRecord{
		Labels: r.instanceLabels(),
		Type:   dnsTypeTXT,
		Class:  class,
		TTL:    ttl,
		RData:  encodeTXTRData(r.cfg.txtRecords()),
	}
}

func (r *pureMDNSResponder) hostARecords(ips []net.IP, ttl uint32, flush bool) []dnsRecord {
	class := dnsClassIN
	if flush {
		class = dnsClassFlush
	}
	var recs []dnsRecord
	for _, ip := range ips {
		v4 := ip.To4()
		if v4 == nil {
			continue
		}
		recs = append(recs, dnsRecord{
			Labels: r.hostLabels(),
			Type:   dnsTypeA,
			Class:  class,
			TTL:    ttl,
			RData:  append([]byte(nil), v4...),
		})
	}
	return recs
}

func (r *pureMDNSResponder) buildAnnouncementPacket(ips []net.IP) []byte {
	answers := []dnsRecord{
		r.serviceEnumPTR(mdnsDefaultTTL),
		r.servicePTR(mdnsDefaultTTL),
		r.serviceSRV(mdnsDefaultTTL, true),
		r.serviceTXT(mdnsDefaultTTL, true),
	}
	answers = append(answers, r.hostARecords(ips, mdnsDefaultTTL, true)...)
	return encodeDNSMessage(0, 0x8400, answers, nil)
}

// buildGoodbyePacket withdraws only this service's PTR/SRV/TXT records (TTL=0),
// leaving <host>.local A records intact for other services on the machine.
func (r *pureMDNSResponder) buildGoodbyePacket() []byte {
	answers := []dnsRecord{
		r.servicePTR(0),
		r.serviceSRV(0, true),
		r.serviceTXT(0, true),
	}
	return encodeDNSMessage(0, 0x8400, answers, nil)
}

func (r *pureMDNSResponder) buildQueryResponse(id uint16, questions []mdnsQuestion, ips []net.IP, legacyUnicast bool) ([]byte, bool) {
	ttl := uint32(mdnsDefaultTTL)
	flush := true
	respID := uint16(0)
	if legacyUnicast {
		ttl = mdnsLegacyMaxTTL
		flush = false
		respID = id
	}
	var answers []dnsRecord
	var additionals []dnsRecord
	seenAns := map[string]bool{}
	seenAdd := map[string]bool{}
	addAns := func(rec dnsRecord) {
		key := dnsRecordKey(rec)
		if !seenAns[key] {
			seenAns[key] = true
			answers = append(answers, rec)
		}
	}
	addAdd := func(rec dnsRecord) {
		key := dnsRecordKey(rec)
		if !seenAns[key] && !seenAdd[key] {
			seenAdd[key] = true
			additionals = append(additionals, rec)
		}
	}
	instLabels := r.instanceLabels()
	hLabels := r.hostLabels()
	sendUnicast := false
	for _, q := range questions {
		if q.QClass != dnsClassIN && q.QClass != dnsTypeANY {
			continue
		}
		matched := false
		switch {
		case equalDNSLabels(q.Labels, mdnsServicesEnumLabels) && (q.QType == dnsTypePTR || q.QType == dnsTypeANY):
			addAns(r.serviceEnumPTR(ttl))
			matched = true
		case equalDNSLabels(q.Labels, mdnsServiceTypeLabels) && (q.QType == dnsTypePTR || q.QType == dnsTypeANY):
			addAns(r.servicePTR(ttl))
			addAdd(r.serviceSRV(ttl, flush))
			addAdd(r.serviceTXT(ttl, flush))
			for _, a := range r.hostARecords(ips, ttl, flush) {
				addAdd(a)
			}
			matched = true
		case equalDNSLabels(q.Labels, instLabels):
			if q.QType == dnsTypeSRV || q.QType == dnsTypeANY {
				addAns(r.serviceSRV(ttl, flush))
				addAdd(r.serviceTXT(ttl, flush))
				for _, a := range r.hostARecords(ips, ttl, flush) {
					addAdd(a)
				}
				matched = true
			}
			if q.QType == dnsTypeTXT || q.QType == dnsTypeANY {
				addAns(r.serviceTXT(ttl, flush))
				addAdd(r.serviceSRV(ttl, flush))
				for _, a := range r.hostARecords(ips, ttl, flush) {
					addAdd(a)
				}
				matched = true
			}
		case equalDNSLabels(q.Labels, hLabels) && (q.QType == dnsTypeA || q.QType == dnsTypeANY):
			aRecs := r.hostARecords(ips, ttl, flush)
			for _, a := range aRecs {
				addAns(a)
				matched = true
			}
		}
		if matched && q.Unicast {
			sendUnicast = true
		}
	}
	if len(answers) == 0 {
		return nil, false
	}
	// Remove any additional record already promoted into answers.
	filteredAdd := additionals[:0]
	for _, rec := range additionals {
		if !seenAns[dnsRecordKey(rec)] {
			filteredAdd = append(filteredAdd, rec)
		}
	}
	return encodeDNSMessage(respID, 0x8400, answers, filteredAdd), sendUnicast
}

func dnsRecordKey(rec dnsRecord) string {
	var b strings.Builder
	for _, l := range rec.Labels {
		b.WriteString(strings.ToLower(l))
		b.WriteByte(0)
	}
	b.WriteString(strconv.Itoa(int(rec.Type)))
	b.WriteByte(':')
	b.Write(rec.RData)
	return b.String()
}

func encodeDNSName(labels []string) []byte {
	total := 1
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			continue
		}
		total += 1 + len(label)
	}
	out := make([]byte, 0, total)
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			continue
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	return out
}

func encodeTXTRData(entries []string) []byte {
	if len(entries) == 0 {
		return []byte{0}
	}
	var out []byte
	for _, entry := range entries {
		if len(entry) > 255 {
			entry = entry[:255]
		}
		out = append(out, byte(len(entry)))
		out = append(out, entry...)
	}
	return out
}

func decodeTXTRData(rdata []byte) ([]string, error) {
	var out []string
	for i := 0; i < len(rdata); {
		n := int(rdata[i])
		i++
		if i+n > len(rdata) {
			return nil, errors.New("truncated TXT record")
		}
		if n > 0 {
			out = append(out, string(rdata[i:i+n]))
		}
		i += n
	}
	return out, nil
}

func encodeDNSMessage(id, flags uint16, answers, additionals []dnsRecord) []byte {
	buf := make([]byte, 12, 256)
	binary.BigEndian.PutUint16(buf[0:2], id)
	binary.BigEndian.PutUint16(buf[2:4], flags)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(answers)))
	binary.BigEndian.PutUint16(buf[8:10], 0)
	binary.BigEndian.PutUint16(buf[10:12], uint16(len(additionals)))
	for _, rec := range answers {
		buf = appendDNSRecord(buf, rec)
	}
	for _, rec := range additionals {
		buf = appendDNSRecord(buf, rec)
	}
	return buf
}

func appendDNSRecord(buf []byte, rec dnsRecord) []byte {
	buf = append(buf, encodeDNSName(rec.Labels)...)
	var meta [10]byte
	binary.BigEndian.PutUint16(meta[0:2], rec.Type)
	binary.BigEndian.PutUint16(meta[2:4], rec.Class)
	binary.BigEndian.PutUint32(meta[4:8], rec.TTL)
	binary.BigEndian.PutUint16(meta[8:10], uint16(len(rec.RData)))
	buf = append(buf, meta[:]...)
	buf = append(buf, rec.RData...)
	return buf
}

func decodeDNSName(msg []byte, offset int) ([]string, int, error) {
	var labels []string
	origNext := -1
	jumps := 0
	for {
		if offset < 0 || offset >= len(msg) {
			return nil, 0, errors.New("dns name out of bounds")
		}
		b := msg[offset]
		if b == 0 {
			if origNext == -1 {
				origNext = offset + 1
			}
			return labels, origNext, nil
		}
		if b&0xC0 == 0xC0 {
			if offset+1 >= len(msg) {
				return nil, 0, errors.New("truncated dns pointer")
			}
			jumps++
			if jumps > 16 {
				return nil, 0, errors.New("dns pointer loop")
			}
			if origNext == -1 {
				origNext = offset + 2
			}
			ptr := int(binary.BigEndian.Uint16(msg[offset:offset+2]) & 0x3FFF)
			if ptr >= len(msg) {
				return nil, 0, errors.New("dns pointer out of bounds")
			}
			offset = ptr
			continue
		}
		if b&0xC0 != 0 {
			return nil, 0, errors.New("invalid dns label")
		}
		length := int(b)
		offset++
		if offset+length > len(msg) {
			return nil, 0, errors.New("truncated dns label")
		}
		labels = append(labels, string(msg[offset:offset+length]))
		offset += length
		if len(labels) > 128 {
			return nil, 0, errors.New("dns name too deep")
		}
	}
}

func parseMDNSQuery(pkt []byte) (uint16, []mdnsQuestion, bool) {
	if len(pkt) < 12 {
		return 0, nil, false
	}
	id := binary.BigEndian.Uint16(pkt[0:2])
	flags := binary.BigEndian.Uint16(pkt[2:4])
	// Ignore responses (QR=1) and non-standard opcodes.
	if flags&0x8000 != 0 || flags&0x7800 != 0 {
		return 0, nil, false
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	if qdCount == 0 || qdCount > 64 {
		return id, nil, false
	}
	off := 12
	questions := make([]mdnsQuestion, 0, qdCount)
	for i := 0; i < qdCount; i++ {
		labels, next, err := decodeDNSName(pkt, off)
		if err != nil || next+4 > len(pkt) {
			return id, nil, false
		}
		qtype := binary.BigEndian.Uint16(pkt[next : next+2])
		rawClass := binary.BigEndian.Uint16(pkt[next+2 : next+4])
		questions = append(questions, mdnsQuestion{
			Labels:  labels,
			QType:   qtype,
			QClass:  rawClass & 0x7FFF,
			Unicast: rawClass&0x8000 != 0,
		})
		off = next + 4
	}
	return id, questions, true
}

func equalDNSLabels(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}
