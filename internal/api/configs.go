// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/registry"
	"github.com/ianglennon/bayplan/internal/stacks"
	"github.com/ianglennon/bayplan/internal/store"
	"github.com/ianglennon/bayplan/internal/tunnel"
)

const (
	configTargetType = "config"

	problemTemplatingInvalid = "templating_invalid"
	problemConfigNotFound    = "config_not_found"
	problemConfigListTooBig  = "config_list_too_large"

	// maxConfigValue is the most bytes of content a config may have: the swarm takes 1,023,999
	// and refuses 1,024,000 (RESEARCH E18, D-40).
	maxConfigValue = stacks.MaxConfigBytes
	// maxConfigBody is the most bytes a create request may have: the base64 of content of
	// maxConfigValue bytes is about 1.37 MB.
	maxConfigBody = 2 << 20

	// templatingNone and templatingGolang are the two values a config's templating takes. The
	// swarm's only templating driver is golang.
	templatingNone   = "none"
	templatingGolang = "golang"
)

// ConfigsDeps are the dependencies of the swarm config routes.
type ConfigsDeps struct {
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
func (d ConfigsDeps) services() ServicesDeps {
	return ServicesDeps(d)
}

// ConfigItem is one swarm config as the list shows it. There is no content: the manager sends
// every config's content with the list, and Bayplan drops it on the way in. The content is read
// on the detail route.
type ConfigItem struct {
	ID         string            `json:"id" doc:"The config ID"`
	Name       string            `json:"name" doc:"The config name"`
	Labels     map[string]string `json:"labels" doc:"Every label of the config"`
	CreatedAt  string            `json:"createdAt" format:"date-time" doc:"When the config was created"`
	UpdatedAt  string            `json:"updatedAt" format:"date-time" doc:"When the config was last updated"`
	Templating string            `json:"templating" enum:"none,golang" doc:"golang when the swarm fills the content in as a Go template when a task starts, else none"`
	UsedBy     int               `json:"usedBy" doc:"How many services' specs reference the config, whatever their tasks are doing, a service scaled to 0 included"`
	Owner      string            `json:"owner" doc:"The stack (com.docker.stack.namespace) label value; empty when the config has none"`
	OwnerKind  string            `json:"ownerKind" enum:"stack," doc:"stack, or empty when the config has no owner"`
}

// ConfigListBody is the answer of the configs list.
type ConfigListBody struct {
	Items           []ConfigItem `json:"items" doc:"At most 500 rows that match the search, sorted by name then ID"`
	Total           int          `json:"total" doc:"How many configs match the search, whatever the cap"`
	UnfilteredTotal int          `json:"unfilteredTotal" doc:"How many configs there are before the search"`
	Capped          bool         `json:"capped" doc:"True when total is more than the rows returned"`
}

// ConfigListInput is the path of the configs list with its search.
type ConfigListInput struct {
	EnvironmentPathInput
	Q string `query:"q" doc:"Search: a case-insensitive substring of the config name, its owner or any label key or value, at most 128 characters (else 422 query_too_long)"`
}

// ConfigListOutput wraps ConfigListBody for huma.
type ConfigListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ConfigListBody
}

// ConfigUserBody is one service whose spec references a config, with how it is mounted.
type ConfigUserBody struct {
	ServiceID string `json:"serviceId" doc:"The service ID"`
	Service   string `json:"service" doc:"The service name"`
	Stack     string `json:"stack" doc:"The stack the service belongs to; empty for a service made on its own"`
	File      string `json:"file" doc:"The file name the config appears as in the container; empty when the service reads it at runtime only"`
	UID       string `json:"uid" doc:"The owner of the file"`
	GID       string `json:"gid" doc:"The group of the file"`
	Mode      string `json:"mode" doc:"The file mode in octal, for example 0444; empty when none is set"`
}

// ConfigDetailBody is one config with its content and the services that use it.
type ConfigDetailBody struct {
	ID         string            `json:"id" doc:"The config ID"`
	Name       string            `json:"name" doc:"The config name"`
	Labels     map[string]string `json:"labels" doc:"Every label of the config"`
	CreatedAt  string            `json:"createdAt" format:"date-time" doc:"When the config was created"`
	UpdatedAt  string            `json:"updatedAt" format:"date-time" doc:"When the config was last updated"`
	Templating string            `json:"templating" enum:"none,golang" doc:"golang when the swarm fills the content in as a Go template when a task starts, else none"`
	Size       int               `json:"size" doc:"The content's size in bytes"`
	Data       string            `json:"data" doc:"The content as standard base64"`
	Text       *string           `json:"text" nullable:"true" doc:"The content as text; null when it is not valid UTF-8, which the page shows as a file to download"`
	UsedBy     int               `json:"usedBy" doc:"How many services' specs reference the config"`
	Owner      string            `json:"owner" doc:"The stack (com.docker.stack.namespace) label value; empty when the config has none"`
	OwnerKind  string            `json:"ownerKind" enum:"stack," doc:"stack, or empty when the config has no owner"`
	UsedByRows []ConfigUserBody  `json:"usedByRows" doc:"Every service whose spec references the config, sorted by service name then ID"`
}

// ConfigDetailOutput wraps ConfigDetailBody for huma.
type ConfigDetailOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ConfigDetailBody
}

// ConfigPathInput is the path of one config.
type ConfigPathInput struct {
	EnvironmentPathInput
	ConfigID string `path:"configId" maxLength:"128" doc:"The config ID or name: the id of a row of the configs list"`
}

// ConfigInspectBody is the raw inspect document of one config.
type ConfigInspectBody struct {
	Inspect map[string]any `json:"inspect" doc:"The manager's inspect document: the ID, the version, the times and the spec's name, labels, content (as base64) and templating"`
}

// ConfigInspectOutput wraps ConfigInspectBody for huma.
type ConfigInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ConfigInspectBody
}

// ConfigCreateRequest is the body of POST /api/env/{envId}/configs.
type ConfigCreateRequest struct {
	Name       string            `json:"name" doc:"The config name: letters, numbers, dot, dash and underscore, starting and ending with a letter or number, at most 64 characters (else 422 name_invalid)"`
	Labels     map[string]string `json:"labels,omitempty" maxProperties:"64" doc:"Labels, at most 64"`
	Data       string            `json:"data,omitempty" doc:"The content, as standard base64. 1 to 1,023,999 bytes once decoded: empty is 422 value_required, 1,024,000 or more is 422 value_too_large, and text that is not base64 is 422 value_invalid"`
	Templating string            `json:"templating,omitempty" doc:"none (the default) or golang, to have the swarm fill the content in as a Go template when a task starts; anything else is 422 templating_invalid"`
	Source     string            `json:"source,omitempty" enum:"typed,file,rotate,apply" doc:"Where the content came from, for the audit log only: typed or file (the create sheet), rotate (a new version of a config) or apply (a config staged with a service). Left out, the audit log says api"`
}

// ConfigCreateInput is the path and body of the create route.
type ConfigCreateInput struct {
	EnvironmentPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
	Body      ConfigCreateRequest
}

// ConfigCreateBody is the answer to a create: the new config's ID and name.
type ConfigCreateBody struct {
	ID   string `json:"id" doc:"The ID of the config the swarm made"`
	Name string `json:"name" doc:"The config name"`
}

// ConfigCreateOutput wraps ConfigCreateBody for huma.
type ConfigCreateOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         ConfigCreateBody
}

// ConfigDeleteInput is the path and the audit context of the delete route. A config needs no
// typed name: its content is not a secret, and the swarm refuses one a service still uses.
type ConfigDeleteInput struct {
	ConfigPathInput
	// UserAgent is read only to record it in the audit log; it is not part of the contract.
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// RegisterConfigs mounts the swarm config routes.
func RegisterConfigs(r *httpx.Router, d ConfigsDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	httpx.Register(r, huma.Operation{
		OperationID: "listConfigs", Method: http.MethodGet, Path: "/api/env/{envId}/configs",
		Summary: "List the swarm configs of an environment",
		Description: "The configs of a swarm environment with their labels, times, templating, owner and how many services use each, at most 500 rows sorted by name then ID, read through a manager. " +
			"No row has content: the manager sends every config's content with the list and Bayplan drops it, so the content is read on one config's own route. " +
			"Optionally a search (q) that runs before the 500-row cap. " +
			"502 config_list_too_large when the configs are too many or too large for one answer; 409 not_a_swarm when the environment is a standalone host; 503 no_manager_connected when no manager is connected. " +
			"Needs configs.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermConfigsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigListInput) (*ConfigListOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := listConfigs(ctx, d, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ConfigListOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "getConfig", Method: http.MethodGet, Path: "/api/env/{envId}/configs/{configId}",
		Summary: "One swarm config, its content and the services that use it",
		Description: "The config's metadata, templating, size and content (as base64, and as text when it is valid UTF-8, else text is null), and every service whose spec references it, with the file name, owner, group and mode each mounts it with. " +
			"The config is named by its ID or its name. A config is not encrypted: anyone who can read configs can read its content. 404 config_not_found; 409 not_a_swarm. " +
			"Needs configs.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermConfigsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigPathInput) (*ConfigDetailOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getConfig(ctx, d, env.ID, in.ConfigID)
			if err != nil {
				return nil, err
			}
			return &ConfigDetailOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "inspectConfig", Method: http.MethodGet, Path: "/api/env/{envId}/configs/{configId}/inspect",
		Summary: "One swarm config's raw inspect document",
		Description: "The manager's inspect document of the config, for the Inspect tab. Like the detail it carries the content (as base64). " +
			"404 config_not_found; 409 not_a_swarm. Needs configs.read (Viewer) on the environment.",
		Tags: []string{"environments"},
	}, rbac.Requirement{Perm: rbac.PermConfigsRead, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigPathInput) (*ConfigInspectOutput, error) {
			env := httpx.EnvFrom(ctx)
			if env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := getConfigInspect(ctx, d, env.ID, in.ConfigID)
			if err != nil {
				return nil, err
			}
			return &ConfigInspectOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "createConfig", Method: http.MethodPost, Path: "/api/env/{envId}/configs",
		Summary: "Create a swarm config",
		Description: "Creates a config once, through a manager, and never retries. The answer holds the new config's ID and name. " +
			"A name that is not 1 to 64 letters, numbers, dots, dashes and underscores starting and ending with a letter or number is 422 name_invalid; templating other than none or golang is 422 templating_invalid; content that is missing or empty is 422 value_required; content of 1,024,000 bytes or more is 422 value_too_large (1,023,999 bytes is accepted); more than 64 labels is 422, and a label key over 256 or value over 4096 characters is 422 labels_invalid. All are checked before anything is sent. " +
			"A request body over 2 MiB is refused. 409 name_taken when the swarm has the name already, 409 not_a_swarm for a standalone host, 409 host_read_only on a read-only host. " +
			"Needs configs.manage (Admin) on the environment. Every attempt is audited as config.created with the name, the number of labels, the size in bytes, the templating and where the request came from; a refusal is a failure row with its code.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusCreated, MaxBodyBytes: maxConfigBody,
	}, rbac.Requirement{Perm: rbac.PermConfigsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigCreateInput) (*ConfigCreateOutput, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			body, err := createConfig(ctx, d, p, env.ID, in)
			if err != nil {
				return nil, err
			}
			return &ConfigCreateOutput{CacheControl: "no-store", Body: body}, nil
		})

	httpx.Register(r, huma.Operation{
		OperationID: "deleteConfig", Method: http.MethodDelete, Path: "/api/env/{envId}/configs/{configId}",
		Summary: "Delete a swarm config",
		Description: "Deletes one config, once and never retried. No name needs to be typed. " +
			"A config that any service's spec references is 409 object_in_use, naming up to three of the services, read live just before the delete and counting a service scaled to 0. " +
			"404 config_not_found for a config that is not there (not audited); 409 not_a_swarm for a standalone host. " +
			"Needs configs.manage (Admin) on the environment. Every attempt on a config that exists is audited as config.removed, with result failure and the code when it was refused.",
		Tags: []string{"environments"}, DefaultStatus: http.StatusNoContent,
	}, rbac.Requirement{Perm: rbac.PermConfigsManage, Scope: rbac.ScopeEnvironment},
		func(ctx context.Context, in *ConfigDeleteInput) (*struct{}, error) {
			p, env := httpx.PrincipalFrom(ctx), httpx.EnvFrom(ctx)
			if p == nil || env == nil {
				return nil, huma.Error500InternalServerError("internal error")
			}
			return nil, deleteConfig(ctx, d, p, env.ID, in)
		})

	registerConfigRotate(r, d)
}

// configOwner is the stack a config belongs to, from its labels.
func configOwner(labels map[string]string) (owner, kind string) {
	return secretOwner(labels)
}

// configTemplatingLabel is a row's templating as the API says it: golang, or none for no
// driver. The swarm has no other driver.
func configTemplatingLabel(driver string) string {
	if driver == templatingGolang {
		return templatingGolang
	}
	return templatingNone
}

func configItem(row dockerx.ConfigRow, used int) ConfigItem {
	owner, kind := configOwner(row.Labels)
	return ConfigItem{
		ID: row.ID, Name: row.Name, Labels: row.Labels, CreatedAt: rfc3339(row.CreatedAt), UpdatedAt: rfc3339(row.UpdatedAt),
		Templating: configTemplatingLabel(row.Templating), UsedBy: used, Owner: owner, OwnerKind: kind,
	}
}

// configUsers lists the services whose spec references the config, by the config's ID, sorted
// by service name then ID (the order refs come in).
func configUsers(refs []dockerx.ServiceRef, configID string) []ConfigUserBody {
	out := []ConfigUserBody{}
	for _, ref := range refs {
		for _, o := range ref.Configs {
			if o.ID != configID {
				continue
			}
			out = append(out, ConfigUserBody{
				ServiceID: ref.ID, Service: ref.Name, File: o.File, UID: o.UID, GID: o.GID, Mode: fileMode(os.FileMode(o.Mode)),
			})
		}
	}
	return out
}

// configMatchesQuery says whether q (already lower-cased) occurs in the name, the owner or any
// label key or value of the row.
func configMatchesQuery(row dockerx.ConfigRow, q string) bool {
	if q == "" {
		return true
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	if owner, _ := configOwner(row.Labels); has(row.Name) || has(owner) {
		return true
	}
	for k, v := range row.Labels {
		if has(k) || has(v) {
			return true
		}
	}
	return false
}

// configReadProblem turns the error of a read through a manager into the problem the caller is
// answered with. The manager sends every config's content with the list, so a swarm of many or
// large configs can pass the tunnel's response cap; that is a 502 that says so (T-06-71).
func configReadProblem(d ConfigsDeps, envID string, err error) error {
	switch {
	case errors.Is(err, dockerx.ErrObjectNotFound):
		return httpx.Problem(http.StatusNotFound, problemConfigNotFound, "This config does not exist. It may have been removed, or a new version replaced it.")
	case errors.Is(err, tunnel.ErrTooLarge):
		return httpx.Problem(http.StatusBadGateway, problemConfigListTooBig,
			"The swarm has too many or too large configs to list in one answer. Delete configs you no longer use, then try again.")
	case envs.ReasonFor(err) == envs.ReasonNoQuorum:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoQuorum, "The swarm has lost its quorum.")
	case errors.Is(err, envs.ErrNoManager), errors.Is(err, envs.ErrOffline), errors.Is(err, envs.ErrNodeUnavailable), envs.ReasonFor(err) == envs.ReasonNotAManager:
		return httpx.Problem(http.StatusServiceUnavailable, problemNoManager, swarmWriteDetail[codeNoManagerConnected])
	}
	return snapshotProblem(d.Log, envID, err)
}

// configClusterRead reads the cluster state: the service references and the services' rows. A
// read that fails but leaves an earlier state serves that state.
func configClusterRead(ctx context.Context, d ConfigsDeps, envID string) (dockerx.ClusterRead, error) {
	st, err := d.Clusters.State(ctx, envID)
	if err != nil && st.FetchedAt.IsZero() {
		return dockerx.ClusterRead{}, configReadProblem(d, envID, err)
	}
	return st.Read, nil
}

// listConfigs answers the configs of a swarm without their content: read through a manager,
// with the number of services that use each counted from the cluster state.
func listConfigs(ctx context.Context, d ConfigsDeps, envID string, in *ConfigListInput) (ConfigListBody, error) {
	if utf8.RuneCountInString(in.Q) > maxServiceQuery {
		return ConfigListBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemQueryTooLong,
			"The search can be at most "+strconv.Itoa(maxServiceQuery)+" characters.")
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return ConfigListBody{}, err
	}
	var rows []dockerx.ConfigRow
	if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		rows, e = c.ConfigList(ctx)
		return e
	}); err != nil {
		return ConfigListBody{}, configReadProblem(d, envID, err)
	}
	cl, err := configClusterRead(ctx, d, envID)
	if err != nil {
		return ConfigListBody{}, err
	}
	used := map[string]int{}
	for _, ref := range cl.ServiceRefs {
		seen := map[string]bool{}
		for _, o := range ref.Configs {
			if !seen[o.ID] {
				seen[o.ID] = true
				used[o.ID]++
			}
		}
	}
	q := strings.ToLower(strings.TrimSpace(in.Q))
	matched := make([]dockerx.ConfigRow, 0, len(rows))
	for _, r := range rows {
		if configMatchesQuery(r, q) {
			matched = append(matched, r)
		}
	}
	shown := matched[:min(len(matched), maxListRows)]
	body := ConfigListBody{
		Items: make([]ConfigItem, 0, len(shown)), Total: len(matched), UnfilteredTotal: len(rows), Capped: len(matched) > maxListRows,
	}
	for _, r := range shown {
		body.Items = append(body.Items, configItem(r, used[r.ID]))
	}
	return body, nil
}

// readConfig reads one config through a manager by ID or name, with its content and its raw
// document.
func readConfig(ctx context.Context, d ConfigsDeps, envID, id string) (dockerx.ConfigDetail, []byte, error) {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return dockerx.ConfigDetail{}, nil, err
	}
	var detail dockerx.ConfigDetail
	var raw []byte
	if err := d.Clusters.ClusterRead(ctx, envID, func(c *dockerx.Client) error {
		var e error
		detail, raw, e = c.ConfigInspect(ctx, id)
		return e
	}); err != nil {
		return dockerx.ConfigDetail{}, nil, configReadProblem(d, envID, err)
	}
	return detail, raw, nil
}

// getConfig answers one config's metadata, its content and the services that use it.
func getConfig(ctx context.Context, d ConfigsDeps, envID, id string) (ConfigDetailBody, error) {
	detail, _, err := readConfig(ctx, d, envID, id)
	if err != nil {
		return ConfigDetailBody{}, err
	}
	cl, err := configClusterRead(ctx, d, envID)
	if err != nil {
		return ConfigDetailBody{}, err
	}
	users := configUsers(cl.ServiceRefs, detail.ID)
	stacks := make(map[string]string, len(cl.Services))
	for _, svc := range cl.Services {
		stacks[svc.ID] = svc.Stack
	}
	for i := range users {
		users[i].Stack = stacks[users[i].ServiceID]
	}
	it := configItem(detail.ConfigRow, len(users))
	var text *string
	if utf8.Valid(detail.Data) {
		s := string(detail.Data)
		text = &s
	}
	return ConfigDetailBody{
		ID: it.ID, Name: it.Name, Labels: it.Labels, CreatedAt: it.CreatedAt, UpdatedAt: it.UpdatedAt, Templating: it.Templating,
		Size: len(detail.Data), Data: base64.StdEncoding.EncodeToString(detail.Data), Text: text,
		UsedBy: it.UsedBy, Owner: it.Owner, OwnerKind: it.OwnerKind, UsedByRows: users,
	}, nil
}

// getConfigInspect answers the raw inspect document of a config.
func getConfigInspect(ctx context.Context, d ConfigsDeps, envID, id string) (ConfigInspectBody, error) {
	_, raw, err := readConfig(ctx, d, envID, id)
	if err != nil {
		return ConfigInspectBody{}, err
	}
	doc, err := decodeInspect(raw)
	if err != nil {
		d.Log.Error("reading a config inspect failed", "environment", envID, "error", err)
		return ConfigInspectBody{}, huma.Error500InternalServerError("internal error")
	}
	return ConfigInspectBody{Inspect: doc}, nil
}

// decodeConfigValue turns the base64 text of a create into the content and checks its size.
func decodeConfigValue(text string) ([]byte, error) {
	if text == "" {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueRequired, "Enter the content.")
	}
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueInvalid, "The content must be standard base64.")
	}
	switch {
	case len(data) == 0:
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueRequired, "Enter the content.")
	case len(data) > maxConfigValue:
		return nil, httpx.Problem(http.StatusUnprocessableEntity, problemValueTooLarge, "Values can be at most 1000 KB.")
	}
	return data, nil
}

// configTemplating turns the templating of a create into the driver to send (empty for none)
// and the word the audit row records.
func configTemplating(v string) (driver, label string, err error) {
	switch v {
	case "", templatingNone:
		return "", templatingNone, nil
	case templatingGolang:
		return templatingGolang, templatingGolang, nil
	}
	return "", "", httpx.Problem(http.StatusUnprocessableEntity, problemTemplatingInvalid, "Templating can be none or golang.")
}

// createConfig checks the request and sends the config to the swarm through a manager, once.
// Every check that can be made without Docker is made first.
func createConfig(ctx context.Context, d ConfigsDeps, p *rbac.Principal, envID string, in *ConfigCreateInput) (ConfigCreateBody, error) {
	body := in.Body
	if !secretNameRe.MatchString(body.Name) {
		return ConfigCreateBody{}, httpx.Problem(http.StatusUnprocessableEntity, problemNameInvalid,
			"Use letters, numbers, dot, dash or underscore, up to 64 characters, starting and ending with a letter or number.")
	}
	driver, templating, err := configTemplating(body.Templating)
	if err != nil {
		return ConfigCreateBody{}, err
	}
	data, err := decodeConfigValue(body.Data)
	if err != nil {
		return ConfigCreateBody{}, err
	}
	if err := validateSecretLabels(body.Labels); err != nil {
		return ConfigCreateBody{}, err
	}
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return ConfigCreateBody{}, err
	}
	source := cmpOr(body.Source, sourceAPI)
	details := func(code string) map[string]string {
		return map[string]string{
			"name": body.Name, "labels": strconv.Itoa(len(body.Labels)), "size": strconv.Itoa(len(data)),
			"templating": templating, "source": source, "code": code,
		}
	}

	// The write is sent once and seen through, so a browser that goes away while it runs does
	// not turn a create that happened into a failure.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var id string
	err = d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		var e error
		id, e = c.ConfigCreate(writeCtx, dockerx.ConfigSpec{Name: body.Name, Labels: body.Labels, Data: data, Templating: driver})
		return e
	})
	if err != nil {
		code, problem := configWriteProblem(d, envID, problemCreateFailed, body.Name, err)
		auditConfig(ctx, d, p, envID, in.UserAgent, audit.ActionConfigCreated, body.Name, body.Name, details(code), code)
		return ConfigCreateBody{}, problem
	}
	auditConfig(ctx, d, p, envID, in.UserAgent, audit.ActionConfigCreated, id, body.Name, details("created"), actionDone)
	return ConfigCreateBody{ID: id, Name: body.Name}, nil
}

// configRefusal is a delete the server turned down after reading the config: nothing was sent.
// code is what the audit row records and problem what the caller is told.
type configRefusal struct {
	code    string
	problem error
}

func (e *configRefusal) Error() string { return "delete refused: " + e.code }

// deleteConfig deletes one config. In one write call on a manager it reads the config, reads the
// services live and refuses while any spec references the config, and otherwise removes it,
// once and never retried. Every attempt on a config that exists is audited; one that is gone is
// not.
func deleteConfig(ctx context.Context, d ConfigsDeps, p *rbac.Principal, envID string, in *ConfigDeleteInput) error {
	if err := requireSwarm(ctx, d.services(), envID); err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	var name, id string
	err := d.Aggregator.WriteOnManager(writeCtx, envID, func(c *dockerx.Client) error {
		cfg, _, err := c.ConfigInspect(writeCtx, in.ConfigID)
		if err != nil {
			return err
		}
		id, name = cfg.ID, cfg.Name
		refs, err := c.ServiceRefs(writeCtx)
		if err != nil {
			return err
		}
		if users := configUsers(refs, cfg.ID); len(users) > 0 {
			return &configRefusal{code: problemObjectInUse, problem: configInUseProblem(cfg.Name, users)}
		}
		return c.ConfigRemove(writeCtx, cfg.ID)
	})
	details := func(code string) map[string]string { return map[string]string{"name": name, "code": code} }
	if err != nil {
		var refused *configRefusal
		if errors.As(err, &refused) {
			auditConfig(ctx, d, p, envID, in.UserAgent, audit.ActionConfigRemoved, id, in.ConfigID, details(refused.code), refused.code)
			return refused.problem
		}
		code, problem := configWriteProblem(d, envID, problemRemoveFailed, name, err)
		if code != problemConfigNotFound {
			auditConfig(ctx, d, p, envID, in.UserAgent, audit.ActionConfigRemoved, id, in.ConfigID, details(code), code)
		}
		return problem
	}
	auditConfig(ctx, d, p, envID, in.UserAgent, audit.ActionConfigRemoved, id, in.ConfigID, details(actionDone), actionDone)
	return nil
}

// configInUseProblem is the 409 for a config that services still reference. It names up to
// three of them, in the order they are listed.
func configInUseProblem(configName string, users []ConfigUserBody) error {
	names := make([]string, 0, len(users))
	for _, u := range users {
		if !slices.Contains(names, u.Service) {
			names = append(names, u.Service)
		}
	}
	return httpx.Problem(http.StatusConflict, problemObjectInUse,
		configName+" is still in use by "+joinMore(names, maxNamedServices)+". Point those services at another config or remove them, then try again.")
}

// configWriteProblem turns the error of a create or a delete into the code its audit row
// records and the problem the caller is answered with. failedType is the type of a refusal that
// is none of the known ones: the manager's own words go in the detail.
func configWriteProblem(d ConfigsDeps, envID, failedType, name string, err error) (string, error) {
	switch {
	case errors.Is(err, dockerx.ErrNameTaken):
		return problemNameTaken, httpx.Problem(http.StatusConflict, problemNameTaken, "A config named "+name+" already exists.")
	case errors.Is(err, dockerx.ErrObjectNotFound):
		return problemConfigNotFound, httpx.Problem(http.StatusNotFound, problemConfigNotFound, "This config does not exist. It may have been removed.")
	case errors.Is(err, dockerx.ErrObjectInUse):
		// A service took the config between the live read and the delete; the manager refused.
		return problemObjectInUse, httpx.Problem(http.StatusConflict, problemObjectInUse,
			name+" is in use by a service. Point those services at another config or remove them, then try again.")
	}
	return serviceActionProblem(d.services(), envID, failedType, err)
}

// auditConfig writes the row of one create or delete attempt: a success when code is done or
// created, else a failure whose reason is the code. It is written on a context that outlives the
// request: the change has already happened or been refused.
func auditConfig(ctx context.Context, d ConfigsDeps, p *rbac.Principal, envID, userAgent, action, id, asked string, details map[string]string, code string) {
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
		Actor: p.AuditActor(), Action: action, TargetType: configTargetType, TargetID: id,
		EnvID: envID, Result: result, Reason: reason,
		IP: httpx.ClientIP(ctx), UserAgent: userAgent, Details: details,
	}); err != nil {
		// The change has been made or refused and the caller must still be told which; the lost
		// row is the server's fault and is logged.
		d.Log.Error("could not audit a config action", "action", action, "config", id, "error", err)
	}
}
