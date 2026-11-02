// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package redact

import (
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"
)

// A swarm service's spec carries the same secrets a container does (D-39, AUTH-07): the values
// of its environment, its command line (Command and Args), the command of its health check and
// the values of its log driver's options. MaskServiceSpec masks the typed spec and
// MaskServiceInspect the raw document, wherever the service is read, with Placeholder
// ("<hidden>") as the one sentinel, exactly as the container's inspect does.

// MaskServiceSpec returns spec as a caller sees it. With reveal true the spec is returned as it
// is and masked is false. Otherwise the value of every Env entry ("NAME=value" becomes
// "NAME=<hidden>", an entry with no "=" has nothing to hide), the Command and the Args (one
// Placeholder each when they are not empty), the command of Healthcheck.Test (the mode, CMD,
// CMD-SHELL or NONE, stays) and every value of the log driver's options are replaced. masked is
// true when something was replaced. The slices and maps it changes are copied; spec is not
// modified.
func MaskServiceSpec(spec swarm.ServiceSpec, reveal bool) (out swarm.ServiceSpec, masked bool) {
	if reveal {
		return spec, false
	}
	out = spec
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		masked = maskContainerSpec(&out, cs) || masked
	}
	if ld := spec.TaskTemplate.LogDriver; ld != nil && len(ld.Options) > 0 {
		opts := maps.Clone(ld.Options)
		for name := range opts {
			opts[name] = Placeholder
		}
		out.TaskTemplate.LogDriver = &swarm.Driver{Name: ld.Name, Options: opts}
		masked = true
	}
	return out, masked
}

// maskContainerSpec puts a masked copy of cs into out and reports whether it replaced anything.
func maskContainerSpec(out *swarm.ServiceSpec, cs *swarm.ContainerSpec) (masked bool) {
	c := *cs
	if len(c.Env) > 0 {
		env := slices.Clone(c.Env)
		for i, kv := range env {
			if name, _, hasValue := strings.Cut(kv, "="); hasValue {
				env[i] = name + "=" + Placeholder
				masked = true
			}
		}
		c.Env = env
	}
	var m bool
	if c.Command, m = Argv(c.Command, false); m {
		masked = true
	}
	if c.Args, m = Argv(c.Args, false); m {
		masked = true
	}
	if hc := c.Healthcheck; hc != nil && len(hc.Test) > 1 {
		// The first element is the mode, not a secret; the rest is the command.
		h := *hc
		h.Test = []string{hc.Test[0], Placeholder}
		c.Healthcheck = &h
		masked = true
	}
	if masked {
		out.TaskTemplate.ContainerSpec = &c
	}
	return masked
}

// MaskServiceInspect prepares the raw inspect document of a service for a browser. With reveal
// true the document is returned as it is and masked is false. Otherwise the same fields
// MaskServiceSpec withholds are replaced under Spec and under PreviousSpec (the spec before the
// last update, which holds the same kinds of secret), and the rest of the document is kept as it
// was, re-encoded with two-space indentation; numbers keep their exact digits. A spec, a
// container spec, an environment, a command or a set of log options of a shape the engine does
// not send is withheld whole rather than passed through. masked is true when anything was
// replaced. A document that is not a JSON object is an error, never passed through, so a
// malformed answer cannot carry a value past the mask.
func MaskServiceInspect(raw []byte, reveal bool) (out []byte, masked bool, err error) {
	if reveal {
		return raw, false, nil
	}
	doc, err := decodeObject(raw, "service inspect")
	if err != nil {
		return nil, false, err
	}
	for _, key := range []string{"Spec", "PreviousSpec"} {
		spec, withheld := objectAt(doc, key)
		masked = masked || withheld
		if spec != nil && maskServiceSpecDoc(spec) {
			masked = true
		}
	}
	out, err = encodeObject(doc, "service inspect")
	return out, masked, err
}

// maskServiceSpecDoc masks one spec of an inspect document in place and reports whether it
// replaced something.
func maskServiceSpecDoc(spec map[string]any) (masked bool) {
	tt, withheld := objectAt(spec, "TaskTemplate")
	masked = withheld
	if tt == nil {
		return masked
	}
	if cs, withheld := objectAt(tt, "ContainerSpec"); withheld {
		masked = true
	} else if cs != nil {
		switch env := cs["Env"].(type) {
		case []any:
			if maskEnvEntries(env) {
				masked = true
			}
		case nil:
		default:
			cs["Env"] = Placeholder
			masked = true
		}
		for _, key := range []string{"Command", "Args"} {
			if maskArgv(cs, key) {
				masked = true
			}
		}
		if hc, withheld := objectAt(cs, "Healthcheck"); withheld {
			masked = true
		} else if hc != nil {
			switch test := hc["Test"].(type) {
			case []any:
				if len(test) > 1 {
					hc["Test"] = []any{test[0], Placeholder}
					masked = true
				}
			case nil:
			default:
				hc["Test"] = Placeholder
				masked = true
			}
		}
	}
	if ld, withheld := objectAt(tt, "LogDriver"); withheld {
		masked = true
	} else if ld != nil {
		switch opts := ld["Options"].(type) {
		case map[string]any:
			for name := range opts {
				opts[name] = Placeholder
				masked = true
			}
		case nil:
		default:
			ld["Options"] = Placeholder
			masked = true
		}
	}
	return masked
}

// objectAt returns m[key] as an object. An absent or null value is nil, false: there is nothing
// to mask. A value that is not an object is not a shape the engine sends, so it is replaced by
// Placeholder and reported withheld.
func objectAt(m map[string]any, key string) (obj map[string]any, withheld bool) {
	switch v := m[key].(type) {
	case map[string]any:
		return v, false
	case nil:
		return nil, false
	default:
		m[key] = Placeholder
		return nil, true
	}
}

var (
	// ErrPlaceholderUnmatched means the submitted spec holds a Placeholder that stands for no
	// current value: a variable, argv, health check command or log option the service does not
	// have. The error is a *PlaceholderError naming the field.
	ErrPlaceholderUnmatched = errors.New("redact: a hidden value has nothing to stand for")
	// ErrPlaceholderReserved means a caller who may reveal secrets sent Placeholder, which is
	// reserved for the masked form and cannot be a real value. The error is a *PlaceholderError
	// naming the field.
	ErrPlaceholderReserved = errors.New("redact: the placeholder is reserved and cannot be a value")
)

// PlaceholderError is ErrPlaceholderUnmatched or ErrPlaceholderReserved with the spec path of
// the field. The path names a variable or an option, never a value.
type PlaceholderError struct {
	Err  error
	Path string
}

func (e *PlaceholderError) Error() string { return e.Err.Error() + " (" + e.Path + ")" }
func (e *PlaceholderError) Unwrap() error { return e.Err }

// MergeMaskedSpec puts the values a caller could not see back into the spec they submitted
// (D-09, D-39). current is the spec as Docker holds it now, submitted the spec the caller
// edited, which for a caller without secrets.reveal came from MaskServiceSpec.
//
// Only Placeholder is recognised. Each env entry NAME=<hidden> takes the value of NAME from
// current, the nth entry of a name taking the nth of current so duplicate names stay
// positional; Command or Args equal to [<hidden>] keep the current argv; a health check test of
// [mode, <hidden>] keeps the current command under the submitted mode; a log option whose value
// is <hidden> keeps the current value, while the driver is the one current has. An entry the
// caller removed is gone, and a value they typed replaces the old one. A Placeholder with
// nothing to stand for is ErrPlaceholderUnmatched.
//
// With reveal true the caller saw every value, so Placeholder in those fields can only be a
// real value typed by mistake: it is ErrPlaceholderReserved and nothing is merged.
// Neither spec is modified.
func MergeMaskedSpec(current, submitted swarm.ServiceSpec, reveal bool) (swarm.ServiceSpec, error) {
	if reveal {
		if err := reservedIn(submitted); err != nil {
			return swarm.ServiceSpec{}, err
		}
		return submitted, nil
	}
	out := submitted
	if cs := submitted.TaskTemplate.ContainerSpec; cs != nil {
		c := *cs
		var cur swarm.ContainerSpec
		if cc := current.TaskTemplate.ContainerSpec; cc != nil {
			cur = *cc
		}
		var err error
		if c.Env, err = mergeEnv(cur.Env, c.Env); err != nil {
			return swarm.ServiceSpec{}, err
		}
		if c.Command, err = mergeArgv(cur.Command, c.Command, containerSpecPath+"Command"); err != nil {
			return swarm.ServiceSpec{}, err
		}
		if c.Args, err = mergeArgv(cur.Args, c.Args, containerSpecPath+"Args"); err != nil {
			return swarm.ServiceSpec{}, err
		}
		if c.Healthcheck, err = mergeHealthcheck(cur.Healthcheck, c.Healthcheck); err != nil {
			return swarm.ServiceSpec{}, err
		}
		out.TaskTemplate.ContainerSpec = &c
	}
	ld, err := mergeLogDriver(current.TaskTemplate.LogDriver, submitted.TaskTemplate.LogDriver)
	if err != nil {
		return swarm.ServiceSpec{}, err
	}
	out.TaskTemplate.LogDriver = ld
	return out, nil
}

const (
	containerSpecPath = "TaskTemplate.ContainerSpec."
	logOptionsPath    = "TaskTemplate.LogDriver.Options"
)

// envValue splits an env entry; hidden is true when its value is exactly Placeholder.
func envValue(kv string) (name string, hidden bool) {
	name, v, has := strings.Cut(kv, "=")
	return name, has && v == Placeholder
}

// reservedIn finds a Placeholder in a secret-bearing field of a spec a revealing caller sent.
func reservedIn(spec swarm.ServiceSpec) error {
	reserved := func(path string) error { return &PlaceholderError{Err: ErrPlaceholderReserved, Path: path} }
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, kv := range cs.Env {
			if name, hidden := envValue(kv); hidden {
				return reserved(containerSpecPath + "Env[" + name + "]")
			}
		}
		if slices.Contains(cs.Command, Placeholder) {
			return reserved(containerSpecPath + "Command")
		}
		if slices.Contains(cs.Args, Placeholder) {
			return reserved(containerSpecPath + "Args")
		}
		if hc := cs.Healthcheck; hc != nil && len(hc.Test) > 1 && slices.Contains(hc.Test[1:], Placeholder) {
			return reserved(containerSpecPath + "Healthcheck.Test")
		}
	}
	if ld := spec.TaskTemplate.LogDriver; ld != nil {
		for _, name := range slices.Sorted(maps.Keys(ld.Options)) {
			if ld.Options[name] == Placeholder {
				return reserved(logOptionsPath + "[" + name + "]")
			}
		}
	}
	return nil
}

// mergeEnv replaces each NAME=<hidden> by the entry of the same name and position in cur.
func mergeEnv(cur, sub []string) ([]string, error) {
	if !slices.ContainsFunc(sub, func(kv string) bool { _, hidden := envValue(kv); return hidden }) {
		return sub, nil
	}
	byName := map[string][]string{}
	for _, kv := range cur {
		name, _, _ := strings.Cut(kv, "=")
		byName[name] = append(byName[name], kv)
	}
	seen := map[string]int{}
	out := make([]string, len(sub))
	for i, kv := range sub {
		name, hidden := envValue(kv)
		if !hidden {
			name, _, _ = strings.Cut(kv, "=")
		}
		n := seen[name]
		seen[name]++
		if !hidden {
			out[i] = kv
			continue
		}
		entries := byName[name]
		if n >= len(entries) || !strings.Contains(entries[n], "=") {
			return nil, &PlaceholderError{Err: ErrPlaceholderUnmatched, Path: containerSpecPath + "Env[" + name + "]"}
		}
		out[i] = entries[n]
	}
	return out, nil
}

// mergeArgv keeps the current argv for a submitted one that is exactly [Placeholder].
func mergeArgv(cur, sub []string, path string) ([]string, error) {
	if !slices.Contains(sub, Placeholder) {
		return sub, nil
	}
	if len(sub) == 1 && len(cur) > 0 {
		return slices.Clone(cur), nil
	}
	return nil, &PlaceholderError{Err: ErrPlaceholderUnmatched, Path: path}
}

// mergeHealthcheck keeps the current command of a health check whose test is [mode, Placeholder],
// under the submitted mode.
func mergeHealthcheck(cur, sub *container.HealthConfig) (*container.HealthConfig, error) {
	if sub == nil || len(sub.Test) < 2 || !slices.Contains(sub.Test[1:], Placeholder) {
		return sub, nil
	}
	if len(sub.Test) != 2 || cur == nil || len(cur.Test) < 2 {
		return nil, &PlaceholderError{Err: ErrPlaceholderUnmatched, Path: containerSpecPath + "Healthcheck.Test"}
	}
	h := *sub
	h.Test = append([]string{sub.Test[0]}, cur.Test[1:]...)
	return &h, nil
}

// mergeLogDriver keeps the current value of each log option submitted as Placeholder, while the
// driver is the one the service has.
func mergeLogDriver(cur, sub *swarm.Driver) (*swarm.Driver, error) {
	if sub == nil {
		return nil, nil
	}
	hiddenAny := false
	for _, v := range sub.Options {
		hiddenAny = hiddenAny || v == Placeholder
	}
	if !hiddenAny {
		return sub, nil
	}
	opts := maps.Clone(sub.Options)
	for _, name := range slices.Sorted(maps.Keys(sub.Options)) {
		if sub.Options[name] != Placeholder {
			continue
		}
		v, ok := "", false
		if cur != nil && cur.Name == sub.Name {
			v, ok = cur.Options[name]
		}
		if !ok {
			return nil, &PlaceholderError{Err: ErrPlaceholderUnmatched, Path: logOptionsPath + "[" + name + "]"}
		}
		opts[name] = v
	}
	return &swarm.Driver{Name: sub.Name, Options: opts}, nil
}

// minScrub is the shortest value ScrubServiceText replaces.
const minScrub = 4

// ScrubServiceText returns text with every secret-bearing value of spec replaced by Placeholder:
// the values of its environment, each element of its Command and Args, the command of its health
// check and the values of its log options. Docker echoes parts of a spec in some of its error
// messages, and a caller who may not reveal secrets must not read them there (Pitfall 7). A value
// shorter than four characters is left alone, since it cannot be told from the prose around it.
func ScrubServiceText(text string, spec swarm.ServiceSpec) string {
	var secrets []string
	add := func(v string) {
		if len(v) >= minScrub {
			secrets = append(secrets, v)
		}
	}
	if cs := spec.TaskTemplate.ContainerSpec; cs != nil {
		for _, kv := range cs.Env {
			if _, v, has := strings.Cut(kv, "="); has {
				add(v)
			}
		}
		for _, v := range cs.Command {
			add(v)
		}
		for _, v := range cs.Args {
			add(v)
		}
		if hc := cs.Healthcheck; hc != nil && len(hc.Test) > 1 {
			for _, v := range hc.Test[1:] {
				add(v)
			}
		}
	}
	if ld := spec.TaskTemplate.LogDriver; ld != nil {
		for _, v := range ld.Options {
			add(v)
		}
	}
	if len(secrets) == 0 {
		return text
	}
	// Longest first, so a value that holds another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	pairs := make([]string, 0, 2*len(secrets))
	for _, v := range slices.Compact(secrets) {
		pairs = append(pairs, v, Placeholder)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}
