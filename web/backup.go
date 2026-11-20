package web

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// backupEvery is how old the newest backup may get before a new one is made.
	backupEvery = 24 * time.Hour
	// backupsKept is how many daily backups are kept; older ones are removed.
	backupsKept = 7
	// backupCheckInterval is how often the backup loop looks at the newest backup.
	backupCheckInterval = time.Hour
	backupDirName       = "backups"
	backupPrefix        = "jobs-"
)

// backupStore can copy its database to a file.
type backupStore interface {
	Backup(ctx context.Context, path string) error
}

// BackupInfo describes the backups for the dashboard.
type BackupInfo struct {
	Enabled bool
	Dir     string
	Last    time.Time
	Count   int
	Size    int64
}

func (s *Service) backupDir() string {
	return filepath.Join(s.dataFolder, backupDirName)
}

// Backups lists the existing backups, newest first.
func (s *Service) backups() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(s.backupDir())
	if err != nil {
		return nil, err
	}

	out := entries[:0]

	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), backupPrefix) && strings.HasSuffix(e.Name(), ".db") {
			out = append(out, e)
		}
	}

	// Names hold the date, so name order is time order.
	sort.Slice(out, func(a, b int) bool { return out[a].Name() > out[b].Name() })

	return out, nil
}

// BackupStatus reports the newest backup and how many are kept.
func (s *Service) BackupStatus() BackupInfo {
	info := BackupInfo{Dir: s.backupDir()}

	if _, ok := s.repo.(backupStore); !ok {
		return info
	}

	info.Enabled = true

	entries, err := s.backups()
	if err != nil {
		return info
	}

	info.Count = len(entries)

	for i, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}

		if i == 0 {
			info.Last = fi.ModTime()
		}

		info.Size += fi.Size()
	}

	return info
}

// BackupNow copies the database into the backups folder and removes the oldest
// copies beyond backupsKept. It returns the new file path.
func (s *Service) BackupNow(ctx context.Context) (string, error) {
	store, ok := s.repo.(backupStore)
	if !ok {
		return "", fmt.Errorf("this database does not support backups")
	}

	if err := os.MkdirAll(s.backupDir(), 0o750); err != nil {
		return "", err
	}

	path := filepath.Join(s.backupDir(), backupPrefix+time.Now().UTC().Format("20060102-150405")+".db")

	if err := store.Backup(ctx, path); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}

	entries, err := s.backups()
	if err == nil {
		for _, e := range entries[min(len(entries), backupsKept):] {
			_ = os.Remove(filepath.Join(s.backupDir(), e.Name()))
		}
	}

	return path, nil
}

// RunBackups makes a backup when the newest one is older than a day, checking
// every hour, until ctx ends.
func (s *Service) RunBackups(ctx context.Context) {
	if _, ok := s.repo.(backupStore); !ok {
		return
	}

	ticker := time.NewTicker(backupCheckInterval)
	defer ticker.Stop()

	for {
		if last := s.BackupStatus().Last; time.Since(last) >= backupEvery {
			if path, err := s.BackupNow(ctx); err != nil {
				log.Printf("database backup failed: %v", err)
			} else {
				log.Printf("database backed up to %s", path)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
