package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/backupfile"
	"github.com/meiome/onlybackup/internal/localclient"
	"github.com/meiome/onlybackup/internal/model"
)

type hashCounter struct{ paths []string }

func countHashes(t *testing.T) *hashCounter {
	t.Helper()
	counter := &hashCounter{}
	original := hashFile
	hashFile = func(ctx context.Context, path string, size int64, digest string) error {
		counter.paths = append(counter.paths, filepath.Base(path))
		return backupfile.VerifyContext(ctx, path, size, digest)
	}
	t.Cleanup(func() { hashFile = original })
	return counter
}

func verifyFixture(t *testing.T) (string, *Service) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"archives", "archives/backups", "archives/quarantine", "maintenance"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(1_800_000_000, 0)
	service := &Service{Root: root, Now: func() time.Time { return now }}
	return root, service
}

func storeBackup(t *testing.T, root string, n int, data []byte) model.Backup {
	t.Helper()
	digest := sha256.Sum256(data)
	backup := model.Backup{Receipt: model.Receipt{ID: fmt.Sprintf("%032x", n), Status: model.BackupComplete, Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}, KeyID: "key"}
	path := filepath.Join(root, "archives", "backups", backup.ID+".backup")
	temp := path + ".new"
	if err := os.WriteFile(temp, data, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	return backup
}

func TestVerifyCacheSkipsUnchangedFiles(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	snapshot := localclient.Snapshot{Backups: []model.Backup{storeBackup(t, root, 1, []byte("first")), storeBackup(t, root, 2, []byte("second"))}}
	for cycle := 0; cycle < 3; cycle++ {
		if findings := service.fileFindings(snapshot); len(findings) != 0 {
			t.Fatalf("ciclo %d: %+v", cycle, findings)
		}
	}
	if len(counter.paths) != 2 {
		t.Fatalf("file invariati riletti: %v", counter.paths)
	}
}

func TestVerifyCacheRehashesReplacedFile(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	backup := storeBackup(t, root, 1, []byte("original"))
	snapshot := localclient.Snapshot{Backups: []model.Backup{backup}}
	if findings := service.fileFindings(snapshot); len(findings) != 0 {
		t.Fatalf("%+v", findings)
	}
	// Same size, different content, new inode: must be detected.
	path := filepath.Join(root, "archives", "backups", backup.ID+".backup")
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	storeBackup(t, root, 1, []byte("tampered"))
	findings := service.fileFindings(snapshot)
	if len(findings) != 1 || findings[0].StableKey != "catalog-file:"+backup.ID {
		t.Fatalf("sostituzione non rilevata: %+v", findings)
	}
	if len(counter.paths) != 2 {
		t.Fatalf("hash attesi 2, ottenuti %v", counter.paths)
	}
}

func TestVerifyCacheDetectsInPlaceRewrite(t *testing.T) {
	root, service := verifyFixture(t)
	backup := storeBackup(t, root, 1, []byte("original"))
	snapshot := localclient.Snapshot{Backups: []model.Backup{backup}}
	if findings := service.fileFindings(snapshot); len(findings) != 0 {
		t.Fatalf("%+v", findings)
	}
	path := filepath.Join(root, "archives", "backups", backup.ID+".backup")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	// Restoring mtime cannot restore ctime.
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if findings := service.fileFindings(snapshot); len(findings) != 1 {
		t.Fatalf("riscrittura sul posto non rilevata: %+v", findings)
	}
}

func TestVerifyCacheRemembersInvalidFile(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	backup := storeBackup(t, root, 1, []byte("content"))
	backup.SHA256 = hex.EncodeToString(make([]byte, 32))
	snapshot := localclient.Snapshot{Backups: []model.Backup{backup}}
	for cycle := 0; cycle < 2; cycle++ {
		if findings := service.fileFindings(snapshot); len(findings) != 1 {
			t.Fatalf("ciclo %d: anomalia persa: %+v", cycle, findings)
		}
	}
	if len(counter.paths) != 1 {
		t.Fatalf("file corrotto riletto ad ogni ciclo: %v", counter.paths)
	}
}

func TestVerifyCacheCatalogDigestChangeForcesHash(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	backup := storeBackup(t, root, 1, []byte("content"))
	service.fileFindings(localclient.Snapshot{Backups: []model.Backup{backup}})
	backup.SHA256 = hex.EncodeToString(make([]byte, 32))
	if findings := service.fileFindings(localclient.Snapshot{Backups: []model.Backup{backup}}); len(findings) != 1 {
		t.Fatalf("digest di catalogo diverso accettato dalla cache: %+v", findings)
	}
	if len(counter.paths) != 2 {
		t.Fatalf("hash attesi 2, ottenuti %v", counter.paths)
	}
}

func TestVerifyCacheCorruptCacheFallsBackToHash(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	snapshot := localclient.Snapshot{Backups: []model.Backup{storeBackup(t, root, 1, []byte("content"))}}
	service.fileFindings(snapshot)
	if err := os.WriteFile(service.verifyCachePath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	service.fileFindings(snapshot)
	if len(counter.paths) != 2 {
		t.Fatalf("cache corrotta considerata valida: %v", counter.paths)
	}
}

func TestVerifyCacheSurvivesRestartAndPrunesRemovedBackups(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	first := storeBackup(t, root, 1, []byte("first"))
	second := storeBackup(t, root, 2, []byte("second"))
	service.fileFindings(localclient.Snapshot{Backups: []model.Backup{first, second}})
	restarted := &Service{Root: root, Now: service.Now}
	restarted.fileFindings(localclient.Snapshot{Backups: []model.Backup{first}})
	if len(counter.paths) != 2 {
		t.Fatalf("cache persa al riavvio: %v", counter.paths)
	}
	if entries := restarted.loadVerifyCache().Entries; len(entries) != 1 {
		t.Fatalf("voce di backup rimosso non eliminata: %+v", entries)
	}
}

func TestScrubRespectsBudgetOldestFirstAndNewFilesFirst(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	data := make([]byte, 1000)
	var backups []model.Backup
	for i := 1; i <= 4; i++ {
		backups = append(backups, storeBackup(t, root, i, data))
	}
	service.fileFindings(localclient.Snapshot{Backups: backups})
	counter.paths = nil

	// Budget: 4000 bytes * 1h / 8h * 4 = 2000 bytes, i.e. two files per cycle.
	service.ScrubPeriod = 8 * time.Hour
	service.CycleInterval = time.Hour
	now := service.Now().Add(4 * time.Hour)
	service.Now = func() time.Time { return now }
	fresh := storeBackup(t, root, 5, []byte("fresh upload"))
	all := append(append([]model.Backup{}, backups...), fresh)
	if findings := service.fileFindings(localclient.Snapshot{Backups: all}); len(findings) != 0 {
		t.Fatalf("%+v", findings)
	}
	if len(counter.paths) != 3 || counter.paths[0] != fresh.ID+".backup" {
		t.Fatalf("atteso nuovo file per primo e due file di scrub: %v", counter.paths)
	}
	counter.paths = nil
	service.fileFindings(localclient.Snapshot{Backups: all})
	if len(counter.paths) != 2 {
		t.Fatalf("lo scrub deve proseguire con i file restanti: %v", counter.paths)
	}
	counter.paths = nil
	service.fileFindings(localclient.Snapshot{Backups: all})
	if len(counter.paths) != 0 {
		t.Fatalf("nessun file dovrebbe essere ancora dovuto: %v", counter.paths)
	}
}

func TestScrubAlwaysProgressesOnLargeFile(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	backup := storeBackup(t, root, 1, make([]byte, 4096))
	snapshot := localclient.Snapshot{Backups: []model.Backup{backup}}
	service.fileFindings(snapshot)
	now := service.Now().Add(DefaultScrubPeriod)
	service.Now = func() time.Time { return now }
	service.fileFindings(snapshot)
	if len(counter.paths) != 2 {
		t.Fatalf("file oltre il budget mai riverificato: %v", counter.paths)
	}
}

func TestDestructiveVerificationIgnoresCache(t *testing.T) {
	root, service := verifyFixture(t)
	counter := countHashes(t)
	backup := storeBackup(t, root, 1, []byte("content"))
	service.fileFindings(localclient.Snapshot{Backups: []model.Backup{backup}})
	source := filepath.Join(root, "archives", "backups", backup.ID+".backup")
	destination := filepath.Join(root, "archives", "quarantine", backup.ID+".backup")
	if err := verifyExclusiveSourceContext(context.Background(), source, destination, backup); err != nil {
		t.Fatal(err)
	}
	if len(counter.paths) != 2 {
		t.Fatalf("operazione distruttiva senza hash completo: %v", counter.paths)
	}
}
