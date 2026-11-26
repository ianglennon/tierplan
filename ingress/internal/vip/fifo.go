// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package vip

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/ianglennon/tierplan/proto/ingressbundle"
)

// maxFIFOLine is the longest FIFO line readLines passes on. keepalived's lines are `INSTANCE "NAME" STATE
// prio`, well under this; a longer one is not keepalived's and is dropped whole.
const maxFIFOLine = 512

// parseFIFOLine reads one line keepalived writes to vrrp_notify_fifo: `INSTANCE "TIERPLAN_VIP" MASTER 100`. It
// returns the state when the line is for this helper's instance and names one of the four states the report
// carries; anything else (another instance, a group line, a malformed line) is ignored.
func parseFIFOLine(line string) (state string, ok bool) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != "INSTANCE" || f[1] != strconv.Quote(InstanceName) {
		return "", false
	}
	if _, err := strconv.Atoi(f[3]); err != nil {
		return "", false
	}
	switch f[2] {
	case ingressbundle.VIPStateMaster, ingressbundle.VIPStateBackup, ingressbundle.VIPStateFault, ingressbundle.VIPStateStop:
		return f[2], true
	}
	return "", false
}

// readLines calls fn with each newline-terminated line of r, without the newline, until r ends or fails. A
// line longer than maxLen bytes is dropped whole, so a writer that never sends a newline cannot grow memory.
func readLines(r io.Reader, maxLen int, fn func(string)) {
	br := bufio.NewReaderSize(r, maxLen)
	for {
		line, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			fn(strings.TrimSuffix(string(line), "\n"))
		case errors.Is(err, bufio.ErrBufferFull):
			// Skip the rest of the overlong line.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return
			}
		default:
			return
		}
	}
}
