// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package gitconn

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/registry"
)

// Credential types: an HTTPS token or an SSH deploy key (Phase 9 D-01).
const (
	TypeHTTPS = "https"
	TypeSSH   = "ssh"
)

// Layers (D-02): an instance row applies to every environment, an environment row to its own
// environment only.
const (
	LayerInstance    = "instance"
	LayerEnvironment = "environment"
)

const (
	maxNameLen     = 64
	maxUsernameLen = 256
	// maxTokenLen is the longest HTTPS token accepted. Tokens are far shorter; the cap keeps the
	// sealed value and the Authorization header bounded.
	maxTokenLen = 4096
)

// The stable codes of a refused submission. The screens word each one.
const (
	CodeNameRequired       = "name_required"
	CodeNameInvalid        = "name_invalid"
	CodeTypeInvalid        = "type_invalid"
	CodeUsernameRequired   = "username_required"
	CodeUsernameInvalid    = "username_invalid"
	CodeSecretRequired     = "secret_required"
	CodeSecretInvalid      = "secret_invalid"
	CodeKeyPassphrase      = "key_passphrase"
	CodeKeyInvalid         = "key_invalid"
	CodeKeyTooLarge        = "key_too_large"
	CodeURLUnsupported     = "url_unsupported"
	CodeURLUserinfo        = "url_userinfo"
	CodeHostKeyInvalid     = "host_key_invalid"
	CodeHostKeyNotSSH      = "host_key_not_ssh"
	CodeCAInvalid          = "ca_invalid"
	CodeCATooLarge         = "ca_too_large"
	CodeCredentialScope    = "credential_scope"    //nolint:gosec // G101: a stable code, not a credential
	CodeCredentialReselect = "credential_reselect" //nolint:gosec // G101: a stable code, not a credential
)

// A name is shown as text everywhere, but markup never reaches a stored row all the same: letters,
// digits and a few separators, starting with a letter or a digit (as registry credentials).
var namePattern = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._()-]*$`)

// FieldError is one problem with a field of a submission. Code is stable: the SPA words it.
type FieldError struct {
	Field, Code string
}

// ValidationError is a submission that was refused. Nothing was stored.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Code
	}
	return "gitconn: invalid submission (" + strings.Join(parts, ", ") + ")"
}

func invalid(field, code string) *ValidationError {
	return &ValidationError{Fields: []FieldError{{Field: field, Code: code}}}
}

// validName checks a credential or repository name and returns it trimmed.
func validName(name string, bad func(field, code string)) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		bad("name", CodeNameRequired)
	case utf8.RuneCountInString(name) > maxNameLen || !namePattern.MatchString(name):
		bad("name", CodeNameInvalid)
	}
	return name
}

// CredentialInput is what a person types to add a credential.
type CredentialInput struct {
	// EnvID is nil for an instance credential.
	EnvID *string
	Name  string
	Type  string
	// Username is the user name the git server expects with an HTTPS token. For an SSH key it
	// defaults to git.
	Username string
	// Secret is the HTTPS token or the SSH private key. It is sealed and never logged.
	Secret string
}

// cleanCredential is a validated credential submission: the cleaned input and, for an SSH key,
// what is shown of it.
type cleanCredential struct {
	CredentialInput
	last4       string
	publicKey   string
	fingerprint string
}

// validCredential checks the fields every credential has, and the secret when there is one (an
// update that keeps the saved secret passes it empty). It returns the cleaned input.
func validCredential(in CredentialInput, wantSecret bool) (cleanCredential, []FieldError) {
	var errs []FieldError
	bad := func(field, code string) { errs = append(errs, FieldError{field, code}) }
	out := cleanCredential{CredentialInput: in}
	out.Name = validName(in.Name, bad)

	out.Username = strings.TrimSpace(in.Username)
	if utf8.RuneCountInString(out.Username) > maxUsernameLen || strings.IndexFunc(out.Username, unicode.IsControl) >= 0 {
		bad("username", CodeUsernameInvalid)
	}
	switch in.Type {
	case TypeHTTPS:
		if out.Username == "" {
			bad("username", CodeUsernameRequired)
		}
		// A token pasted with the newline it was copied with would never sign in.
		out.Secret = strings.TrimSpace(in.Secret)
		switch {
		case out.Secret == "":
			if wantSecret {
				bad("secret", CodeSecretRequired)
			}
		case len(out.Secret) > maxTokenLen || strings.IndexFunc(out.Secret, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0:
			bad("secret", CodeSecretInvalid)
		default:
			out.last4 = registry.Last4(out.Secret)
		}
	case TypeSSH:
		if out.Username == "" {
			out.Username = "git"
		}
		// The key keeps its own line breaks; only the text around it is trimmed, and the one
		// trailing newline a PEM block ends with is put back.
		out.Secret = strings.TrimSpace(in.Secret)
		if out.Secret == "" {
			if wantSecret {
				bad("secret", CodeSecretRequired)
			}
			break
		}
		key, err := ParsePrivateKey(out.Secret)
		switch {
		case errors.Is(err, ErrKeyTooLarge):
			bad("secret", CodeKeyTooLarge)
		case errors.Is(err, ErrKeyPassphrase):
			bad("secret", CodeKeyPassphrase)
		case err != nil:
			bad("secret", CodeKeyInvalid)
		default:
			out.Secret += "\n"
			out.publicKey, out.fingerprint = key.AuthorizedKey, key.Fingerprint
		}
	default:
		bad("type", CodeTypeInvalid)
		out.Secret = strings.TrimSpace(in.Secret)
	}
	return out, errs
}

// validRepo checks the address and the certificate authority of a repository.
func validRepo(name, url, caPEM string) (cleanName, cleanURL, cleanCA string, host string, errs []FieldError) {
	bad := func(field, code string) { errs = append(errs, FieldError{field, code}) }
	cleanName = validName(name, bad)
	u, err := gitx.ValidateURL(url)
	switch {
	case errors.Is(err, gitx.ErrURLUserinfo):
		bad("url", CodeURLUserinfo)
	case err != nil:
		bad("url", CodeURLUnsupported)
	default:
		cleanURL, host = u.Raw, u.Host
	}
	cleanCA = strings.TrimSpace(caPEM)
	switch err := gitx.ValidateCA(cleanCA); {
	case errors.Is(err, gitx.ErrCATooLarge):
		bad("caPem", CodeCATooLarge)
	case err != nil:
		bad("caPem", CodeCAInvalid)
	}
	return cleanName, cleanURL, cleanCA, host, errs
}

// CheckCredentialScope is the layer rule of D-02. A repository of an environment may use a
// credential of its own environment or of the instance; an instance repository only an instance
// credential; anything else is credential_scope. repoEnv is the repository's environment, empty
// for an instance repository.
func CheckCredentialScope(repoEnv string, c Credential) error {
	if c.EnvID == "" || (repoEnv != "" && c.EnvID == repoEnv) {
		return nil
	}
	return invalid("credentialId", CodeCredentialScope)
}

// maxHostKeyBytes is the longest host key line accepted. The longest real key, RSA 8192, is a
// little over 1.5 KiB in authorized_keys form.
const maxHostKeyBytes = 4096

// cleanHostKey is a validated SSH host key.
type cleanHostKey struct {
	// Line is the key in authorized_keys form, with no options, comment or newline.
	Line string
	// Fingerprint is its SHA256 fingerprint (SHA256:...).
	Fingerprint string
}

// validHostKey reads one SSH host key in authorized_keys form (`ssh-ed25519 AAAA...`, as `ssh-keyscan`
// prints after its host column) and returns it normalised. The empty string is "no key" and is fine.
// A key for an address that is not ssh is refused: it could never be used.
func validHostKey(line, rawURL string) (cleanHostKey, *FieldError) {
	line = strings.TrimSpace(line)
	if line == "" {
		return cleanHostKey{}, nil
	}
	if len(line) > maxHostKeyBytes || strings.ContainsAny(line, "\r\n") {
		return cleanHostKey{}, &FieldError{Field: "hostKey", Code: CodeHostKeyInvalid}
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(strings.TrimSpace(string(rest))) > 0 {
		return cleanHostKey{}, &FieldError{Field: "hostKey", Code: CodeHostKeyInvalid}
	}
	if u, err := gitx.ValidateURL(rawURL); err == nil && u.Scheme != "ssh" {
		return cleanHostKey{}, &FieldError{Field: "hostKey", Code: CodeHostKeyNotSSH}
	}
	return cleanHostKey{
		Line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
	}, nil
}
