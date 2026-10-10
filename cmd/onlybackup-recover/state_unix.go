//go:build !windows

package main

import (
	"errors"
	"path/filepath"

	"github.com/meiome/onlybackup/internal/model"
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
	switch backup.Status {
	case model.BackupComplete:
	case model.BackupDeleting:
		return target, errors.New("backup con richiesta di quarantena pendente; annullarla dalla console amministrativa")
	case model.BackupQuarantined:
		return target, errors.New("backup recuperabile dalla quarantena tramite onlybackup-admin quarantena")
	case model.BackupPurging:
		return target, errors.New("backup in eliminazione definitiva; recupero non disponibile")
	case model.BackupDeleted:
		return target, errors.New("backup eliminato definitivamente; rimane solo lo storico di catalogo")
	default:
		return target, errors.New("backup non disponibile per il recupero")
	}
	return recoveryTarget{
		ID:            backup.ID,
		Source:        filepath.Join(root, "archives", "backups", backup.ID+".backup"),
		Size:          backup.Size,
		SHA256:        backup.SHA256,
		ContentFormat: backup.ContentFormat,
		OriginalName:  backup.OriginalName,
	}, nil
}
