// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package dockerx

import (
	"context"
	"errors"
	"io"
	"time"
)

// ServiceLogOptions say which part of a service's or task's log to read.
type ServiceLogOptions struct {
	Follow      bool
	Since, Tail string
}

// ServiceLogRecord is one record of a service log.
type ServiceLogRecord struct {
	T                                          time.Time
	Raw                                        string
	TaskID, NodeID, ServiceID, Message, Stream string
}

func (c *Client) ServiceLogs(ctx context.Context, serviceID string, o ServiceLogOptions) (io.ReadCloser, bool, error) {
	return nil, false, errors.New("not implemented")
}

func (c *Client) TaskLogs(ctx context.Context, taskID string, o ServiceLogOptions) (io.ReadCloser, bool, error) {
	return nil, false, errors.New("not implemented")
}

func ParseServiceLogRecord(s string) (ServiceLogRecord, error) {
	return ServiceLogRecord{}, errors.New("not implemented")
}

func ServiceLogFrames(r io.Reader, tty bool, emit func(stream string, rec []byte)) error {
	return errors.New("not implemented")
}
