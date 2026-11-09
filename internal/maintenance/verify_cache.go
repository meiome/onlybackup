package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/meiome/onlybackup/internal/backupfile"
	"github.com/meiome/onlybackup/internal/model"
)

// Archived files are immutable (mode 0440, written once by the writer), so a
// full SHA-256 on every five-minute cycle only burns disk bandwidth: on a
// 357 GiB archive one pass outlasts the cycle and maintenance never idles.
//
// Monitoring therefore remembers each verified file's identity. A file is
// hashed again when it is new, when its identity changes, or when its last
// verification is old enough to be due for the rolling scrub. Destructive
// operations never consult this cache: they always hash the file first.

const (
	verifyCacheFile    = "verify-cache.json"
	verifyCacheVersion = 1
	// DefaultScrubPeriod bounds how old a monitoring verification may become.
	DefaultScrubPeriod = 7 * 24 * time.Hour
	// DefaultCycleInterval mirrors the onlybackup-maintenance default.
	DefaultCycleInterval = 5 * time.Minute
	// Hosts may be powered only part of the day: the per-cycle budget is
	// sized for a quarter of the nominal cycles so coverage keeps up.
	scrubSafetyFactor = 4
)

// hashFile is the single full-content check; tests count its calls.
var hashFile = backupfile.VerifyContext

type fileIdentity struct {
	Size    int64  `json:"size"`
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
	MtimeNs int64  `json:"mtime_ns"`
	CtimeNs int64  `json:"ctime_ns"`
}

type verifyRecord struct {
	SHA256     string       `json:"sha256"`
	Identity   fileIdentity `json:"identity"`
	Valid      bool         `json:"valid"`
	VerifiedAt int64        `json:"verified_at_unix"`
}

type verifyCache struct {
	Version int                     `json:"version"`
	Entries map[string]verifyRecord `json:"entries"`
}

func identityOf(info os.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{
		Size:    info.Size(),
		Dev:     uint64(stat.Dev),
		Ino:     uint64(stat.Ino),
		MtimeNs: int64(stat.Mtim.Sec)*int64(time.Second) + int64(stat.Mtim.Nsec),
		CtimeNs: int64(stat.Ctim.Sec)*int64(time.Second) + int64(stat.Ctim.Nsec),
	}, true
}

func (s *Service) verifyCachePath() string {
	return filepath.Join(s.Root, "maintenance", verifyCacheFile)
}

// loadVerifyCache never fails: an unreadable or foreign cache only costs a
// full verification, it can never vouch for a file.
func (s *Service) loadVerifyCache() *verifyCache {
	empty := &verifyCache{Version: verifyCacheVersion, Entries: map[string]verifyRecord{}}
	data, err := os.ReadFile(s.verifyCachePath())
	if err != nil {
		return empty
	}
	var cache verifyCache
	if json.Unmarshal(data, &cache) != nil || cache.Version != verifyCacheVersion || cache.Entries == nil {
		return empty
	}
	return &cache
}

func (s *Service) saveVerifyCache(cache *verifyCache) error {
	directory := filepath.Join(s.Root, "maintenance")
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return errors.New("directory maintenance non disponibile")
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	return atomicPrivateFile(s.verifyCachePath(), data)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) scrubPeriod() time.Duration {
	if s.ScrubPeriod > 0 {
		return s.ScrubPeriod
	}
	return DefaultScrubPeriod
}

func (s *Service) cycleInterval() time.Duration {
	if s.CycleInterval > 0 {
		return s.CycleInterval
	}
	return DefaultCycleInterval
}

// scrubBudget is the number of bytes a cycle may spend re-verifying files that
// are unchanged but due for the rolling scrub.
func (s *Service) scrubBudget(totalBytes int64) int64 {
	period, interval := s.scrubPeriod(), s.cycleInterval()
	if totalBytes <= 0 || interval >= period {
		return totalBytes
	}
	budget := float64(totalBytes) * float64(interval) / float64(period) * scrubSafetyFactor
	if budget > float64(totalBytes) {
		return totalBytes
	}
	return int64(budget)
}

type verifyCandidate struct {
	key    string
	path   string
	backup model.Backup
}

// fileVerifier answers validFile for monitoring, hashing only what is needed.
type fileVerifier struct {
	service *Service
	cache   *verifyCache
	now     time.Time
	budget  int64
	// scrub marks unchanged files whose rolling re-verification is due and
	// fits this cycle's budget; they are hashed even though cached.
	scrub map[string]bool
	dirty bool
}

func (s *Service) newFileVerifier(candidates []verifyCandidate) *fileVerifier {
	v := &fileVerifier{service: s, cache: s.loadVerifyCache(), now: s.now(), scrub: map[string]bool{}}
	var total int64
	type due struct {
		key        string
		size       int64
		verifiedAt int64
	}
	var stale []due
	halfPeriod := s.scrubPeriod() / 2
	for _, candidate := range candidates {
		total += candidate.backup.Size
		record, ok := v.cache.Entries[candidate.key]
		if !ok {
			continue
		}
		// Re-verification starts at half the period so that, with the budget
		// sized for the remaining half, no file exceeds the full period.
		if v.now.Sub(time.Unix(record.VerifiedAt, 0)) >= halfPeriod {
			stale = append(stale, due{candidate.key, candidate.backup.Size, record.VerifiedAt})
		}
	}
	sort.Slice(stale, func(i, j int) bool {
		if stale[i].verifiedAt != stale[j].verifiedAt {
			return stale[i].verifiedAt < stale[j].verifiedAt
		}
		return stale[i].key < stale[j].key
	})
	v.budget = s.scrubBudget(total)
	spent := int64(0)
	for i, item := range stale {
		// Always make progress on at least one file, even a large one.
		if i > 0 && spent+item.size > v.budget {
			break
		}
		spent += item.size
		v.scrub[item.key] = true
	}
	return v
}

func (v *fileVerifier) valid(ctx context.Context, key, path string, backup model.Backup) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != backup.Size {
		return false
	}
	identity, ok := identityOf(info)
	if !ok {
		return validFileContext(ctx, path, backup)
	}
	if record, cached := v.cache.Entries[key]; cached && !v.scrub[key] &&
		record.SHA256 == backup.SHA256 && record.Identity == identity {
		return record.Valid
	}
	err = hashFile(ctx, path, backup.Size, backup.SHA256)
	if ctx.Err() != nil {
		return false
	}
	// Re-read the identity: a file replaced during hashing must not be cached.
	after, statErr := os.Lstat(path)
	if statErr != nil {
		return false
	}
	if afterIdentity, ok := identityOf(after); ok && afterIdentity == identity {
		v.cache.Entries[key] = verifyRecord{SHA256: backup.SHA256, Identity: identity, Valid: err == nil, VerifiedAt: v.now.Unix()}
		delete(v.scrub, key)
		v.dirty = true
		// Persist after each hash so an interrupted cycle (shutdown, lease
		// loss) keeps the work already done.
		_ = v.service.saveVerifyCache(v.cache)
		return err == nil
	}
	return false
}

// prune drops entries for files no longer expected and saves the cache.
func (v *fileVerifier) prune(expected map[string]bool) {
	for key := range v.cache.Entries {
		if !expected[key] {
			delete(v.cache.Entries, key)
			v.dirty = true
		}
	}
	if v.dirty {
		_ = v.service.saveVerifyCache(v.cache)
		v.dirty = false
	}
}
