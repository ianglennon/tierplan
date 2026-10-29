// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ianglennon/bayplan/internal/gitconn"
	"github.com/ianglennon/bayplan/internal/gitx"
	"github.com/ianglennon/bayplan/internal/stackops"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/vault"
)

// Backing up everything git (plan 09-32, PLAT-03, P3 D-28): the credentials, repositories, git stacks and webhooks
// come back on a server with another vault key, their secrets open under it, and what is derived (the state of the last
// check) is not carried, so every git stack is checked again.

const (
	gitHTTPSToken     = "glpat-RESTOREtoken0123456789WXYZ" //nolint:gosec // G101: a made-up token for the test
	gitWebhookSecret  = "whsec-restore-0123456789abcdef"   //nolint:gosec // G101: a made-up secret for the test
	gitWebhookRouting = "routing-token-for-the-restore-test"
)

func TestBackupRestoreGitState(t *testing.T) {
	f := newExportFixture(t)
	ctx := context.Background()
	conns := gitconn.NewStore(f.st, f.live, nil)
	repo := stackops.Repo{Store: f.st, Vault: f.live}

	// An HTTPS token on the instance, an SSH deploy key on the environment, and a repository for each layer.
	httpsCred, err := conns.CreateCredential(ctx, gitconn.CredentialInput{Name: "gitea-token", Type: gitconn.TypeHTTPS, Username: "bot", Secret: gitHTTPSToken}, "u1", nil)
	if err != nil {
		t.Fatalf("CreateCredential https: %v", err)
	}
	keyPEM, fingerprint := deployKeyPEM(t)
	env := "env-1"
	sshCred, err := conns.CreateCredential(ctx, gitconn.CredentialInput{EnvID: &env, Name: "deploy-key", Type: gitconn.TypeSSH, Secret: keyPEM}, "u1", nil)
	if err != nil {
		t.Fatalf("CreateCredential ssh: %v", err)
	}
	instanceRepo, err := conns.CreateRepo(ctx, gitconn.RepoInput{Name: "shared", URL: "https://git.example.org/acme/shared.git", CredentialID: &httpsCred.ID}, "u1", nil)
	if err != nil {
		t.Fatalf("CreateRepo instance: %v", err)
	}
	envRepo, err := conns.CreateRepo(ctx, gitconn.RepoInput{EnvID: &env, Name: "stacks", URL: "ssh://git@git.example.org/acme/stacks.git", CredentialID: &sshCred.ID}, "u1", nil)
	if err != nil {
		t.Fatalf("CreateRepo env: %v", err)
	}

	// A git stack on the environment's repository with a webhook and auto-deploy on, and the state of a check.
	st, _, err := repo.CreateGitStack(ctx, stackops.CreateGitStackInput{
		EnvID: env, Name: "web", Target: stacks.TargetStandalone, Actor: "alice",
		Copy: stacks.WorkingCopy{
			Files:        []stacks.File{{Path: "compose.yaml", Content: []byte("services: {}\n"), Origin: stacks.OriginRepo}},
			ComposeFiles: []string{"compose.yaml"},
		},
		Rev: stackops.GitRevision{
			Commit: "0123456789abcdef0123456789abcdef01234567", Tree: "89abcdef0123456789abcdef0123456789abcdef",
			Source: stackops.GitSource{RepoID: envRepo.ID, RepoName: "stacks", RefKind: "branch", Ref: "main", Folder: "stacks/web", ComposeFiles: []string{"compose.yaml"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateGitStack: %v", err)
	}
	routing := sha256.Sum256([]byte(gitWebhookRouting))
	tokenHash := hex.EncodeToString(routing[:])
	if err := repo.CreateGitWebhook(ctx, stackops.NewGitWebhook{
		StackID: st.ID, Flavour: "github", Placement: "path", TokenHash: tokenHash, Secret: gitWebhookSecret, Last4: "cdef", CreatedBy: "alice",
	}, nil); err != nil {
		t.Fatalf("CreateGitWebhook: %v", err)
	}
	if err := repo.SetAutoDeploy(ctx, st.ID, true, "", 0, nil); err != nil {
		t.Fatalf("SetAutoDeploy: %v", err)
	}
	checked := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	if err := repo.WriteGitState(ctx, stackops.GitState{
		StackID: st.ID, DeployedCommit: "0123456789abcdef0123456789abcdef01234567", HeadCommit: "fedcba9876543210fedcba9876543210fedcba98",
		Behind: 2, Status: stackops.GitStatusBehind, CheckedAt: &checked,
	}); err != nil {
		t.Fatalf("WriteGitState: %v", err)
	}
	before, err := repo.StackGit(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}

	_, file, err := f.export(t, exportPass, testOptions)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// The sealed secrets are in the file only as ciphertext, and the derived state is not in it at all.
	o := openBackup(t, file, exportPass)
	if o.count(t, "git_credentials") != 2 || o.count(t, "git_repos") != 2 || o.count(t, "stack_git") != 1 {
		t.Fatalf("the backup holds %d credentials, %d repositories and %d git stacks, want 2, 2 and 1",
			o.count(t, "git_credentials"), o.count(t, "git_repos"), o.count(t, "stack_git"))
	}
	if n := o.count(t, "stack_git_state"); n != 0 {
		t.Fatalf("the backup holds %d stack_git_state rows, want none (derived state)", n)
	}

	dataDir := t.TempDir()
	target := targetVault(t)
	if _, err := restoreInto(t, file, dataDir, target); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := store.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restored.Close() }()
	rconns := gitconn.NewStore(restored, target, nil)
	rrepo := stackops.Repo{Store: restored, Vault: target}

	// The HTTPS token and the deploy key open under the new key and are the ones that were saved.
	rr, err := rconns.GetRepo(ctx, instanceRepo.ID)
	if err != nil || rr.Name != "shared" || rr.CredentialID != httpsCred.ID {
		t.Fatalf("instance repository after the restore = %+v, %v", rr, err)
	}
	rem, err := rconns.Remote(ctx, rr)
	if err != nil {
		t.Fatalf("Remote of the instance repository under the new key: %v", err)
	}
	if auth, ok := rem.Auth.(gitx.TokenAuth); !ok || auth.Token != gitHTTPSToken || auth.Username != "bot" {
		t.Errorf("HTTPS credential after the restore does not hold the token that was saved")
	}
	rr, err = rconns.GetRepo(ctx, envRepo.ID)
	if err != nil || rr.EnvID != env || rr.CredentialID != sshCred.ID {
		t.Fatalf("environment repository after the restore = %+v, %v", rr, err)
	}
	rem, err = rconns.Remote(ctx, rr)
	if err != nil {
		t.Fatalf("Remote of the environment repository under the new key: %v", err)
	}
	if auth, ok := rem.Auth.(gitx.SSHKeyAuth); !ok || ssh.FingerprintSHA256(auth.Signer.PublicKey()) != fingerprint {
		t.Errorf("SSH credential after the restore does not sign with the key that was saved")
	}
	// The secrets were re-wrapped, not copied: the old key no longer opens them.
	var sealedToken, sealedHook string
	if err := restored.Read.QueryRow(`SELECT secret_sealed FROM git_credentials WHERE id = ?`, httpsCred.ID).Scan(&sealedToken); err != nil {
		t.Fatal(err)
	}
	if err := restored.Read.QueryRow(`SELECT webhook_secret_sealed FROM stack_git WHERE stack_id = ?`, st.ID).Scan(&sealedHook); err != nil {
		t.Fatal(err)
	}
	if _, err := f.live.Open(vault.ContextGitCred+httpsCred.ID, sealedToken); err == nil {
		t.Error("the token still opens under the source key")
	}
	if _, err := f.live.Open(vault.ContextGitWebhook+st.ID, sealedHook); err == nil {
		t.Error("the webhook secret still opens under the source key")
	}

	// The webhook secret opens under the new key, found by the same routing token.
	hook, err := rrepo.GitWebhookByToken(ctx, tokenHash)
	if err != nil || hook.StackID != st.ID || string(hook.Secret) != gitWebhookSecret || hook.Flavour != "github" || hook.Placement != "path" {
		t.Fatalf("webhook after the restore = %+v, %v; want the stack's, with its secret", hook, err)
	}

	// The stack's settings are back, auto-deploy included.
	after, err := rrepo.StackGit(ctx, st.ID)
	if err != nil {
		t.Fatalf("StackGit after the restore: %v", err)
	}
	if after.RepoID != before.RepoID || after.RefKind != before.RefKind || after.Ref != before.Ref || after.Folder != before.Folder ||
		len(after.ComposeFiles) != 1 || after.ComposeFiles[0] != "compose.yaml" || after.PollEnabled != before.PollEnabled ||
		after.PollIntervalSeconds != before.PollIntervalSeconds || !after.AutoDeploy || after.WebhookTokenHash != tokenHash ||
		after.WebhookSecretLast4 != "cdef" {
		t.Errorf("git settings after the restore = %+v, before = %+v", after, before)
	}

	// The state of the last check is not carried: it is empty and the stack reads unknown until it is checked again.
	var states int
	if err := restored.Read.QueryRow(`SELECT COUNT(*) FROM stack_git_state`).Scan(&states); err != nil || states != 0 {
		t.Fatalf("stack_git_state rows after the restore = %d, %v, want none", states, err)
	}
	state, err := rrepo.GitState(ctx, st.ID)
	if err != nil || state.Status != stackops.GitStatusUnknown || state.CheckedAt != nil {
		t.Errorf("git state after the restore = %+v, %v; want unknown and never checked", state, err)
	}
}

// deployKeyPEM is a made-up SSH deploy key in PEM and its SHA256 fingerprint.
func deployKeyPEM(t *testing.T) (pemText, fingerprint string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), ssh.FingerprintSHA256(sshPub)
}
