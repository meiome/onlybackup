package policy

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

func backup(id, key string, at time.Time, size int64, status string) model.Backup {
	return model.Backup{Receipt: model.Receipt{ID: id, Status: status, Size: size, ReceivedAt: at.UTC().Format(time.RFC3339Nano)}, KeyID: key, StartedAt: at.Unix()}
}

func TestAutomaticThresholdCurrentDayAndPendingBytes(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	items := []model.Backup{
		backup("00000000000000000000000000000001", "key", now.AddDate(0, 0, -20), 100, model.BackupComplete),
		backup("00000000000000000000000000000002", "key", now.AddDate(0, 0, -10), 100, model.BackupComplete),
		backup("00000000000000000000000000000003", "key", now, 100, model.BackupComplete),
	}
	base := SelectionInput{Now: now, Timezone: "UTC", ThresholdBasisPoints: 8000, RetentionDays: 7, Backups: items, RevokedKeys: map[string]bool{}}
	base.Filesystem = Filesystem{Blocks: 10000, Bfree: 2001, Bavail: 2001, BlockSize: 1}
	result, err := Select(base)
	if err != nil || result.Eligible || len(result.Candidates) != 0 {
		t.Fatalf("79.99%% selected: %+v %v", result, err)
	}
	base.Filesystem.Bfree, base.Filesystem.Bavail = 2000, 2000
	result, err = Select(base)
	if err != nil || !result.Eligible || len(result.Candidates) == 0 {
		t.Fatalf("80%% not selected: %+v %v", result, err)
	}
	if result.BytesNeeded != 1000 {
		t.Fatalf("il lotto minimo non e il 10%% del filesystem: %+v", result)
	}
	for _, candidate := range result.Candidates {
		if candidate.ID == items[2].ID {
			t.Fatal("current-day backup selected")
		}
	}
	base.PendingPurgeBytes = result.BytesNeeded
	result, err = Select(base)
	if err != nil || !result.Eligible || len(result.Candidates) != 0 || result.Reason != "richieste pendenti gia sufficienti" {
		t.Fatalf("pending bytes ignored: %+v %v", result, err)
	}
}

func TestSelectionReportsInsufficientLegalCandidates(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	items := []model.Backup{
		backup("00000000000000000000000000000001", "key", now.AddDate(0, 0, -30), 100, model.BackupComplete),
		backup("00000000000000000000000000000002", "key", now.AddDate(0, 0, -1), 100, model.BackupComplete),
	}
	result, err := Select(SelectionInput{Now: now, Timezone: "UTC", Filesystem: Filesystem{Blocks: 10000, Bfree: 1000, Bavail: 1000, BlockSize: 1},
		ThresholdBasisPoints: 8000, RetentionDays: 7, Backups: items, RevokedKeys: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Eligible || len(result.Candidates) != 1 || result.Reason != "candidati leciti insufficienti" || result.BytesNeeded <= uint64(result.Candidates[0].Size) || result.SelectedBytes != 100 || result.ShortfallBytes != result.BytesNeeded-100 {
		t.Fatalf("insufficienza non rappresentata esplicitamente: %+v", result)
	}
}

func TestSelectionUsesLargerOfMinimumBatchAndForecast(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var items []model.Backup
	for i := 0; i < 20; i++ {
		items = append(items, backup(time.Date(2026, 1, 1, 0, 0, 0, i+1, time.UTC).Format("20060102150405.000000000000000"), "key", now.AddDate(0, 0, -30+i), 100, model.BackupComplete))
	}
	result, err := Select(SelectionInput{Now: now, Timezone: "UTC", Filesystem: Filesystem{Blocks: 10000, Bfree: 2000, Bavail: 2000, BlockSize: 1},
		ThresholdBasisPoints: 8000, RetentionDays: 7, Expected48hBytes: 1500, Backups: items, RevokedKeys: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesNeeded != 1501 || result.SelectedBytes != 1600 || result.ShortfallBytes != 0 {
		t.Fatalf("la previsione piu grande del lotto minimo non e rispettata: %+v", result)
	}
}

func TestForecast48HoursIncludesGrowthMarginAndMultipleCopies(t *testing.T) {
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	modelA := KeyModel{KeyID: "a", Timezone: "UTC", LearnedAt: now, Reliable: true, Schedule: map[time.Weekday][]Appointment{
		time.Monday:  {{MinuteOfDay: 60, MedianSize: 100}, {MinuteOfDay: 120, MedianSize: 200}},
		time.Tuesday: {{MinuteOfDay: 60, MedianSize: 300, GrowthPerDay: 10}},
	}}
	modelB := KeyModel{KeyID: "b", Timezone: "UTC", LearnedAt: now, Reliable: true, Schedule: map[time.Weekday][]Appointment{
		time.Monday: {{MinuteOfDay: 180, MedianSize: 400}},
	}}
	forecast, err := Forecast48Hours([]KeyModel{modelA, modelB}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Monday: 120 + 240 + 480; Tuesday is slightly over one growth day: 373.
	if forecast != 1213 {
		t.Fatalf("previsione inattesa: %d", forecast)
	}
	if daily := DailyRequirement(forecast); daily != 607 {
		t.Fatalf("fabbisogno giornaliero inatteso: %d", daily)
	}
}

func TestForecast48HoursUsesWorstWindowInNextWeek(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	learned := KeyModel{KeyID: "key", Timezone: "UTC", LearnedAt: now, Reliable: true, Schedule: map[time.Weekday][]Appointment{
		time.Monday:   {{MinuteOfDay: 60, MedianSize: 100}},
		time.Saturday: {{MinuteOfDay: 60, MedianSize: 1000}},
		time.Sunday:   {{MinuteOfDay: 60, MedianSize: 1000}},
	}}
	forecast, err := Forecast48Hours([]KeyModel{learned}, now)
	if err != nil {
		t.Fatal(err)
	}
	if forecast != 2400 {
		t.Fatalf("non e stata scelta la finestra settimanale peggiore: %d", forecast)
	}
}

func TestLearnRequiresTwoConcordantWeeksAndHandlesMidnight(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 4, 6, 12, 0, 0, 0, loc)
	var observations []Observation
	for day := 14; day >= 1; day-- {
		date := now.AddDate(0, 0, -day)
		at := time.Date(date.Year(), date.Month(), date.Day(), 23, 55, 0, 0, loc)
		observations = append(observations, Observation{BackupID: date.Format("20060102") + "aaaaaaaaaaaaaaaaaaaaaaaa", KeyID: "key", At: at, Size: 1000 + int64(14-day)})
	}
	learned, err := Learn("key", "Europe/Rome", observations, now)
	if err != nil || !learned.Reliable || learned.ReviewRequired {
		t.Fatalf("model not learned: %+v %v", learned, err)
	}
	observations = observations[:len(observations)-1]
	unreliable, err := Learn("key", "Europe/Rome", observations, now)
	if err != nil || unreliable.Reliable {
		t.Fatalf("irregular history accepted: %+v %v", unreliable, err)
	}
}

func TestLearnIsolatesEachKeyHistory(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var observations []Observation
	for day := 14; day >= 1; day-- {
		date := now.AddDate(0, 0, -day)
		observations = append(observations,
			Observation{BackupID: date.Format("20060102") + "aaaaaaaaaaaaaaaaaaaaaaaa", KeyID: "key-a", At: time.Date(date.Year(), date.Month(), date.Day(), 2, 0, 0, 0, time.UTC), Size: 1000},
			Observation{BackupID: date.Format("20060102") + "bbbbbbbbbbbbbbbbbbbbbbbb", KeyID: "key-b", At: time.Date(date.Year(), date.Month(), date.Day(), 18, 0, 0, 0, time.UTC), Size: 9000},
		)
	}
	learned, err := Learn("key-a", "UTC", observations, now)
	if err != nil || !learned.Reliable {
		t.Fatalf("model not learned: %+v %v", learned, err)
	}
	for _, appointments := range learned.Schedule {
		if len(appointments) != 1 || appointments[0].MinuteOfDay != 120 || appointments[0].MedianSize != 1000 {
			t.Fatalf("other key contaminated model: %+v", learned.Schedule)
		}
	}
}

func TestLearnUsesCurrentWindowForScheduleTime(t *testing.T) {
	now := time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)
	var observations []Observation
	for day := 70; day >= 15; day-- {
		date := now.AddDate(0, 0, -day)
		observations = append(observations, Observation{BackupID: date.Format("20060102") + "aaaaaaaaaaaaaaaaaaaaaaaa", KeyID: "key", At: time.Date(date.Year(), date.Month(), date.Day(), 2, 0, 0, 0, time.UTC), Size: 1000})
	}
	for day := 14; day >= 1; day-- {
		date := now.AddDate(0, 0, -day)
		observations = append(observations, Observation{BackupID: date.Format("20060102") + "bbbbbbbbbbbbbbbbbbbbbbbb", KeyID: "key", At: time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, time.UTC), Size: 1000})
	}
	learned, err := Learn("key", "UTC", observations, now)
	if err != nil || !learned.Reliable {
		t.Fatalf("model not learned: %+v %v", learned, err)
	}
	for weekday, appointments := range learned.Schedule {
		if len(appointments) != 1 || appointments[0].MinuteOfDay != 12*60 {
			t.Fatalf("weekday %s retained old schedule: %+v", weekday, appointments)
		}
	}
}

func TestCheckHandlesDSTCivilAppointments(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		day  time.Time
	}{
		{"spring-forward", time.Date(2026, 3, 29, 0, 0, 0, 0, loc)},
		{"fall-back", time.Date(2026, 10, 25, 0, 0, 0, 0, loc)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			appointment := Appointment{MinuteOfDay: 2*60 + 30, MedianSize: 1000}
			at := appointmentTime(tc.day, appointment.MinuteOfDay, loc)
			model := KeyModel{KeyID: "key", Timezone: "Europe/Rome", Reliable: true,
				Schedule: map[time.Weekday][]Appointment{tc.day.Weekday(): {appointment}}}
			findings := Check(model, []Observation{{BackupID: "00000000000000000000000000000001", KeyID: "key", At: at, Size: 1000}}, tc.day.AddDate(0, 0, 1).Add(12*time.Hour))
			if len(findings) != 0 {
				t.Fatalf("falsa anomalia DST: %+v (appuntamento %s)", findings, at)
			}
		})
	}
}

func TestCheckMatchesOverlappingWindowsAtMostOnce(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	day := now.AddDate(0, 0, -1)
	model := KeyModel{KeyID: "key", Timezone: "UTC", Reliable: true,
		Schedule: map[time.Weekday][]Appointment{day.Weekday(): {
			{MinuteOfDay: 10 * 60, MedianSize: 1000},
			{MinuteOfDay: 10*60 + 20, MedianSize: 1000},
		}}}
	observations := []Observation{
		{BackupID: "00000000000000000000000000000001", KeyID: "key", At: time.Date(day.Year(), day.Month(), day.Day(), 10, 10, 0, 0, time.UTC), Size: 1000},
		{BackupID: "00000000000000000000000000000002", KeyID: "key", At: time.Date(day.Year(), day.Month(), day.Day(), 10, 30, 0, 0, time.UTC), Size: 1000},
	}
	if findings := Check(model, observations, now); len(findings) != 0 {
		t.Fatalf("finestre sovrapposte abbinate in modo errato: %+v", findings)
	}
	findings := Check(model, observations[:1], now)
	missing := 0
	for _, finding := range findings {
		if finding.Kind == "schedule_missing" {
			missing++
		}
	}
	if missing != 1 {
		t.Fatalf("una osservazione e stata usata due volte: %+v", findings)
	}
}

func TestCheckReportsCurrentDayExtrasAsSoonAsCertain(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	model := KeyModel{KeyID: "key", Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -14), Reliable: true,
		Schedule: map[time.Weekday][]Appointment{now.Weekday(): {{MinuteOfDay: 2 * 60, MedianSize: 100}}}}
	observations := []Observation{
		{BackupID: "00000000000000000000000000000001", KeyID: "key", At: time.Date(2026, 9, 28, 2, 0, 0, 0, time.UTC), Size: 100},
		{BackupID: "00000000000000000000000000000002", KeyID: "key", At: time.Date(2026, 9, 28, 2, 5, 0, 0, time.UTC), Size: 100},
		{BackupID: "00000000000000000000000000000003", KeyID: "key", At: time.Date(2026, 9, 28, 2, 10, 0, 0, time.UTC), Size: 100},
	}
	findings := Check(model, observations, now)
	extra := 0
	for _, finding := range findings {
		if finding.Kind == "extra" {
			extra++
		}
	}
	if extra != 2 {
		t.Fatalf("copie extra odierne=%d: %+v", extra, findings)
	}
}

func TestCheckProtectsSlotsAcrossMidnightAndOverlappingWindows(t *testing.T) {
	now := time.Date(2026, 9, 28, 23, 55, 0, 0, time.UTC)
	tomorrow := now.AddDate(0, 0, 1)
	model := KeyModel{KeyID: "key", Timezone: "UTC", Reliable: true,
		Schedule: map[time.Weekday][]Appointment{tomorrow.Weekday(): {
			{MinuteOfDay: 5, MedianSize: 100},
			{MinuteOfDay: 25, MedianSize: 100},
		}}}
	observations := []Observation{
		{BackupID: "00000000000000000000000000000001", KeyID: "key", At: now.Add(-5 * time.Minute), Size: 100},
		{BackupID: "00000000000000000000000000000002", KeyID: "key", At: now, Size: 100},
	}
	for _, finding := range Check(model, observations, now) {
		if finding.Kind == "extra" || finding.Kind == "schedule_missing" {
			t.Fatalf("slot futuro ancora valido classificato prematuramente: %+v", finding)
		}
	}
}

func TestCheckPeriodRecoversEveryMissedDayAfterFiveDayStop(t *testing.T) {
	schedule := make(map[time.Weekday][]Appointment)
	for day := time.Sunday; day <= time.Saturday; day++ {
		schedule[day] = []Appointment{{MinuteOfDay: 2 * 60, MedianSize: 1000}}
	}
	learned := KeyModel{KeyID: "key", Timezone: "Europe/Rome", LearnedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Schedule: schedule, Reliable: true}
	from := time.Date(2026, 9, 23, 11, 30, 0, 0, time.UTC)
	to := time.Date(2026, 9, 28, 11, 30, 0, 0, time.UTC)
	received := time.Date(2026, 9, 28, 2, 0, 0, 0, time.FixedZone("CEST", 2*60*60))
	observations := []Observation{{BackupID: "00000000000000000000000000000028", KeyID: "key", At: received, Size: 1000}}
	findings := CheckPeriod(learned, observations, from, to, to.Add(time.Hour))
	var missing []string
	for _, finding := range findings {
		if finding.Kind == "schedule_missing" {
			missing = append(missing, finding.At.In(time.FixedZone("CEST", 2*60*60)).Format("2006-01-02"))
		}
	}
	want := []string{"2026-09-24", "2026-09-25", "2026-09-26", "2026-09-27"}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("giorni saltati non recuperati: got %v want %v; findings=%+v", missing, want, findings)
	}
	repeated := CheckPeriod(learned, observations, from, to, to.Add(time.Hour))
	if !reflect.DeepEqual(findings, repeated) {
		t.Fatalf("ripetizione non deterministica: first=%+v repeated=%+v", findings, repeated)
	}
}

func TestCheckPeriodUsesCivilAppointmentsAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	for _, date := range []time.Time{
		time.Date(2026, 3, 28, 0, 0, 0, 0, location),
		time.Date(2026, 10, 24, 0, 0, 0, 0, location),
	} {
		schedule := make(map[time.Weekday][]Appointment)
		var observations []Observation
		for day := date; !day.After(date.AddDate(0, 0, 2)); day = day.AddDate(0, 0, 1) {
			schedule[day.Weekday()] = []Appointment{{MinuteOfDay: 8 * 60, MedianSize: 1000}}
			observations = append(observations, Observation{BackupID: day.Format("20060102") + "000000000000000000000000", KeyID: "key", At: appointmentTime(day, 8*60, location), Size: 1000})
		}
		learned := KeyModel{KeyID: "key", Timezone: "Europe/Rome", LearnedAt: date.AddDate(0, 0, -14), Schedule: schedule, Reliable: true}
		from := date.Add(-time.Nanosecond)
		to := date.AddDate(0, 0, 3).Add(-time.Nanosecond)
		if findings := CheckPeriod(learned, observations, from, to, to); len(findings) != 0 {
			t.Fatalf("appuntamenti DST %s: %+v", date.Format("2006-01-02"), findings)
		}
	}
}

func TestSelectionRespectsDifferentKeyMinimaAndCivilBoundary(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Rome")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, loc)
	cutoff := localMidnight(now, loc).AddDate(0, 0, -7)
	items := []model.Backup{
		backup("old-a", "a", now.AddDate(0, 0, -20), 100, model.BackupComplete),
		backup("old-b", "b", now.AddDate(0, 0, -20), 100, model.BackupComplete),
		backup("border", "a", cutoff, 100, model.BackupComplete),
		backup("outside", "a", cutoff.Add(-time.Second), 100, model.BackupComplete),
		backup("today-a", "a", now, 100, model.BackupComplete),
		backup("today-b", "b", now, 100, model.BackupComplete),
	}
	in := SelectionInput{Now: now, Timezone: "Europe/Rome", Filesystem: Filesystem{Blocks: 10000, Bfree: 2000, Bavail: 2000, BlockSize: 1}, ThresholdBasisPoints: 8000, RetentionDays: 7, KeyRetentionDays: map[string]int{"a": 7, "b": 30}, Backups: items}
	result, err := Select(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Candidates) != 2 || result.Candidates[0].ID != "old-a" || result.Candidates[1].ID != "outside" || result.ShortfallBytes != 800 {
		t.Fatalf("minima not respected: %+v", result)
	}
	in.Filesystem.Bfree = 2001
	result, err = Select(in)
	if err != nil || len(result.Candidates) != 0 {
		t.Fatalf("selection below threshold: %+v %v", result, err)
	}
}

func TestNextBackupForecastUsesNextAppointmentAndModelAge(t *testing.T) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	learned := KeyModel{KeyID: "key", Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -30), Reliable: true, Schedule: map[time.Weekday][]Appointment{
		time.Tuesday:   {{MinuteOfDay: 8 * 60, MedianSize: 9000}, {MinuteOfDay: 12 * 60, MedianSize: 100, GrowthPerDay: 10}},
		time.Wednesday: {{MinuteOfDay: 12 * 60, MedianSize: 2000}},
	}}
	size, err := ForecastNextBackup(learned, now, nil)
	if err != nil || size != 402 {
		t.Fatalf("next forecast=%d err=%v", size, err)
	}
	learned.Schedule = map[time.Weekday][]Appointment{time.Tuesday: {{MinuteOfDay: 8 * 60, MedianSize: 100, GrowthPerDay: 10}}}
	size, err = ForecastNextBackup(learned, now, nil)
	if err != nil || size != 470 {
		t.Fatalf("weekly forecast=%d err=%v", size, err)
	}
	learned.Schedule[time.Tuesday][0].GrowthPerDay = math.Inf(1)
	if _, err = ForecastNextBackup(learned, now, nil); err == nil {
		t.Fatal("invalid growth accepted")
	}
}

func TestQuotaForecastPendingAppointments(t *testing.T) {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		now          time.Time
		observations []Observation
		want         uint64
	}{
		{"nominal time", base, nil, 400},
		{"pending within tolerance", base.Add(5 * time.Minute), nil, 400},
		{"tolerance boundary", base.Add(ScheduleTolerance), nil, 400},
		{"expired", base.Add(ScheduleTolerance + time.Nanosecond), nil, 1},
		{"received", base.Add(5 * time.Minute), []Observation{{KeyID: "key", At: base, Size: 400}}, 1},
		{"early receipt", base.Add(-5 * time.Minute), []Observation{{KeyID: "key", At: base.Add(-10 * time.Minute), Size: 400}}, 1},
		{"other key", base.Add(5 * time.Minute), []Observation{{KeyID: "other", At: base, Size: 400}}, 400},
		{"future receipt ignored", base.Add(5 * time.Minute), []Observation{{KeyID: "key", At: base.Add(10 * time.Minute), Size: 400}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := KeyModel{KeyID: "key", Timezone: "UTC", LearnedAt: base.AddDate(0, 0, -30), Reliable: true, Schedule: map[time.Weekday][]Appointment{
				time.Tuesday:   {{MinuteOfDay: 720, MedianSize: 100, GrowthPerDay: 10}},
				time.Wednesday: {{MinuteOfDay: 720, MedianSize: 1}},
			}}
			want := tc.want
			got, err := ForecastNextBackup(m, tc.now, tc.observations)
			if err != nil || got != want {
				t.Fatalf("forecast=%d want=%d err=%v", got, want, err)
			}
		})
	}
}

func TestQuotaForecastMatchingAcrossMidnight(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 5, 0, 0, time.UTC)
	m := KeyModel{KeyID: "key", Timezone: "UTC", LearnedAt: now.AddDate(0, 0, -30), Reliable: true, Schedule: map[time.Weekday][]Appointment{
		time.Tuesday:   {{MinuteOfDay: 23*60 + 50, MedianSize: 100}},
		time.Wednesday: {{MinuteOfDay: 10, MedianSize: 200}, {MinuteOfDay: 720, MedianSize: 1}},
	}}
	// A single copy in overlapping windows satisfies only the earlier slot.
	obs := []Observation{{BackupID: "a", KeyID: "key", At: now.Add(-10 * time.Minute), Size: 100}}
	got, err := ForecastNextBackup(m, now, obs)
	if err != nil || got != 200 {
		t.Fatalf("forecast=%d err=%v", got, err)
	}
	obs = append(obs, Observation{BackupID: "b", KeyID: "key", At: now, Size: 200})
	got, err = ForecastNextBackup(m, now, obs)
	if err != nil || got != 1 {
		t.Fatalf("satisfied forecast=%d err=%v", got, err)
	}
	delete(m.Schedule, time.Wednesday)
	got, err = ForecastNextBackup(m, now, nil)
	if err != nil || got != 100 {
		t.Fatalf("previous day pending forecast=%d err=%v", got, err)
	}
}
