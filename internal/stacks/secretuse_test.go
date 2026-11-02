// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package stacks_test

import (
	"testing"

	"github.com/ianglennon/tierplan/internal/stacks"
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

// The decision reads where a value comes from, never what a message says (07-REVIEW-gaps2 CR-01).
func TestSecretUseWithholdsByWhereAValueComesFromNeverByText(t *testing.T) {
	t.Parallel()
	const compose = "services:\n  web:\n    image: nginx:1.27-alpine\n    mem_limit: ${PW}\n    memswap_limit: 1g\n"
	s := stacks.NewSecretUse(copyOf(compose, secretVar("PW", "Password2024!")))
	for path, want := range map[string]bool{
		"":                           true,
		"services.web.mem_limit":     true,
		"services.web.no_such_key":   true, // a path no key matches fails closed
		"services.web.memswap_limit": false,
		"services.web.image":         false,
	} {
		if got := s.Withholds(path); got != want {
			t.Errorf("Withholds(%q) = %v, want %v", path, got, want)
		}
	}
	if got, want := s.WithheldMessage("services.web.mem_limit"), stacks.SecretValueMessage("services.web.mem_limit"); got != want {
		t.Errorf("WithheldMessage(mem_limit) = %q, want %q", got, want)
	}
	if got, want := s.WithheldMessage(""), stacks.SecretValueMessage(""); got != want {
		t.Errorf("WithheldMessage(\"\") = %q, want %q", got, want)
	}
	none := stacks.NewSecretUse(copyOf("services:\n  web:\n    image: nginx:1.27-alpine\n    mem_limit: 1g\n", secretVar("PW", "Password2024!")))
	for _, path := range []string{"", "services.web.mem_limit", "services.web.no_such_key"} {
		if none.Withholds(path) {
			t.Errorf("Withholds(%q) = true in a file that references no secret, want false", path)
		}
	}
}

func TestSecretUseZeroValueKnowsNothing(t *testing.T) {
	t.Parallel()
	var s stacks.SecretUse
	if s.Any() || s.Uses("services.web.mem_limit") || s.Uses("") || s.Withholds("a") || s.Withholds("") {
		t.Error("the zero SecretUse answered true, want false everywhere")
	}
}

// "It comes from a secret variable" is said only where the check traced the field to one
// (07-REVIEW-gaps2 WR-02). Anywhere else the sentence speaks of the file.
func TestSecretUseSaysAValueComesFromASecretOnlyWhereItFindsOne(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	pw := secretVar("PW", "Zq9xkwmpr")
	t.Run("a file with a plain list and a secret elsewhere", func(t *testing.T) {
		t.Parallel()
		s := stacks.NewSecretUse(copyOf(head+"    mem_limit: ${PW}\n    ports: [{target: 80, published: 8080}]\n    environment:\n      DB: ${PW}\n", pw))
		if got, want := s.WithheldMessage("services.web.mem_limit"), stacks.SecretValueMessage("services.web.mem_limit"); got != want {
			t.Errorf("WithheldMessage(mem_limit) = %q, want %q", got, want)
		}
		const missing = "services.web.no_such_key"
		if !s.Withholds(missing) {
			t.Errorf("Withholds(%q) = false, want true: a path no key matches fails closed", missing)
		}
		if got, want := s.WithheldMessage(missing), stacks.SecretFileMessage(missing); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", missing, got, want)
		}
		if s.Withholds("services.web.ports.[].target") {
			t.Error("Withholds(ports.[].target) = true, want false: the list holds no secret")
		}
		if got, want := s.WithheldMessage(""), stacks.SecretValueMessage(""); got != want {
			t.Errorf("WithheldMessage(\"\") = %q, want %q", got, want)
		}
	})
	t.Run("a list item that uses a secret cannot be pinned", func(t *testing.T) {
		t.Parallel()
		s := stacks.NewSecretUse(copyOf(head+"    ports: [{target: \"${PW}\"}]\n", pw))
		const path = "services.web.ports.[].target"
		if !s.Withholds(path) {
			t.Errorf("Withholds(%q) = false, want true", path)
		}
		if got, want := s.WithheldMessage(path), stacks.SecretFileMessage(path); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", path, got, want)
		}
	})
	t.Run("a file that is not YAML", func(t *testing.T) {
		t.Parallel()
		s := stacks.NewSecretUse(copyOf("services: [", pw))
		const path = "services.web.mem_limit"
		if !s.Withholds(path) {
			t.Errorf("Withholds(%q) = false, want true", path)
		}
		if got, want := s.WithheldMessage(path), stacks.SecretFileMessage(path); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", path, got, want)
		}
	})
	t.Run("the file's sentence", func(t *testing.T) {
		t.Parallel()
		const want = "x.y: this value isn't valid. The file uses a secret variable, so the details aren't shown here, because they could quote it."
		if got := stacks.SecretFileMessage("x.y"); got != want {
			t.Errorf("SecretFileMessage(x.y) = %q, want %q", got, want)
		}
		if got := stacks.SecretFileMessage(""); got != stacks.SecretValueMessage("") {
			t.Errorf("SecretFileMessage(\"\") = %q, want the general sentence", got)
		}
	})
}

// The loader's index in a path is a position in compose-go's merged list: the documents of a file are
// concatenated, duplicate items are merged even in one document, and port ranges are expanded. So a list
// item is judged by its whole list across every document, and its sentence speaks of the file
// (07-45, D-02, 07-REVIEW-gaps3 CR-01, T-07-189).
func TestSecretUseJudgesAListItemByItsWholeList(t *testing.T) {
	t.Parallel()
	const head = "services:\n  web:\n    image: nginx:1.27-alpine\n"
	const pw = "Password2024!"
	const sizePath = "services.web.volumes.1.tmpfs.size"
	t.Run("two documents, a tmpfs size in the second", func(t *testing.T) {
		t.Parallel()
		s := stacks.NewSecretUse(copyOf(head+
			"    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n---\nservices:\n  web:\n    volumes:\n      - {type: tmpfs, target: /b, tmpfs: {size: \"${PW}\"}}\n      - {type: tmpfs, target: /c, tmpfs: {size: 1m}}\n",
			secretVar("PW", pw)))
		for _, path := range []string{sizePath, "services.web.volumes.0.tmpfs.size", "services.web.volumes.[1].tmpfs.size"} {
			if !s.Withholds(path) {
				t.Errorf("Withholds(%q) = false, want true: the list holds a secret-fed item", path)
			}
		}
		if got, want := s.WithheldMessage(sizePath), stacks.SecretFileMessage(sizePath); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", sizePath, got, want)
		}
		if s.Withholds("services.web.image") {
			t.Error("Withholds(services.web.image) = true, want false")
		}
	})
	t.Run("one document, the secret item repeats an earlier target", func(t *testing.T) {
		t.Parallel()
		const path = "services.web.volumes.0.tmpfs.size"
		s := stacks.NewSecretUse(copyOf(head+
			"    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n      - {type: tmpfs, target: /b, tmpfs: {size: 1m}}\n      - {type: tmpfs, target: /a, tmpfs: {size: \"${PW}\"}}\n",
			secretVar("PW", pw)))
		if !s.Withholds(path) {
			t.Errorf("Withholds(%q) = false, want true", path)
		}
		if got, want := s.WithheldMessage(path), stacks.SecretFileMessage(path); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", path, got, want)
		}
	})
	t.Run("two documents, a plain list while the secret is elsewhere", func(t *testing.T) {
		t.Parallel()
		s := stacks.NewSecretUse(copyOf(head+
			"    environment:\n      DB: ${PW}\n    volumes:\n      - {type: tmpfs, target: /a, tmpfs: {size: 1m}}\n---\nservices:\n  web:\n    volumes:\n      - {type: tmpfs, target: /b, tmpfs: {size: lots}}\n",
			secretVar("PW", pw)))
		if s.Withholds(sizePath) {
			t.Errorf("Withholds(%q) = true, want false: the list holds no secret", sizePath)
		}
	})
	t.Run("two documents set a field that is not in a list", func(t *testing.T) {
		t.Parallel()
		const path = "services.web.mem_limit"
		s := stacks.NewSecretUse(copyOf(head+"    mem_limit: 1m\n---\nservices:\n  web:\n    mem_limit: ${PW}\n", secretVar("PW", pw)))
		if !s.Withholds(path) {
			t.Errorf("Withholds(%q) = false, want true", path)
		}
		if got, want := s.WithheldMessage(path), stacks.SecretValueMessage(path); got != want {
			t.Errorf("WithheldMessage(%q) = %q, want %q", path, got, want)
		}
	})
}
