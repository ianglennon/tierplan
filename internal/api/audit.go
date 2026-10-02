// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"context"
	"log/slog"
	"net/http"
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
}

// AuditFilterInput holds the filters the list and the export share.
type AuditFilterInput struct {
	User   string   `query:"user" maxLength:"128" doc:"Only entries by this actor, matched exactly against the actor name"`
	Action []string `query:"action,explode" maxItems:"50" doc:"Only entries of these action types; repeat the parameter to give several"`
	From   string   `query:"from" format:"date-time" doc:"Only entries at or after this time (RFC 3339)"`
	To     string   `query:"to" format:"date-time" doc:"Only entries at or before this time (RFC 3339)"`
}

// filter turns the query parameters into an audit.Filter. A time that does not
// parse is a 422 (huma already rejects most before this runs).
func (in *AuditFilterInput) filter() (audit.Filter, error) {
	f := audit.Filter{Actor: in.User, Actions: in.Action}
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

// AuditActorsOutput lists the actor names seen.
type AuditActorsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         []string
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
	}
}

// RegisterAudit registers the audit log routes: the filtered list, the actor
// list behind the user filter and the CSV or JSON export. All need audit.read,
// which only the seeded admin holds in Phase 1 (D-43); the policy refuses
// anyone else with 403 and audits the denial (D-49). Reading the log is not
// itself audited.
func RegisterAudit(r *httpx.Router, d AuditDeps) {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	need := rbac.Requirement{Perm: rbac.PermAuditRead, Scope: rbac.ScopeGlobal}
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
			"inclusive time range. Pages are cut by insertion sequence, so entries that share a timestamp are never repeated " +
			"or skipped; pass nextCursor back as cursor for the next page. 422 invalid_time_range for a malformed from or to. " +
			"Needs audit.read.",
		Tags: []string{"audit"},
	}, need, func(ctx context.Context, in *ListAuditInput) (*AuditListOutput, error) {
		f, err := in.filter()
		if err != nil {
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
		Description: "The distinct actor names seen, sorted, at most 200, for the user filter. Needs audit.read.",
		Tags:        []string{"audit"},
	}, need, func(ctx context.Context, _ *struct{}) (*AuditActorsOutput, error) {
		names, err := audit.Actors(ctx, d.Store)
		if err != nil {
			return nil, fail("list actors", err)
		}
		return &AuditActorsOutput{CacheControl: "no-store", Body: names}, nil
	})

	httpx.Register(r, huma.Operation{
		OperationID: "exportAudit",
		Method:      http.MethodGet,
		Path:        "/api/audit/export",
		Summary:     "Export the audit log as CSV or JSON",
		Description: "Streams every entry matching the filters, newest first, as a download named " +
			"bayplan-audit-<UTC timestamp>.csv or .json. CSV follows RFC 4180, and any cell that starts with =, +, -, @, a tab " +
			"or a carriage return is prefixed with a single quote so a spreadsheet cannot run it as a formula. Exports are " +
			"reads and are not audited. Needs audit.read.",
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
