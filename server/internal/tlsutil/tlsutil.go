// Package tlsutil creates a private CA for the home server and keeps a server
// certificate valid for the machine's current hostnames and LAN IPs.
//
// Clients pin the CA fingerprint (delivered in the pairing QR code), so the
// leaf certificate can be re-issued when the laptop's IP changes without
// re-pairing.
package tlsutil

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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

type Manager struct {
	dir    string
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caDER  []byte
	leaf   atomic.Pointer[tls.Certificate]
	issued []string // SANs on the current leaf
}

// Load creates or loads the CA in dir and issues a leaf for the current names.
func Load(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir}
	if err := m.loadOrCreateCA(); err != nil {
		return nil, err
	}
	if _, err := m.Refresh(); err != nil {
		return nil, err
	}
	return m, nil
}

// CAFingerprint is the SHA-256 of the CA certificate (hex), which clients pin.
func (m *Manager) CAFingerprint() string {
	sum := sha256.Sum256(m.caDER)
	return hex.EncodeToString(sum[:])
}

// CAPEM returns the CA certificate for clients that want to install it.
func (m *Manager) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: m.caDER})
}

func (m *Manager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return m.leaf.Load(), nil
		},
	}
}

// Refresh re-issues the leaf if the host's names/IPs changed or it is close
// to expiry. It reports whether a new certificate was issued.
func (m *Manager) Refresh() (bool, error) {
	names, ips := hostIdentities()
	want := sanKey(names, ips)
	if cur := m.leaf.Load(); cur != nil && slices.Equal(want, m.issued) &&
		time.Until(cur.Leaf.NotAfter) > 30*24*time.Hour {
		return false, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: names[0], Organization: []string{"PersonalDrive"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(397 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.ca, &key.PublicKey, m.caKey)
	if err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return false, err
	}
	m.leaf.Store(&tls.Certificate{Certificate: [][]byte{der, m.caDER}, PrivateKey: key, Leaf: leaf})
	m.issued = want
	return true, nil
}

func (m *Manager) loadOrCreateCA() error {
	certPath, keyPath := filepath.Join(m.dir, "ca.pem"), filepath.Join(m.dir, "ca.key")
	if certPEM, err := os.ReadFile(certPath); err == nil {
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return fmt.Errorf("CA certificate exists but key is unreadable: %w", err)
		}
		cb, _ := pem.Decode(certPEM)
		kb, _ := pem.Decode(keyPEM)
		if cb == nil || kb == nil {
			return errors.New("corrupt CA files in " + m.dir)
		}
		ca, err := x509.ParseCertificate(cb.Bytes)
		if err != nil {
			return err
		}
		key, err := x509.ParseECPrivateKey(kb.Bytes)
		if err != nil {
			return err
		}
		m.ca, m.caKey, m.caDER = ca, key, cb.Bytes
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: "PersonalDrive CA (" + host + ")", Organization: []string{"PersonalDrive"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	m.ca, _ = x509.ParseCertificate(der)
	m.caKey, m.caDER = key, der
	return nil
}

// hostIdentities returns DNS names (hostname, hostname.local, localhost) and
// the non-loopback unicast IPs of this machine plus loopback.
func hostIdentities() ([]string, []net.IP) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "pdrive"
	}
	names := []string{host, host + ".local", "localhost"}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			ips = append(ips, ipn.IP)
		}
	}
	return names, ips
}

func sanKey(names []string, ips []net.IP) []string {
	out := slices.Clone(names)
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

// virtualIface matches container/VM bridges that phones on the Wi-Fi can't reach.
var virtualPrefixes = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "lxc", "cni", "flannel", "tailscale", "wg"}

// LANAddresses returns IPv4 addresses on physical interfaces, for pairing QR
// codes. (The certificate still covers every address.)
func LANAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || isVirtual(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLinkLocalUnicast() {
					out = append(out, ip4.String())
				}
			}
		}
	}
	return out
}

func isVirtual(name string) bool {
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
