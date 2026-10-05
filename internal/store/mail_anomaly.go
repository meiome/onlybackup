package store

import (
	"database/sql"
	"fmt"
	"time"
)

func missingBackupMailTx(tx *sql.Tx, keyID string, expectedAt time.Time) (string, string, error) {
	var name, timezone, received string
	err := tx.QueryRow(`SELECT
 COALESCE((SELECT name FROM keys WHERE id=?),?),timezone,
 COALESCE((SELECT received_at FROM backups WHERE key_id=? AND received_at<>''
 AND status IN ('complete','deleting','quarantined','purging','deleted')
 ORDER BY julianday(received_at) DESC,received_at DESC LIMIT 1),'')
 FROM automation_state WHERE id=1`, keyID, keyID, keyID).Scan(&name, &timezone, &received)
	if err != nil {
		return "", "", err
	}
	zone, err := time.LoadLocation(timezone)
	if err != nil || timezone == "" {
		zone = time.UTC
	}
	const dateFormat = "02/01/2006 15:04 MST"
	latest := "nessuna"
	if received != "" {
		latest = received
		if at, parseErr := time.Parse(time.RFC3339Nano, received); parseErr == nil {
			latest = at.In(zone).Format(dateFormat)
		}
	}
	subject := "OnlyBackup: backup mancante | " + name
	body := fmt.Sprintf("%s: backup del %s non ricevuto.\nUltima ricevuta: %s.\nVerifica server e invio del client.",
		name, expectedAt.In(zone).Format(dateFormat), latest)
	return subject, body, nil
}
