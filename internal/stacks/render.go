// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PinPending is the trailing comment of an image line that will be looked up when the stack is
// deployed: the service has no pin yet, or its image text changed since the last good deploy.
const PinPending = "will be pinned when you deploy"

// RenderInput is what Render needs: the project a check loaded, the image each service ran at the
// stack's last good revision, and the masker of the working copy.
type RenderInput struct {
	Loaded *Loaded
	// Pins are the images the last good revision ran, by swarm service name (stack_service), as
	// stackops.Repo.GoodPins reads them. A service with none reads as not yet pinned.
	Pins map[string]LivePin
	// Masker hides the secret values of the working copy. The zero Masker hides nothing.
	Masker Masker
}

// Render is the stack as it will be deployed (D-18): the compose project after variables and
// env_file are filled in, with each service's image shown as the pin it will run. A service whose
// image text is unchanged since the last good revision reads tag@sha256:...; one that already
// carries a digest is shown as written; any other reads as written with a trailing comment, since
// the deploy will look it up.
//
// Render is a view. It works on the loaded project, which is a copy of the working copy, and
// never writes to the working copy or to the user's compose file (D-18, T-07-63). Every secret
// value is hidden, in the YAML values themselves and not only in the text, so a value that YAML
// escapes (a quote, a line break) is hidden as well.
func Render(in RenderInput) ([]byte, error) {
	if in.Loaded == nil || in.Loaded.Project == nil {
		return nil, errors.New("stacks: render: no loaded project")
	}
	raw, err := in.Loaded.Project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("stacks: render: marshal: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("stacks: render: read back: %w", err)
	}
	if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
		if services := mappingValue(doc.Content[0], "services"); services != nil && services.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(services.Content); i += 2 {
				pinImage(services.Content[i+1], in.Loaded.Project.Name, services.Content[i].Value, in.Pins)
				ShortenPorts(services.Content[i+1])
			}
		}
	}
	maskNode(&doc, in.Masker)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("stacks: render: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("stacks: render: encode: %w", err)
	}
	return []byte(in.Masker.Mask(buf.String())), nil
}

// mappingValue is the value node of key in a mapping node, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// pinImage sets the image line of one service from the pin the last good revision ran, or marks
// it as one the deploy will look up. A service with no image line is left alone.
func pinImage(svc *yaml.Node, project, service string, pins map[string]LivePin) {
	if svc.Kind != yaml.MappingNode {
		return
	}
	image := mappingValue(svc, "image")
	if image == nil || image.Kind != yaml.ScalarNode || image.Value == "" {
		return
	}
	text := image.Value
	in := PinInput{Service: service, AsWritten: text}
	pin, ok := pins[project+"_"+service]
	if !ok {
		pin, ok = pins[service]
	}
	if ok && pin.Image != "" && SameImageText(pin.AsWritten, text) {
		in.Stored = &LivePin{AsWritten: text, Image: pin.Image}
	}
	switch dec := DecidePin(in); dec.Action {
	case PinKeep:
		image.Value = dec.Ref
	case PinResolve:
		image.LineComment = PinPending
	}
}

// maskNode hides the secret values in every scalar of the tree, keys included. It works on the
// decoded text of a scalar, which is what a secret was typed as, not on the escaped form it takes
// in the YAML.
func maskNode(n *yaml.Node, m Masker) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if masked := m.Mask(n.Value); masked != n.Value {
			n.Value, n.Tag, n.Style = masked, "!!str", 0
		}
	}
	for _, c := range n.Content {
		maskNode(c, m)
	}
}

// ShortenPorts writes the ports of a service the way a person writes them, "9090:80", when the
// long form compose-go marshals says nothing the short form cannot: the ingress mode and the
// tcp protocol are what the short form means by default. A port with another mode, a name or an
// application protocol keeps its long form, so nothing the file says is lost.
func ShortenPorts(svc *yaml.Node) {
	if svc.Kind != yaml.MappingNode {
		return
	}
	ports := mappingValue(svc, "ports")
	if ports == nil || ports.Kind != yaml.SequenceNode {
		return
	}
	for i, p := range ports.Content {
		if short, ok := shortPort(p); ok {
			ports.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: short, Style: yaml.DoubleQuotedStyle}
		}
	}
}

// shortPort is the short syntax of a long-form port, and false when it has none.
func shortPort(p *yaml.Node) (string, bool) {
	if p.Kind != yaml.MappingNode {
		return "", false
	}
	var hostIP, published, target, protocol string
	for i := 0; i+1 < len(p.Content); i += 2 {
		k, v := p.Content[i].Value, p.Content[i+1]
		if v.Kind != yaml.ScalarNode {
			return "", false
		}
		switch k {
		case "mode":
			if v.Value != "ingress" {
				return "", false
			}
		case "host_ip":
			hostIP = v.Value
		case "published":
			published = v.Value
		case "target":
			target = v.Value
		case "protocol":
			protocol = v.Value
		default:
			return "", false
		}
	}
	if target == "" || (hostIP != "" && published == "") {
		return "", false
	}
	var b strings.Builder
	if hostIP != "" {
		if strings.Contains(hostIP, ":") {
			hostIP = "[" + hostIP + "]"
		}
		b.WriteString(hostIP + ":")
	}
	if published != "" {
		b.WriteString(published + ":")
	}
	b.WriteString(target)
	if protocol != "" && protocol != "tcp" {
		b.WriteString("/" + protocol)
	}
	return b.String(), true
}
