package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mdp/qrterminal/v3"
)

type pairingInfo struct {
	Host string   `json:"host"`
	URLs []string `json:"urls"`
	Pin  string   `json:"pin,omitempty"`
}

func (p pairingInfo) link(token string) (string, error) {
	var addresses []string
	for _, address := range p.URLs {
		u, err := url.Parse(address)
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") || (p.Pin != "" && u.Scheme != "https") {
			continue
		}
		addresses = append(addresses, strings.TrimRight(address, "/"))
	}
	if len(addresses) == 0 {
		return "", errors.New("No network listener is ready. Connect Wi-Fi or Tailscale, then try again")
	}
	// Prefer LAN for the scan, retaining Tailscale for use away from home.
	for i, address := range addresses {
		u, _ := url.Parse(address)
		if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsPrivate() {
			addresses[0], addresses[i] = addresses[i], addresses[0]
			break
		}
	}
	values := url.Values{"url": {addresses[0]}, "t": {token}, "name": {p.Host}}
	if p.Pin != "" {
		values.Set("pin", p.Pin)
	}
	if len(addresses) > 1 {
		alternates, _ := json.Marshal(addresses[1:])
		values.Set("alts", string(alternates))
	}
	link := "fastcontrol://pair?" + values.Encode()
	if len(link) > 2048 {
		return "", errors.New("Pairing data is too large for a terminal QR")
	}
	return link, nil
}

func printPairing(ctx context.Context, port int, token string, output io.Writer) error {
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		request, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/pairing", port), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err == nil {
			var info pairingInfo
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&info)
			response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				return errors.New("Pairing credentials do not match the running relay")
			}
			if response.StatusCode == http.StatusNotFound {
				return errors.New("Update the running relay before showing its pairing QR")
			}
			if response.StatusCode == http.StatusOK && decodeErr == nil {
				link, err := info.link(token)
				if err == nil {
					fmt.Fprintf(output, "\nPair with %s\nScan this QR with your phone camera and open fastcontrol.\n\n", info.Host)
					restoreColors := pairingQRColors(output)
					qrterminal.GenerateWithConfig(link, qrterminal.Config{Level: qrterminal.L, Writer: output, HalfBlocks: true, QuietZone: 4})
					restoreColors()
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("The relay is not ready. Start fastcontrol and connect Wi-Fi or Tailscale, then run -pair again")
		case <-time.After(250 * time.Millisecond):
		}
	}
}
