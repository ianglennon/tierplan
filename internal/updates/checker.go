// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/ianglennon/tierplan/internal/registry"
)

// Heads is what the Checker asks a registry. It is a port so a test answers for the registry and the
// per-platform judgement and the fleet dedupe can be added behind it.
type Heads interface {
	// Digest returns the digest ref's tag points at, read with a manifest HEAD under the credential the
	// environment resolves for it, and the ID of that credential ("" for an anonymous call). The
	// credential ID is returned with an error too whenever it is known, so a failure is stored under the
	// credential that met it.
	Digest(ctx context.Context, envID string, ref registry.Ref) (digest, credID string, err error)
	// Index reads what a digest names with a GET by digest, which a registry may count as a pull:
	// whether it is a multi-platform index and the manifest digest of each platform it lists (platform
	// key linux/amd64 to digest). The Checker asks only when a tag's digest changed.
	Index(ctx context.Context, envID string, ref registry.Ref, digest string) (index bool, platforms map[string]string, err error)
}

// registryHeads is Heads over the registry client, dialing through the same egress policy as every other
// registry call (T-12-02).
type registryHeads struct {
	Resolver *registry.Resolver
	Clients  *registry.ClientCache
}

// NewHeads returns the Heads that asks real registries with the stored credentials of resolver.
func NewHeads(resolver *registry.Resolver, clients *registry.ClientCache) Heads {
	return registryHeads{Resolver: resolver, Clients: clients}
}

// client chooses the registry client for ref under the credential the environment resolves for it, and
// that credential's ID ("" for an anonymous call). The ID is returned with an error whenever it is known.
func (h registryHeads) client(ctx context.Context, envID string, ref registry.Ref) (*registry.Client, string, error) {
	cred, err := h.Resolver.ForBrowse(ctx, envID, nil, ref)
	if err != nil {
		id, _ := h.Resolver.CredentialFor(ctx, envID, ref) // the identity alone; opens no secret
		return nil, id, err
	}
	if cred == nil {
		return h.Clients.Anonymous(ref.Domain), "", nil
	}
	return h.Clients.ForCredential(cred.ID, cred.UpdatedAt, cred.HostOptions), cred.ID, nil
}

// Digest implements Heads. A tag that is not a tag as the reference grammar writes one is refused as
// not found before any request: a name that is not must never be put in a registry URL.
func (h registryHeads) Digest(ctx context.Context, envID string, ref registry.Ref) (string, string, error) {
	if !registry.ValidTag(ref.Tag) {
		return "", "", errNoSuchTag
	}
	c, credID, err := h.client(ctx, envID, ref)
	if err != nil {
		return "", credID, err
	}
	d, err := c.ManifestDigest(ctx, ref.Path, ref.Tag)
	return d, credID, err
}

// Index implements Heads: one read of the manifest the digest names, by digest. It is the only request
// of the check that is a pull, so the Checker makes it only when the tag's digest changed.
func (h registryHeads) Index(ctx context.Context, envID string, ref registry.Ref, digest string) (bool, map[string]string, error) {
	c, _, err := h.client(ctx, envID, ref)
	if err != nil {
		return false, nil, err
	}
	index, ps, err := c.IndexPlatforms(ctx, ref.Path, digest)
	if err != nil || !index {
		return false, nil, err
	}
	platforms := make(map[string]string, len(ps))
	for _, p := range ps {
		platforms[registry.PlatformKey(p)] = p.Digest
	}
	return true, platforms, nil
}

// errNoSuchTag is the error of a tag no request was made for. It is stored as not found.
var errNoSuchTag = errors.New("updates: not a tag")

// reasonFor words an error of a registry call as the Reason a row stores: from the error's type, never
// from the registry's text.
func reasonFor(err error) Reason {
	switch {
	case errors.Is(err, errNoSuchTag):
		return ReasonNotFound
	case errors.Is(err, registry.ErrNoDigest):
		return ReasonUnsupported
	case errors.Is(err, errBreakerOpen):
		return ReasonRateLimited
	}
	return ReasonOf(registry.Classify(err))
}

// CredentialHeads is the part of Heads that names, without asking the registry, the credential a check
// would be made with. A Heads that has it lets the checker share one request between environments that
// resolve the same credential (D-19); one that has not is asked once for each environment.
type CredentialHeads interface {
	CredentialID(ctx context.Context, envID string, ref registry.Ref) (string, error)
}

// CredentialID implements CredentialHeads: the ID of the credential Digest would sign in with, "" for an
// anonymous call. It opens no secret.
func (h registryHeads) CredentialID(ctx context.Context, envID string, ref registry.Ref) (string, error) {
	return h.Resolver.CredentialFor(ctx, envID, ref)
}

// CredentialLog is where the checker records that a stored credential keeps failing, and that it works
// again. *registry.Resolver is one, and the Registries page shows what it keeps.
type CredentialLog interface {
	RecordFailure(ctx context.Context, id, code string) error
	ClearFailure(ctx context.Context, id string) error
}

// MinRecheck is how long a key that was checked successfully is left alone by a check that was not
// asked for by a person. A person's Check now is never held back by it.
const MinRecheck = 10 * time.Minute

// Checker asks the registries about the images of the environments and keeps what they answer.
type Checker struct {
	Heads Heads
	Store Store
	// Runs, when set, is given the platforms of an index the check is about to replace, which is the
	// index the workloads that have not been updated yet still run.
	Runs *RunIndex
	// Creds, when set, is told which credentials a run found refused and which worked.
	Creds CredentialLog
	Now   func() time.Time
	Log   *slog.Logger

	once    sync.Once
	pace    *hostPacer
	flights singleflight.Group
}

// CheckResult counts what one check of an environment found.
type CheckResult struct {
	// Checked is how many distinct tags were asked about, Updates how many workloads run another
	// digest than the one their tag points at now, Failed how many tags could not be read.
	Checked, Updates, Failed int
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// pacer is the one pacer of this checker, made on first use so it reads the checker's clock.
func (c *Checker) pacer() *hostPacer {
	c.once.Do(func() { c.pace = newHostPacer(c.now) })
	return c.pace
}

// CheckEnv checks one environment as a person asked for it: CheckFleet over that environment alone,
// forced.
func (c *Checker) CheckEnv(ctx context.Context, envID string, ws []Workload) (CheckResult, error) {
	res, err := c.CheckFleet(ctx, []string{envID}, map[string][]Workload{envID: ws}, true)
	return res[envID], err
}

// unit is one thing to ask a registry: a key, and the environment whose workload it is asked for.
type unit struct {
	// id names the unit across the run and across runs in flight: the key, with the environment in front
	// when the credential could not be named before asking.
	id  string
	key Key
	env string
	ref registry.Ref
	// resolved is true when key.CredID is the credential the check will be made with.
	resolved bool
}

// outcome is what asking one unit came to.
type outcome struct {
	chk *Check
	// asked is true when a request reached the registry; ok that it answered with a digest. credID and
	// code say, for a failure, whom the registry refused and in what words of internal/registry.
	asked, ok bool
	credID    string
	code      string
	// skipped is true when the stored check was recent enough that nobody was asked.
	skipped bool
}

func unitID(env string, k Key) string {
	return strings.Join([]string{env, k.Host, k.Repo, k.Tag, k.CredID}, "\x00")
}

// unitFor names what asking about ref in env is: the key of its check, which holds the credential env
// resolves for it.
func (c *Checker) unitFor(ctx context.Context, env string, ref registry.Ref) (*unit, error) {
	u := &unit{key: Key{Host: ref.Domain, Repo: ref.Path, Tag: ref.Tag}, env: env, ref: ref}
	ch, named := c.Heads.(CredentialHeads)
	switch {
	case !registry.ValidTag(ref.Tag):
		u.resolved = true // no request is made, so there is no credential to share
	case named:
		id, err := ch.CredentialID(ctx, env, ref)
		if err != nil {
			return nil, fmt.Errorf("resolve the credential for %s: %w", ref.Repo(), err)
		}
		u.key.CredID, u.resolved = id, true
	}
	if u.resolved {
		u.id = unitID("", u.key)
	} else {
		u.id = unitID(env, u.key)
	}
	return u, nil
}

// CheckFleet asks the registries for the digest of the tag of each workload of the environments, once for
// each distinct registry, repository, tag and credential across all of them (D-01, D-19): environments that
// resolve the same credential, or none, share one request and one stored row, and an environment's own
// credential never answers for another's. Each registry host is asked politely (hostPacer), and a key it
// cannot be asked about while its breaker is open is stored as rate limited without a request.
//
// A tag that cannot be read is stored as a failure with its Reason and keeps the earlier answer, so the
// page never reads a failed check as up to date. A key checked successfully less than MinRecheck ago is
// left alone unless force; a forced check is also satisfied by any check of the key completed since the
// call began, so a Check now that meets a scheduled run joins it instead of asking again. A cancelled
// context stops the check and stores nothing for the tag it cut short.
//
// The result holds one CheckResult for each environment asked about.
func (c *Checker) CheckFleet(ctx context.Context, envIDs []string, workloads map[string][]Workload, force bool) (map[string]CheckResult, error) {
	began := c.now()
	type entry struct {
		env string
		w   Workload
		uid string
	}
	var entries []entry
	byID := map[string]*unit{}
	var order []*unit
	for _, env := range envIDs {
		for _, w := range workloads[env] {
			if !w.Checkable() {
				continue
			}
			u, err := c.unitFor(ctx, env, w.Ref)
			if err != nil {
				return tally(envIDs), err
			}
			if have, ok := byID[u.id]; ok {
				u = have
			} else {
				byID[u.id] = u
				order = append(order, u)
			}
			entries = append(entries, entry{env: env, w: w, uid: u.id})
		}
	}

	var mu sync.Mutex
	outs := make(map[string]*outcome, len(order))
	g, gctx := errgroup.WithContext(ctx)
	for _, u := range order {
		g.Go(func() error {
			o, err := c.ask(gctx, u, force, began)
			if err != nil {
				return err
			}
			mu.Lock()
			outs[u.id] = o
			mu.Unlock()
			return nil
		})
	}
	err := g.Wait()

	results := tally(envIDs)
	type tagKey struct{ env, host, repo, tag string }
	seen := map[tagKey]bool{}
	for _, e := range entries {
		o := outs[e.uid]
		if o == nil {
			continue
		}
		res := results[e.env]
		if tk := (tagKey{e.env, e.w.Ref.Domain, e.w.Ref.Path, e.w.Ref.Tag}); !seen[tk] {
			seen[tk] = true
			res.Checked++
			if o.chk.Error != "" {
				res.Failed++
			}
		}
		if o.chk.Error == "" && o.chk.Digest != "" && e.w.RunningDigest != "" && o.chk.Digest != e.w.RunningDigest {
			res.Updates++
		}
		results[e.env] = res
	}
	if err != nil {
		return results, err
	}
	c.recordCredentials(ctx, outs)
	return results, nil
}

// tally is an empty result for each environment.
func tally(envIDs []string) map[string]CheckResult {
	out := make(map[string]CheckResult, len(envIDs))
	for _, env := range envIDs {
		out[env] = CheckResult{}
	}
	return out
}

// recordCredentials tells the Registries page, once for each credential, what this run found: a credential
// the registry refused is recorded as failing, and one that was signed in with and answered is cleared.
// A credential that met both is failing. It is best effort: a record that cannot be written is logged, and
// the checks it came from stand.
func (c *Checker) recordCredentials(ctx context.Context, outs map[string]*outcome) {
	if c.Creds == nil {
		return
	}
	refused := map[string]string{}
	worked := map[string]bool{}
	for _, o := range outs {
		if !o.asked || o.credID == "" {
			continue
		}
		switch {
		case o.ok:
			worked[o.credID] = true
		case o.code == registry.CodeSignInRefused || o.code == registry.CodeNoPullAccess:
			refused[o.credID] = o.code
		}
	}
	for _, id := range slices.Sorted(maps.Keys(refused)) {
		if err := c.Creds.RecordFailure(ctx, id, refused[id]); err != nil {
			c.log().Warn("could not record a failing registry credential", "credential", id, "err", err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(worked)) {
		if _, bad := refused[id]; bad {
			continue
		}
		if err := c.Creds.ClearFailure(ctx, id); err != nil {
			c.log().Warn("could not clear a registry credential's failure", "credential", id, "err", err)
		}
	}
}

// ask checks one unit, joining a check of the same key already in flight, from this run or another: the
// answer of the one that asked is the answer of all of them, and it is stored once. The check in flight
// runs under the context of the call that began it.
func (c *Checker) ask(ctx context.Context, u *unit, force bool, began time.Time) (*outcome, error) {
	var o *outcome
	for range 2 {
		v, err, _ := c.flights.Do(u.id, func() (any, error) { return c.checkKey(ctx, u, force, began) })
		if err != nil {
			return nil, err
		}
		o = v.(*outcome) //nolint:forcetypeassert // checkKey returns nothing else
		// A forced check that joined one which left the key alone as recent enough was not given what it
		// asked for: it asks once more, as the one that begins.
		if !force || !o.skipped || c.fresh(o.chk, true, began) {
			break
		}
	}
	return o, nil
}

// fresh reports whether the stored check needs no new request.
func (c *Checker) fresh(prev *Check, force bool, began time.Time) bool {
	if prev == nil || prev.Error != "" || prev.SuccessAt.IsZero() {
		return false
	}
	if force {
		return !prev.CheckedAt.Before(began)
	}
	return c.now().Sub(prev.SuccessAt) < MinRecheck
}

// checkKey asks about one tag and stores the answer. The outcome's check is the one stored.
func (c *Checker) checkKey(ctx context.Context, u *unit, force bool, began time.Time) (*outcome, error) {
	key, ref := u.key, u.ref
	var prev *Check
	if u.resolved {
		var err error
		if prev, err = c.stored(ctx, key); err != nil {
			return nil, err
		}
		if c.fresh(prev, force, began) {
			return &outcome{chk: prev, skipped: true}, nil
		}
	}
	var (
		digest string
		asked  bool
		err    error
	)
	if !registry.ValidTag(ref.Tag) {
		err = errNoSuchTag
	} else {
		err = c.pacer().Do(ctx, key.Host, func(ctx context.Context) error {
			asked = true
			d, cred, derr := c.Heads.Digest(ctx, u.env, ref)
			digest = d
			if !u.resolved {
				key.CredID = cred
			}
			return derr
		})
	}
	if err == nil && digest == "" {
		err = registry.ErrNoDigest
	}
	var index bool
	var platforms map[string]string
	if err == nil {
		if !u.resolved {
			if prev, err = c.stored(ctx, key); err != nil {
				return nil, err
			}
		}
		index, platforms, err = c.platformsOf(ctx, u, digest, prev)
	}
	at := c.now()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		reason := reasonFor(err)
		if errors.Is(err, errBreakerOpen) {
			c.log().Debug("an image check waits for the registry's limit to pass", "host", key.Host, "repo", key.Repo, "tag", key.Tag)
		} else {
			c.log().Info("an image check failed", "host", key.Host, "repo", key.Repo, "tag", key.Tag, "reason", string(reason))
		}
		if serr := c.Store.SaveFailure(ctx, key, at, reason); serr != nil {
			return nil, fmt.Errorf("store the failed check: %w", serr)
		}
		return &outcome{
			chk: &Check{Key: key, CheckedAt: at, Error: reason}, asked: asked && !errors.Is(err, errNoSuchTag),
			credID: key.CredID, code: registry.Classify(err),
		}, nil
	}
	chk := Check{Key: key, Digest: digest, Index: index, Platforms: platforms, CheckedAt: at, SuccessAt: at}
	if serr := c.Store.SaveSuccess(ctx, chk); serr != nil {
		return nil, fmt.Errorf("store the check: %w", serr)
	}
	if prev != nil && prev.Index && prev.Digest != digest {
		// The index the tag leaves is the one workloads that are not updated yet still run.
		c.Runs.Put(ref, prev.Digest, prev.Platforms)
	}
	return &outcome{chk: &chk, asked: true, ok: true, credID: key.CredID}, nil
}

// stored returns the check stored for key, nil when there is none.
func (c *Checker) stored(ctx context.Context, key Key) (*Check, error) {
	got, err := c.Store.Checks(ctx, []Key{key})
	if err != nil {
		return nil, fmt.Errorf("read the stored check: %w", err)
	}
	if p, ok := got[key]; ok {
		return &p, nil
	}
	return nil, nil //nolint:nilnil // no stored check is an answer
}

// platformsOf says what digest names. The HEAD that found it costs no pull, so the read of the index
// (one GET by digest) is made only when the digest is not the one already stored, or the stored index
// has no platforms; otherwise the stored answer stands (D-02, RESEARCH Pitfall 2). An index that lists
// no runnable platform is read as one image: there is nothing to compare platform by platform.
func (c *Checker) platformsOf(ctx context.Context, u *unit, digest string, prev *Check) (bool, map[string]string, error) {
	if prev != nil && prev.Digest == digest && !prev.SuccessAt.IsZero() && (!prev.Index || len(prev.Platforms) > 0) {
		return prev.Index, prev.Platforms, nil
	}
	var (
		index     bool
		platforms map[string]string
	)
	err := c.pacer().Do(ctx, u.key.Host, func(ctx context.Context) error {
		var err error
		index, platforms, err = c.Heads.Index(ctx, u.env, u.ref, digest)
		return err
	})
	if err != nil {
		return false, nil, err
	}
	if !index || len(platforms) == 0 {
		return false, nil, nil
	}
	return true, platforms, nil
}
