// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package stacks_test

import (
	"strconv"
	"testing"

	"github.com/ianglennon/bayplan/internal/stacks"
)

func secretVar(name, value string) stacks.Var {
	return stacks.Var{Name: name, Value: value, Secret: true}
}

func TestSecretUseFollowsEveryWayAFieldCanComeFromASecret(t *testing.T) {
	t.Parallel()
	const compose = `x-m: &m ${PW}
x-base: &base
  mem_reservation: ${PW}
services:
  web:
    <<: *base
    image: nginx:1.27-alpine
    mem_limit: *m
    shm_size: ${A:-${PW}}
    cpus: ${B:+${PW}}
    environment: [PW]
    memswap_limit: ${PLAIN}
`
	s := stacks.NewSecretUse(copyOf(compose, secretVar("PW", "Password2024!"),
		stacks.Var{Name: "A", Value: "1"}, stacks.Var{Name: "B", Value: "1"}, stacks.Var{Name: "PLAIN", Value: "1m"}))
	if !s.Any() {
		t.Fatal("Any = false, want true: the file uses a secret")
	}
	for _, path := range []string{
		"services.web.mem_limit", "services.web.mem_reservation", "services.web.shm_size", "services.web.cpus",
		"services.web.environment", "", "services.web.no_such_key",
	} {
		if !s.Uses(path) {
			t.Errorf("Uses(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"services.web.memswap_limit", "services.web.image"} {
		if s.Uses(path) {
			t.Errorf("Uses(%q) = true, want false", path)
		}
	}
	// An environment list written once under an anchor and aliased into a service.
	const aliased = "x-env: &env [PW]\nservices:\n  web:\n    image: nginx:1.27-alpine\n    environment: *env\n"
	if !stacks.NewSecretUse(copyOf(aliased, secretVar("PW", "Password2024!"))).Uses("services.web.environment") {
		t.Error("Uses(services.web.environment) = false for an aliased list that names PW, want true")
	}
}

func TestSecretUseMatchesTheLoadersGoFieldNameToTheFilesKey(t *testing.T) {
	t.Parallel()
	const path = "services.web.blkio_config.device_read_bps.0.Rate"
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n    blkio_config:\n      device_read_bps:\n        - path: /dev/sda\n"
	s := stacks.NewSecretUse(copyOf(head+"          rate: ${PW}\n", secretVar("PW", "Password2024!")))
	if !s.Uses(path) {
		t.Errorf("Uses(%q) = false, want true: the file's rate uses the secret", path)
	}
	s = stacks.NewSecretUse(copyOf(head+"          rate: 1xx\n    environment:\n      DB: ${PW}\n", secretVar("PW", "Password2024!")))
	if s.Uses(path) {
		t.Errorf("Uses(%q) = true, want false: the file's rate is plain", path)
	}
}

func TestSecretUseKnowsNothingWithoutASecretInTheFile(t *testing.T) {
	t.Parallel()
	s := stacks.NewSecretUse(copyOf("services:\n  web:\n    image: nginx:${PLAIN}\n",
		secretVar("PW", "Password2024!"), stacks.Var{Name: "PLAIN", Value: "1"}))
	if s.Any() {
		t.Error("Any = true, want false: no secret is referenced")
	}
	for _, path := range []string{"", "services.web.image", "services.web.no_such_key"} {
		if s.Uses(path) {
			t.Errorf("Uses(%q) = true, want false", path)
		}
	}
}

func TestSecretUseQuotesARunOfASecret(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27-alpine\n    mem_limit: ${PW}\n"
	quotes := func(secret, text string) bool {
		return stacks.NewSecretUse(copyOf(compose, secretVar("PW", secret))).Quotes(text)
	}
	for name, tc := range map[string]struct {
		secret, text string
		want         bool
	}{
		"a substring the parser quotes": {"Password2024!", `parsing "Password2024"`, true},
		"the shortest run":              {"Password2024!", "Pass", true},
		"under the shortest run":        {"Password2024!", "Pas", false},
		"the parser's own words":        {"Password2024!", "invalid size: 'lots'", false},
		"the Go form":                   {`pa"ss-12`, strconv.Quote(`pa"ss-12`), true},
		"the schema form":               {"ab'cd", `'ab\'cd'`, true},
		"a line of several":             {"first-line-1234\nsecond-5678", "second-5678", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := quotes(tc.secret, tc.text); got != tc.want {
				t.Errorf("Quotes(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestSecretUseZeroValueKnowsNothing(t *testing.T) {
	t.Parallel()
	var s stacks.SecretUse
	if s.Any() || s.Uses("services.web.mem_limit") || s.Uses("") || s.Quotes("Password2024!") || s.Withholds("a", "Password2024!") {
		t.Error("the zero SecretUse answered true, want false everywhere")
	}
}
