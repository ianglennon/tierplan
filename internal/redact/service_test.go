// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package redact_test

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/bayplan/internal/redact"
)

// A service's spec carries the same secrets a container's does (D-39, AUTH-07): the values of
// its environment, its command line, its health check and its log driver options. These
// tests pin what leaves the server for a caller who may not reveal them (plan 06-06).

func specWith(mutate func(cs *swarm.ContainerSpec, tt *swarm.TaskSpec)) swarm.ServiceSpec {
	spec := swarm.ServiceSpec{}
	spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: "nginx:1"}
	mutate(spec.TaskTemplate.ContainerSpec, &spec.TaskTemplate)
	return spec
}

func TestMaskServiceSpecHidesEnvValuesAndKeepsNamesAndNameOnlyEntries(t *testing.T) {
	in := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Env = []string{"A=1", "B=two=2", "C"}
	})
	got, masked := redact.MaskServiceSpec(in, false)
	want := []string{"A=<hidden>", "B=<hidden>", "C"}
	if !masked || !slices.Equal(got.TaskTemplate.ContainerSpec.Env, want) {
		t.Fatalf("Env = %v masked %v, want %v masked true", got.TaskTemplate.ContainerSpec.Env, masked, want)
	}
}

func TestMaskServiceSpecReplacesACommandAndItsArgsByOnePlaceholderEach(t *testing.T) {
	in := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Command = []string{"sh", "-c", "x"}
		cs.Args = []string{"--password", "hunter2"}
	})
	got, masked := redact.MaskServiceSpec(in, false)
	cs := got.TaskTemplate.ContainerSpec
	if !masked || !slices.Equal(cs.Command, []string{"<hidden>"}) || !slices.Equal(cs.Args, []string{"<hidden>"}) {
		t.Fatalf("Command = %v, Args = %v, masked %v", cs.Command, cs.Args, masked)
	}
}

func TestMaskServiceSpecKeepsTheModeOfAHealthCheckAndHidesItsCommand(t *testing.T) {
	cases := []struct {
		name   string
		test   []string
		want   []string
		masked bool
	}{
		{"a shell command", []string{"CMD-SHELL", "curl -u a:b x"}, []string{"CMD-SHELL", "<hidden>"}, true},
		{"an exec command", []string{"CMD", "check", "--key=k"}, []string{"CMD", "<hidden>"}, true},
		{"none", []string{"NONE"}, []string{"NONE"}, false},
		{"inherit", nil, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
				cs.Healthcheck = &container.HealthConfig{Test: tc.test}
			})
			got, masked := redact.MaskServiceSpec(in, false)
			if masked != tc.masked || !slices.Equal(got.TaskTemplate.ContainerSpec.Healthcheck.Test, tc.want) {
				t.Fatalf("Test = %v masked %v, want %v masked %v", got.TaskTemplate.ContainerSpec.Healthcheck.Test, masked, tc.want, tc.masked)
			}
		})
	}
}

func TestMaskServiceSpecHidesEveryLogDriverOptionValueAndKeepsTheNames(t *testing.T) {
	in := specWith(func(_ *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		tt.LogDriver = &swarm.Driver{Name: "splunk", Options: map[string]string{"splunk-token": "t", "tag": "web"}}
	})
	got, masked := redact.MaskServiceSpec(in, false)
	want := map[string]string{"splunk-token": "<hidden>", "tag": "<hidden>"}
	if !masked || got.TaskTemplate.LogDriver.Name != "splunk" || !reflect.DeepEqual(got.TaskTemplate.LogDriver.Options, want) {
		t.Fatalf("LogDriver = %+v masked %v, want options %v", got.TaskTemplate.LogDriver, masked, want)
	}
}

func TestMaskServiceSpecWithRevealReturnsTheSpecUnchangedAndNotMasked(t *testing.T) {
	in := specWith(func(cs *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		cs.Env = []string{"A=1"}
		cs.Command = []string{"run"}
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "x"}}
		tt.LogDriver = &swarm.Driver{Options: map[string]string{"k": "v"}}
	})
	got, masked := redact.MaskServiceSpec(in, true)
	if masked || !reflect.DeepEqual(got, in) {
		t.Fatalf("with reveal: masked %v, spec %+v, want the input", masked, got)
	}
}

func TestMaskServiceSpecOfASpecWithNothingSecretIsNotMasked(t *testing.T) {
	in := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Env = []string{"FLAG"}
		cs.Healthcheck = &container.HealthConfig{Test: []string{"NONE"}}
	})
	if got, masked := redact.MaskServiceSpec(in, false); masked || !reflect.DeepEqual(got, in) {
		t.Fatalf("masked %v, spec %+v", masked, got)
	}
	// A spec with no container spec at all (a plugin or attachment task) has nothing to hide.
	if _, masked := redact.MaskServiceSpec(swarm.ServiceSpec{}, false); masked {
		t.Fatal("an empty spec was reported masked")
	}
}

func TestMaskServiceSpecDoesNotChangeTheSpecItWasGiven(t *testing.T) {
	in := specWith(func(cs *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		cs.Env = []string{"A=1"}
		cs.Command = []string{"run", "x"}
		cs.Args = []string{"a"}
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "x"}}
		tt.LogDriver = &swarm.Driver{Name: "d", Options: map[string]string{"k": "v"}}
	})
	_, _ = redact.MaskServiceSpec(in, false)
	cs := in.TaskTemplate.ContainerSpec
	if cs.Env[0] != "A=1" || cs.Command[0] != "run" || cs.Args[0] != "a" || cs.Healthcheck.Test[1] != "x" || in.TaskTemplate.LogDriver.Options["k"] != "v" {
		t.Fatalf("the input was changed: %+v %+v", cs, in.TaskTemplate.LogDriver)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/servicespec/" + name) //nolint:gosec // G304: a fixture name written in this file
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// secretTokens are the values secret-bearing.json and secret-bearing-container.json hide,
// one per kind of secret-bearing field.
var secretTokens = map[string]string{
	"env":     "SECRET-ENV-VALUE",
	"dsn":     "SECRET-DSN",
	"command": "SECRET-CMD-ARG",
	"args":    "SECRET-ARGS-VALUE",
	"health":  "SECRET-HC-CMD",
	"logopt":  "SECRET-LOG-OPT",
}

func TestMaskServiceInspectMasksSpecAndPreviousSpecAndKeepsEverythingElse(t *testing.T) {
	raw := readFixture(t, "secret-bearing.json")
	out, masked, err := redact.MaskServiceInspect(raw, false)
	if err != nil || !masked {
		t.Fatalf("masked %v, err %v", masked, err)
	}
	for kind, token := range secretTokens {
		if strings.Contains(string(out), token) {
			t.Errorf("the masked document still carries the %s secret %q", kind, token)
		}
	}
	var doc struct {
		Version struct{ Index json.Number }
		Spec    struct {
			Name         string
			Labels       map[string]string
			TaskTemplate struct {
				ContainerSpec struct {
					Image       string
					Env         []string
					Command     []string
					Args        []string
					Healthcheck struct{ Test []string }
					Mounts      []map[string]any
				}
				LogDriver struct {
					Name    string
					Options map[string]string
				}
				ForceUpdate int
			}
		}
		PreviousSpec struct {
			TaskTemplate struct {
				ContainerSpec struct {
					Env         []string
					Healthcheck struct{ Test []string }
				}
				LogDriver struct{ Options map[string]string }
			}
		}
	}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the masked document is not JSON: %v\n%s", err, out)
	}
	cs := doc.Spec.TaskTemplate.ContainerSpec
	checks := []struct {
		what      string
		got, want any
	}{
		{"Version.Index keeps every digit", doc.Version.Index.String(), "9007199254740993"},
		{"Spec.Name", doc.Spec.Name, "api"},
		{"Spec.Labels", doc.Spec.Labels["com.docker.stack.namespace"], "shop"},
		{"Image", cs.Image, "registry.example.com/shop/api:1.4@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"Env", cs.Env, []string{"DB_PASSWORD=<hidden>", "DSN=<hidden>", "FLAG"}},
		{"Command", cs.Command, []string{"<hidden>"}},
		{"Args", cs.Args, []string{"<hidden>"}},
		{"Healthcheck.Test", cs.Healthcheck.Test, []string{"CMD-SHELL", "<hidden>"}},
		{"Mounts", len(cs.Mounts), 1},
		{"LogDriver.Name", doc.Spec.TaskTemplate.LogDriver.Name, "splunk"},
		{"LogDriver.Options", doc.Spec.TaskTemplate.LogDriver.Options, map[string]string{"splunk-token": "<hidden>", "splunk-url": "<hidden>"}},
		{"ForceUpdate", doc.Spec.TaskTemplate.ForceUpdate, 3},
		{"PreviousSpec Env", doc.PreviousSpec.TaskTemplate.ContainerSpec.Env, []string{"DB_PASSWORD=<hidden>"}},
		{"PreviousSpec Healthcheck.Test", doc.PreviousSpec.TaskTemplate.ContainerSpec.Healthcheck.Test, []string{"CMD", "<hidden>"}},
		{"PreviousSpec LogDriver.Options", doc.PreviousSpec.TaskTemplate.LogDriver.Options, map[string]string{"splunk-token": "<hidden>"}},
	}
	for _, c := range checks {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s = %v, want %v", c.what, c.got, c.want)
		}
	}
}

func TestMaskServiceInspectWithRevealReturnsTheInputAndReportsNothingMasked(t *testing.T) {
	raw := readFixture(t, "secret-bearing.json")
	out, masked, err := redact.MaskServiceInspect(raw, true)
	if err != nil || masked || string(out) != string(raw) {
		t.Fatalf("masked %v, err %v, changed %v", masked, err, string(out) != string(raw))
	}
}

func TestMaskServiceInspectOfAServiceWithNothingSecretIsNotMasked(t *testing.T) {
	raw := []byte(`{"ID":"s","Spec":{"Name":"web","TaskTemplate":{"ContainerSpec":{"Image":"nginx","Env":["FLAG"],"Healthcheck":{"Test":["NONE"]}}}}}`)
	if _, masked, err := redact.MaskServiceInspect(raw, false); err != nil || masked {
		t.Fatalf("masked %v, err %v", masked, err)
	}
}

func TestMaskServiceInspectWithholdsShapesTheEngineDoesNotSendRatherThanPassingThemThrough(t *testing.T) {
	cases := map[string]string{
		"env that is not a list":        `{"Spec":{"TaskTemplate":{"ContainerSpec":{"Env":"A=SECRET-X"}}}}`,
		"an env entry that is a number": `{"Spec":{"TaskTemplate":{"ContainerSpec":{"Env":[12345678]}}}}`,
		"command that is text":          `{"Spec":{"TaskTemplate":{"ContainerSpec":{"Command":"run SECRET-X"}}}}`,
		"log options that are text":     `{"Spec":{"TaskTemplate":{"LogDriver":{"Options":"SECRET-X"}}}}`,
		"a container spec that is text": `{"Spec":{"TaskTemplate":{"ContainerSpec":"SECRET-X"}}}`,
		"a previous spec that is text":  `{"PreviousSpec":"SECRET-X"}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			out, masked, err := redact.MaskServiceInspect([]byte(doc), false)
			if err != nil || !masked {
				t.Fatalf("masked %v, err %v", masked, err)
			}
			if strings.Contains(string(out), "SECRET-X") || strings.Contains(string(out), "12345678") {
				t.Fatalf("the document still carries the value: %s", out)
			}
		})
	}
}

func TestMaskServiceInspectOfAnythingThatIsNotAJSONObjectIsAnErrorNeverAPassthrough(t *testing.T) {
	for _, raw := range []string{``, `null`, `[]`, `"SECRET-X"`, `{"Spec":`, `{"Spec":{}} {"x":1}`} {
		out, masked, err := redact.MaskServiceInspect([]byte(raw), false)
		if err == nil || masked || out != nil {
			t.Errorf("MaskServiceInspect(%q) = %q, %v, %v, want an error and no output", raw, out, masked, err)
		}
	}
}

// TestServiceAndContainerMaskingAgree runs the same secrets through the container's mask and
// the service's: every kind of value the container hides, the service hides too, so no
// service endpoint can hand a Viewer a value the container inspect would withhold.
func TestServiceAndContainerMaskingAgree(t *testing.T) {
	containerOut, cMasked, err := redact.MaskContainerInspect(readFixture(t, "secret-bearing-container.json"), false)
	if err != nil || !cMasked {
		t.Fatalf("container: masked %v, err %v", cMasked, err)
	}
	serviceOut, sMasked, err := redact.MaskServiceInspect(readFixture(t, "secret-bearing.json"), false)
	if err != nil || !sMasked {
		t.Fatalf("service: masked %v, err %v", sMasked, err)
	}
	for kind, token := range secretTokens {
		inContainer := strings.Contains(string(containerOut), token)
		inService := strings.Contains(string(serviceOut), token)
		if inContainer {
			t.Errorf("the container mask lets the %s secret through: the fixtures disagree", kind)
		}
		if inService {
			t.Errorf("the service mask lets the %s secret through, which the container mask withholds", kind)
		}
	}
	// With reveal both documents keep every secret, so the tokens above are really in the fixtures.
	for kind, token := range secretTokens {
		for name, fixture := range map[string]string{"container": "secret-bearing-container.json", "service": "secret-bearing.json"} {
			if !strings.Contains(string(readFixture(t, fixture)), token) {
				t.Errorf("the %s fixture has no %s secret %q", name, kind, token)
			}
		}
	}
}

// MergeMaskedSpec puts back what the caller could not see (plan 06-18, D-09, D-39).

func merge(t *testing.T, current, submitted swarm.ServiceSpec, reveal bool) (swarm.ServiceSpec, error) {
	t.Helper()
	return redact.MergeMaskedSpec(current, submitted, reveal)
}

func TestMergeMaskedSpecEnvKeepsHiddenValuesPositionallyAndTakesWhatTheCallerTyped(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=1", "A=2", "B=3"} })
	sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Env = []string{"A=<hidden>", "A=<hidden>", "C=4"}
	})
	got, err := merge(t, cur, sub, false)
	if err != nil {
		t.Fatalf("MergeMaskedSpec: %v", err)
	}
	if want := []string{"A=1", "A=2", "C=4"}; !slices.Equal(got.TaskTemplate.ContainerSpec.Env, want) {
		t.Fatalf("Env = %v, want %v (B removed, C added)", got.TaskTemplate.ContainerSpec.Env, want)
	}
	// A real value replaces the old one, a name-only entry stays as written.
	sub = specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=new", "A=<hidden>", "NAMEONLY"} })
	got, err = merge(t, cur, sub, false)
	if want := []string{"A=new", "A=2", "NAMEONLY"}; err != nil || !slices.Equal(got.TaskTemplate.ContainerSpec.Env, want) {
		t.Fatalf("Env = %v, %v; want %v", got.TaskTemplate.ContainerSpec.Env, err, want)
	}
}

func TestMergeMaskedSpecAHiddenVariableTheServiceDoesNotHaveIsUnmatchedAndNamesIt(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"B=3"} })
	sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=<hidden>"} })
	_, err := merge(t, cur, sub, false)
	var pe *redact.PlaceholderError
	if !errors.Is(err, redact.ErrPlaceholderUnmatched) || !errors.As(err, &pe) || !strings.Contains(pe.Path, "A") {
		t.Fatalf("err = %v, want ErrPlaceholderUnmatched naming A", err)
	}
	// One more hidden A than the service has is unmatched as well.
	cur = specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=1"} })
	sub = specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=<hidden>", "A=<hidden>"} })
	if _, err := merge(t, cur, sub, false); !errors.Is(err, redact.ErrPlaceholderUnmatched) {
		t.Fatalf("err = %v, want ErrPlaceholderUnmatched", err)
	}
}

func TestMergeMaskedSpecCommandAndArgsEqualToThePlaceholderKeepTheCurrentArgv(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Command = []string{"sh", "-c", "run --pw hunter2"}
		cs.Args = []string{"--token", "t0ken"}
	})
	sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Command = []string{"<hidden>"}
		cs.Args = []string{"--token", "other"}
	})
	got, err := merge(t, cur, sub, false)
	cs := got.TaskTemplate.ContainerSpec
	if err != nil || !slices.Equal(cs.Command, []string{"sh", "-c", "run --pw hunter2"}) || !slices.Equal(cs.Args, []string{"--token", "other"}) {
		t.Fatalf("Command = %v, Args = %v, %v", cs.Command, cs.Args, err)
	}
	// Args likewise.
	sub = specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Args = []string{"<hidden>"} })
	got, err = merge(t, cur, sub, false)
	if err != nil || !slices.Equal(got.TaskTemplate.ContainerSpec.Args, []string{"--token", "t0ken"}) || len(got.TaskTemplate.ContainerSpec.Command) != 0 {
		t.Fatalf("Args = %v, Command = %v, %v", got.TaskTemplate.ContainerSpec.Args, got.TaskTemplate.ContainerSpec.Command, err)
	}
	// A placeholder with no argv behind it, or mixed into a longer argv, is unmatched.
	empty := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {})
	for name, in := range map[string][]string{"nothing to keep": {"<hidden>"}, "mixed": {"sh", "<hidden>"}} {
		sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Command = in })
		cur := cur
		if name == "nothing to keep" {
			cur = empty
		}
		if _, err := merge(t, cur, sub, false); !errors.Is(err, redact.ErrPlaceholderUnmatched) {
			t.Errorf("%s: err = %v, want ErrPlaceholderUnmatched", name, err)
		}
	}
}

func TestMergeMaskedSpecAHiddenHealthCheckCommandKeepsTheCurrentOneUnderTheSubmittedMode(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "curl", "-u", "a:secret", "http://x"}, Retries: 3}
	})
	sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD-SHELL", "<hidden>"}, Retries: 5}
	})
	got, err := merge(t, cur, sub, false)
	hc := got.TaskTemplate.ContainerSpec.Healthcheck
	if err != nil || hc == nil || !slices.Equal(hc.Test, []string{"CMD-SHELL", "curl", "-u", "a:secret", "http://x"}) || hc.Retries != 5 {
		t.Fatalf("Healthcheck = %+v, %v", hc, err)
	}
	// A health check typed anew is taken as it is; a hidden one with nothing behind it is unmatched.
	sub = specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "true"}}
	})
	if got, err := merge(t, cur, sub, false); err != nil || !slices.Equal(got.TaskTemplate.ContainerSpec.Healthcheck.Test, []string{"CMD", "true"}) {
		t.Fatalf("typed health check = %+v, %v", got.TaskTemplate.ContainerSpec.Healthcheck, err)
	}
	none := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {})
	hidden := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "<hidden>"}}
	})
	if _, err := merge(t, none, hidden, false); !errors.Is(err, redact.ErrPlaceholderUnmatched) {
		t.Fatalf("err = %v, want ErrPlaceholderUnmatched", err)
	}
}

func TestMergeMaskedSpecLogOptionsKeepTheHiddenValueAndTakeTheTypedOne(t *testing.T) {
	cur := specWith(func(_ *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		tt.LogDriver = &swarm.Driver{Name: "splunk", Options: map[string]string{"a": "va", "b": "vb", "gone": "x"}}
	})
	sub := specWith(func(_ *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		tt.LogDriver = &swarm.Driver{Name: "splunk", Options: map[string]string{"a": "<hidden>", "b": "typed", "c": "vc"}}
	})
	got, err := merge(t, cur, sub, false)
	want := map[string]string{"a": "va", "b": "typed", "c": "vc"}
	if err != nil || !maps.Equal(got.TaskTemplate.LogDriver.Options, want) {
		t.Fatalf("Options = %v, %v; want %v (gone removed)", got.TaskTemplate.LogDriver.Options, err, want)
	}
	// An option the service does not have, or a changed driver, has nothing to stand for.
	for name, drv := range map[string]*swarm.Driver{
		"no such option": {Name: "splunk", Options: map[string]string{"zzz": "<hidden>"}},
		"other driver":   {Name: "syslog", Options: map[string]string{"a": "<hidden>"}},
	} {
		sub := specWith(func(_ *swarm.ContainerSpec, tt *swarm.TaskSpec) { tt.LogDriver = drv })
		if _, err := merge(t, cur, sub, false); !errors.Is(err, redact.ErrPlaceholderUnmatched) {
			t.Errorf("%s: err = %v, want ErrPlaceholderUnmatched", name, err)
		}
	}
}

func TestMergeMaskedSpecWithRevealAnyPlaceholderInASecretFieldIsReserved(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=1"} })
	for name, sub := range map[string]swarm.ServiceSpec{
		"env":     specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=<hidden>"} }),
		"command": specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Command = []string{"<hidden>"} }),
		"args":    specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Args = []string{"x", "<hidden>"} }),
		"health": specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) {
			cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "<hidden>"}}
		}),
		"log option": specWith(func(_ *swarm.ContainerSpec, tt *swarm.TaskSpec) {
			tt.LogDriver = &swarm.Driver{Name: "x", Options: map[string]string{"a": "<hidden>"}}
		}),
	} {
		_, err := merge(t, cur, sub, true)
		var pe *redact.PlaceholderError
		if !errors.Is(err, redact.ErrPlaceholderReserved) || !errors.As(err, &pe) || pe.Path == "" {
			t.Errorf("%s: err = %v, want ErrPlaceholderReserved with a path", name, err)
		}
	}
	// With reveal and no placeholder the submitted spec is taken as it is.
	sub := specWith(func(cs *swarm.ContainerSpec, _ *swarm.TaskSpec) { cs.Env = []string{"A=typed"} })
	if got, err := merge(t, cur, sub, true); err != nil || !slices.Equal(got.TaskTemplate.ContainerSpec.Env, []string{"A=typed"}) {
		t.Fatalf("reveal without a placeholder = %+v, %v", got.TaskTemplate.ContainerSpec, err)
	}
}

func TestMergeMaskedSpecRoundTripsAMaskedSpecAndChangesNeitherInput(t *testing.T) {
	cur := specWith(func(cs *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		cs.Env = []string{"DB=postgres://u:pw@h/db", "MODE=prod", "BARE"}
		cs.Command = []string{"app", "--key=k"}
		cs.Args = []string{"serve"}
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD-SHELL", "wget -q --header=X:secret localhost"}}
		tt.LogDriver = &swarm.Driver{Name: "gelf", Options: map[string]string{"gelf-address": "udp://tok@h:12201"}}
	})
	masked, was := redact.MaskServiceSpec(cur, false)
	if !was {
		t.Fatal("fixture was not masked")
	}
	snapCur, snapMasked := mustJSON(t, cur), mustJSON(t, masked)
	got, err := merge(t, cur, masked, false)
	if err != nil {
		t.Fatalf("MergeMaskedSpec: %v", err)
	}
	if mustJSON(t, got) != snapCur {
		t.Errorf("a masked spec sent back untouched did not round-trip:\n got %s\nwant %s", mustJSON(t, got), snapCur)
	}
	if mustJSON(t, cur) != snapCur || mustJSON(t, masked) != snapMasked {
		t.Error("MergeMaskedSpec modified one of its inputs")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ScrubServiceText keeps a value Docker echoes out of an error (Pitfall 7).

func TestScrubServiceTextReplacesEverySecretBearingValueOfTheSpec(t *testing.T) {
	spec := specWith(func(cs *swarm.ContainerSpec, tt *swarm.TaskSpec) {
		cs.Env = []string{"DB=postgres://u:hunter2@h/db", "SHORT=ab", "NAME"}
		cs.Command = []string{"app", "--key=k3y"}
		cs.Healthcheck = &container.HealthConfig{Test: []string{"CMD", "probe", "tokenvalue"}}
		tt.LogDriver = &swarm.Driver{Name: "gelf", Options: map[string]string{"addr": "udp://logtoken@h"}}
	})
	text := "invalid env postgres://u:hunter2@h/db, command app --key=k3y, probe tokenvalue, udp://logtoken@h"
	got := redact.ScrubServiceText(text, spec)
	for _, secret := range []string{"hunter2", "k3y", "tokenvalue", "logtoken"} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubbed text %q still holds %q", got, secret)
		}
	}
	if !strings.Contains(got, redact.Placeholder) || !strings.Contains(got, "invalid env") {
		t.Errorf("scrubbed text = %q, want the message kept around the placeholders", got)
	}
	// Values too short to be told from prose are not scrubbed, so an ordinary word survives.
	if got := redact.ScrubServiceText("a bad value: ab", spec); got != "a bad value: ab" {
		t.Errorf("a two-character value was scrubbed: %q", got)
	}
}
