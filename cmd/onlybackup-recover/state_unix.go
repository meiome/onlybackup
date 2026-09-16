//go:build !windows

package main

import (
	"errors"
	"path/filepath"

	"github.com/meiome/onlybackup/internal/store"
)

func targetFromState(root, id string) (recoveryTarget, error) {
	var target recoveryTarget
	database, err := store.Open(filepath.Join(root, "metadata.db"), true)
	if err != nil {
		return target, err
	}
	defer database.Close()
	backup, err := database.Backup(id)
	if err != nil {
		return target, err
	}
	if backup.Status != "complete" {
		return target, errors.New("backup non completato")
	}
	return recoveryTarget{
		ID:            backup.ID,
		Source:        filepath.Join(root, "backups", backup.ID+".backup"),
		Size:          backup.Size,
		SHA256:        backup.SHA256,
		ContentFormat: backup.ContentFormat,
		OriginalName:  backup.OriginalName,
	}, nil
}
