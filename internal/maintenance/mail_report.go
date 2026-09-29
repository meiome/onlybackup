package maintenance

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

// buildEmailReport keeps the human-facing report short. The full monitoring
// details remain available from the administrator CLI and audit history.
func buildEmailReport(snapshot localclient.Snapshot, filesystem policy.Filesystem, findings []policy.Finding, now time.Time) string {
	status := snapshot.Status
	activeKeys, keysWithoutCopy := 0, 0
	for _, key := range snapshot.Keys {
		if key.Revoked {
			continue
		}
		activeKeys++
		found := false
		for _, backup := range snapshot.Backups {
			if backup.KeyID == key.ID && backup.Status == model.BackupComplete {
				found = true
				break
			}
		}
		if !found {
			keysWithoutCopy++
		}
	}
	problem := status.ActiveAnomalies > 0 || len(findings) > 0 || !status.Enabled ||
		activeKeys == 0 || keysWithoutCopy > 0 ||
		(status.MonitoringState != model.MonitoringLearning && status.MonitoringState != model.MonitoringRegular) ||
		(status.MonitoringState == model.MonitoringRegular && status.DeletionBlocked)
	esito := "OK"
	if problem {
		esito = "ATTENZIONE"
	}

	total, used, _, _ := filesystem.Bytes()
	percent := 0.0
	if total > 0 {
		percent = 100 * float64(used) / float64(total)
	}
	retention := "attiva"
	switch {
	case !status.Enabled:
		retention = "da abilitare; cancellazioni sospese"
	case status.MonitoringState == model.MonitoringLearning:
		retention = "in apprendimento; cancellazioni sospese"
	case status.DeletionBlocked:
		retention = "bloccata; cancellazioni sospese"
	}
	if status.DeletionBlocked && status.BlockReason != "" && status.MonitoringState != model.MonitoringLearning {
		retention += " (" + status.BlockReason + ")"
	}

	backups := 0
	for _, backup := range snapshot.Backups {
		if backup.Status == model.BackupComplete || backup.Status == model.BackupQuarantined {
			backups++
		}
	}
	lines := []string{
		"Esito: " + esito,
		"Monitoraggio: " + status.MonitoringState,
		fmt.Sprintf("Copie conservate: %d", backups),
		fmt.Sprintf("Anomalie attive: %d", status.ActiveAnomalies),
		fmt.Sprintf("Disco occupato: %.1f%%", percent),
		"Retention: " + retention,
		"Ultime copie:",
	}

	keys := append([]model.Key(nil), snapshot.Keys...)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	zone, err := time.LoadLocation(status.Timezone)
	if err != nil {
		zone = time.UTC
	}
	for _, key := range keys {
		if key.Revoked {
			continue
		}
		var latest *model.Backup
		for i := range snapshot.Backups {
			backup := &snapshot.Backups[i]
			if backup.KeyID == key.ID && backup.Status == model.BackupComplete &&
				(latest == nil || backup.ReceivedAt > latest.ReceivedAt) {
				latest = backup
			}
		}
		value := "nessuna copia completa"
		if latest != nil {
			value = latest.ReceivedAt + " · " + formatEmailBytes(latest.Size)
			if at, parseErr := time.Parse(time.RFC3339Nano, latest.ReceivedAt); parseErr == nil {
				value = at.In(zone).Format("02/01/2006 15:04") + " · " + formatEmailBytes(latest.Size)
			}
		}
		lines = append(lines, "- "+key.Name+": "+value)
	}
	if len(keys) == 0 {
		lines = append(lines, "- Nessun client configurato")
	}

	action := "nessuna oggi"
	switch {
	case status.ActiveAnomalies > 0 || len(findings) > 0:
		action = "leggi gli avvisi di anomalia e verifica il client interessato"
	case activeKeys == 0:
		action = "configura almeno un client di backup"
	case keysWithoutCopy > 0:
		action = "verifica i client senza copie complete"
	case !status.Enabled:
		action = "verifica la configurazione della retention"
	case status.MonitoringState == model.MonitoringRegular && status.DeletionBlocked:
		action = "verifica il monitoraggio e sblocca la retention con onlybackup-admin retention resume"
	case status.MonitoringState != model.MonitoringRegular && status.MonitoringState != model.MonitoringLearning:
		action = "verifica lo stato del monitoraggio"
	}
	lines = append(lines, "Azione: "+action, "Generato: "+now.In(zone).Format("02/01/2006 15:04 MST"))
	return strings.Join(lines, "\n")
}

func formatEmailBytes(size int64) string {
	if size >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(size)/float64(1<<30))
	}
	if size >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(size)/float64(1<<20))
	}
	return fmt.Sprintf("%d KiB", (size+1023)/1024)
}
