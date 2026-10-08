// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"time"
)

// scheduledName is the one name a scheduled backup has: bayplan-backup- then the
// UTC date and time to the minute. Nothing else in the backups folder (the
// pre-migration snapshots bayplan-<time>.db, the pre-restore snapshots
// prerestore-<time>.db, a backup someone copied in by hand) matches it, and
// nothing that does not match it is listed, served or deleted (T-03-112,
// T-03-113, RESEARCH Pitfall 9).
var scheduledName = regexp.MustCompile(`^bayplan-backup-[0-9]{8}-[0-9]{4}\.bayplan$`)

// scheduledNamePrefix, scheduledNameLayout and scheduledNameSuffix build that name.
const (
	scheduledNamePrefix = "bayplan-backup-"
	scheduledNameLayout = "20060102-1504"
	scheduledNameSuffix = FileExtension
)

// ScheduledName is the file name of a scheduled backup made at t (UTC).
func ScheduledName(t time.Time) string {
	return scheduledNamePrefix + t.UTC().Format(scheduledNameLayout) + scheduledNameSuffix
}

// IsScheduledName reports whether name is exactly the name of a scheduled backup.
func IsScheduledName(name string) bool { return scheduledName.MatchString(name) }

// ErrNoSuchBackup is returned for a name that is not a scheduled backup's name or
// is not a regular file in the folder.
var ErrNoSuchBackup = errors.New("no such backup")

// BackupFile is one scheduled backup on disk.
type BackupFile struct {
	Name string
	Size int64
	// Created is the time in the name (UTC, to the minute); a name that is not a
	// real date falls back to the file's modification time.
	Created time.Time
}

func fileOf(name string, info fs.FileInfo) BackupFile {
	created, err := time.Parse(scheduledNameLayout, name[len(scheduledNamePrefix):len(name)-len(scheduledNameSuffix)])
	if err != nil {
		created = info.ModTime()
	}
	return BackupFile{Name: name, Size: info.Size(), Created: created.UTC()}
}

// ListBackupFiles lists the scheduled backups in dir, newest first. Only regular
// files whose names match the pattern are listed; a folder that does not exist yet
// holds none.
func ListBackupFiles(dir string) ([]BackupFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []BackupFile{}, nil
		}
		return nil, fmt.Errorf("list the backups folder: %w", err)
	}
	files := []BackupFile{}
	for _, e := range entries {
		if !e.Type().IsRegular() || !IsScheduledName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed since the listing
		}
		files = append(files, fileOf(e.Name(), info))
	}
	// The time in the name sorts in time order.
	sort.Slice(files, func(i, j int) bool { return files[i].Name > files[j].Name })
	return files, nil
}

// OpenBackupFile opens one scheduled backup for reading. The name is matched
// against the fixed pattern before anything touches the file system, and the
// file is then opened through a root on the folder, so neither a crafted name
// nor a link inside the folder can reach a file outside it. A name that does not
// match, a missing file and anything that is not a regular file are all
// ErrNoSuchBackup.
func OpenBackupFile(dir, name string) (*os.File, BackupFile, error) {
	if !IsScheduledName(name) {
		return nil, BackupFile{}, ErrNoSuchBackup
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, BackupFile{}, ErrNoSuchBackup
		}
		return nil, BackupFile{}, fmt.Errorf("open the backups folder: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isEscape(err) {
			return nil, BackupFile{}, ErrNoSuchBackup
		}
		return nil, BackupFile{}, fmt.Errorf("open the backup: %w", err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		if err != nil {
			return nil, BackupFile{}, fmt.Errorf("measure the backup: %w", err)
		}
		return nil, BackupFile{}, ErrNoSuchBackup
	}
	return f, fileOf(name, info), nil
}

// isEscape reports whether err is the root refusing a path that leads out of the
// folder, such as a link to a file elsewhere.
func isEscape(err error) bool {
	var pe *fs.PathError
	// The sentinel is not exported by os; its text is part of the Go 1 contract
	// that TestAFileNameThatIsALinkOutOfTheFolderIsNotServed pins.
	return errors.As(err, &pe) && pe.Err != nil && pe.Err.Error() == "path escapes from parent"
}
