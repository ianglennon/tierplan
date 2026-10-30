// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package rebuild

import (
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/distribution/reference"
	"go.yaml.in/yaml/v3"
)

// Base is the definition a stack already has, which a rebuild of a stack Coaming manages is made on top of (D-54).
// Adoption rebuilds a stack nobody has described, so it has no Base and writes what runs as literals (D-15). A
// stack that has a deployed revision keeps what that revision said where the engine cannot say it better: the
// stack's own secrets and configs stay files, its networks and volumes keep the declarations the file gave them,
// an image keeps the text it was written with while it is still the image that runs, and a value that is one of
// the stack's secret variables is written as that variable, so no secret's value is ever copied into the file.
type Base struct {
	// Project is the deployed revision's compose file, loaded and interpolated as every deploy loads it.
	Project *types.Project
	// SecretValues maps the value of each secret variable of the revision to the variable's name. An environment
	// value, a command or an entrypoint word equal to one of them is written as ${NAME}.
	SecretValues map[string]string
	// ImageText is the image of each service as the revision's file wrote it, by the service's name in the file
	// (nginx:${TAG} where Project holds nginx:1.27). A service with no entry has Project's text.
	ImageText map[string]string
}

// ImageTextOf reads, from a compose file as written, the image of each service as the file wrote it. A service
// whose image is not one plain word is left out, so a value that could not be put back in place without changing
// what the file means is never kept.
func ImageTextOf(compose []byte) map[string]string {
	plain, _ := imageTexts(compose)
	return plain
}

// ImageTextOfFiles is ImageTextOf for the compose files of a stack, in merge order: the image a later file gives a
// service replaces the earlier one, and a later file that sets the image in a way that is not one plain word leaves
// the service out, as the merged file would.
func ImageTextOfFiles(composes ...[]byte) map[string]string {
	var out map[string]string
	for _, c := range composes {
		plain, set := imageTexts(c)
		for svc := range set {
			delete(out, svc)
		}
		for svc, text := range plain {
			if out == nil {
				out = map[string]string{}
			}
			out[svc] = text
		}
	}
	return out
}

// imageTexts reads the plain image text of each service of a compose file, and the services that set an image at
// all, plain or not.
func imageTexts(compose []byte) (plain map[string]string, set map[string]bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(compose, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil
	}
	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}
	plain, set = map[string]string{}, map[string]bool{}
	for i := 0; i+1 < len(services.Content); i += 2 {
		svc := services.Content[i+1]
		if svc.Kind != yaml.MappingNode {
			continue
		}
		img := mappingValue(svc, "image")
		if img == nil {
			continue
		}
		set[services.Content[i].Value] = true
		if img.Kind != yaml.ScalarNode || img.Value == "" || strings.ContainsAny(img.Value, " \t\r\n#'\"") {
			continue
		}
		plain[services.Content[i].Value] = img.Value
	}
	return plain, set
}

// substitutions are the values a rebuild writes into the file as they are, past the escaping every other value
// gets: the rebuild puts a placeholder in the project, and Marshal swaps it for the text once the file is a tree.
type substitutions struct {
	text map[string]string
	by   map[string]string
}

// placeholderPrefix starts every placeholder. It holds only characters that need no quoting, and each placeholder
// ends in an underscore, so no placeholder is the start of another.
const placeholderPrefix = "COAMING_SUBSTITUTION_"

func (s *substitutions) add(text string) string {
	if p, ok := s.by[text]; ok {
		return p
	}
	if s.text == nil {
		s.text, s.by = map[string]string{}, map[string]string{}
	}
	p := placeholderPrefix + strconv.Itoa(len(s.text)) + "_"
	s.text[p], s.by[text] = text, p
	return p
}

// unfold puts the text back in the strings of a service that hold a placeholder, so the project the result
// carries does not.
func (s *substitutions) unfold(svc *types.ServiceConfig) {
	if s == nil || len(s.text) == 0 {
		return
	}
	if t, ok := s.text[svc.Image]; ok {
		svc.Image = t
	}
	for k, v := range svc.Environment {
		if v == nil {
			continue
		}
		if t, ok := s.text[*v]; ok {
			t := t
			svc.Environment[k] = &t
		}
	}
	for _, words := range []types.ShellCommand{svc.Command, svc.Entrypoint} {
		for i, w := range words {
			if t, ok := s.text[w]; ok {
				words[i] = t
			}
		}
	}
}

// apply swaps the placeholders of a tree for their text. A swapped scalar is encoded afresh, so it is quoted when
// its place in the file needs it.
func (s *substitutions) apply(n *yaml.Node) {
	if s == nil || n == nil || len(s.text) == 0 {
		return
	}
	if n.Kind == yaml.ScalarNode {
		if t, ok := s.text[n.Value]; ok {
			n.Value, n.Style = t, 0
		}
		return
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			continue
		}
		s.apply(c)
	}
}

// secretName is the variable whose value v is, "" for none. An empty value is never a secret's.
func (b *Base) secretName(v string) string {
	if b == nil || v == "" {
		return ""
	}
	return b.SecretValues[v]
}

// hideSecrets writes every environment value, entrypoint word and command word of svc that is the value of a
// secret variable as a reference to the variable.
func (b *Base) hideSecrets(svc *types.ServiceConfig, subs *substitutions) {
	if b == nil || len(b.SecretValues) == 0 {
		return
	}
	for k, v := range svc.Environment {
		if v == nil {
			continue
		}
		if name := b.secretName(*v); name != "" {
			p := subs.add("${" + name + "}")
			svc.Environment[k] = &p
		}
	}
	for _, words := range []types.ShellCommand{svc.Command, svc.Entrypoint} {
		for i, w := range words {
			if name := b.secretName(w); name != "" {
				words[i] = subs.add("${" + name + "}")
			}
		}
	}
}

// keepImage gives the service its image as the base's file wrote it, when the image that runs is the one the
// base's file names: the same repository and the same tag (or digest). Any other is a change somebody made, and
// the draft keeps it.
func (b *Base) keepImage(short string, svc *types.ServiceConfig, subs *substitutions) {
	if b == nil || b.Project == nil || svc.Image == "" {
		return
	}
	was, ok := b.Project.Services[short]
	if !ok || !sameImage(was.Image, svc.Image) {
		return
	}
	text, ok := b.ImageText[short]
	if !ok || text == was.Image {
		svc.Image = was.Image
		return
	}
	svc.Image = subs.add(text)
}

// sameImage says whether two image references name the same image: the same repository and the same tag, an
// untagged one being latest, and the same digest when both carry one.
func sameImage(a, b string) bool {
	if a == b {
		return true
	}
	x, err := reference.ParseNormalizedNamed(a)
	if err != nil {
		return false
	}
	y, err := reference.ParseNormalizedNamed(b)
	if err != nil {
		return false
	}
	if x.Name() != y.Name() {
		return false
	}
	dx, dy := digestOf(x), digestOf(y)
	if dx != "" && dy != "" && dx != dy {
		return false
	}
	return tagOf(x) == tagOf(y)
}

func tagOf(r reference.Named) string {
	if t, ok := r.(reference.Tagged); ok {
		return t.Tag()
	}
	if _, ok := r.(reference.Digested); ok {
		return ""
	}
	return "latest"
}

func digestOf(r reference.Named) string {
	if d, ok := r.(reference.Digested); ok {
		return d.Digest().String()
	}
	return ""
}

// ownedNetwork is the base's declaration of the network under key when the stack owns it (it is not external),
// as the file wrote it: the name the loader gave it is left out again.
func (b *Base) ownedNetwork(key string) (types.NetworkConfig, bool) {
	if b == nil || b.Project == nil {
		return types.NetworkConfig{}, false
	}
	cfg, ok := b.Project.Networks[key]
	if !ok || bool(cfg.External) {
		return types.NetworkConfig{}, false
	}
	if cfg.Name == b.Project.Name+"_"+key {
		cfg.Name = ""
	}
	if key == defaultKey && isPlainNetwork(cfg) {
		// The loader adds the default network to every project that uses it; a file that never declared it has
		// the plain one, and the rebuild leaves it implicit.
		return types.NetworkConfig{}, false
	}
	return cfg, true
}

// ownedVolume is ownedNetwork for a volume.
func (b *Base) ownedVolume(key string) (types.VolumeConfig, bool) {
	if b == nil || b.Project == nil {
		return types.VolumeConfig{}, false
	}
	cfg, ok := b.Project.Volumes[key]
	if !ok || bool(cfg.External) {
		return types.VolumeConfig{}, false
	}
	if cfg.Name == b.Project.Name+"_"+key {
		cfg.Name = ""
	}
	return cfg, true
}

// ownedSecret is the base's declaration of the secret under key, and whether the stack owns it (it is a file, a
// value or a variable of the stack, not external).
func (b *Base) ownedSecret(key string) (types.SecretConfig, bool) {
	if b == nil || b.Project == nil {
		return types.SecretConfig{}, false
	}
	cfg, ok := b.Project.Secrets[key]
	if !ok || bool(cfg.External) {
		return types.SecretConfig{}, false
	}
	if cfg.Name == b.Project.Name+"_"+key {
		cfg.Name = ""
	}
	return cfg, true
}

// ownedConfig is ownedSecret for a config.
func (b *Base) ownedConfig(key string) (types.ConfigObjConfig, bool) {
	if b == nil || b.Project == nil {
		return types.ConfigObjConfig{}, false
	}
	cfg, ok := b.Project.Configs[key]
	if !ok || bool(cfg.External) {
		return types.ConfigObjConfig{}, false
	}
	if cfg.Name == b.Project.Name+"_"+key {
		cfg.Name = ""
	}
	return cfg, true
}
