// Package model defines the deposit protocol shared by client, receiver and writer.
package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const UploadPath = "/v2/backups"
const LegacyUploadPath = "/v1/backups"
const MetadataHeader = "X-Onlybackup-Metadata"
const DigestHeader = "X-Onlybackup-Sha256"
const IdempotencyHeader = "X-Onlybackup-Idempotency-Key"
const MaxHeaderBytes = 16 << 10

const ContentFormatAgeV1 = "age-v1"

func ValidUploadPath(path string) bool { return path == UploadPath || path == LegacyUploadPath }

var ErrUnauthorized = errors.New("chiave non valida o revocata")
var ErrQuota = errors.New("spazio disponibile per la chiave insufficiente")
var ErrSize = errors.New("dimensione del backup non consentita")
var ErrRate = errors.New("numero massimo di invii nelle ultime 24 ore raggiunto")
var ErrConcurrent = errors.New("numero massimo di upload simultanei raggiunto")
var ErrDisk = errors.New("spazio libero del server insufficiente")
var ErrIdempotencyConflict = errors.New("chiave di idempotenza gia usata per un deposito diverso")
var ErrIdempotencyInProgress = errors.New("deposito con la stessa chiave di idempotenza ancora in corso")
var ErrIdempotencyUnavailable = errors.New("deposito gia sottoposto a retention e non piu disponibile; usare una nuova chiave di idempotenza")
var ErrRetentionBlocked = errors.New("cancellazioni sospese")
var ErrRetentionMinimum = errors.New("backup protetto dal minimo di conservazione della chiave")
var ErrCurrentDay = errors.New("i backup della giornata corrente non possono essere cancellati")
var ErrLastCopy = errors.New("l'ultima copia disponibile della sorgente non puo essere cancellata")
var ErrTooEarly = errors.New("quarantena minima di 48 ore non ancora trascorsa")
var ErrTicket = errors.New("ticket di manutenzione non valido, scaduto o riferito a un'altra operazione")
var ErrLease = errors.New("lease di manutenzione scaduta, persa o riferita a un'altra acquisizione")

type Metadata struct {
	Description  string `json:"description"`
	OriginalName string `json:"original_name"`
	// ContentFormat is empty for plaintext and legacy clients. age-v1 means
	// the stored bytes are a binary age-encryption.org/v1 file.
	ContentFormat string `json:"content_format,omitempty"`
}

func (m Metadata) Validate() error {
	if !utf8.ValidString(m.Description) || strings.TrimSpace(m.Description) == "" || utf8.RuneCountInString(m.Description) > 1000 {
		return errors.New("descrizione obbligatoria, massimo 1000 caratteri UTF-8")
	}
	for _, r := range m.Description {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("descrizione con caratteri di controllo")
		}
	}
	n := m.OriginalName
	if !utf8.ValidString(n) || strings.TrimSpace(n) == "" || utf8.RuneCountInString(n) > 255 || n == "." || n == ".." || strings.ContainsAny(n, "/\\") {
		return errors.New("nome file obbligatorio, massimo 255 caratteri e senza percorsi")
	}
	for _, r := range n {
		if unicode.IsControl(r) {
			return errors.New("nome file con caratteri di controllo")
		}
	}
	if m.ContentFormat != "" && m.ContentFormat != ContentFormatAgeV1 {
		return errors.New("formato contenuto non supportato")
	}
	return nil
}

func EncodeMetadata(m Metadata) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func DecodeMetadata(s string) (Metadata, error) {
	var m Metadata
	if len(s) > 12<<10 {
		return m, errors.New("metadati troppo grandi")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return m, errors.New("metadati non validi")
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&m); err != nil {
		return m, errors.New("metadati non validi")
	}
	// Reject trailing JSON or arbitrary bytes.
	var extra any
	if err = d.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, errors.New("metadati aggiuntivi non consentiti")
	}
	return m, m.Validate()
}

func ValidToken(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}
func TokenHash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "obk_" + base64.RawURLEncoding.EncodeToString(b), nil
}
func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func NewIdempotencyKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "obi_" + base64.RawURLEncoding.EncodeToString(b), nil
}
func ValidIdempotencyKey(s string) bool {
	if len(s) < 16 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}
func ValidID(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 16 && s == strings.ToLower(s)
}
func ValidDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}

type Profile struct {
	Name           string `json:"name"`
	TotalBytes     int64  `json:"total_bytes"`
	MaxBackupBytes int64  `json:"max_backup_bytes"`
	UploadsPerDay  int64  `json:"uploads_per_day"`
	Concurrent     int64  `json:"concurrent_uploads"`
}

func (p Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" || !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > 100 {
		return errors.New("nome profilo obbligatorio, massimo 100 caratteri")
	}
	for _, r := range p.Name {
		if unicode.IsControl(r) {
			return errors.New("nome profilo non valido")
		}
	}
	if p.TotalBytes <= 0 || p.TotalBytes > 1<<60 || p.MaxBackupBytes <= 0 || p.MaxBackupBytes > p.TotalBytes || p.UploadsPerDay < 1 || p.UploadsPerDay > 1000000 || p.Concurrent < 1 || p.Concurrent > 1024 {
		return errors.New("limiti non validi: valori positivi, dimensione backup <= spazio totale, concorrenza <= 1024")
	}
	return nil
}
func DefaultProfiles() []Profile {
	const GiB int64 = 1 << 30
	return []Profile{{"XS", 10 * GiB, GiB, 6, 1}, {"S", 100 * GiB, 10 * GiB, 12, 1}, {"M", 500 * GiB, 20 * GiB, 24, 2}, {"L", 2000 * GiB, 100 * GiB, 48, 4}, {"XL", 10000 * GiB, 500 * GiB, 96, 8}}
}
func ParseBytes(s string) (int64, error) {
	var multiplier int64 = 1
	for _, u := range []struct {
		s string
		m int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TB", 1000000000000}, {"GB", 1000000000}, {"MB", 1000000}, {"KB", 1000}, {"B", 1}} {
		if strings.HasSuffix(s, u.s) {
			s = strings.TrimSuffix(s, u.s)
			multiplier = u.m
			break
		}
	}
	var n int64
	if s == "" {
		return 0, errors.New("dimensione vuota")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("dimensione non valida: usare interi e B, KiB, MiB, GiB, TiB oppure KB, MB, GB, TB")
		}
		if n > ((1<<60)/multiplier-int64(c-'0'))/10 {
			return 0, errors.New("dimensione troppo grande")
		}
		n = n*10 + int64(c-'0')
	}
	if n > (1<<60)/multiplier {
		return 0, errors.New("dimensione troppo grande")
	}
	return n * multiplier, nil
}

type Receipt struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Size       int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	ReceivedAt string `json:"received_at"`
}

type Backup struct {
	Receipt
	KeyID          string `json:"key_id"`
	IdempotencyKey string `json:"-"`
	Metadata
	StartedAt      int64  `json:"started_at_unix"`
	Failure        string `json:"failure,omitempty"`
	QuarantinedAt  int64  `json:"quarantined_at_unix,omitempty"`
	PurgeNotBefore int64  `json:"purge_not_before_unix,omitempty"`
	PurgedAt       int64  `json:"purged_at_unix,omitempty"`
}

const (
	BackupReceiving   = "receiving"
	BackupComplete    = "complete"
	BackupFailed      = "failed"
	BackupDeleting    = "deleting"
	BackupQuarantined = "quarantined"
	BackupPurging     = "purging"
	BackupDeleted     = "deleted"
)

const (
	MonitoringLearning = "APPRENDIMENTO"
	MonitoringRegular  = "REGOLARE"
	MonitoringAnomaly  = "ANOMALIA"
	MonitoringPaused   = "SOSPESO"
)

type AutomationStatus struct {
	ManualPaused      bool           `json:"manual_paused"`
	ManualPauseReason string         `json:"manual_pause_reason,omitempty"`
	KeyRetentionDays  map[string]int `json:"key_retention_days"`
	Enabled           bool           `json:"enabled"`
	MailTested        bool           `json:"mail_tested"`
	MonitoringState   string         `json:"monitoring_state"`
	DeletionBlocked   bool           `json:"deletion_blocked"`
	BlockReason       string         `json:"block_reason,omitempty"`
	Timezone          string         `json:"timezone"`
	RetentionDays     int            `json:"retention_days"`
	ThresholdBasis    int            `json:"threshold_basis_points"`
	ReserveFree       int64          `json:"reserve_free_bytes"`
	ModelRevision     int64          `json:"model_revision"`
	LastCheckAt       int64          `json:"last_check_at_unix,omitempty"`
	// LastCheckRegular is false when a later event invalidated the regular check.
	LastCheckRegular bool  `json:"last_check_regular"`
	NextReportAt     int64 `json:"next_report_at_unix,omitempty"`
	ActiveAnomalies  int   `json:"active_anomalies"`
}

type RetentionOperation struct {
	ID                 int64  `json:"id"`
	BackupID           string `json:"backup_id"`
	Kind               string `json:"kind"`
	State              string `json:"state"`
	Origin             string `json:"origin"`
	Actor              string `json:"actor"`
	Reason             string `json:"reason"`
	Revision           int64  `json:"revision"`
	RequestedAt        int64  `json:"requested_at_unix"`
	AuthorizedAt       int64  `json:"authorized_at_unix,omitempty"`
	CompletedAt        int64  `json:"completed_at_unix,omitempty"`
	Error              string `json:"error,omitempty"`
	Ticket             string `json:"ticket,omitempty"`
	LeaseGeneration    int64  `json:"lease_generation,omitempty"`
	ExecutionCommitted bool   `json:"execution_committed"`
}

type MaintenanceLease struct {
	Owner      string `json:"owner"`
	Generation int64  `json:"generation"`
	ExpiresAt  int64  `json:"expires_at_unix"`
}

type Anomaly struct {
	ID             int64  `json:"id"`
	StableKey      string `json:"stable_key"`
	Kind           string `json:"kind"`
	KeyID          string `json:"key_id,omitempty"`
	BackupID       string `json:"backup_id,omitempty"`
	Detail         string `json:"detail"`
	EventAt        int64  `json:"event_at_unix"`
	OpenedAt       int64  `json:"opened_at_unix"`
	LastSeenAt     int64  `json:"last_seen_at_unix"`
	ResolvedAt     int64  `json:"resolved_at_unix,omitempty"`
	AcknowledgedAt int64  `json:"acknowledged_at_unix,omitempty"`
	ResolvedBy     string `json:"resolved_by,omitempty"`
	ResolutionNote string `json:"resolution_note,omitempty"`
}

type AnomalyExclusion struct {
	ID              int64    `json:"id"`
	SourceAnomalyID int64    `json:"source_anomaly_id"`
	KeyID           string   `json:"key_id"`
	StartsAt        int64    `json:"starts_at_unix"`
	EndsAt          int64    `json:"ends_at_unix"`
	Actor           string   `json:"actor"`
	Reason          string   `json:"reason"`
	CreatedAt       int64    `json:"created_at_unix"`
	Kinds           []string `json:"kinds"`
}

type MonitoringCheckModel struct {
	CheckID     int64           `json:"check_id"`
	KeyID       string          `json:"key_id"`
	Revision    int64           `json:"revision"`
	CoveredFrom int64           `json:"covered_from_unix"`
	CoveredTo   int64           `json:"covered_to_unix"`
	Model       json.RawMessage `json:"model,omitempty"`
}

type MonitoringCheck struct {
	ID         int64                  `json:"id"`
	StartedAt  int64                  `json:"started_at_unix"`
	FinishedAt int64                  `json:"finished_at_unix,omitempty"`
	PeriodFrom int64                  `json:"period_from_unix"`
	PeriodTo   int64                  `json:"period_to_unix"`
	Status     string                 `json:"status"`
	Summary    string                 `json:"summary"`
	Models     []MonitoringCheckModel `json:"models,omitempty"`
}

type MailMessage struct {
	ID          int64  `json:"id"`
	StableID    string `json:"stable_id"`
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	Attempts    int    `json:"attempts"`
	NextAttempt int64  `json:"next_attempt_unix"`
	SentAt      int64  `json:"sent_at_unix,omitempty"`
}

type MailSettings struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	TLS        bool   `json:"tls"`
	From       string `json:"from"`
	Recipients string `json:"recipients"`
}

type Key struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Revoked bool   `json:"revoked"`
}

type Quota struct {
	KeyID       string  `json:"key_id"`
	Profile     Profile `json:"profile"`
	Used        int64   `json:"used_bytes"`
	Reserved    int64   `json:"reserved_bytes"`
	Active      int64   `json:"active_uploads"`
	Attempts24h int64   `json:"attempts_last_24h"`
}

// RetentionDaysForKey returns the explicit minimum, or the archive default.
func (s AutomationStatus) RetentionDaysForKey(keyID string) int {
	if days, ok := s.KeyRetentionDays[keyID]; ok {
		return days
	}
	return s.RetentionDays
}
