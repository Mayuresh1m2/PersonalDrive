package tlsutil

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestCAPersistsAndLeafVerifies(t *testing.T) {
	dir := t.TempDir()
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.CAFingerprint() != m2.CAFingerprint() {
		t.Fatal("CA must persist across restarts (clients pin it)")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(m.CAPEM())
	leaf, _ := m.TLSConfig().GetCertificate(&tls.ClientHelloInfo{})
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Fatal(err)
	}
	if !isVirtual("docker0") || !isVirtual("br-1234") || isVirtual("wlp3s0") {
		t.Fatal("virtual interface detection wrong")
	}
	t.Logf("LAN addresses: %v", LANAddresses())
}
