// fastcontrol serves coding sessions from Codex and Claude Code over LAN or Tailscale.
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/mdp/qrterminal/v3"
)

//go:embed web
var webFS embed.FS

func loadToken(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			return t, nil
		}
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return t, os.WriteFile(path, []byte(t+"\n"), 0o600)
}

// autoAddrs picks loopback, Tailscale (100.64.0.0/10) and the default-route
// interface (usually Wi-Fi). VPN and virtual adapters are left out.
func autoAddrs() (addrs []string, preferred string) {
	addrs = []string{"127.0.0.1"}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, _ := ifc.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && cgnat.Contains(ipn.IP.To4()) {
				addrs = append(addrs, ipn.IP.String())
				preferred = ipn.IP.String()
			}
		}
	}
	if lan := defaultRouteIP(); lan != "" && net.ParseIP(lan).IsPrivate() {
		addrs = append(addrs, lan)
		if preferred == "" {
			preferred = lan
		}
	}
	if preferred == "" {
		preferred = "127.0.0.1"
	}
	return addrs, preferred
}

func main() {
	home, _ := os.UserHomeDir()
	port := flag.Int("port", 8787, "port to listen on")
	bind := flag.String("bind", "auto", "comma-separated IPs to bind, or 'auto' for loopback, Tailscale and the default-route (Wi-Fi) address")
	tokenFile := flag.String("token-file", filepath.Join(home, ".fastcontrol", "token"), "pairing token file (created if missing)")
	ipcPath := flag.String("ipc", defaultIPCPath(), "desktop app IPC socket")
	flag.String("asar", desktopBundlePath(), "desktop app bundle, checked for IPC protocol changes")
	installSpeech := flag.Bool("install-speech", false, "download and prepare the local speech model")
	speechStatus := flag.Bool("speech-status", false, "print local speech model availability and exit")
	usageJSON := flag.Bool("usage-json", false, "print this computer usage and limits as JSON, then exit")
	noQR := flag.Bool("no-qr", false, "do not print the pairing QR code")
	flag.Parse()
	if *installSpeech {
		if err := newSpeech().install(); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Local Speech Transcription Is Ready")
		return
	}
	if *speechStatus {
		fmt.Println(string(mustJSON(newSpeech().capability())))
		return
	}
	if *usageJSON {
		fmt.Println(string(mustJSON(newUsageManager(home).collect(time.Now()))))
		return
	}
	log.SetFlags(log.Ltime)

	token, err := loadToken(*tokenFile)
	if err != nil {
		log.Fatalf("token: %v", err)
	}
	db := stateDBPath()
	_, codexErr := codexBin()
	if db == "" && claudeBinary() == "" && codexErr != nil {
		log.Fatalf("Install Codex Or Claude Code Before Starting fastcontrol")
	}

	asar := flag.Lookup("asar").Value.String()
	if asar == "" {
		log.Printf("Codex Uses The Local CLI On This Platform")
	} else if drift, err := checkProtocolDrift(asar); err != nil {
		log.Printf("WARNING: could not verify the desktop IPC protocol (%v); live mode may fail", err)
	} else if len(drift) > 0 {
		log.Printf("WARNING: the desktop app changed its IPC protocol; live mode may fail until ipc.go is updated:")
		for _, d := range drift {
			log.Printf("  %s", d)
		}
	} else {
		log.Printf("desktop IPC protocol versions match")
	}

	ipc := NewIPCClient(*ipcPath)
	hub := NewHub(ipc, db)
	if runtime.GOOS == "linux" {
		hub.headless = newHeadlessCodex(hub)
	} else {
		go ipc.Run()
	}
	hub.claude = newClaudeManager(hub)
	go hub.claude.run(context.Background())
	hub.refreshList()
	go hub.RunList(context.Background())

	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))
	authed := func(r *http.Request) bool {
		t := r.URL.Query().Get("t")
		if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			t = b
		}
		return subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1
	}
	mux.HandleFunc("/auth", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	speech := newSpeech()
	usage := newUsageManager(home)
	mux.HandleFunc("/usage", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		usage.serve(w, r)
	}))
	mux.HandleFunc("/image", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		serveImage(w, r)
	}))
	mux.HandleFunc("/audio", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		serveAudio(w, r)
	}))
	mux.HandleFunc("/file", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		serveFile(w, r)
	}))
	mux.HandleFunc("/speech/install", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		speech.serveInstall(w, r)
	}))
	mux.HandleFunc("/transcribe", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		speech.serve(w, r)
	}))
	mux.HandleFunc("/upload", cors(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST a file", http.StatusMethodNotAllowed)
			return
		}
		path, err := saveUpload(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		kind := "file"
		if imageExts[strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))] != "" {
			kind = "image"
		}
		_, _ = w.Write(mustJSON(map[string]string{"path": path, "kind": kind}))
	}))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		// Origins are not checked: the Android app loads the UI from
		// https://appassets.androidplatform.net and a browser may switch between
		// Macs. The token is the authentication.
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover, InsecureSkipVerify: true})
		if err != nil {
			return
		}
		conn.SetReadLimit(1 << 20)
		serveClient(r.Context(), hub, conn, r.URL.Query().Get("events") == "1")
	})

	var addrs []string
	preferred := ""
	if *bind == "auto" {
		addrs, preferred = autoAddrs()
	} else {
		addrs = strings.Split(*bind, ",")
		preferred = addrs[len(addrs)-1]
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a != "127.0.0.1" && a != "::1" {
			hub.addrs = append(hub.addrs, fmt.Sprintf("http://%s", net.JoinHostPort(a, fmt.Sprint(*port))))
		}
	}
	for _, a := range addrs {
		ln, err := net.Listen("tcp", net.JoinHostPort(strings.TrimSpace(a), fmt.Sprint(*port)))
		if err != nil {
			log.Printf("listen %s: %v", a, err)
			continue
		}
		log.Printf("listening on http://%s", ln.Addr())
		go func() { log.Println(srv.Serve(ln)) }()
	}

	base := fmt.Sprintf("http://%s:%d", preferred, *port)
	url := fmt.Sprintf("%s/#t=%s", base, token)
	app := fmt.Sprintf("fastcontrol://pair?url=%s&t=%s&name=%s", neturl.QueryEscape(base), token, neturl.QueryEscape(hostName()))
	fmt.Printf("\nOpen on your phone:\n  %s\n\nPair the Android app:\n  %s\n\n", url, app)
	if !*noQR {
		qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
	}
	select {}
}

func serveClient(ctx context.Context, hub *Hub, conn *websocket.Conn, eventsOnly bool) {
	c := &Client{hub: hub, out: make(chan []byte, 1024), done: make(chan struct{}), eventsOnly: eventsOnly, events: eventsOnly}
	hub.addClient(c)
	defer hub.removeClient(c)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case b := <-c.out:
				wctx, wc := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Write(wctx, websocket.MessageText, b)
				wc()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, pc := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pctx)
				pc()
				if err != nil {
					return
				}
			case <-c.done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	// addrs lets clients fall back between Wi-Fi and Tailscale with one pairing.
	c.push(mustJSON(map[string]any{"t": "hello", "host": hostName(), "desktop": hub.ipc.ClientID() != "", "v": 3, "addrs": hub.addrs, "providers": map[string]bool{"codex": hub.db != "", "claude": hub.claude != nil && hub.claude.bin != ""}}))
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow()
			return
		}
		hub.handle(c, data)
	}
}

// cors lets the Android app (https://appassets.androidplatform.net) and pages
// served by another Mac's relay call the HTTP endpoints. Private Network
// Access preflights need the extra header.
func cors(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-File-Name")
			w.Header().Set("Access-Control-Allow-Private-Network", "true")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h(w, r)
	}
}

var imageExts = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "image/heic": ".heic"}

// saveUpload publishes a complete private upload, retaining its display filename.
func saveUpload(r *http.Request) (string, error) {
	const limit = 25 << 20
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	name, _ := neturl.QueryUnescape(r.Header.Get("X-File-Name"))
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(c rune) rune {
		if c < 32 || strings.ContainsRune("<>:\"|?*", c) {
			return '_'
		}
		return c
	}, name)
	name = strings.Trim(name, " .")
	if len(name) > 160 {
		ext := filepath.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		name = "attachment" + ext
	}
	dir := uploadDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".upload-")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(r.Body, limit+1))
	closeErr := f.Close()
	if err != nil {
		return "", fmt.Errorf("Could Not Read File: %w", err)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > limit {
		return "", fmt.Errorf("File Must Be Smaller Than 25 MB")
	}
	if strings.HasPrefix(ct, "image/") {
		head := make([]byte, 512)
		read, openErr := os.Open(f.Name())
		if openErr != nil {
			return "", openErr
		}
		count, _ := read.Read(head)
		read.Close()
		actual := imageMIME(head[:count])
		if actual == "" {
			return "", fmt.Errorf("Unsupported Image. Choose a JPEG, PNG, WebP or GIF")
		}
		if name == "" {
			name = "image" + imageExts[actual]
		}
	}
	if name == "" {
		name = "attachment"
	}
	path := filepath.Join(dir, uuid()+"-"+name)
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	log.Printf("upload: %s (%d KB)", filepath.Base(path), n/1024)
	return path, nil
}
