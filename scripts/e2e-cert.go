// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

//go:build ignore

// e2e-cert writes the throwaway TLS certificate the end-to-end proxy serves
// (plan 02-32, D-59): an ECDSA P-256 key and a self-signed certificate valid for
// seven days, with the names the harness reaches it by: 127.0.0.1 and localhost
// for the browser, and the proxy's fixed address on the compose network for the
// agent, which cannot resolve compose names from inside a nested Docker.
//
//	go run scripts/e2e-cert.go -out .devdata/e2e-tls
//
// It writes cert.pem, key.pem and pin.txt. pin.txt holds the SPKI pin the agent
// is given in TIERPLAN_AGENT_PIN, written by authn.SPKIPin, the same function that
// writes the pin TierPlan shows for its own certificate. An existing, unexpired
// certificate is kept, so a second run does not move the pin under a stack that
// is already up.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/ianglennon/tierplan/proto/authn"
)

const (
	validFor = 7 * 24 * time.Hour
	// renewBelow: a certificate with less life than this is replaced, so a stack
	// started now does not outlive it halfway through a run.
	renewBelow = 24 * time.Hour
	proxyIP    = "172.30.0.10"
)

func main() {
	out := flag.String("out", ".devdata/e2e-tls", "directory for cert.pem, key.pem and pin.txt")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-cert:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	pinPath := filepath.Join(dir, "pin.txt")

	if cert, err := readCert(certPath); err == nil && time.Until(cert.NotAfter) > renewBelow {
		if _, kerr := os.Stat(keyPath); kerr == nil {
			pin := authn.SPKIPin(cert)
			if err := os.WriteFile(pinPath, []byte(pin+"\n"), 0o644); err != nil {
				return err
			}
			fmt.Println("e2e-cert: kept", certPath, pin)
			return nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tierplan-e2e-proxy", Organization: []string{"TierPlan end-to-end harness"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(proxyIP)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// The key file is read by the Caddy container, whose user is not the one that
	// ran this, so it is readable by anyone on the machine for as long as the
	// directory exists. It is a seven-day key for a loopback-only test proxy.
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o644); err != nil {
		return err
	}
	pin := authn.SPKIPin(cert)
	if err := os.WriteFile(pinPath, []byte(pin+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Println("e2e-cert: wrote", certPath, pin)
	return nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s holds no PEM block", path)
	}
	return x509.ParseCertificate(block.Bytes)
}
