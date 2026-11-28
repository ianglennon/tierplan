// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// fixedNotBefore is the start of every test certificate's validity. No test asserts anything about
// the current time, so the certificates carry a fixed, long window instead of one read from a clock.
var fixedNotBefore = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// newLeaf makes a self-signed ECDSA P-256 certificate for host and its key, both PEM.
func newLeaf(t *testing.T, host string) (chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the key for %s: %v", host, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		t.Fatalf("make a serial number: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             fixedNotBefore,
		NotAfter:              fixedNotBefore.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign the certificate for %s: %v", host, err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal the key for %s: %v", host, err)
	}
	chainPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return chainPEM, keyPEM
}

// leaf is one certificate and key as bundle files, named by content as the renderer names them.
type leaf struct {
	certPath, keyPath string
	cert, key         []byte
}

func newLeafFiles(t *testing.T, host string) leaf {
	t.Helper()
	chain, key := newLeaf(t, host)
	return leaf{
		certPath: ingressbundle.CertsDir + "/" + ingressbundle.Checksum(chain) + ".crt",
		keyPath:  ingressbundle.CertsDir + "/" + ingressbundle.Checksum(key) + ".key",
		cert:     chain,
		key:      key,
	}
}

// dynamicFor renders a dynamic file shaped like the renderer's: tls.options.default with sniStrict
// and minVersion, one certificates pair per leaf by absolute path, and one websecure router per host.
func dynamicFor(hosts []string, leaves []leaf) []byte {
	var sb strings.Builder
	sb.WriteString("tls:\n  options:\n    default:\n      sniStrict: true\n      minVersion: VersionTLS12\n")
	if len(leaves) > 0 {
		sb.WriteString("  certificates:\n")
		for _, l := range leaves {
			fmt.Fprintf(&sb, "    - certFile: %s/%s\n      keyFile: %s/%s\n",
				ingressbundle.NodeRoot, l.certPath, ingressbundle.NodeRoot, l.keyPath)
		}
	}
	if len(hosts) > 0 {
		sb.WriteString("http:\n  routers:\n")
		for i, h := range hosts {
			fmt.Fprintf(&sb, "    r-%d:\n      rule: Host(`%s`)\n      entryPoints:\n        - websecure\n      service: s-%d\n      priority: 1\n      tls: {}\n", i, h, i)
		}
		sb.WriteString("  services:\n")
		for i := range hosts {
			fmt.Fprintf(&sb, "    s-%d:\n      loadBalancer:\n        servers:\n          - url: http://web:8080\n", i)
		}
	}
	return []byte(sb.String())
}

// testBundle builds a valid bundle with one certificate and key per host and a renderer-shaped
// dynamic file with one router per host.
func testBundle(t *testing.T, hosts ...string) ingressbundle.Bundle {
	t.Helper()
	var files []ingressbundle.File
	var leaves []leaf
	for _, h := range hosts {
		l := newLeafFiles(t, h)
		leaves = append(leaves, l)
		files = append(files,
			ingressbundle.File{Path: l.certPath, Data: l.cert},
			ingressbundle.File{Path: l.keyPath, Data: l.key})
	}
	files = append(files, ingressbundle.File{Path: ingressbundle.DynamicFile, Data: dynamicFor(hosts, leaves)})
	b, err := ingressbundle.Build(ingressbundle.Schema2, ingressbundle.Version{Config: 1}, files, nil)
	if err != nil {
		t.Fatalf("build the test bundle: %v", err)
	}
	return b
}

// withFile returns a copy of b with the file at path set to data (added when absent). The manifest and
// ID are left as they were, so the result fails Verify; rebuild recomputes them.
func withFile(b ingressbundle.Bundle, path string, data []byte) ingressbundle.Bundle {
	out := b
	out.Files = make([]ingressbundle.File, 0, len(b.Files)+1)
	found := false
	for _, f := range b.Files {
		if f.Path == path {
			f.Data = data
			found = true
		}
		out.Files = append(out.Files, f)
	}
	if !found {
		out.Files = append(out.Files, ingressbundle.File{Path: path, Data: data})
	}
	return out
}

// withoutFile returns a copy of b without the file at path.
func withoutFile(b ingressbundle.Bundle, path string) ingressbundle.Bundle {
	out := b
	out.Files = make([]ingressbundle.File, 0, len(b.Files))
	for _, f := range b.Files {
		if f.Path != path {
			out.Files = append(out.Files, f)
		}
	}
	return out
}

// rebuild recomputes the manifest and ID of b's files, so a change made with withFile passes Verify and
// reaches the later checks.
func rebuild(t *testing.T, b ingressbundle.Bundle) ingressbundle.Bundle {
	t.Helper()
	out, err := ingressbundle.Build(b.Schema, b.Version, b.Files, b.Manifest.Routes)
	if err != nil {
		t.Fatalf("rebuild the bundle: %v", err)
	}
	return out
}
