package tlsconf

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/capable78638974979473297813001-pixel/chips/internal/config"
)

type ca struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) ca {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return ca{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM certificate and key for name, signed by c.
func (c ca) issue(t *testing.T, name string, serial int64, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func write(t *testing.T, path string, b []byte) {
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func serve(t *testing.T, s *Store) *httptest.Server {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Proto)
	}))
	ts.TLS = s.ServerConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func client(root ca, cert *tls.Certificate, maxVersion uint16) *http.Client {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(root.pem)
	tc := &tls.Config{RootCAs: pool, MaxVersion: maxVersion}
	if cert != nil {
		tc.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true, DisableKeepAlives: true}}
}

func peerSerial(t *testing.T, c *http.Client, url string) int64 {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "HTTP/2.0" {
		t.Errorf("served over %s, want HTTP/2.0", body)
	}
	return resp.TLS.PeerCertificates[0].SerialNumber.Int64()
}

func TestServesAndRotates(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t)
	cfg := config.TLS{CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	c1, k1 := root.issue(t, "router", 100, x509.ExtKeyUsageServerAuth)
	write(t, cfg.CertFile, c1)
	write(t, cfg.KeyFile, k1)
	s, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := serve(t, s)
	cl := client(root, nil, 0)
	if got := peerSerial(t, cl, ts.URL); got != 100 {
		t.Fatalf("serial %d", got)
	}

	if changed, err := s.ReloadIfChanged(); changed || err != nil {
		t.Errorf("reload without changes: %v %v", changed, err)
	}
	// Rotate the files in place, as cert-manager does.
	c2, k2 := root.issue(t, "router", 200, x509.ExtKeyUsageServerAuth)
	later := time.Now().Add(time.Minute)
	write(t, cfg.CertFile, c2)
	write(t, cfg.KeyFile, k2)
	os.Chtimes(cfg.CertFile, later, later)
	if changed, err := s.ReloadIfChanged(); !changed || err != nil {
		t.Fatalf("rotation not picked up: %v %v", changed, err)
	}
	if got := peerSerial(t, cl, ts.URL); got != 200 {
		t.Errorf("after rotation serial %d, want 200", got)
	}

	// A broken file keeps the old certificate in service.
	write(t, cfg.KeyFile, []byte("garbage"))
	os.Chtimes(cfg.KeyFile, later.Add(time.Minute), later.Add(time.Minute))
	if _, err := s.ReloadIfChanged(); err == nil {
		t.Error("garbage key accepted")
	}
	if got := peerSerial(t, cl, ts.URL); got != 200 {
		t.Errorf("after failed reload serial %d, want 200", got)
	}
}

func TestMutualTLS(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t)
	clientCA := newCA(t)
	cfg := config.TLS{
		CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key"),
		ClientCAFile: filepath.Join(dir, "clients.pem"), MinVersion: "1.3",
	}
	c, k := root.issue(t, "router", 1, x509.ExtKeyUsageServerAuth)
	write(t, cfg.CertFile, c)
	write(t, cfg.KeyFile, k)
	write(t, cfg.ClientCAFile, clientCA.pem)
	s, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := serve(t, s)

	if _, err := client(root, nil, 0).Get(ts.URL); err == nil {
		t.Error("client without a certificate was let in")
	}
	cc, ck := clientCA.issue(t, "app", 7, x509.ExtKeyUsageClientAuth)
	pair, err := tls.X509KeyPair(cc, ck)
	if err != nil {
		t.Fatal(err)
	}
	peerSerial(t, client(root, &pair, 0), ts.URL)

	// Signed by the wrong CA.
	wc, wk := root.issue(t, "intruder", 8, x509.ExtKeyUsageClientAuth)
	wrong, _ := tls.X509KeyPair(wc, wk)
	if _, err := client(root, &wrong, 0).Get(ts.URL); err == nil {
		t.Error("client certificate from another CA was accepted")
	}
	// min_version 1.3 refuses TLS 1.2 clients.
	if _, err := client(root, &pair, tls.VersionTLS12).Get(ts.URL); err == nil {
		t.Error("TLS 1.2 client accepted with min_version 1.3")
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(config.TLS{CertFile: filepath.Join(dir, "none"), KeyFile: filepath.Join(dir, "none")}); err == nil {
		t.Error("missing files accepted")
	}
}
