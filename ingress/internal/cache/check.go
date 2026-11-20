// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package cache is the node's volume and the one check every bundle passes before anything is
// written to it (D-12, D-15). Check is pure: it takes a Bundle value, touches no filesystem and
// returns the first failure. The same function verifies a bundle that arrives on the channel and a
// bundle read back from the volume at start, so the two cannot disagree about what is acceptable.
package cache

import (
	"bytes"
	"crypto/tls"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// dynamicDoc is the part of the dynamic file Check reads: the TLS options and stores, the certificate
// pairs, and the root CAs of the servers transports. Every other key is ignored. It matches what the
// renderer writes (internal/ingressrender, typed structs only), and Phase 28 adds the rootCAs key.
type dynamicDoc struct {
	TLS struct {
		Options map[string]struct {
			SNIStrict *bool `yaml:"sniStrict"`
		} `yaml:"options"`
		Stores       map[string]any `yaml:"stores"`
		Certificates []certPair     `yaml:"certificates"`
	} `yaml:"tls"`
	HTTP struct {
		ServersTransports map[string]struct {
			RootCAs []string `yaml:"rootCAs"`
		} `yaml:"serversTransports"`
	} `yaml:"http"`
}

type certPair struct {
	CertFile string `yaml:"certFile"`
	KeyFile  string `yaml:"keyFile"`
}

// parseDynamic reads the dynamic file's TLS and certificate keys.
func parseDynamic(dynamic []byte) (dynamicDoc, error) {
	var doc dynamicDoc
	if err := yaml.Unmarshal(dynamic, &doc); err != nil {
		return dynamicDoc{}, err
	}
	return doc, nil
}

// refs lists every file the document names: each certificate pair's certFile and keyFile, in file
// order, then each servers transport's rootCAs, in transport-name order so the first failure is the
// same on every run.
func (d dynamicDoc) refs() []string {
	var out []string
	for _, p := range d.TLS.Certificates {
		out = append(out, p.CertFile, p.KeyFile)
	}
	for _, name := range slices.Sorted(maps.Keys(d.HTTP.ServersTransports)) {
		out = append(out, d.HTTP.ServersTransports[name].RootCAs...)
	}
	return out
}

// certRefs returns every certFile, keyFile and rootCAs value of the dynamic file, as written. Only
// those three keys name files: a stripPrefix prefix legitimately starts with "/" and is not one.
func certRefs(dynamic []byte) ([]string, error) {
	doc, err := parseDynamic(dynamic)
	if err != nil {
		return nil, err
	}
	return doc.refs(), nil
}

// relativeCertPath turns a reference of the form CertRoot/<flat name> into the bundle path
// certs/<name>. It reports false for any other form: a relative path, a path outside CertRoot, an
// empty, dot or dot-dot name, or a name with a separator.
func relativeCertPath(ref string) (string, bool) {
	name, ok := strings.CutPrefix(ref, ingressbundle.CertRoot+"/")
	if !ok || name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return "", false
	}
	return ingressbundle.CertsDir + "/" + name, true
}

// Check reports why a bundle must not be written, or nil. It runs, in order: the schema is one this
// node applies; Bundle.Verify; the dynamic file is present, holds no "{{" (Traefik's file provider
// expands Go templates, S1) and is YAML; tls.options.default.sniStrict is true and tls.stores.default
// is not set (so a node never presents Traefik's default certificate, PROH-21-02); every certificate,
// key and root CA reference is CertRoot/<flat name> of a file in the bundle; and each certificate
// matches its key. Every failure is a *ingressbundle.VerifyError carrying a stable code. Check writes
// nothing and logs nothing.
func Check(b ingressbundle.Bundle) error {
	if !slices.Contains(ingressbundle.SupportedSchemas(), b.Schema) {
		return &ingressbundle.VerifyError{Reason: ingressbundle.ReasonSchemaUnsupported}
	}
	if err := b.Verify(); err != nil {
		return err
	}
	files := make(map[string][]byte, len(b.Files))
	for _, f := range b.Files {
		files[f.Path] = f.Data
	}
	dynamic, ok := files[ingressbundle.DynamicFile]
	if !ok {
		return &ingressbundle.VerifyError{Path: ingressbundle.DynamicFile, Reason: ingressbundle.ReasonMissingFile}
	}
	if bytes.Contains(dynamic, []byte("{{")) {
		return &ingressbundle.VerifyError{Path: ingressbundle.DynamicFile, Reason: ingressbundle.ReasonTemplateSyntax}
	}
	doc, err := parseDynamic(dynamic)
	if err != nil {
		return &ingressbundle.VerifyError{Path: ingressbundle.DynamicFile, Reason: ingressbundle.ReasonYAMLInvalid}
	}
	if def, ok := doc.TLS.Options["default"]; !ok || def.SNIStrict == nil || !*def.SNIStrict {
		return &ingressbundle.VerifyError{Path: ingressbundle.DynamicFile, Reason: ingressbundle.ReasonTLSOptions}
	}
	if _, ok := doc.TLS.Stores["default"]; ok {
		return &ingressbundle.VerifyError{Path: ingressbundle.DynamicFile, Reason: ingressbundle.ReasonTLSOptions}
	}
	for _, ref := range doc.refs() {
		rel, ok := relativeCertPath(ref)
		if !ok {
			// Path is the relative form when the reference has one under the cert root, never the raw
			// text of an arbitrary reference.
			var path string
			if name, under := strings.CutPrefix(ref, ingressbundle.CertRoot+"/"); under {
				path = ingressbundle.CertsDir + "/" + name
			}
			return &ingressbundle.VerifyError{Path: path, Reason: ingressbundle.ReasonCertPath}
		}
		if _, present := files[rel]; !present {
			return &ingressbundle.VerifyError{Path: rel, Reason: ingressbundle.ReasonCertMissing}
		}
	}
	for _, p := range doc.TLS.Certificates {
		certRel, _ := relativeCertPath(p.CertFile)
		keyRel, _ := relativeCertPath(p.KeyFile)
		if _, err := tls.X509KeyPair(files[certRel], files[keyRel]); err != nil {
			return &ingressbundle.VerifyError{Path: certRel, Reason: ingressbundle.ReasonKeyMismatch}
		}
	}
	return nil
}
