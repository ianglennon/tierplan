// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/audit"
	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/envs"
	"github.com/ianglennon/coaming/internal/httpx"
	"github.com/ianglennon/coaming/internal/rbac"
	"github.com/ianglennon/coaming/internal/registry"
	"github.com/ianglennon/coaming/internal/stacks"
	"github.com/ianglennon/coaming/internal/store"
)

const (
	secretTargetType = "secret"

	problemNameInvalid    = "name_invalid"
	problemValueRequired  = "value_required"
	problemValueInvalid   = "value_invalid"
	problemValueTooLarge  = "value_too_large"
	problemLabelsInvalid  = "labels_invalid"
	problemObjectInUse    = "object_in_use"
	problemSecretNotFound = "secret_not_found"
	problemCreateFailed   = "create_failed"

	// maxSecretValue is the most bytes of value a secret may have: the swarm takes 511,999 and
	// refuses 512,000 (RESEARCH E18, D-40).
	maxSecretValue = stacks.MaxSecretBytes
	// maxSecretBody is the most bytes a create request may have: the base64 of a value of
	// maxSecretValue bytes is about 683 KB.
	maxSecretBody = 1 << 20
	// maxNamedServices is how many services an in-use refusal names before "and n more".
	maxNamedServices = 3
	// maxLabelKey and maxLabelValue bound a label, as the agent does.
	maxLabelKey   = 256
	maxLabelValue = 4096

	// sourceAPI is the source of a create that did not say where it came from.
	sourceAPI = "api"
)

// secretNameRe is a swarm secret name: letters and numbers, with dot, dash and underscore
// between them, at most 64 characters. The agent checks the same shape.
var secretNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_.-]{0,62}[A-Za-z0-9])?$`)

// SecretsDeps are the dependencies of the swarm secret routes.
type SecretsDeps struct {
	// Aggregator reaches the manager a write goes to: WriteOnManager.
	Aggregator *envs.Aggregator
	// Clusters is the cluster reader the lists and the in-use check are served from.
	Clusters *envs.Clusters
	// Resolver chooses the registry credential a rotate's service updates are sent with.
	Resolver *registry.Resolver
	Audit    *audit.Writer
	// Checker decides the second permission a rotate needs (services.control) and who may have host access.
	Checker *rbac.Checker
	// Store holds the stacks a stack label is confirmed against (T-07-146). Nil links no stack.
	Store *store.Store
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
}

// services is the part of the dependencies the shared swarm problem mapping and a rotate read.
func (d SecretsDeps) services() ServicesDeps {
	return ServicesDeps{Aggregator: d.Aggregator, Clusters: d.Clusters, Resolver: d.Resolver, Audit: d.Audit, Checker: d.Checker, Store: d.Store, Log: d.Log}
}

// SecretItem is one swarm secret as the list shows it. There is no value: the swarm never
// returns one, and Coaming keeps none.
type SecretItem struct {
	ID             string            `json:"id" doc:"The secret ID"`
	Name           string            `json:"name" doc:"The secret name"`
	Labels         map[string]string `json:"labels" doc:"Every label of the secret"`
	CreatedAt      string            `json:"createdAt" format:"date-time" doc:"When the secret was created"`
	UpdatedAt      string            `json:"updatedAt" format:"date-time" doc:"When the secret was last updated"`
	UsedBy         int               `json:"usedBy" doc:"How many services' specs reference the secret, whatever their tasks are doing, a service scaled to 0 included"`
	Owner          string            `json:"owner" doc:"The stack (com.docker.stack.namespace) label value; empty when the secret has none"`
	OwnerKind      string            `json:"ownerKind" enum:"stack," doc:"stack, or empty when the secret has no owner"`
	CoamingStackID string            `json:"coamingStackId" doc:"The ID of the Coaming stack the secret belongs to: set only when this environment holds a stack with that ID under the owner name above (coaming.stack.id). Empty for a stack Coaming does not manage"`
}

// SecretListBody is the answer of the secrets list.
type SecretListBody struct {
	Items           []SecretItem `json:"items" doc:"At most 500 rows that match the search, sorted by name then ID"`
	Total           int          `json:"total" doc:"How many secrets match the search, whatever the cap"`
	UnfilteredTotal int          `json:"unfilteredTotal" doc:"How many secrets there are before the search"`
	Capped          bool         `json:"capped" doc:"True when total is more than the rows returned"`
}

// SecretListInput is the path of the secrets list with its search.
type SecretListInput struct {
	EnvironmentPathInput
	Q string `query:"q" doc:"Search: a case-insensitive substring of the secret name, its owner or any label key or value, at most 128 characters (else 422 query_too_long)"`
}

// SecretListOutput wraps SecretListBody for huma.
type SecretListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SecretListBody
}

// SecretUserBody is one service whose spec references a secret, with how it is mounted.
type SecretUserBody struct {
	ServiceID string `json:"serviceId" doc:"The service ID"`
	Service   string `json:"service" doc:"The service name"`
	Stack     string `json:"stack" doc:"The stack the service belongs to; empty for a service made on its own"`
	File      string `json:"file" doc:"The file name the secret appears as in the container"`
	UID       string `json:"uid" doc:"The owner of the file"`
	GID       string `json:"gid" doc:"The group of the file"`
	Mode      string `json:"mode" doc:"The file mode in octal, for example 0444"`
}

// SecretDetailBody is one secret with the services that use it.
type SecretDetailBody struct {
	ID             string            `json:"id" doc:"The secret ID"`
	Name           string            `json:"name" doc:"The secret name"`
	Labels         map[string]string `json:"labels" doc:"Every label of the secret"`
	CreatedAt      string            `json:"createdAt" format:"date-time" doc:"When the secret was created"`
	UpdatedAt      string            `json:"updatedAt" format:"date-time" doc:"When the secret was last updated"`
	UsedBy         int               `json:"usedBy" doc:"How many services' specs reference the secret"`
	Owner          string            `json:"owner" doc:"The stack (com.docker.stack.namespace) label value; empty when the secret has none"`
	OwnerKind      string            `json:"ownerKind" enum:"stack," doc:"stack, or empty when the secret has no owner"`
	CoamingStackID string            `json:"coamingStackId" doc:"The ID of the Coaming stack the secret belongs to: set only when this environment holds a stack with that ID under the owner name above (coaming.stack.id). Empty for a stack Coaming does not manage"`
	UsedByRows     []SecretUserBody  `json:"usedByRows" doc:"Every service whose spec references the secret, sorted by service name then ID"`
}

// SecretDetailOutput wraps SecretDetailBody for huma.
type SecretDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SecretDetailBody
}

// SecretPathInput is the path of one secret.
type SecretPathInput struct {
	EnvironmentPathInput
	SecretID string `path:"secretId" maxLength:"128" doc:"The secret ID or name: the id of a row of the secrets list"`
}

// SecretInspectBody is the raw inspect document of one secret.
type SecretInspectBody struct {
	Inspect map[string]any `json:"inspect" doc:"The manager's inspect document: the ID, the version, the times and the spec's name and labels. It has no data field, and none is ever added"`
}

// SecretInspectOutput wraps SecretInspectBody for huma.
type SecretInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SecretInspectBody
}

// SecretCreateRequest is the body of POST /api/env/{envId}/secrets.
type SecretCreateRequest struct {
	Name   string            `json:"name" doc:"The secret name: letters, numbers, dot, dash and underscore, starting and ending with a letter or number, at most 64 characters (else 422 name_invalid)"`
	Labels map[string]string `json:"labels,omitempty" maxProperties:"64" doc:"Labels, at most 64"`
	Data   string            `json:"data,omitempty" doc:"The value, as standard base64. 1 to 511,999 bytes once decoded: empty is 422 value_required, 512,000 or more is 422 value_too_large, and text that is not base64 is 422 value_invalid. It is sent to the swarm once and never kept, logged, audited or returned"`
	Source string            `json:"source,omitempty" enum:"typed,file,rotate,apply" doc:"Where the value came from, for the audit log only: typed or file (the create sheet), rotate (a new version of a secret) or apply (a secret staged with a service). Left out, the audit log says api"`
}

// SecretCreateInput is the path and body of the create route.
type SecretCreateInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      SecretCreateRequest
}

// SecretCreateBody is the answer to a create: no value, whatever was sent.
type SecretCreateBody struct {
	ID   string `json:"id" doc:"The ID of the secret the swarm made"`
	Name string `json:"name" doc:"The secret name"`
}

// SecretCreateOutput wraps SecretCreateBody for huma.
type SecretCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         SecretCreateBody
}

// SecretDeleteInput is the path and query of the delete route.
type SecretDeleteInput struct {
	SecretPathInput
	Confirm string `query:"confirm" maxLength:"128" doc:"The secret's name, typed (else 422 confirmation_required and nothing is sent)"`
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RegisterSecrets mounts the swarm secret routes.
func RegisterSecrets(r *httpx.Router, d SecretsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listSecrets", Method: http.MethodGet, Path: "/api/env/{envId}/secrets",
		Summary: "List the swarm secrets of an environment",
		Description: "The secrets of a swarm environment with their labels, times, owner and how many services use each, at most 500 rows sorted by name then ID, read through a manager. " +
			"No row has a value: the swarm never returns one. Optionally a search (q) that runs before the 500-row cap. " +
			"409 not_a_swarm when the environment is a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs secrets.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermSecretsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretListInput) (*SecretListOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := listSecrets(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &SecretListOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getSecret", Method: http.MethodGet, Path: "/api/env/{envId}/secrets/{secretId}",
		Summary: "One swarm secret and the services that use it",
		Description: "The secret's metadata and every service whose spec references it, with the file name, owner, group and mode each mounts it with. " +
			"The secret is named by its ID or its name. There is no value, ever. 404 secret_not_found; 409 not_a_swarm. " +
			"Needs secrets.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermSecretsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretPathInput) (*SecretDetailOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getSecret(ctx, d, env.ID, in.SecretID)
			if err != nil {
				return nil, err
			}
			return &SecretDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectSecret", Method: http.MethodGet, Path: "/api/env/{envId}/secrets/{secretId}/inspect",
		Summary: "One swarm secret's raw inspect document",
		Description: "The manager's inspect document of the secret, for the Inspect tab. It carries no data field, and a data field is removed if one ever appears. " +
			"404 secret_not_found; 409 not_a_swarm. Needs secrets.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermSecretsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretPathInput) (*SecretInspectOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getSecretInspect(ctx, d, env.ID, in.SecretID)
			if err != nil {
				return nil, err
			}
			return &SecretInspectOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "createSecret", Method: http.MethodPost, Path: "/api/env/{envId}/secrets",
		Summary: "Create a swarm secret",
		Description: "Creates a secret once, through a manager, and never retries. The value goes to the swarm's encrypted store and nowhere else: Coaming does not return, log, audit, cache or store it, and the answer holds the new secret's ID and name only. " +
			"A name that is not 1 to 64 letters, numbers, dots, dashes and underscores starting and ending with a letter or number is 422 name_invalid; a value that is missing or empty is 422 value_required; one of 512,000 bytes or more is 422 value_too_large (a value of 511,999 bytes is accepted); more than 64 labels is 422, and a label key over 256 or value over 4096 characters is 422 labels_invalid. All are checked before anything is sent. " +
			"A request body over 1 MiB is refused. 409 name_taken when the swarm has the name already, 409 not_a_swarm for a standalone host, 409 host_read_only on a read-only host. " +
			"Needs secrets.manage (Admin) on the environment. Every attempt is audited as secret.created with the name, the number of labels, the size in bytes and where the request came from, never the value; a refusal is a failure row with its code.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxSecretBody,
	}, rbac.Requirement{Perm: rbac.PermSecretsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretCreateInput) (*SecretCreateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := createSecret(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &SecretCreateOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteSecret", Method: http.MethodDelete, Path: "/api/env/{envId}/secrets/{secretId}",
		Summary: "Delete a swarm secret",
		Description: "Deletes one secret, once and never retried. The secret's name must be typed in confirm, else 422 confirmation_required and nothing is sent. " +
			"A secret that any service's spec references is 409 object_in_use, naming up to three of the services, read live just before the delete and counting a service scaled to 0. " +
			"404 secret_not_found for a secret that is not there (not audited); 409 not_a_swarm for a standalone host. " +
			"Needs secrets.manage (Admin) on the environment. Every attempt on a secret that exists is audited as secret.removed, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermSecretsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *SecretDeleteInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, deleteSecret(ctx, d, p, env.ID, in)
		})

	registerSecretRotate(r, d)
}

// secretOwner is the stack a secret belongs to, from its labels.
func secretOwner(labels map[string]string) (owner, kind string) {
	if v := labels[labelStack]; v != "" {
		return v, "stack"
	}
	return "", ""
}

func secretItem(row dockerx.SecretRow, used int, known stackIDs) SecretItem {
	owner, kind := secretOwner(row.Labels)
	return SecretItem{
		ID: row.ID, Name: row.Name, Labels: row.Labels, CreatedAt: rfc3339(row.CreatedAt), UpdatedAt: rfc3339(row.UpdatedAt),
		UsedBy: used, Owner: owner, OwnerKind: kind, CoamingStackID: known.confirmObject(owner, row.Labels[stacks.LabelStackID]),
	}
}

// secretUsers lists the services whose spec references the secret, by the secret's ID,
// sorted by service name then ID (the order refs come in).
func secretUsers(refs []dockerx.ServiceRef, secretID string) []SecretUserBody {
	out := []SecretUserBody{}
	for _, ref := range refs {
		for _, o := range ref.Secrets {
			if o.ID != secretID {
				continue
			}
			out = append(out, SecretUserBody{
				ServiceID: ref.ID, Service: ref.Name, File: o.File, UID: o.UID, GID: o.GID, Mode: fileMode(os.FileMode(o.Mode)),
			})
		}
	}
	return out
}

// secretMatchesQuery says whether q (already lower-cased) occurs in the name, the owner or
// any label key or value of the row.
func secretMatchesQuery(row dockerx.SecretRow, q string) bool {
	if q == "" {
		return true
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	if owner, _ := secretOwner(row.Labels); has(row.Name) || has(owner) {
		return true
	}
	for k, v := range row.Labels {
		if has(k) || has(v) {
			return true
		}
	}
	return false
}

// secretReadProblem turns the error of a read through a manager into the problem the caller
// is answered with.
func secretReadProblem(d SecretsDeps, envID string, err error) error {
	switch {
	case errors.Is(err, dockerx.ErrObjectNotFound):
		return httpx.Problem(http.StatusNotFound, problemSecretNotFound, "This secret does not exist. It may have been removed, or a new version replaced it.")
	case envs.ReasonFor(err) == envs.ReasonNoQuorum:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoQuorum, "The swarm has lost its quorum.")
	case errors.Is(err, envs.ErrNoManager), errors.Is(err, envs.ErrOffline), errors.Is(err, envs.ErrNodeUnavailable), envs.ReasonFor(err) == envs.ReasonNotAManager:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoManager, swarmWriteDetail[codeNoManagerConnected])
	}
	return snapshotProblem(d.Log, envID, err)
}

// clusterRead reads the cluster state: the service references and the services' rows. A read
// that fails but leaves an earlier state serves that state.
func clusterRead(ctx context.Context, d SecretsDeps, envID string) (dockerx.ClusterRead, error) {
	st, err := d.Clusters.State(ctx, envID)
	if err != nil && st.FetchedAt.IsZero() {
		return dockerx.ClusterRead{}, secretReadProblem(d, envID, err)
	}
	return st.Read, nil
}

// listSecrets answers the secrets of a swarm: read through a manager, with the number of
// services that use each counted from the cluster state.
func listSecrets(ctx context.Context, d SecretsDeps, envID string, in *SecretListInput) (SecretListBody, error) {
	if utf8.RuneCountInString(in.Q) > maxServiceQuery {
		return SecretListBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemQueryTooLong,
			"The search can be at most "+strconv.Itoa(maxServiceQuery)+" characters.")
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return SecretListBody{}, err
	}
	var rows []dockerx.SecretRow
	if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.SecretList(ctx)
		return e
	}); err != nil {
		return SecretListBody{}, secretReadProblem(d, envID, err)
	}
	cl, err := clusterRead(ctx, d, envID)
	if err != nil {
		return SecretListBody{}, err
	}
	used := map[string]int{}
	for _, ref := range cl.ServiceRefs {
		seen := map[string]bool{}
		for _, o := range ref.Secrets {
			if !seen[o.ID] {
				seen[o.ID] = true
				used[o.ID]++
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(in.Q))
	matched := make([]dockerx.SecretRow, 0, len(rows))
	for _, r := range rows {
		if secretMatchesQuery(r, q) {
			matched = append(matched, r)
		}
	}
	shown := matched[:min(len(matched), maxListRows)]
	body := SecretListBody{
		Items: make([]SecretItem, 0, len(shown)), Total: len(matched), UnfilteredTotal: len(rows), Capped: len(matched) > maxListRows,
	}
	known := loadStackIDs(ctx, d.Store, envID, d.Log)
	for _, r := range shown {
		body.Items = append(body.Items, secretItem(r, used[r.ID], known))
	}
	return body, nil
}

// readSecret reads one secret through a manager by ID or name, with its raw document.
func readSecret(ctx context.Context, d SecretsDeps, envID, id string) (dockerx.SecretRow, []byte, error) {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return dockerx.SecretRow{}, nil, err
	}
	var row dockerx.SecretRow
	var raw []byte
	if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		row, raw, e = c.SecretInspect(ctx, id)
		return e
	}); err != nil {
		return dockerx.SecretRow{}, nil, secretReadProblem(d, envID, err)
	}
	return row, raw, nil
}

// getSecret answers one secret's metadata and the services that use it.
func getSecret(ctx context.Context, d SecretsDeps, envID, id string) (SecretDetailBody, error) {
	row, _, err := readSecret(ctx, d, envID, id)
	if err != nil {
		return SecretDetailBody{}, err
	}
	cl, err := clusterRead(ctx, d, envID)
	if err != nil {
		return SecretDetailBody{}, err
	}
	users := secretUsers(cl.ServiceRefs, row.ID)
	stacks := make(map[string]string, len(cl.Services))
	for _, svc := range cl.Services {
		stacks[svc.ID] = svc.Stack
	}
	for i := range users {
		users[i].Stack = stacks[users[i].ServiceID]
	}
	it := secretItem(row, len(users), loadStackIDs(ctx, d.Store, envID, d.Log))
	return SecretDetailBody{
		ID: it.ID, Name: it.Name, Labels: it.Labels, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt,
		UsedBy: it.UsedBy, Owner: it.Owner, OwnerKind: it.OwnerKind, CoamingStackID: it.CoamingStackID, UsedByRows: users,
	}, nil
}

// getSecretInspect answers the raw inspect document of a secret. The manager's document has
// no value; a data field is removed anyway, so that no change of the manager's can put one
// in a response (RES-08).
func getSecretInspect(ctx context.Context, d SecretsDeps, envID, id string) (SecretInspectBody, error) {
	_, raw, err := readSecret(ctx, d, envID, id)
	if err != nil {
		return SecretInspectBody{}, err
	}
	doc, err := decodeInspect(raw)
	if err != nil {
		d.Log.Error("reading a secret inspect failed", "environment", envID, "error", err)
		return SecretInspectBody{}, huma.Error500InternalServerError("internal error")
	}
	delete(doc, "Data")
	if spec, ok := doc["Spec"].(map[string]any); ok {
		delete(spec, "Data")
	}
	return SecretInspectBody{Inspect: doc}, nil
}

// validateSecretLabels checks the labels of a create the way the agent does, so a refusal is
// ours and names the rule.
func validateSecretLabels(labels map[string]string) error {
	for k, v := range labels {
		if len(k) > maxLabelKey || len(v) > maxLabelValue {
			return httpx.Problem(http.StatusUnprocessableEntity, problemLabelsInvalid,
				"A label name can be at most "+strconv.Itoa(maxLabelKey)+" characters and a label value at most "+strconv.Itoa(maxLabelValue)+".")
		}
	}
	return nil
}

// decodeSecretValue turns the base64 text of a create into the value and checks its size.
// Nothing here puts the value, or any part of it, in an error.
func decodeSecretValue(text string) ([]byte, error) {
	if text == "" {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueRequired, "Enter a value.")
	}
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueInvalid, "The value must be standard base64.")
	}
	switch {
	case len(data) == 0:
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueRequired, "Enter a value.")
	case len(data) > maxSecretValue:
		clear(data)
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueTooLarge, "Values can be at most 500 KB.")
	}
	return data, nil
}

// createSecret checks the request and sends the secret to the swarm through a manager, once.
// Every check that can be made without Docker is made first, and the value is dropped from
// memory as soon as the call has returned.
func createSecret(ctx context.Context, d SecretsDeps, p *rbac.Principal, envID string, in *SecretCreateInput) (SecretCreateBody, error) {
	body := in.Body
	if !secretNameRe.MatchString(body.Name) {
		return SecretCreateBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemNameInvalid,
			"Use letters, numbers, dot, dash or underscore, up to 64 characters, starting and ending with a letter or number.")
	}
	data, err := decodeSecretValue(body.Data)
	if err != nil {
		return SecretCreateBody{}, err
	}
	defer clear(data)
	if err := validateSecretLabels(body.Labels); err != nil {
		return SecretCreateBody{}, err
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return SecretCreateBody{}, err
	}
	source := cmpOr(body.Source, sourceAPI)
	details := func(code string) map[string]string {
		return map[string]string{
			"name": body.Name, "labels": strconv.Itoa(len(body.Labels)), "size": strconv.Itoa(len(data)),
			"source": source, "code": code,
		}
	}

	// The write is sent once and seen through, so a browser that goes away while it runs does
	// not turn a create that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var id string
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		id, e = c.SecretCreate(writeCtx, dockerx.SecretSpec{Name: body.Name, Labels: body.Labels, Data: data})
		return e
	})
	if err != nil {
		code, problem := secretWriteProblem(d, envID, problemCreateFailed, body.Name, err)
		auditSecret(ctx, d, p, envID, in.UserAgent, audit.ActionSecretCreated, body.Name, body.Name, details(code), code)
		return SecretCreateBody{}, problem
	}
	auditSecret(ctx, d, p, envID, in.UserAgent, audit.ActionSecretCreated, id, body.Name, details("created"), actionDone)
	return SecretCreateBody{ID: id, Name: body.Name}, nil
}

// secretRefusal is a delete the server turned down after reading the secret: nothing was
// sent. code is what the audit row records and problem what the caller is told.
type secretRefusal struct {
	code    string
	problem error
}

func (e *secretRefusal) Error() string { return "delete refused: " + e.code }

// deleteSecret deletes one secret. In one write call on a manager it reads the secret, wants
// its name typed, reads the services live and refuses while any spec references the secret,
// and otherwise removes it, once and never retried. Every attempt on a secret that exists is
// audited; one that is gone is not.
func deleteSecret(ctx context.Context, d SecretsDeps, p *rbac.Principal, envID string, in *SecretDeleteInput) error {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var name, id string
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		row, _, err := c.SecretInspect(writeCtx, in.SecretID)
		if err != nil {
			return err
		}
		id, name = row.ID, row.Name
		if in.Confirm != row.Name {
			return &secretRefusal{code: codeConfirmationRequired, problem: httpx.Problem(http.StatusUnprocessableEntity, codeConfirmationRequired,
				"Type the secret's name, "+row.Name+", to confirm. Nothing was deleted.")}
		}
		refs, err := c.ServiceRefs(writeCtx)
		if err != nil {
			return err
		}
		if users := secretUsers(refs, row.ID); len(users) > 0 {
			return &secretRefusal{code: problemObjectInUse, problem: inUseProblem(row.Name, users)}
		}
		return c.SecretRemove(writeCtx, row.ID)
	})
	details := func(code string) map[string]string { return map[string]string{"name": name, "code": code} }
	if err != nil {
		var refused *secretRefusal
		if errors.As(err, &refused) {
			auditSecret(ctx, d, p, envID, in.UserAgent, audit.ActionSecretRemoved, id, in.SecretID, details(refused.code), refused.code)
			return refused.problem
		}
		code, problem := secretWriteProblem(d, envID, problemRemoveFailed, name, err)
		if code != problemSecretNotFound {
			auditSecret(ctx, d, p, envID, in.UserAgent, audit.ActionSecretRemoved, id, in.SecretID, details(code), code)
		}
		return problem
	}
	auditSecret(ctx, d, p, envID, in.UserAgent, audit.ActionSecretRemoved, id, in.SecretID, details(actionDone), actionDone)
	return nil
}

// inUseProblem is the 409 for a secret that services still reference. It names up to three
// of them, in the order they are listed.
func inUseProblem(secretName string, users []SecretUserBody) error {
	names := make([]string, 0, len(users))
	for _, u := range users {
		if !slices.Contains(names, u.Service) {
			names = append(names, u.Service)
		}
	}
	return httpx.Problem(http.StatusConflict, problemObjectInUse,
		secretName+" is still in use by "+joinMore(names, maxNamedServices)+". Point those services at another secret or remove them, then try again.")
}

// secretWriteProblem turns the error of a create or a delete into the code its audit row
// records and the problem the caller is answered with. failedType is the type of a refusal
// that is none of the known ones: the manager's own words go in the detail.
func secretWriteProblem(d SecretsDeps, envID, failedType, name string, err error) (string, error) {
	switch {
	case errors.Is(err, dockerx.ErrNameTaken):
		return problemNameTaken, httpx.Problem(http.StatusConflict, problemNameTaken, "A secret named "+name+" already exists.")
	case errors.Is(err, dockerx.ErrObjectNotFound):
		return problemSecretNotFound, httpx.Problem(http.StatusNotFound, problemSecretNotFound, "This secret does not exist. It may have been removed.")
	case errors.Is(err, dockerx.ErrObjectInUse):
		// A service took the secret between the live read and the delete; the manager refused.
		return problemObjectInUse, httpx.Problem(http.StatusConflict, problemObjectInUse,
			name+" is in use by a service. Point those services at another secret or remove them, then try again.")
	}
	return serviceActionProblem(d.services(), envID, failedType, err)
}

// auditSecret writes the row of one create or delete attempt: a success when code is done or
// created, else a failure whose reason is the code. It is written on a context that outlives
// the request: the change has already happened or been refused. No detail holds the value.
func auditSecret(ctx context.Context, d SecretsDeps, p *rbac.Principal, envID, userAgent, action, id, asked string, details map[string]string, code string) {
	result, reason := audit.ResultSuccess, ""
	if code != actionDone {
		result, reason = audit.ResultFailure, code
	}
	if id == "" {
		id = asked
	}
	wctx, cancel := doneActionAuditContext(ctx)
	defer cancel()
	if err := d.Audit.Record(wctx, audit.Entry{
		Actor: p.AuditActor(), Action: action, TargetType: secretTargetType, TargetID: id,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		// The change has been made or refused and the caller must still be told which; the
		// lost row is the server's fault and is logged.
		d.Log.Error("could not audit a secret action", "action", action, "secret", id, "error", err)
	}
}
