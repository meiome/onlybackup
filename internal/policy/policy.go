// Package policy contains deterministic monitoring and retention decisions.
// It has no filesystem, database or wall-clock dependencies.
package policy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

const (
	ScheduleTolerance         = 30 * time.Minute
	SizeTolerance             = 0.20
	ForecastSafetyMargin      = 0.20
	MinimumDays               = 7
	MinimumReleaseBasisPoints = 1000
)

type Observation struct {
	BackupID string    `json:"backup_id"`
	KeyID    string    `json:"key_id"`
	At       time.Time `json:"at"`
	Size     int64     `json:"size_bytes"`
}

type Appointment struct {
	MinuteOfDay    int     `json:"minute_of_day"`
	MedianSize     int64   `json:"median_size"`
	GrowthPerDay   float64 `json:"growth_per_day"`
	MedianResidual float64 `json:"median_residual"`
}

type KeyModel struct {
	KeyID          string                         `json:"key_id"`
	Timezone       string                         `json:"timezone"`
	LearnedAt      time.Time                      `json:"learned_at"`
	Schedule       map[time.Weekday][]Appointment `json:"schedule"`
	Reliable       bool                           `json:"reliable"`
	ReviewRequired bool                           `json:"review_required"`
	Reason         string                         `json:"reason,omitempty"`
}

type Finding struct {
	StableKey string    `json:"stable_key"`
	Kind      string    `json:"kind"`
	KeyID     string    `json:"key_id"`
	BackupID  string    `json:"backup_id,omitempty"`
	Detail    string    `json:"detail"`
	At        time.Time `json:"at,omitempty"`
}

func localMidnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// Learn uses the last fourteen complete civil days. Counts for both instances
// of every weekday must agree; the current day is never training data.
func Learn(keyID, timezone string, observations []Observation, now time.Time) (KeyModel, error) {
	result := KeyModel{KeyID: keyID, Timezone: timezone, LearnedAt: now.UTC(), Schedule: make(map[time.Weekday][]Appointment)}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return result, err
	}
	today := localMidnight(now, loc)
	start := today.AddDate(0, 0, -14)
	byDay := make(map[string][]Observation)
	allByDay := make(map[string][]Observation)
	for _, observation := range observations {
		if observation.KeyID != keyID {
			continue
		}
		local := observation.At.In(loc)
		if !local.Before(today) || observation.Size <= 0 {
			continue
		}
		key := local.Format("2006-01-02")
		allByDay[key] = append(allByDay[key], observation)
		if local.Before(start) {
			continue
		}
		byDay[key] = append(byDay[key], observation)
	}
	if len(byDay) == 0 {
		result.Reason = "storico assente negli ultimi 14 giorni completi"
		return result, nil
	}
	for weekday := time.Sunday; weekday <= time.Saturday; weekday++ {
		var days [2][]Observation
		index := 0
		for day := start; day.Before(today); day = day.AddDate(0, 0, 1) {
			if day.Weekday() != weekday {
				continue
			}
			items := append([]Observation(nil), byDay[day.Format("2006-01-02")]...)
			sort.Slice(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
			if index < 2 {
				days[index] = items
			}
			index++
		}
		if len(days[0]) != len(days[1]) {
			result.Reason = fmt.Sprintf("numero copie non concorde per %s", weekday)
			return result, nil
		}
		for i := range days[0] {
			a, b := days[0][i], days[1][i]
			minuteA := minute(a.At.In(loc))
			minuteB := minute(b.At.In(loc))
			if circularDistance(minuteA, minuteB) > int(ScheduleTolerance/time.Minute)*2 {
				result.Reason = fmt.Sprintf("orari non concordi per %s", weekday)
				return result, nil
			}
			var series []Observation
			minutes := []int{minute(a.At.In(loc)), minute(b.At.In(loc))}
			for date, values := range allByDay {
				day, parseErr := time.ParseInLocation("2006-01-02", date, loc)
				if parseErr != nil || day.Weekday() != weekday {
					continue
				}
				sort.Slice(values, func(i, j int) bool { return values[i].At.Before(values[j].At) })
				if i < len(values) {
					series = append(series, values[i])
				}
			}
			sort.Slice(series, func(i, j int) bool { return series[i].At.Before(series[j].At) })
			central := circularMean(minutes)
			var slopes []float64
			for left := 0; left < len(series); left++ {
				for right := left + 1; right < len(series); right++ {
					daysBetween := series[right].At.Sub(series[left].At).Hours() / 24
					if daysBetween > 0 {
						slopes = append(slopes, float64(series[right].Size-series[left].Size)/daysBetween)
					}
				}
			}
			growth := medianFloat(slopes)
			if growth < 0 {
				result.ReviewRequired = true
				growth = 0
			}
			projected := make([]float64, 0, len(series))
			for _, observation := range series {
				projected = append(projected, float64(observation.Size)+growth*now.Sub(observation.At).Hours()/24)
			}
			baseline := medianFloat(projected)
			var residuals []float64
			for _, observation := range series {
				expectedAtObservation := baseline - growth*now.Sub(observation.At).Hours()/24
				residuals = append(residuals, math.Abs(float64(observation.Size)-expectedAtObservation))
			}
			result.Schedule[weekday] = append(result.Schedule[weekday], Appointment{
				MinuteOfDay: central, MedianSize: int64(math.Round(baseline)), GrowthPerDay: growth, MedianResidual: medianFloat(residuals),
			})
		}
	}
	if result.ReviewRequired {
		result.Reason = "riduzione sistematica delle dimensioni da revisionare"
		return result, nil
	}
	result.Reliable = true
	return result, nil
}

func minute(t time.Time) int { return t.Hour()*60 + t.Minute() }

func circularDistance(a, b int) int {
	d := abs(a - b)
	if d > 720 {
		d = 1440 - d
	}
	return d
}

func circularMean(values []int) int {
	var x, y float64
	for _, value := range values {
		angle := float64(value) * 2 * math.Pi / 1440
		x += math.Cos(angle)
		y += math.Sin(angle)
	}
	angle := math.Atan2(y, x)
	if angle < 0 {
		angle += 2 * math.Pi
	}
	return int(math.Round(angle*1440/(2*math.Pi))) % 1440
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	if len(values)%2 == 1 {
		return values[len(values)/2]
	}
	return values[len(values)/2-1] + (values[len(values)/2]-values[len(values)/2-1])/2
}

func appointmentTime(day time.Time, minuteOfDay int, loc *time.Location) time.Time {
	y, m, d := day.In(loc).Date()
	return time.Date(y, m, d, minuteOfDay/60, minuteOfDay%60, 0, 0, loc)
}

// Forecast48Hours returns the largest forecast deposit volume in any rolling
// 48-hour window starting during the next seven days. Every expected copy is
// projected to its appointment using the learned non-negative growth and gets
// an explicit 20% safety margin.
func Forecast48Hours(models []KeyModel, now time.Time) (uint64, error) {
	type event struct {
		at    time.Time
		bytes uint64
	}
	var events []event
	windowStartLimit := now.Add(7 * 24 * time.Hour)
	eventLimit := windowStartLimit.Add(48 * time.Hour)
	for _, learned := range models {
		if !learned.Reliable || learned.ReviewRequired {
			continue
		}
		loc, err := time.LoadLocation(learned.Timezone)
		if err != nil {
			return 0, err
		}
		firstDay := localMidnight(now, loc)
		for day := firstDay; day.Before(eventLimit.In(loc)); day = day.AddDate(0, 0, 1) {
			for _, appointment := range learned.Schedule[day.Weekday()] {
				if appointment.MinuteOfDay < 0 || appointment.MinuteOfDay >= 24*60 || appointment.MedianSize <= 0 || appointment.GrowthPerDay < 0 {
					return 0, errors.New("modello di previsione non valido")
				}
				at := appointmentTime(day, appointment.MinuteOfDay, loc)
				if at.Before(now) || !at.Before(eventLimit) {
					continue
				}
				size, err := forecastSize(learned, appointment, at, ForecastSafetyMargin)
				if err != nil {
					return 0, err
				}
				events = append(events, event{at: at, bytes: size})
			}
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].at.Before(events[j].at) })
	starts := []time.Time{now}
	for _, candidate := range events {
		if candidate.at.Before(windowStartLimit) {
			starts = append(starts, candidate.at)
		}
	}
	var largest uint64
	for _, start := range starts {
		end := start.Add(48 * time.Hour)
		var sum uint64
		for _, candidate := range events {
			if candidate.at.Before(start) || !candidate.at.Before(end) {
				continue
			}
			if math.MaxUint64-sum < candidate.bytes {
				return 0, errors.New("previsione spazio non rappresentabile")
			}
			sum += candidate.bytes
		}
		if sum > largest {
			largest = sum
		}
	}
	return largest, nil
}

// forecastSize uses the baseline at LearnedAt for both quota and disk forecasts.
func forecastSize(learned KeyModel, appointment Appointment, at time.Time, margin float64) (uint64, error) {
	if appointment.MinuteOfDay < 0 || appointment.MinuteOfDay >= 24*60 || appointment.MedianSize <= 0 || appointment.GrowthPerDay < 0 {
		return 0, errors.New("modello di previsione non valido")
	}
	days := at.Sub(learned.LearnedAt).Hours() / 24
	projected := float64(appointment.MedianSize) + appointment.GrowthPerDay*days
	size := math.Ceil(projected * (1 + margin))
	if projected <= 0 || math.IsNaN(size) || math.IsInf(size, 0) || size >= float64(math.MaxUint64) {
		return 0, errors.New("previsione spazio non rappresentabile")
	}
	return uint64(size), nil
}

// ForecastNextBackup projects the largest still-pending appointment within
// tolerance or the next unsatisfied future appointment, including model growth.
// Quota checks use its size without the disk's extra 20% margin.
func ForecastNextBackup(learned KeyModel, now time.Time, observations []Observation) (uint64, error) {
	if !learned.Reliable || learned.ReviewRequired {
		return 0, nil
	}
	loc, err := time.LoadLocation(learned.Timezone)
	if err != nil {
		return 0, err
	}
	type slotKey struct {
		at  time.Time
		app Appointment
	}
	satisfied := make(map[slotKey]int)
	slots, _, _, matches := matchAppointments(learned, observations, now.Add(-ScheduleTolerance), now.Add(ScheduleTolerance), now, loc)
	for i, slot := range slots {
		if matches[i] >= 0 {
			satisfied[slotKey{slot.at, slot.app}]++
		}
	}
	var next time.Time
	var size, pending uint64
	first := localMidnight(now.Add(-ScheduleTolerance), loc)
	for i := 0; i <= 8; i++ {
		day := first.AddDate(0, 0, i)
		for _, app := range learned.Schedule[day.Weekday()] {
			at := appointmentTime(day, app.MinuteOfDay, loc)
			if at.Add(ScheduleTolerance).Before(now) {
				continue
			}
			key := slotKey{at, app}
			if satisfied[key] > 0 {
				satisfied[key]--
				continue
			}
			projected, err := forecastSize(learned, app, at, 0)
			if err != nil {
				return 0, err
			}
			if !at.After(now) {
				pending = max(pending, projected)
				continue
			}
			if next.IsZero() || at.Before(next) {
				next, size = at, projected
			} else if at.Equal(next) && projected > size {
				size = projected
			}
		}
	}
	size = max(size, pending)
	return size, nil
}

func DailyRequirement(expected48h uint64) uint64 {
	return expected48h/2 + expected48h%2
}

type checkSlot struct {
	at     time.Time
	app    Appointment
	report bool
}

// Check matches every expected appointment at most once. It includes adjacent
// civil days only while matching, so a copy close to midnight can satisfy the
// correct slot without extending the period that is reported. Chronological
// matching is deterministic and maximizes the number of satisfied slots even
// when their tolerance windows overlap.
func Check(m KeyModel, observations []Observation, now time.Time) []Finding {
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return []Finding{{StableKey: "timezone:" + m.KeyID, Kind: "model", KeyID: m.KeyID, Detail: "fuso del modello non valido"}}
	}
	today := localMidnight(now, loc)
	return checkWindow(m, observations, today.AddDate(0, 0, -1).Add(-time.Nanosecond), today.AddDate(0, 0, 1).Add(-time.Nanosecond), now)
}

// CheckPeriod verifies every civil appointment and received copy in the
// interval (from, to]. Stable finding keys make retried work idempotent, while
// the open lower bound prevents adjacent completed periods from double-counting
// an appointment or observation at their shared boundary.
func CheckPeriod(m KeyModel, observations []Observation, from, to, now time.Time) []Finding {
	if to.Before(from) {
		return []Finding{{StableKey: "period:" + m.KeyID, Kind: "model", KeyID: m.KeyID, Detail: "periodo di controllo non valido"}}
	}
	return checkWindow(m, observations, from, to, now)
}

func checkWindow(m KeyModel, observations []Observation, reportFrom, reportTo, now time.Time) []Finding {
	if !m.Reliable || m.ReviewRequired {
		return []Finding{{StableKey: "model:" + m.KeyID, Kind: "model", KeyID: m.KeyID, Detail: "modello non affidabile o da revisionare"}}
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return []Finding{{StableKey: "timezone:" + m.KeyID, Kind: "model", KeyID: m.KeyID, Detail: "fuso del modello non valido"}}
	}
	expected, ordered, matchObservation, matchSlot := matchAppointments(m, observations, reportFrom, reportTo, now, loc)
	var findings []Finding
	for slotIndex, slot := range expected {
		if !slot.report || now.Before(slot.at.Add(ScheduleTolerance)) {
			continue
		}
		stable := fmt.Sprintf("slot:%s:%s", m.KeyID, slot.at.UTC().Format(time.RFC3339))
		if matchSlot[slotIndex] < 0 {
			findings = append(findings, Finding{StableKey: stable, Kind: "schedule_missing", KeyID: m.KeyID, Detail: "copia attesa non ricevuta", At: slot.at})
			continue
		}
		observation := ordered[matchSlot[slotIndex]]
		days := observation.At.Sub(m.LearnedAt).Hours() / 24
		expectedSize := float64(slot.app.MedianSize) + slot.app.GrowthPerDay*days
		if expectedSize <= 0 || math.Abs(float64(observation.Size)-expectedSize) > expectedSize*SizeTolerance {
			findings = append(findings, Finding{StableKey: "size:" + observation.BackupID, Kind: "size", KeyID: m.KeyID, BackupID: observation.BackupID, Detail: "dimensione fuori dalla tolleranza del 20%", At: observation.At})
		}
	}
	expectedCount, receivedCount, excessBytes := 0, 0, int64(0)
	for _, slot := range expected {
		if slot.report {
			expectedCount++
		}
	}
	for i, observation := range ordered {
		if observation.KeyID == m.KeyID && observation.At.After(reportFrom) && !observation.At.After(reportTo) && !observation.At.After(now) {
			receivedCount++
			if matchObservation[i] < 0 {
				excessBytes += observation.Size
			}
		}
	}
	period := fmt.Sprintf("%s..%s", reportFrom.In(loc).Format(time.RFC3339), reportTo.In(loc).Format(time.RFC3339))
	for i, observation := range ordered {
		if matchObservation[i] < 0 && observation.KeyID == m.KeyID && observation.At.After(reportFrom) && !observation.At.After(reportTo) && !observation.At.After(now) {
			detail := fmt.Sprintf("AVVISO NON BLOCCANTE - copie eccedenti: chiave %s, periodo %s, attese %d, ricevute %d, volume eccedente %d byte; verificare configurazione o possibili caricamenti impropri", m.KeyID, period, expectedCount, receivedCount, excessBytes)
			findings = append(findings, Finding{StableKey: "extra:" + observation.BackupID, Kind: "extra", KeyID: m.KeyID, BackupID: observation.BackupID, Detail: detail, At: observation.At})
		}
	}
	return findings
}

// matchAppointments shares chronological, one-to-one matching between monitoring
// and quota forecasting, including observations across civil-day boundaries.
func matchAppointments(m KeyModel, observations []Observation, reportFrom, reportTo, now time.Time, loc *time.Location) ([]checkSlot, []Observation, []int, []int) {
	startDay := localMidnight(reportFrom, loc).AddDate(0, 0, -1)
	// Matching must not depend on the report boundary, even for a chain of
	// overlapping slots spanning several days. Include the observed prefix;
	// only the requested interval produces findings.
	for _, observation := range observations {
		if observation.KeyID == m.KeyID && !observation.At.After(now) {
			candidate := localMidnight(observation.At, loc).AddDate(0, 0, -1)
			if candidate.Before(startDay) {
				startDay = candidate
			}
		}
	}
	endDay := localMidnight(reportTo, loc).AddDate(0, 0, 2)
	var expected []checkSlot
	for day := startDay; day.Before(endDay); day = day.AddDate(0, 0, 1) {
		for _, app := range m.Schedule[day.Weekday()] {
			at := appointmentTime(day, app.MinuteOfDay, loc)
			if at.Add(-ScheduleTolerance).After(now) {
				continue
			}
			expected = append(expected, checkSlot{at: at, app: app, report: at.After(reportFrom) && !at.After(reportTo)})
		}
	}
	sort.SliceStable(expected, func(i, j int) bool { return expected[i].at.Before(expected[j].at) })
	ordered := append([]Observation(nil), observations...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].At.Equal(ordered[j].At) {
			return ordered[i].BackupID < ordered[j].BackupID
		}
		return ordered[i].At.Before(ordered[j].At)
	})
	matchObservation := make([]int, len(ordered))
	matchSlot := make([]int, len(expected))
	for i := range matchObservation {
		matchObservation[i] = -1
	}
	for i := range matchSlot {
		matchSlot[i] = -1
	}
	// With equal symmetric tolerance windows, pairing the earliest copy with
	// the earliest available compatible slot is a maximum matching (an
	// exchange of crossing pairs never loses a match). Later arrivals cannot
	// displace earlier copies, even across completed monitoring boundaries.
	slotIndex := 0
	for observationIndex, observation := range ordered {
		if observation.KeyID != m.KeyID || observation.At.After(now) {
			continue
		}
		for slotIndex < len(expected) && expected[slotIndex].at.Add(ScheduleTolerance).Before(observation.At) {
			slotIndex++
		}
		if slotIndex < len(expected) && !observation.At.Before(expected[slotIndex].at.Add(-ScheduleTolerance)) {
			matchObservation[observationIndex] = slotIndex
			matchSlot[slotIndex] = observationIndex
			slotIndex++
		}
	}
	return expected, ordered, matchObservation, matchSlot
}

type Filesystem struct {
	Blocks    uint64 `json:"blocks"`
	Bfree     uint64 `json:"blocks_free"`
	Bavail    uint64 `json:"blocks_available"`
	BlockSize uint64 `json:"block_size"`
}

func (f Filesystem) UsedBasisPoints() (int, error) {
	if f.Blocks == 0 || f.Bfree > f.Blocks || f.BlockSize == 0 {
		return 0, errors.New("misura filesystem non valida")
	}
	used := f.Blocks - f.Bfree
	if used > math.MaxUint64/10000 {
		return 0, errors.New("misura filesystem non rappresentabile")
	}
	return int(used * 10000 / f.Blocks), nil
}

func (f Filesystem) Bytes() (total, used, available uint64, err error) {
	if _, err = f.UsedBasisPoints(); err != nil || f.Blocks > math.MaxUint64/f.BlockSize || f.Bfree > math.MaxUint64/f.BlockSize || f.Bavail > math.MaxUint64/f.BlockSize {
		if err == nil {
			err = errors.New("misura filesystem non rappresentabile")
		}
		return
	}
	total = f.Blocks * f.BlockSize
	used = (f.Blocks - f.Bfree) * f.BlockSize
	available = f.Bavail * f.BlockSize
	return
}

// RetentionProtected preserves the current day and the configured number of
// complete civil days, including DST boundaries.
func RetentionProtected(backup model.Backup, now time.Time, timezone string, days int) (bool, error) {
	if days < MinimumDays || days > 365000 {
		return true, errors.New("minimo di conservazione non valido")
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return true, err
	}
	cutoff := localMidnight(now, loc).AddDate(0, 0, -days)
	return !backupTimeForPolicy(backup).Before(cutoff), nil
}

type SelectionInput struct {
	Now                  time.Time
	Timezone             string
	Filesystem           Filesystem
	ThresholdBasisPoints int
	RetentionDays        int
	KeyRetentionDays     map[string]int
	Expected48hBytes     uint64
	PendingPurgeBytes    uint64
	Backups              []model.Backup
	RevokedKeys          map[string]bool
}

type Selection struct {
	Eligible        bool           `json:"eligible"`
	Reason          string         `json:"reason"`
	UsedBasisPoints int            `json:"used_basis_points"`
	BytesNeeded     uint64         `json:"bytes_needed"`
	SelectedBytes   uint64         `json:"selected_bytes"`
	ShortfallBytes  uint64         `json:"shortfall_bytes"`
	Candidates      []model.Backup `json:"candidates"`
}

func Select(input SelectionInput) (Selection, error) {
	var result Selection
	if input.ThresholdBasisPoints < 1 || input.ThresholdBasisPoints > 10000 {
		return result, errors.New("soglia filesystem non valida")
	}
	usedBasis, err := input.Filesystem.UsedBasisPoints()
	if err != nil {
		return result, err
	}
	result.UsedBasisPoints = usedBasis
	if usedBasis < input.ThresholdBasisPoints {
		result.Reason = "occupazione inferiore alla soglia"
		return result, nil
	}
	loc, err := time.LoadLocation(input.Timezone)
	if err != nil {
		return result, err
	}
	if input.RetentionDays < MinimumDays {
		return result, errors.New("finestra di conservazione inferiore a 7 giorni")
	}
	total, used, _, err := input.Filesystem.Bytes()
	if err != nil {
		return result, err
	}
	threshold := uint64(input.ThresholdBasisPoints)
	target := total/10000*threshold + (total%10000)*threshold/10000
	// A trigger at exactly 80% must select enough to return strictly below the
	// threshold, not merely back to the same boundary.
	if target > 0 {
		target--
	}
	projected := used
	if math.MaxUint64-projected < input.Expected48hBytes {
		return result, errors.New("previsione spazio non rappresentabile")
	}
	projected += input.Expected48hBytes
	needed := uint64(0)
	if projected > target {
		needed = projected - target
	}
	minimumRelease := total/10000*MinimumReleaseBasisPoints + (total%10000)*MinimumReleaseBasisPoints/10000
	if total%10000*MinimumReleaseBasisPoints%10000 != 0 {
		minimumRelease++
	}
	if needed < minimumRelease {
		needed = minimumRelease
	}
	if input.PendingPurgeBytes >= needed {
		result.Eligible = true
		result.Reason = "richieste pendenti gia sufficienti"
		return result, nil
	}
	needed -= input.PendingPurgeBytes
	result.BytesNeeded = needed
	today := localMidnight(input.Now, loc)

	counts := make(map[string]int)
	for _, backup := range input.Backups {
		if backup.Status == model.BackupComplete {
			counts[backup.KeyID]++
		}
	}
	byDay := make(map[string][]model.Backup)
	var days []string
	for _, backup := range input.Backups {
		if backup.Status != model.BackupComplete && backup.Status != model.BackupDeleting && backup.Status != model.BackupQuarantined && backup.Status != model.BackupPurging {
			continue
		}
		minimum := input.RetentionDays
		if days, ok := input.KeyRetentionDays[backup.KeyID]; ok {
			minimum = days
		}
		if minimum < MinimumDays || minimum > 365000 {
			return result, errors.New("minimo di conservazione non valido")
		}
		protectedAfter := today.AddDate(0, 0, -minimum)
		day := localMidnight(backupTimeForPolicy(backup), loc)
		if !day.Before(protectedAfter) || !day.Before(today) {
			continue
		}
		key := day.Format("2006-01-02")
		if _, exists := byDay[key]; !exists {
			days = append(days, key)
		}
		byDay[key] = append(byDay[key], backup)
	}
	sort.Strings(days)
	var selected uint64
	for _, day := range days {
		items := byDay[day]
		valid := true
		dayCounts := make(map[string]int)
		for _, backup := range items {
			dayCounts[backup.KeyID]++
		}
		for _, backup := range items {
			if backup.Status != model.BackupComplete || input.RevokedKeys[backup.KeyID] || counts[backup.KeyID]-dayCounts[backup.KeyID] < 1 {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		for _, backup := range items {
			result.Candidates = append(result.Candidates, backup)
			selected += uint64(backup.Size)
			counts[backup.KeyID]--
		}
		if selected >= needed {
			break
		}
	}
	result.SelectedBytes = selected
	result.Eligible = len(result.Candidates) != 0
	if selected < needed {
		result.ShortfallBytes = needed - selected
		result.Reason = "candidati leciti insufficienti"
	} else {
		result.Reason = "candidati limitati alla quantita necessaria"
	}
	return result, nil
}

func backupTimeForPolicy(backup model.Backup) time.Time {
	if backup.ReceivedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, backup.ReceivedAt); err == nil {
			return t
		}
	}
	return time.Unix(backup.StartedAt, 0).UTC()
}
