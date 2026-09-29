package policy

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func extraIDs(findings []Finding) []string {
	var ids []string
	for _, f := range findings {
		if f.Kind == "extra" {
			ids = append(ids, f.BackupID)
		}
	}
	sort.Strings(ids)
	return ids
}

func TestMatchingAgreesWithExhaustiveAssignment(t *testing.T) {
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	slots := []int{600, 620, 640}
	m := KeyModel{KeyID: "k", Timezone: "UTC", LearnedAt: day, Reliable: true, Schedule: map[time.Weekday][]Appointment{day.Weekday(): {{MinuteOfDay: 600, MedianSize: 100}, {MinuteOfDay: 620, MedianSize: 100}, {MinuteOfDay: 640, MedianSize: 100}}}}
	// Exhaustively enumerate assignments independently of the production greedy
	// matcher, including choosing not to use a compatible copy.
	for mask := 0; mask < 256; mask++ {
		var obs []Observation
		for i, minute := range []int{569, 570, 590, 610, 610, 630, 650, 671} {
			if mask&(1<<i) != 0 {
				obs = append(obs, Observation{KeyID: "k", BackupID: fmt.Sprint(i), At: day.Add(time.Duration(minute) * time.Minute), Size: 100})
			}
		}
		bestCount, bestPriority := -1, -1
		var enumerate func(int, int, int, int)
		enumerate = func(i, used, count, priority int) {
			if i == len(obs) {
				if count > bestCount || count == bestCount && priority > bestPriority {
					bestCount, bestPriority = count, priority
				}
				return
			}
			enumerate(i+1, used, count, priority)
			for j, minute := range slots {
				d := obs[i].At.Sub(day.Add(time.Duration(minute) * time.Minute))
				if d < 0 {
					d = -d
				}
				if used&(1<<j) == 0 && d <= 30*time.Minute {
					enumerate(i+1, used|1<<j, count+1, priority|1<<(len(obs)-i-1))
				}
			}
		}
		enumerate(0, 0, 0, 0)
		var want []string
		for i, o := range obs {
			if bestPriority&(1<<(len(obs)-i-1)) == 0 {
				want = append(want, o.BackupID)
			}
		}
		sort.Strings(want)
		got := extraIDs(CheckPeriod(m, obs, day, day.Add(24*time.Hour-time.Nanosecond), day.Add(24*time.Hour)))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("mask=%d got=%v want=%v max=%d", mask, got, want, bestCount)
		}
		// Permuting input cannot change tie-breaking or classification.
		for i, j := 0, len(obs)-1; i < j; i, j = i+1, j-1 {
			obs[i], obs[j] = obs[j], obs[i]
		}
		if reversed := extraIDs(Check(m, obs, day.Add(12*time.Hour))); !reflect.DeepEqual(reversed, want) {
			t.Fatalf("permutation: got=%v want=%v", reversed, want)
		}
	}
}

func TestMatchingStableAcrossReportPartitionsAndMidnight(t *testing.T) {
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	m := KeyModel{KeyID: "k", Timezone: "UTC", LearnedAt: day.AddDate(0, 0, -14), Reliable: true, Schedule: map[time.Weekday][]Appointment{}}
	for weekday := time.Sunday; weekday <= time.Saturday; weekday++ {
		for minute := 0; minute < 1440; minute += 20 {
			m.Schedule[weekday] = append(m.Schedule[weekday], Appointment{MinuteOfDay: minute, MedianSize: 100})
		}
	}
	var obs []Observation
	for minute := -3 * 1440; minute < 1440; minute += 20 {
		obs = append(obs, Observation{KeyID: "k", BackupID: fmt.Sprint(minute), At: day.Add(time.Duration(minute+10) * time.Minute), Size: 100})
	}
	obs = append(obs, Observation{KeyID: "k", BackupID: "late-extra", At: day.Add(1420 * time.Minute), Size: 100})
	now := day.Add(2 * 24 * time.Hour)
	from, to := day.Add(-3*24*time.Hour), day.Add(24*time.Hour)
	whole := extraIDs(CheckPeriod(m, obs, from, to, now))
	for i, j := 0, len(obs)-1; i < j; i, j = i+1, j-1 {
		obs[i], obs[j] = obs[j], obs[i]
	}
	var partitioned []string
	for start := from; start.Before(to); start = start.Add(35 * time.Minute) {
		end := start.Add(35 * time.Minute)
		if end.After(to) {
			end = to
		}
		partitioned = append(partitioned, extraIDs(CheckPeriod(m, obs, start, end, now))...)
	}
	sort.Strings(partitioned)
	if !reflect.DeepEqual(whole, partitioned) {
		t.Fatalf("whole=%v partitions=%v", whole, partitioned)
	}
}
