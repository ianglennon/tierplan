// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/jobs"
)

// jobStream is one server-sent event stream of a job in the hub: what a pull and a stack deploy
// share. The caller has already decided that the person may read the job (and answered 404 if
// not); serveJobStream owns the limit on open streams, the resume, the headers, the heartbeat and
// the frames.
type jobStream struct {
	Hub   *jobs.Hub
	JobID string
	// Key is who the open streams are counted for, and Limiter holds the count.
	Key     string
	Limiter *streamLimiter
	// What names the sort of job in the refusal sentences ("pull", "deploy").
	What string
	// GoneType and GoneDetail are the 404 for a job the hub no longer has.
	GoneType, GoneDetail string
	// TooManyType is the 429 problem type for a person who has the most streams open.
	TooManyType string
	// Heartbeat is how often an idle stream gets a ping, and MaxLifetime how long one stream
	// may live.
	Heartbeat, MaxLifetime time.Duration
	Log                    *slog.Logger
	// Snapshot is the body of the first frame: the job as it stands. It has no event ID.
	Snapshot func(jobs.Job) any
	// Encode turns an event into the body of its frame, and false for an event that has no
	// frame. The frame is named after the event's type (item, output, done). It is also called
	// with a done event made from the job when the stream finds the job already ended.
	Encode func(jobs.Event) (any, bool)
}

// serveJobStream follows one job: the job as it stands as a snapshot frame, then a frame for each
// event and a done frame at the end, each with the hub's event ID. A browser that reconnects
// sends the last ID it saw in Last-Event-ID (or lastEventId in the query) and is sent a new
// snapshot and what it missed after that ID. An idle stream gets a comment line every Heartbeat,
// and each one re-checks that the caller is still allowed in.
//
// The job does not depend on this stream: it runs on the server, and closing the stream does not
// stop it.
func serveJobStream(w http.ResponseWriter, req *http.Request, js jobStream) {
	ctx := req.Context()
	release, ok := js.Limiter.acquire(js.Key)
	if !ok {
		writeJSONProblem(w, http.StatusTooManyRequests, js.TooManyType,
			fmt.Sprintf("You already have %d %s streams open. Close one and try again.", js.Limiter.max, js.What))
		return
	}
	defer release()

	// A browser's EventSource sends Last-Event-ID itself when it reconnects on its own. The
	// SPA reopens the stream by hand, which cannot set a header, so it says the same in
	// the query.
	var after uint64
	last := req.Header.Get("Last-Event-ID")
	if last == "" {
		last = req.URL.Query().Get("lastEventId")
	}
	if last != "" {
		if n, err := strconv.ParseUint(last, 10, 64); err == nil {
			after = n
		}
	}
	snap, ch, cancel, err := js.Hub.Subscribe(ctx, js.JobID, after)
	if err != nil {
		writeJSONProblem(w, http.StatusNotFound, js.GoneType, js.GoneDetail)
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}
	write := func(format string, args ...any) bool {
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	frame := func(id, event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			js.Log.Error("encode a "+js.What+" frame", "error", err)
			return false
		}
		if id == "" {
			return write("event: %s\ndata: %s\n\n", event, data)
		}
		return write("id: %s\nevent: %s\ndata: %s\n\n", id, event, data)
	}
	// The snapshot has no id: it is not an event.
	if !frame("", "snapshot", js.Snapshot(snap)) {
		return
	}

	beat := time.NewTicker(js.Heartbeat)
	defer beat.Stop()
	life := time.NewTimer(js.MaxLifetime)
	defer life.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-life.C:
			return
		case ev, open := <-ch:
			if !open {
				// The channel closed without a done event: the job had already finished when
				// the stream opened and nothing newer than Last-Event-ID was left to replay.
				if job, ok := js.Hub.Get(js.JobID); ok && job.Finished() {
					if body, ok := js.Encode(jobs.Event{Type: jobs.EventDone, JobID: js.JobID, Job: &job}); ok {
						_ = frame("", "done", body)
					}
				}
				return
			}
			body, ok := js.Encode(ev)
			if !ok {
				continue
			}
			if !frame(strconv.FormatUint(ev.ID, 10), ev.Type, body) {
				return
			}
			if ev.Type == jobs.EventDone {
				return
			}
		case <-beat.C:
			if !httpx.StillAuthorized(ctx) {
				return
			}
			if !write(": ping\n\n") {
				return
			}
		}
	}
}
