package localcli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
)

type adminInventory struct {
	Status  model.AutomationStatus     `json:"status"`
	Keys    []model.Key                `json:"keys"`
	Backups []model.Backup             `json:"backups"`
	Models  map[string]json.RawMessage `json:"models"`
}

func retentionAdmin(args []string, root, adminSocket string, out, errOut io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	recognized := args[0] == "automation" || args[0] == "mail" || args[0] == "retention" || args[0] == "monitoring" || args[0] == "cancellazione" || args[0] == "quarantena"
	if !recognized {
		return false, nil
	}
	client := localclient.New(adminSocket, 30*time.Second)
	call := func(method, path string, request, response any) error {
		return client.Do(context.Background(), method, path, request, response)
	}
	switch args[0] {
	case "automation":
		if len(args) < 2 {
			return true, errors.New("automation richiede setup o status")
		}
		switch args[1] {
		case "status":
			if len(args) != 2 {
				return true, errors.New("automation status non accetta argomenti")
			}
			var status model.AutomationStatus
			if err := call("GET", "/v1/status", nil, &status); err != nil {
				return true, err
			}
			return true, Print(out, status)
		case "setup":
			f := flags("automation setup", errOut)
			host := f.String("smtp-host", "", "host relay SMTP")
			port := f.Int("smtp-port", 25, "porta relay SMTP")
			tls := f.Bool("smtp-tls", true, "richiede TLS")
			from := f.String("from", "", "mittente")
			to := f.String("to", "", "destinatari separati da virgola")
			zone := f.String("timezone", detectedTimezone(), "fuso civile IANA rilevato dal server")
			mode := f.String("retention", "", "auto o manual; se omesso conserva la scelta, auto per nuovi archivi")
			confirmation := f.String("confirm", "", "conferma esplicita (altrimenti richiesta da stdin)")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			if *mode != "" && *mode != "auto" && *mode != "manual" {
				return true, errors.New("specificare --retention auto oppure manual")
			}
			expected := model.ConfirmAutomationSetup(*mode)
			description := "Configurazione mail e nuovo apprendimento. Conserva la scelta retention esistente (automatica per un nuovo archivio); le pause manuali restano attive."
			if *mode != "" {
				description = "Configurazione mail e nuovo apprendimento. Modalità retention scelta: " + *mode + ". Le pause manuali restano attive."
			}
			if err := confirmRetentionChange(os.Stdin, out, *confirmation, expected, description); err != nil {
				return true, err
			}
			request := map[string]any{"host": *host, "port": *port, "tls": *tls, "from": *from, "recipients": *to, "timezone": *zone, "retention_mode": *mode, "confirmation": expected}
			var response any
			if err := call("POST", "/v1/automation/setup", request, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		default:
			return true, errors.New("comando automation sconosciuto")
		}
	case "mail":
		if len(args) != 2 || args[1] != "test" {
			return true, errors.New("mail richiede test")
		}
		var response any
		if err := call("POST", "/v1/mail/test", map[string]string{}, &response); err != nil {
			return true, err
		}
		return true, Print(out, response)
	case "retention":
		if len(args) < 2 {
			return true, errors.New("retention richiede simulate, minimum, enable, pause o resume")
		}
		switch args[1] {
		case "simulate":
			if len(args) != 2 {
				return true, errors.New("retention simulate non accetta argomenti")
			}
			return true, simulateRetention(call, root, out)
		case "enable":
			f := flags("retention enable", errOut)
			confirmation := f.String("confirm", "", "conferma esplicita ABILITA")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			if err := confirmRetentionChange(os.Stdin, out, *confirmation, model.ConfirmRetentionEnable, "Abilita la retention. I blocchi e i requisiti di sicurezza restano validi."); err != nil {
				return true, err
			}
			var response any
			if err := call("POST", "/v1/automation/enable", map[string]string{"confirmation": model.ConfirmRetentionEnable}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		case "minimum":
			f := flags("retention minimum", errOut)
			key := f.String("key", "", "ID della chiave")
			days := f.Int("days", 0, "giorni minimi da conservare (almeno 7)")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			if !model.ValidID(*key) || *days < policy.MinimumDays {
				return true, errors.New("specificare --key ID e --days (almeno 7)")
			}
			var response any
			if err := call("POST", "/v1/retention/minimum", map[string]any{"key_id": *key, "days": *days}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		case "pause":
			f := flags("retention pause", errOut)
			reason := f.String("reason", "sospensione amministrativa", "motivazione")
			confirmation := f.String("confirm", "", "conferma esplicita SOSPENDI")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			var response any
			if err := confirmRetentionChange(os.Stdin, out, *confirmation, model.ConfirmRetentionPause, "Sospendi nuove cancellazioni e annulla le attivazioni prenotate. La pausa resta attiva fino a una ripresa esplicita."); err != nil {
				return true, err
			}
			if err := call("POST", "/v1/retention/pause", map[string]string{"reason": *reason, "confirmation": model.ConfirmRetentionPause}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		case "resume":
			f := flags("retention resume", errOut)
			whenReady := f.Bool("when-ready", false, "sblocca una sola volta al prossimo controllo valido")
			confirmation := f.String("confirm", "", "conferma esplicita RIPRENDI o PRENOTA con --when-ready")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			path := "/v1/retention/resume"
			expected := model.ConfirmRetentionResume
			description := "Rimuovi la pausa attuale se il controllo recente è valido. Le cancellazioni rispettano soglia e protezioni delle copie."
			if *whenReady {
				path = "/v1/retention/resume-when-ready"
				expected = model.ConfirmRetentionResumeWhenReady
				description = "Abilita la retention e prenota un solo sblocco della pausa attuale al prossimo controllo valido. Non servirà un'altra conferma alla conclusione del controllo."
			}
			if err := confirmRetentionChange(os.Stdin, out, *confirmation, expected, description); err != nil {
				return true, err
			}
			var response any
			if err := call("POST", path, map[string]string{"confirmation": expected}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		default:
			return true, errors.New("comando retention sconosciuto")
		}
	case "monitoring":
		if len(args) < 2 {
			return true, errors.New("monitoring richiede checks, relearn, anomalies, exclusions, acknowledge o exclude")
		}
		switch args[1] {
		case "checks":
			f := flags("monitoring checks", errOut)
			limit := f.Int("limit", 20, "numero massimo di controlli, fino a 1000")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			return true, printMonitoringChecks(call, *limit, out)
		case "relearn":
			if len(args) != 2 {
				return true, errors.New("monitoring relearn non accetta argomenti")
			}
			var response any
			if err := call("POST", "/v1/monitoring/relearn", map[string]string{}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		case "anomalies":
			if len(args) != 2 {
				return true, errors.New("monitoring anomalies non accetta argomenti")
			}
			var anomalies []model.Anomaly
			if err := call("GET", "/v1/anomalies", nil, &anomalies); err != nil {
				return true, err
			}
			return true, Print(out, anomalies)
		case "exclusions":
			if len(args) != 2 {
				return true, errors.New("monitoring exclusions non accetta argomenti")
			}
			var exclusions []model.AnomalyExclusion
			if err := call("GET", "/v1/anomaly-exclusions", nil, &exclusions); err != nil {
				return true, err
			}
			return true, Print(out, exclusions)
		case "acknowledge":
			f := flags("monitoring acknowledge", errOut)
			id := f.Int64("id", 0, "ID anomalia mancanza")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			var response any
			if err := call("POST", "/v1/monitoring/acknowledge", map[string]int64{"id": *id}, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		case "exclude":
			f := flags("monitoring exclude", errOut)
			id := f.Int64("id", 0, "ID anomalia originaria")
			from := f.String("from", "", "inizio intervallo RFC3339")
			to := f.String("to", "", "fine intervallo RFC3339, massimo sette giorni")
			kinds := f.String("kinds", "", "tipi separati da virgola: upload,time,schedule_missing,extra")
			reason := f.String("reason", "", "motivazione obbligatoria")
			if err := parse(f, args[2:]); err != nil {
				return true, err
			}
			startsAt, err := time.Parse(time.RFC3339, *from)
			if err != nil {
				return true, errors.New("--from deve essere RFC3339")
			}
			endsAt, err := time.Parse(time.RFC3339, *to)
			if err != nil {
				return true, errors.New("--to deve essere RFC3339")
			}
			var selected []string
			for _, kind := range strings.Split(*kinds, ",") {
				if value := strings.TrimSpace(kind); value != "" {
					selected = append(selected, value)
				}
			}
			request := map[string]any{"source_anomaly_id": *id, "starts_at_unix": startsAt.Unix(), "ends_at_unix": endsAt.Unix(), "actor": actorName(), "reason": *reason, "kinds": selected}
			var response model.AnomalyExclusion
			if err = call("POST", "/v1/monitoring/exclude", request, &response); err != nil {
				return true, err
			}
			return true, Print(out, response)
		default:
			return true, errors.New("comando monitoring sconosciuto")
		}
	case "cancellazione":
		if len(args) != 1 {
			return true, errors.New("cancellazione non accetta argomenti")
		}
		return true, guidedDeletion(call, out)
	case "quarantena":
		if len(args) != 1 {
			return true, errors.New("quarantena non accetta argomenti")
		}
		return true, guidedQuarantine(call, out)
	}
	return true, flag.ErrHelp
}

func confirmRetentionChange(in io.Reader, out io.Writer, provided, expected, description string) error {
	if provided == "" {
		fmt.Fprintf(out, "%s\nDigitare %s per confermare: ", description, expected)
		var err error
		provided, err = readLine(bufio.NewReader(in))
		if err != nil {
			return errors.New("conferma non ricevuta; nessuna modifica")
		}
	}
	if provided != expected {
		return fmt.Errorf("conferma non corrispondente: richiesta %s; nessuna modifica", expected)
	}
	return nil
}

func printMonitoringChecks(call func(string, string, any, any) error, limit int, out io.Writer) error {
	var checks []model.MonitoringCheck
	if err := call("GET", "/v1/monitoring-checks?limit="+strconv.Itoa(limit), nil, &checks); err != nil {
		return err
	}
	return Print(out, checks)
}

func detectedTimezone() string {
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if value := strings.TrimSpace(string(data)); value != "" {
			if _, loadErr := time.LoadLocation(value); loadErr == nil {
				return value
			}
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if index := strings.Index(target, "zoneinfo/"); index >= 0 {
			value := target[index+len("zoneinfo/"):]
			if _, loadErr := time.LoadLocation(value); loadErr == nil {
				return value
			}
		}
	}
	if value := os.Getenv("TZ"); value != "" {
		if _, err := time.LoadLocation(value); err == nil {
			return value
		}
	}
	return time.Now().Location().String()
}

func actorName() string {
	if account, err := user.Current(); err == nil {
		return account.Username
	}
	return fmt.Sprintf("uid:%d", os.Geteuid())
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	return strings.TrimSpace(line), err
}

func loadInventory(call func(string, string, any, any) error) (adminInventory, error) {
	var inventory adminInventory
	err := call("GET", "/v1/inventory", nil, &inventory)
	return inventory, err
}

func guidedDeletion(call func(string, string, any, any) error, out io.Writer) error {
	inventory, err := loadInventory(call)
	if err != nil {
		return err
	}
	if len(inventory.Keys) == 0 {
		return errors.New("nessuna chiave")
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintln(out, "Chiavi:")
	for i, key := range inventory.Keys {
		fmt.Fprintf(out, "%d) %s (%s) revocata=%t\n", i+1, key.Name, key.ID, key.Revoked)
	}
	fmt.Fprint(out, "Numero chiave: ")
	choice, err := readLine(reader)
	if err != nil {
		return err
	}
	index, err := strconv.Atoi(choice)
	if err != nil || index < 1 || index > len(inventory.Keys) {
		return errors.New("selezione chiave non valida")
	}
	key := inventory.Keys[index-1]
	loc, _ := time.LoadLocation(inventory.Status.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	completeCopies := 0
	for _, candidate := range inventory.Backups {
		if candidate.KeyID == key.ID && candidate.Status == model.BackupComplete {
			completeCopies++
		}
	}
	var candidates []model.Backup
	for _, backup := range inventory.Backups {
		if backup.KeyID == key.ID {
			candidates = append(candidates, backup)
		}
	}
	if len(candidates) == 0 {
		return errors.New("nessun backup per la chiave")
	}
	for i, backup := range candidates {
		var reasons []string
		if inventory.Status.DeletionBlocked {
			reasons = append(reasons, "retention sospesa")
		}
		if key.Revoked {
			reasons = append(reasons, "chiave revocata")
		}
		if backup.Status != model.BackupComplete {
			reasons = append(reasons, "stato "+backup.Status)
		}
		if completeCopies <= 1 && backup.Status == model.BackupComplete {
			reasons = append(reasons, "ultima copia recuperabile")
		}
		if at, parseErr := time.Parse(time.RFC3339Nano, backup.ReceivedAt); parseErr == nil {
			by, bm, bd := at.In(loc).Date()
			ny, nm, nd := time.Now().In(loc).Date()
			if by == ny && bm == nm && bd == nd {
				reasons = append(reasons, "giornata corrente")
			}
		}
		days := max(inventory.Status.RetentionDaysForKey(key.ID), policy.MinimumDays)
		protected, protectionErr := policy.RetentionProtected(backup, time.Now(), loc.String(), days)
		if protectionErr != nil {
			return protectionErr
		}
		if protected {
			reasons = append(reasons, fmt.Sprintf("minimo di %d giorni", days))
		}
		eligibility := "selezionabile"
		if len(reasons) != 0 {
			eligibility = "non selezionabile: " + strings.Join(reasons, ", ")
		}
		fmt.Fprintf(out, "%d) %s  %s  %d byte  stato=%s  %s\n", i+1, backup.ID, backup.ReceivedAt, backup.Size, backup.Status, eligibility)
	}
	fmt.Fprint(out, "Numero backup: ")
	choice, err = readLine(reader)
	if err != nil {
		return err
	}
	index, err = strconv.Atoi(choice)
	if err != nil || index < 1 || index > len(candidates) {
		return errors.New("selezione backup non valida")
	}
	backup := candidates[index-1]
	fmt.Fprintf(out, "Richiesta manuale per %s (%s), %d byte. La quarantena dura almeno 48 ore. Digitare CANCELLA: ", backup.ID, backup.ReceivedAt, backup.Size)
	confirmation, err := readLine(reader)
	if err != nil {
		return err
	}
	if confirmation != "CANCELLA" {
		return errors.New("conferma non corrispondente; nessuna richiesta creata")
	}
	request := map[string]string{"backup_id": backup.ID, "actor": actorName(), "reason": "selezione manuale amministrativa"}
	var operation model.RetentionOperation
	if err = call("POST", "/v1/retention/request", request, &operation); err != nil {
		return err
	}
	return Print(out, operation)
}

func guidedQuarantine(call func(string, string, any, any) error, out io.Writer) error {
	var backups []model.Backup
	if err := call("GET", "/v1/quarantine", nil, &backups); err != nil {
		return err
	}
	if len(backups) == 0 {
		return errors.New("nessun backup in quarantena o in attesa")
	}
	for _, backup := range backups {
		fmt.Fprintf(out, "%s  stato=%s  quarantena=%s  purge non prima di=%s\n", backup.ID, backup.Status, unixTime(backup.QuarantinedAt), unixTime(backup.PurgeNotBefore))
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprint(out, "ID backup: ")
	id, err := readLine(reader)
	if err != nil {
		return err
	}
	var selected *model.Backup
	for i := range backups {
		if backups[i].ID == id {
			selected = &backups[i]
			break
		}
	}
	if selected == nil {
		return errors.New("backup non presente nell'elenco")
	}
	if selected.Status == model.BackupDeleting {
		fmt.Fprint(out, "Digitare ANNULLA per annullare la richiesta non eseguita: ")
		confirmation, err := readLine(reader)
		if err != nil {
			return err
		}
		if confirmation != "ANNULLA" {
			return errors.New("conferma non corrispondente")
		}
		var response any
		if err = call("POST", "/v1/retention/cancel", map[string]string{"backup_id": id, "actor": actorName()}, &response); err != nil {
			return err
		}
		return Print(out, response)
	}
	if selected.Status != model.BackupQuarantined {
		return errors.New("backup non recuperabile nello stato corrente")
	}
	fmt.Fprint(out, "Digitare RECUPERA per richiedere il ripristino: ")
	confirmation, err := readLine(reader)
	if err != nil {
		return err
	}
	if confirmation != "RECUPERA" {
		return errors.New("conferma non corrispondente")
	}
	var operation model.RetentionOperation
	if err = call("POST", "/v1/retention/recover", map[string]string{"backup_id": id, "actor": actorName(), "reason": "recupero richiesto dall'amministratore"}, &operation); err != nil {
		return err
	}
	return Print(out, operation)
}

func unixTime(value int64) string {
	if value == 0 {
		return "-"
	}
	return time.Unix(value, 0).Format(time.RFC3339)
}

func simulateRetention(call func(string, string, any, any) error, root string, out io.Writer) error {
	now := time.Now()
	var status model.AutomationStatus
	if err := call("GET", "/v1/status", nil, &status); err != nil {
		return err
	}
	inventory, err := loadInventory(call)
	if err != nil {
		return err
	}
	var stat syscall.Statfs_t
	if err = syscall.Statfs(filepath.Join(root, "archives", "backups"), &stat); err != nil {
		return err
	}
	revoked := make(map[string]bool)
	for _, key := range inventory.Keys {
		revoked[key.ID] = key.Revoked
	}
	var pending uint64
	for _, backup := range inventory.Backups {
		if backup.Status == model.BackupDeleting || backup.Status == model.BackupQuarantined || backup.Status == model.BackupPurging {
			protected, err := policy.RetentionProtected(backup, now, status.Timezone, status.RetentionDaysForKey(backup.KeyID))
			if err != nil {
				return err
			}
			if !protected && !revoked[backup.KeyID] {
				pending += uint64(backup.Size)
			}
		}
	}
	var learnedModels []policy.KeyModel
	for _, key := range inventory.Keys {
		if key.Revoked {
			continue
		}
		raw, ok := inventory.Models[key.ID]
		if !ok {
			continue
		}
		var learned policy.KeyModel
		if err = json.Unmarshal(raw, &learned); err != nil {
			return err
		}
		learnedModels = append(learnedModels, learned)
	}
	expected48h, err := policy.Forecast48Hours(learnedModels, now)
	if err != nil {
		return err
	}
	selection, err := policy.Select(policy.SelectionInput{Now: now, Timezone: status.Timezone,
		Filesystem:           policy.Filesystem{Blocks: stat.Blocks, Bfree: stat.Bfree, Bavail: stat.Bavail, BlockSize: uint64(stat.Bsize)},
		ThresholdBasisPoints: status.ThresholdBasis, RetentionDays: status.RetentionDays, KeyRetentionDays: status.KeyRetentionDays, Expected48hBytes: expected48h, PendingPurgeBytes: pending,
		Backups: inventory.Backups, RevokedKeys: revoked})
	if err != nil {
		return err
	}
	return Print(out, selection)
}
