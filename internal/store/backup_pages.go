package store

import (
	"errors"

	"github.com/meiome/onlybackup/internal/model"
)

// BackupPage bounds each transport response without truncating the catalogue.
// The high-water rowid excludes deposits inserted after the first page. IDs
// provide a stable cursor even when statuses change between requests.
func (s *Store) BackupPage(after string, through int64) (model.BackupPage, error) {
	var page model.BackupPage
	if through < 0 || (after != "" && (!model.ValidID(after) || through == 0)) {
		return page, errors.New("cursore catalogo non valido")
	}
	if after == "" {
		if err := s.db.QueryRow("SELECT COALESCE(MAX(rowid),0) FROM backups").Scan(&through); err != nil {
			return page, err
		}
	}
	backups, err := s.queryBackups("SELECT "+s.backupCols()+" FROM backups WHERE rowid<=? AND id>? ORDER BY id LIMIT ?", through, after, model.BackupPageSize+1)
	if err != nil {
		return page, err
	}
	page.Through = through
	if len(backups) > model.BackupPageSize {
		backups = backups[:model.BackupPageSize]
		page.Next = backups[len(backups)-1].ID
	}
	page.Backups = backups
	return page, nil
}
