// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ianglennon/tierplan/internal/acme"
	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
	"github.com/ianglennon/tierplan/internal/rbac"
)

// goneACMERun is the 404 for a run the hub does not have, or a job that is not a certificate run.
const goneACMERun = "This run is no longer here. TierPlan keeps finished runs for 10 minutes."

// The states of a run as the stream reports them.
const (
	acmeRunRunning   = "running"
	acmeRunDone      = "done"
	acmeRunFailed    = "failed"
	acmeRunCancelled = "cancelled"
)

// HTTPSACMEStepBody is one step of a run. It carries the step's id, its state, a class code for a
// failure and a fixed sentence; never anything the CA said and never key material.
type HTTPSACMEStepBody struct {
	ID      string `json:"id" enum:"account,order,challenge,validate,finalize,stored" doc:"The step"`
	Label   string `json:"label" doc:"The step's name in English"`
	State   string `json:"state" enum:"waiting,running,done,failed,cancelled,skipped" doc:"Where the step is. skipped is a step that was not needed (the account was already registered) or not reached"`
	Code    string `json:"code,omitempty" doc:"The class of a failure, for example validation_dns; the same codes as lastError in the status"`
	Message string `json:"message,omitempty" doc:"A fixed sentence about the step"`
}

// HTTPSACMERunFrameBody is a run as the stream sends it: in the snapshot frame as it stands and in the
// done frame as it ended. An item frame carries one HTTPSACMEStepBody.
type HTTPSACMERunFrameBody struct {
	JobID     string              `json:"jobId" doc:"The run's job"`
	Kind      string              `json:"kind" enum:"issue,renew" doc:"A first issue or a renewal"`
	Trigger   string              `json:"trigger" enum:"admin,schedule,resume" doc:"What started it"`
	State     string              `json:"state" enum:"running,done,failed,cancelled" doc:"How the run is: done is a certificate stored and served, failed names a step that failed"`
	StartedAt time.Time           `json:"startedAt" doc:"When it started"`
	Items     []HTTPSACMEStepBody `json:"items" doc:"The six steps, in order"`
}

// acmeStepBody maps a job item to the step the stream sends.
func acmeStepBody(it jobs.Item) HTTPSACMEStepBody {
	return HTTPSACMEStepBody{ID: it.ID, Label: it.Label, State: it.State, Code: it.Code, Message: it.Message}
}

// acmeRunState reads the state of a run from its steps. The hub's own state counts a skipped step
// (the account was already registered) as neither done nor failed, so a successful run ends
// "partial" there; the run is done when it ended with no step failed or cancelled.
func acmeRunState(j jobs.Job) string {
	if !j.Finished() {
		return acmeRunRunning
	}
	cancelled := j.State == jobs.JobCancelled
	for _, it := range j.Items {
		switch it.State {
		case jobs.ItemFailed:
			return acmeRunFailed
		case jobs.ItemCancelled:
			cancelled = true
		}
	}
	if cancelled {
		return acmeRunCancelled
	}
	return acmeRunDone
}

// acmeRunFrameBody maps a job to the run the stream sends.
func acmeRunFrameBody(j jobs.Job) HTTPSACMERunFrameBody {
	b := HTTPSACMERunFrameBody{
		JobID: j.ID, Kind: j.Meta["kind"], Trigger: j.Meta["trigger"], State: acmeRunState(j),
		StartedAt: j.CreatedAt.UTC(), Items: make([]HTTPSACMEStepBody, 0, len(j.Items)),
	}
	for _, it := range j.Items {
		b.Items = append(b.Items, acmeStepBody(it))
	}
	return b
}

// registerHTTPSACMEStream wires the run stream. It needs settings.read: any reader of the HTTPS
// settings may watch a run, whoever started it (D-09).
func registerHTTPSACMEStream(r *httpx.Router, d HTTPSDeps) {
	if d.Heartbeat <= 0 {
		d.Heartbeat = defaultHeartbeat
	}
	if d.MaxLifetime <= 0 {
		d.MaxLifetime = defaultMaxStreamLifetime
	}
	if d.MaxStreamsPerUser <= 0 {
		d.MaxStreamsPerUser = defaultMaxStreams
	}
	lim := &streamLimiter{max: d.MaxStreamsPerUser, open: map[string]int{}}
	read := rbac.Requirement{Perm: rbac.PermSettingsRead, Scope: rbac.ScopeGlobal}
	r.Guarded(http.MethodGet, "/api/https/acme/runs/{jobId}/stream", read, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		serveACMERunStream(w, req, d, lim)
	}))
}

// serveACMERunStream follows one certificate run through the shared job stream (jobstream.go): the run
// as it stands as a snapshot frame, then an item frame for each step change and a done frame at the
// end. The run does not depend on the stream, and any settings.read user may follow any run; a job
// that is not a certificate run answers 404 like one that is gone.
func serveACMERunStream(w http.ResponseWriter, req *http.Request, d HTTPSDeps, lim *streamLimiter) {
	p := httpx.PrincipalFrom(req.Context())
	if p == nil {
		writeJSONProblem(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if d.Jobs == nil {
		writeJSONProblem(w, http.StatusServiceUnavailable, problemHTTPSUnavailable, "Let's Encrypt is not available on this server.")
		return
	}
	id := chi.URLParam(req, "jobId")
	if job, ok := d.Jobs.Get(id); !ok || job.Kind != acme.JobKind {
		writeJSONProblem(w, http.StatusNotFound, problemACMERunNotFound, goneACMERun)
		return
	}
	serveJobStream(w, req, jobStream{
		Hub: d.Jobs, JobID: id, Key: string(p.Type) + ":" + p.ID, Limiter: lim, What: "acme run",
		GoneType: problemACMERunNotFound, GoneDetail: goneACMERun, TooManyType: problemTooManyStreams,
		Heartbeat: d.Heartbeat, MaxLifetime: d.MaxLifetime, Log: d.Log,
		Snapshot: func(j jobs.Job) any { return acmeRunFrameBody(j) },
		Encode: func(ev jobs.Event) (any, bool) {
			switch {
			case ev.Type == jobs.EventItem && ev.Item != nil:
				return acmeStepBody(*ev.Item), true
			case ev.Type == jobs.EventDone && ev.Job != nil:
				return acmeRunFrameBody(*ev.Job), true
			}
			return nil, false
		},
	})
}
