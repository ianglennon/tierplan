// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/bayplan/internal/audit"
	"github.com/ianglennon/bayplan/internal/httpx"
	"github.com/ianglennon/bayplan/internal/rbac"
	"github.com/ianglennon/bayplan/internal/store"
)

// Stable problem types of the audit routes.
const problemInvalidTimeRange = "invalid_time_range"

// reasonNoAuditScope is the reason a refused read of the audit log is audited
// with: the caller is neither an instance admin nor an Admin of any environment.
const reasonNoAuditScope = "no_audit_scope"

// auditPageDefault and auditPageMax are the page sizes of GET /api/audit (D-43).
const (
	auditPageDefault = 50
	auditPageMax     = 100
)

// AuditDeps are the dependencies of the audit routes.
type AuditDeps struct {
	Store *store.Store
	// RetentionDays is the configured audit retention; 0 keeps entries forever.
	RetentionDays int
	// Now stamps export file names. Nil means time.Now.
	Now func() time.Time
	// Log receives server-side failures. Nil means slog.Default().
	Log *slog.Logger
	// Checker decides who reads what of the log: an instance admin everything, an
	// Admin of environments the entries on those environments (D-09). Nil means
	// nobody reads it, which is only for a router that serves nothing.
	Checker *rbac.Checker
	// Refusals records the permission.denied entry of a refused read. It is the
	// router's own auditor, so these refusals are coalesced like any other (D-20).
	// Nil records nothing, like a router without one.
	Refusals httpx.Auditor
}

// auditCaller is read only to record a refusal in the audit log; it is not part
// of the contract.
type auditCaller struct {
	UserAgent string `header:"User-Agent" hidden:"true"`
}

// AuditActorsInput is the (empty) query of GET /api/audit/actors.
type AuditActorsInput struct {
	auditCaller
}

// AuditFilterInput holds the filters the list and the export share.
type AuditFilterInput struct {
	auditCaller
	User   string   `query:"user" maxLength:"128" doc:"Only entries by this actor, matched exactly against the actor name"`
	Action []string `query:"action,explode" maxItems:"50" doc:"Only entries of these action types; repeat the parameter to give several"`
	From   string   `query:"from" format:"date-time" doc:"Only entries at or after this time (RFC 3339)"`
	To     string   `query:"to" format:"date-time" doc:"Only entries at or before this time (RFC 3339)"`
	Bulk   string   `query:"bulk" format:"uuid" doc:"Only entries of one bulk operation: those whose bulk detail is this ID (the bulkId a container action answers with). 422 when it is not a UUID"`
}

// filter turns the query parameters into an audit.Filter. A time that does not
// parse is a 422 (huma already rejects most before this runs).
func (in *AuditFilterInput) filter() (audit.Filter, error) {
	f := audit.Filter{Actor: in.User, Actions: in.Action, Bulk: in.Bulk}
	for _, p := range []struct {
		raw  string
		dest *time.Time
		name string
	}{{in.From, &f.From, "from"}, {in.To, &f.To, "to"}} {
		if p.raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, p.raw)
		if err != nil {
			return audit.Filter{}, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidTimeRange,
				p.name+" must be an RFC 3339 time, for example 2026-10-01T00:00:00Z.")
		}
		*p.dest = t
	}
	return f, nil
}

// ListAuditInput is the query of GET /api/audit.
type ListAuditInput struct {
	AuditFilterInput
	Cursor int64 `query:"cursor" minimum:"0" doc:"The nextCursor of the previous page; leave out for the newest entries"`
	Limit  int   `query:"limit" minimum:"1" doc:"Entries per page; 50 by default, at most 100 (larger values are clamped)"`
}

// AuditEntryDTO is one audit entry: who, what, where, when and the result. It
// never carries the tamper-evidence chain or any secret (D-39).
type AuditEntryDTO struct {
	ID         string            `json:"id"`
	Seq        int64             `json:"seq" doc:"The insertion sequence; entries are ordered by it, newest first"`
	Time       string            `json:"time" format:"date-time" doc:"When it happened, in UTC"`
	ActorType  string            `json:"actorType" enum:"user,system,anonymous"`
	Actor      string            `json:"actor" doc:"The actor's name. For a failed sign-in this is anonymous; the target is the matched account's username, or (unknown user) when none matched"`
	Action     string            `json:"action" doc:"A stable action key such as auth.login"`
	TargetType string            `json:"targetType"`
	Target     string            `json:"target"`
	Env        *string           `json:"environment" nullable:"true" doc:"The environment ID, or null when the action was not about one"`
	Result     string            `json:"result" enum:"success,failure,denied"`
	Reason     string            `json:"reason"`
	IP         string            `json:"ip"`
	UserAgent  string            `json:"userAgent"`
	Details    map[string]string `json:"details"`
	// ActorDeleted is worked out when the entry is read; the entry itself is never rewritten.
	ActorDeleted bool `json:"actorDeleted" doc:"True when the user behind the entry has since been deleted. The entry itself is unchanged"`
}

// AuditListBody is one page of the audit log.
type AuditListBody struct {
	Items         []AuditEntryDTO `json:"items" doc:"Newest first"`
	NextCursor    *int64          `json:"nextCursor" nullable:"true" doc:"Pass as cursor to get the next page; null when there are no more entries"`
	RetentionDays int             `json:"retentionDays" doc:"How many days entries are kept; 0 means forever"`
}

// AuditListOutput wraps AuditListBody for huma.
type AuditListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AuditListBody
}

// AuditActorDTO is one actor name the log holds.
type AuditActorDTO struct {
	Name    string `json:"name" doc:"The actor's name as recorded"`
	Deleted bool   `json:"deleted" doc:"True when the user behind the name has since been deleted"`
}

// AuditActorsOutput lists the actor names seen.
type AuditActorsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []AuditActorDTO
}

// ExportAuditInput is the query of GET /api/audit/export.
type ExportAuditInput struct {
	AuditFilterInput
	Format string `query:"format" enum:"csv,json" default:"csv" doc:"csv or json"`
}

func auditEntryDTO(r audit.Row) AuditEntryDTO {
	var env *string
	if r.EnvID != "" {
		env = &r.EnvID
	}
	return AuditEntryDTO{
		ID: r.ID, Seq: r.Seq, Time: r.Time.UTC().Format(time.RFC3339Nano),
		ActorType: r.ActorType, Actor: r.ActorName, Action: r.Action,
		TargetType: r.TargetType, Target: r.TargetID, Env: env,
		Result: r.Result, Reason: r.Reason, IP: r.IP, UserAgent: r.UserAgent, Details: r.Details,
		ActorDeleted: r.ActorDeleted,
	}
}

// auditScope works out what the caller may read: everything for an instance admin,
// otherwise the entries on the environments where they hold environments.audit
// (Admin), or on every environment for an Admin of all of them. Instance-level
// events have no environment, so a delegated Admin never reads them. A caller
// with no such scope is refused with 403 and the refusal is audited with reason
// no_audit_scope; if it cannot be written the answer is 500, as for any refusal
// (D-49).
func auditScope(ctx context.Context, d AuditDeps, route, userAgent string) (audit.Filter, error) {
	p := httpx.PrincipalFrom(ctx)
	if p == nil {
		return audit.Filter{}, httpx.Problem(http.StatusUnauthorized, "", "Sign in to continue.")
	}
	var acc rbac.Access
	if d.Checker != nil {
		var err error
		if acc, err = d.Checker.Access(ctx, p); err != nil {
			d.Log.Error("audit request failed", "step", "resolve audit scope", "error", err)
			return audit.Filter{}, huma.Error500InternalServerError("internal error")
		}
	}
	if acc.InstanceAdmin {
		return audit.Filter{}, nil
	}
	perm, _ := rbac.Lookup(rbac.PermEnvironmentsAudit)
	scope := audit.Filter{Scoped: true}
	if acc.For("").Allows(perm) {
		scope.AnyEnv = true
	}
	for envID := range acc.Envs {
		if envID != rbac.AllEnvironments && acc.For(envID).Allows(perm) {
			scope.Envs = append(scope.Envs, envID)
		}
	}
	if scope.AnyEnv || len(scope.Envs) > 0 {
		slices.Sort(scope.Envs)
		return scope, nil
	}
	if d.Refusals != nil {
		if err := d.Refusals.Record(ctx, audit.Entry{
			Actor: p.AuditActor(), Action: audit.ActionPermissionDenied, Result: audit.ResultDenied,
			Reason: reasonNoAuditScope, TargetType: "route", TargetID: route,
			IP: httpx.ClientIP(ctx), UserAgent: userAgent,
		}); err != nil {
			d.Log.Error("could not audit a refused audit read; refusing with 500", "error", err)
			return audit.Filter{}, huma.Error500InternalServerError("internal error")
		}
	}
	return audit.Filter{}, httpx.Problem(http.StatusForbidden, "", "You do not have permission to do this.")
}

// scoped adds the caller's scope to the filters they asked for.
func scoped(ctx context.Context, d AuditDeps, route, userAgent string, f audit.Filter) (audit.Filter, error) {
	scope, err := auditScope(ctx, d, route, userAgent)
	if err != nil {
		return audit.Filter{}, err
	}
	f.Scoped, f.Envs, f.AnyEnv = scope.Scoped, scope.Envs, scope.AnyEnv
	return f, nil
}

// RegisterAudit registers the audit log routes: the filtered list, the actor
// list behind the user filter and the CSV or JSON export. They are open to any
// signed-in user and the handler works out the scope of the caller (D-09): an
// instance admin reads the whole log, an Admin of environments reads the entries
// on those environments, and anyone else is refused with 403 and the refusal is
// audited (D-49). Reading the log is not itself audited.
func RegisterAudit(r *httpx.Router, d AuditDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	// SelfService, like the environment list: the policy only needs a signed-in
	// caller, and the handler narrows what that caller may read.
	need := rbac.SelfService
	fail := func(what string, err error) error {
		d.Log.Error("audit request failed", "step", what, "error", err)
		return huma.Error500InternalServerError("internal error")
	}

	httpx.Register(r, huma.Operation{
		OperationID: "listAudit",
		Method:      http.MethodGet,
		Path:        "/api/audit",
		Summary:     "List audit entries",
		Description: "Newest first, 50 per page by default and 100 at most, filtered by actor, one or more action types and an " +
			"inclusive time range, and optionally by one bulk operation (bulk, the bulkId of a container action). The export takes the same filters. Pages are cut by insertion sequence, so entries that share a timestamp are never repeated " +
			"or skipped; pass nextCursor back as cursor for the next page. 422 invalid_time_range for a malformed from or to, and 422 for a bulk that is not a UUID. " +
			"An instance admin reads every entry. An Admin of one or more environments (environments.audit) reads only the " +
			"entries on those environments, or on every environment when they are Admin of all of them, never an instance-level " +
			"event such as a sign-in or a settings change; the scope is applied before paging. Anyone else gets 403, audited as " +
			"permission.denied with reason no_audit_scope.",
		Tags: []string{"audit"},
	}, need, func(ctx context.Context, in *ListAuditInput) (*AuditListOutput, error) {
		f, err := in.filter()
		if err != nil {
			return nil, err
		}
		if f, err = scoped(ctx, d, "GET /api/audit", in.UserAgent, f); err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit <= 0 {
			limit = auditPageDefault
		}
		limit = min(limit, auditPageMax)
		rows, next, err := audit.Query(ctx, d.Store, f, in.Cursor, limit)
		if err != nil {
			return nil, fail("query audit", err)
		}
		items := make([]AuditEntryDTO, len(rows))
		for i, row := range rows {
			items[i] = auditEntryDTO(row)
		}
		body := AuditListBody{Items: items, RetentionDays: d.RetentionDays}
		if next != 0 {
			body.NextCursor = &next
		}
		return &AuditListOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "listAuditActors",
		Method:      http.MethodGet,
		Path:        "/api/audit/actors",
		Summary:     "List the actor names in the audit log",
		Description: "The distinct actor names seen, sorted, at most 200, for the user filter, each marked when the user behind it " +
			"has been deleted. Under the same scope as the list: an Admin of environments sees only the actors of entries on " +
			"those environments. 403 as for the list.",
		Tags: []string{"audit"},
	}, need, func(ctx context.Context, in *AuditActorsInput) (*AuditActorsOutput, error) {
		scope, err := auditScope(ctx, d, "GET /api/audit/actors", in.UserAgent)
		if err != nil {
			return nil, err
		}
		actors, err := audit.Actors(ctx, d.Store, scope)
		if err != nil {
			return nil, fail("list actors", err)
		}
		body := make([]AuditActorDTO, len(actors))
		for i, a := range actors {
			body[i] = AuditActorDTO{Name: a.Name, Deleted: a.Deleted}
		}
		return &AuditActorsOutput{CacheControl: "no-store", Body: body}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "exportAudit",
		Method:      http.MethodGet,
		Path:        "/api/audit/export",
		Summary:     "Export the audit log as CSV or JSON",
		Description: "Streams every entry matching the filters, newest first, as a download named " +
			"bayplan-audit-<UTC timestamp>.csv or .json. CSV follows RFC 4180, and any cell that starts with =, +, -, @, a tab " +
			"or a carriage return is prefixed with a single quote so a spreadsheet cannot run it as a formula. Exports are " +
			"reads and are not audited. Under the same scope as the list: an Admin of environments exports only the entries on " +
			"those environments. 403 as for the list.",
		Tags: []string{"audit"},
		Responses: map[string]*huma.Response{"200": {
			Description: "The export",
			Content: map[string]*huma.MediaType{
				"text/csv":         {Schema: &huma.Schema{Type: "string"}},
				"application/json": {Schema: &huma.Schema{Type: "array", Items: &huma.Schema{Type: "object"}}},
			},
			Headers: map[string]*huma.Param{"Content-Disposition": {Schema: &huma.Schema{Type: "string"}}},
		}},
	}, need, func(ctx context.Context, in *ExportAuditInput) (*huma.StreamResponse, error) {
		f, err := in.filter()
		if err != nil {
			return nil, err
		}
		if f, err = scoped(ctx, d, "GET /api/audit/export", in.UserAgent, f); err != nil {
			return nil, err
		}
		contentType, ext, write := "text/csv; charset=utf-8", "csv", audit.ExportCSV
		if in.Format == "json" {
			contentType, ext, write = "application/json", "json", audit.ExportJSON
		}
		name := "bayplan-audit-" + d.Now().UTC().Format("20060102T150405Z") + "." + ext
		return &huma.StreamResponse{Body: func(hctx huma.Context) {
			hctx.SetHeader("Content-Type", contentType)
			hctx.SetHeader("Content-Disposition", "attachment; filename="+strconv.Quote(name))
			hctx.SetHeader("Cache-Control", "no-store")
			hctx.SetHeader("X-Content-Type-Options", "nosniff")
			hctx.SetStatus(http.StatusOK)
			// Headers are on the wire once the first batch is written, so a
			// failure past that point can only be logged; the client sees a
			// truncated download.
			if err := write(ctx, d.Store, f, hctx.BodyWriter()); err != nil {
				d.Log.Error("audit export failed", "format", ext, "error", err)
			}
		}}, nil
	})
}
