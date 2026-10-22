package honeycomb

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testCert writes a self-signed certificate for 127.0.0.1 and returns the
// certificate and key file paths.
func testCert(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "honeycomb test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

// TestListenAndServeTLSIssue14 covers issue #14: listen and certificate
// errors are returned, not fatal, and the server stops when ctx ends.
func TestListenAndServeTLSIssue14(t *testing.T) {
	certFile, keyFile := testCert(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	srv, err := NewServer(securityDB(t), ServerOptions{Addr: busy.Addr().String(), Tokens: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ListenAndServeTLS(context.Background(), srv, certFile, keyFile); err == nil {
		t.Error("a busy port returned no error")
	}

	srv, _ = NewServer(securityDB(t), ServerOptions{Addr: "127.0.0.1:0", Tokens: []string{"k"}})
	if _, err := ListenAndServeTLS(context.Background(), srv, filepath.Join(t.TempDir(), "missing.pem"), keyFile); err == nil {
		t.Error("a missing certificate returned no error")
	}

	// A free port: serve, answer a request, then stop with ctx.
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	srv, _ = NewServer(securityDB(t), ServerOptions{Addr: addr, Tokens: []string{"k"}})
	ctx, cancel := context.WithCancel(context.Background())
	done, err := ListenAndServeTLS(ctx, srv, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/tags/Valve", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request to the running server: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /tags/Valve = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("server stopped with %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("server did not stop after ctx ended")
	}
}
