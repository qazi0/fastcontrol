package main

// Each relay has a private TLS identity. Android verifies the public-key hash
// carried in its pairing link, independently of public DNS or certificate CAs.
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type relayTLS struct {
	mu          sync.Mutex
	dir         string
	names       []string
	certificate *tls.Certificate
	nextCheck   time.Time
}

func tlsFingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}
func (r *relayTLS) getCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.certificate != nil && time.Now().Before(r.nextCheck) {
		return r.certificate, nil
	}
	cert, err := relayCertificate(r.dir, r.names, time.Now())
	if err != nil {
		return nil, err
	}
	r.certificate = &cert
	r.nextCheck = time.Now().Add(24 * time.Hour)
	return r.certificate, nil
}
func newRelayTLS(dir string, names []string) (*tls.Config, string, error) {
	identity := &relayTLS{dir: dir, names: append([]string(nil), names...)}
	cert, err := identity.getCertificate(nil)
	if err != nil {
		return nil, "", err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, "", err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: identity.getCertificate}, tlsFingerprint(leaf), nil
}
func privateAtomicFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}
func relayCertificate(dir string, names []string, now time.Time) (tls.Certificate, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return tls.Certificate{}, err
	}
	keyPath, certPath := filepath.Join(dir, "key.pem"), filepath.Join(dir, "cert.pem")
	var key *ecdsa.PrivateKey
	data, err := os.ReadFile(keyPath)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return tls.Certificate{}, errors.New("invalid relay TLS key; restore it or pair devices again after replacing it")
		}
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return tls.Certificate{}, err
		}
		var ok bool
		key, ok = parsed.(*ecdsa.PrivateKey)
		if !ok {
			return tls.Certificate{}, errors.New("unsupported relay TLS key")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, err
	} else {
		// Never silently replace a lost key while its certificate still exists.
		if _, err := os.Stat(certPath); err == nil {
			return tls.Certificate{}, errors.New("relay TLS key is missing; restore it or remove its certificate and pair devices again")
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return tls.Certificate{}, err
		}
		encoded, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return tls.Certificate{}, err
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})
		if err = privateAtomicFile(keyPath, data); err != nil {
			return tls.Certificate{}, err
		}
	}
	certPEM, err := os.ReadFile(certPath)
	if err == nil {
		cert, loadErr := tls.X509KeyPair(certPEM, data)
		if loadErr == nil {
			leaf, parseErr := x509.ParseCertificate(cert.Certificate[0])
			valid := parseErr == nil && now.After(leaf.NotBefore) && leaf.NotAfter.After(now.Add(30*24*time.Hour))
			if valid {
				for _, name := range names {
					if leaf.VerifyHostname(name) != nil {
						valid = false
						break
					}
				}
			}
			if valid {
				return cert, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "fastcontrol"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			leaf.IPAddresses = append(leaf.IPAddresses, ip)
		} else {
			leaf.DNSNames = append(leaf.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = privateAtomicFile(certPath, certPEM); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, data)
}
