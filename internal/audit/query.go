// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package audit

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ianglennon/tierplan/internal/store"
	"github.com/ianglennon/tierplan/internal/store/sqlc"
)

// Paging limits. A page of the viewer is 50 entries and never more than 100;
// exports read the same query in larger batches so memory stays flat however
// many entries match.
const (
	defaultPageSize = 50
	maxPageSize     = 100
	exportBatchSize = 500
)

// Filter narrows the audit log. The zero value matches every entry. Actor is an
// exact match on the stored actor name, Actions matches any of the listed
// action keys, and From and To are inclusive bounds on the entry time. Bulk
// keeps only the entries whose bulk detail is that bulk-operation ID (D-14).
//
// Scoped narrows the log to the entries of certain environments, for a reader
// who is not an instance admin (D-09): an entry matches when its environment is
// in Envs, or, with AnyEnv, when it has an environment at all. An instance-level
// event has none, so it never matches a scoped read. The scope is applied in SQL
// before paging, so cursors, the actor list and both exports see exactly the
// scoped entries. A scoped filter with no Envs and no AnyEnv matches nothing.
type Filter struct {
	Actor   string
	Actions []string
	From    time.Time
	To      time.Time
	Bulk    string

	Scoped bool
	Envs   []string
	AnyEnv bool
}

// scope encodes the scope as query parameters.
func (f Filter) scope() (scoped, anyEnv int64, envs string, err error) {
	ids := f.Envs
	if ids == nil {
		ids = []string{}
	}
	enc, err := json.Marshal(ids)
	if err != nil {
		return 0, 0, "", fmt.Errorf("audit: encode environments: %w", err)
	}
	if f.Scoped {
		scoped = 1
	}
	if f.AnyEnv {
		anyEnv = 1
	}
	return scoped, anyEnv, string(enc), nil
}

// Row is one audit entry as read: who (actor), what (action, target), where
// (environment, IP, user agent), when, and the result. It deliberately has no
// field for the tamper-evidence chain, so no read path can leak one (D-39).
type Row struct {
	Seq        int64             `json:"seq"`
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	ActorType  string            `json:"actorType"`
	ActorName  string            `json:"actor"`
	Action     string            `json:"action"`
	TargetType string            `json:"targetType"`
	TargetID   string            `json:"target"`
	EnvID      string            `json:"environment,omitempty"`
	Result     string            `json:"result"`
	Reason     string            `json:"reason"`
	IP         string            `json:"ip"`
	UserAgent  string            `json:"userAgent"`
	Details    map[string]string `json:"details"`
	// ActorDeleted is true when the user behind the entry has since been deleted
	// (D-22c). It is worked out when the entry is read, from the user's tombstone;
	// the signed row is never rewritten.
	ActorDeleted bool `json:"actorDeleted"`
}

// NamedActor is an actor name the log holds, and whether the user behind it has
// been deleted.
type NamedActor struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

// params turns f into query parameters for one page.
func (f Filter) params(cursor int64, limit int) (sqlc.ListAuditPageParams, error) {
	actions := f.Actions
	if actions == nil {
		actions = []string{}
	}
	enc, err := json.Marshal(actions)
	if err != nil {
		return sqlc.ListAuditPageParams{}, fmt.Errorf("audit: encode actions: %w", err)
	}
	scoped, anyEnv, envs, err := f.scope()
	if err != nil {
		return sqlc.ListAuditPageParams{}, err
	}
	p := sqlc.ListAuditPageParams{
		Actor: f.Actor, Actions: string(enc), Bulk: f.Bulk, Cursor: cursor, Lim: int64(limit),
		Scoped: scoped, AnyEnv: anyEnv, Envs: envs,
	}
	if !f.From.IsZero() {
		p.FromTs = f.From.UTC().UnixNano()
	}
	if !f.To.IsZero() {
		p.ToTs = f.To.UTC().UnixNano()
	}
	return p, nil
}

// page reads up to limit entries older than cursor (0 means from the newest),
// newest first. Entries are ordered by insertion sequence, never by time, so
// entries that share a timestamp keep a deterministic order and a cursor never
// repeats or skips one.
func page(ctx context.Context, s *store.Store, f Filter, cursor int64, limit int) ([]Row, error) {
	p, err := f.params(cursor, limit)
	if err != nil {
		return nil, err
	}
	recs, err := s.RQ.ListAuditPage(ctx, p)
	if err != nil {
		return nil, fmt.Errorf("audit: list entries: %w", err)
	}
	rows := make([]Row, len(recs))
	for i, r := range recs {
		rows[i] = toRow(r)
	}
	return rows, nil
}

func toRow(r sqlc.ListAuditPageRow) Row {
	details := map[string]string{}
	if err := json.Unmarshal([]byte(r.Details), &details); err != nil || details == nil {
		details = map[string]string{}
	}
	return Row{
		Seq: r.Seq, ID: r.ID, Time: time.Unix(0, r.Ts).UTC(),
		ActorType: r.ActorType, ActorName: r.ActorName,
		Action: r.Action, TargetType: r.TargetType, TargetID: r.TargetID, EnvID: r.EnvID.String,
		Result: r.Result, Reason: r.Reason, IP: r.Ip, UserAgent: r.UserAgent, Details: details,
		ActorDeleted: r.ActorDeleted != 0,
	}
}

// Query returns one page of entries matching f, newest first. cursor is the
// next value of the previous page, or 0 for the first page; limit is clamped to
// 1..100 and defaults to 50. next is the sequence to pass as the cursor for the
// following page, or 0 when there is none: one extra entry is read to know, so
// a log that is an exact multiple of the page size does not end in an empty page.
func Query(ctx context.Context, s *store.Store, f Filter, cursor int64, limit int) (rows []Row, next int64, err error) {
	switch {
	case limit <= 0:
		limit = defaultPageSize
	case limit > maxPageSize:
		limit = maxPageSize
	}
	rows, err = page(ctx, s, f, cursor, limit+1)
	if err != nil {
		return nil, 0, err
	}
	if len(rows) > limit {
		rows = rows[:limit]
		next = rows[limit-1].Seq
	}
	return rows, next, nil
}

// Actors returns the distinct actor names in the log that f's scope reaches,
// sorted, at most 200, each marked when the user behind it has been deleted. It
// feeds the user filter. Only the scope of f applies; its other fields narrow
// entries, not the list of who acted.
func Actors(ctx context.Context, s *store.Store, f Filter) ([]NamedActor, error) {
	scoped, anyEnv, envs, err := f.scope()
	if err != nil {
		return nil, err
	}
	recs, err := s.RQ.ListAuditActors(ctx, sqlc.ListAuditActorsParams{Scoped: scoped, AnyEnv: anyEnv, Envs: envs})
	if err != nil {
		return nil, fmt.Errorf("audit: list actors: %w", err)
	}
	actors := make([]NamedActor, len(recs))
	for i, r := range recs {
		actors[i] = NamedActor{Name: r.ActorName, Deleted: r.Deleted != 0}
	}
	return actors, nil
}

// each calls fn for every entry matching f, newest first, reading in batches.
func each(ctx context.Context, s *store.Store, f Filter, fn func(Row) error) error {
	var cursor int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := page(ctx, s, f, cursor, exportBatchSize)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(rows) < exportBatchSize {
			return nil
		}
		cursor = rows[len(rows)-1].Seq
	}
}

// csvHeader is the first row of every CSV export.
var csvHeader = []string{
	"id", "time", "actor_type", "actor", "action", "target_type", "target",
	"environment", "result", "reason", "ip", "user_agent", "details", "actor_deleted",
}

// csvSafe stops a spreadsheet from running a cell as a formula. Fields such as
// the user agent and the username of a failed sign-in are chosen by whoever
// made the request, and an admin opens the export in a spreadsheet, so a cell
// that starts with =, +, -, @, a tab or a carriage return gets a single quote in
// front (RESEARCH Pitfall 19, T-01-97).
func csvSafe(cell string) string {
	if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
		return "'" + cell
	}
	return cell
}

// ExportCSV writes every entry matching f, newest first, as RFC 4180 CSV with a
// header row. Every cell passes through csvSafe.
func ExportCSV(ctx context.Context, s *store.Store, f Filter, w io.Writer) error {
	cw := csv.NewWriter(w)
	rec := make([]string, len(csvHeader))
	if err := cw.Write(csvHeader); err != nil {
		return err
	}
	err := each(ctx, s, f, func(r Row) error {
		details, err := json.Marshal(r.Details)
		if err != nil {
			return fmt.Errorf("audit: encode details: %w", err)
		}
		cells := []string{
			r.ID, r.Time.Format(time.RFC3339Nano), r.ActorType, r.ActorName, r.Action,
			r.TargetType, r.TargetID, r.EnvID, r.Result, r.Reason, r.IP, r.UserAgent, string(details),
			strconv.FormatBool(r.ActorDeleted),
		}
		for i, c := range cells {
			rec[i] = csvSafe(c)
		}
		return cw.Write(rec)
	})
	if err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// ExportJSON writes every entry matching f, newest first, as one JSON array,
// streamed one object at a time.
func ExportJSON(ctx context.Context, s *store.Store, f Filter, w io.Writer) error {
	if _, err := io.WriteString(w, "["); err != nil {
		return err
	}
	first := true
	enc := json.NewEncoder(w)
	err := each(ctx, s, f, func(r Row) error {
		if !first {
			if _, err := io.WriteString(w, ","); err != nil {
				return err
			}
		}
		first = false
		return enc.Encode(r)
	})
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, "]\n")
	return err
}
