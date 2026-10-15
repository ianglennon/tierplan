// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockertest

import (
	"net/url"
	"time"
)

// FakeServiceLogLine is one line a task of a service wrote.
type FakeServiceLogLine struct {
	TaskID, NodeID, Stream string
	Time                   time.Time
	Text                   string
}

func (f *Fake) PushServiceLog(service string, l FakeServiceLogLine) {}

func (f *Fake) ServiceLogQueries() []url.Values { return nil }
