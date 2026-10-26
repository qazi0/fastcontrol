package main

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRelayTLSIdentitySurvivesRenewalAndAddressChanges(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	original, err := relayCertificate(dir, []string{"localhost", "127.0.0.1", "100.64.0.2"}, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := x509.ParseCertificate(original.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := relayCertificate(dir, []string{"localhost", "127.0.0.1", "100.64.0.2", "192.168.1.50"}, now.Add(350*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	next, err := x509.ParseCertificate(renewed.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if tlsFingerprint(first) != tlsFingerprint(next) {
		t.Fatal("renewal changed the paired identity")
	}
	if err := next.VerifyHostname("192.168.1.50"); err != nil {
		t.Fatal(err)
	}
	if err := next.VerifyHostname("wrong.invalid"); err == nil {
		t.Fatal("unexpected hostname accepted")
	}
	if first.SerialNumber.Cmp(next.SerialNumber) == 0 {
		t.Fatal("certificate was not renewed")
	}
	if _, err := tls.X509KeyPair(mustRead(t, filepath.Join(dir, "cert.pem")), mustRead(t, filepath.Join(dir, "key.pem"))); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0077 != 0 && os.PathSeparator != '\\' {
		t.Fatalf("private key permissions %o", st.Mode().Perm())
	}
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func TestRelayTLSMissingKeyDoesNotSilentlyReplaceIdentity(t *testing.T) {
	dir := t.TempDir()
	if _, err := relayCertificate(dir, []string{"localhost"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := relayCertificate(dir, []string{"localhost"}, time.Now()); err == nil {
		t.Fatal("lost key was silently replaced")
	}
}
