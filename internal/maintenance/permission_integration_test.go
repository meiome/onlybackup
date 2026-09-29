package maintenance

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/meiome/onlybackup/internal/localapi"
	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/policy"
	"github.com/meiome/onlybackup/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExtraCopyAcrossCompletedCheckBoundary(t *testing.T) {
	f := newPermissionFixture(t)
	day := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	first := f.add(t, day.Add(95*time.Minute))
	f.now = day.Add(125 * time.Minute)
	s := f.service(t, nil)
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := f.add(t, day.Add(130*time.Minute))
	for _, minute := range []int{165, 170} {
		f.now = day.Add(time.Duration(minute) * time.Minute)
		if err := s.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	anomalies, err := f.db.Anomalies(true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, a := range anomalies {
		if a.Kind == "extra" {
			count++
			if a.BackupID != second.ID || a.BackupID == first.ID || a.EventAt != day.Add(130*time.Minute).Unix() {
				t.Fatalf("wrong extra: %+v", a)
			}
		}
	}
	if count != 1 {
		t.Fatalf("extra count=%d: %+v", count, anomalies)
	}
	messages, err := f.db.DueMail(f.now, 100)
	if err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, m := range messages {
		if m.Kind == "anomaly" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate/lost extra mail: %d", count)
	}
}

// Inject changes after the writer commits final permission, before maintenance
// receives its response and therefore strictly before rename/unlink.
func TestFinalPermissionSurvivesRevocationAndTimeout(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		for _, event := range []string{"revoke", "timeout"} {
			t.Run(kind+"/"+event, func(t *testing.T) {
				f := newPermissionFixture(t)
				op, lease := f.authorize(t, kind)
				injected := false
				s := f.service(t, func() {
					injected = true
					persisted, err := f.db.Operation(op.ID)
					if err != nil || !persisted.ExecutionCommitted {
						t.Errorf("permission not durable: %+v %v", persisted, err)
					}
					if event == "revoke" {
						if err = f.db.Revoke(f.key); err != nil {
							t.Error(err)
						}
					} else {
						f.now = f.now.Add(11 * time.Minute)
					}
					other := New(f.root, "unused", "other", nil)
					if err = other.RunOnce(context.Background()); !errors.Is(err, ErrExecutorActive) {
						t.Errorf("concurrent executor admitted: %v", err)
					}
				})
				lock, err := s.lockExecution()
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				guard := &leaseGuard{service: s, lease: lease}
				if err = s.executeLeased(context.Background(), guard, op, f.backups[0]); err != nil {
					t.Fatal(err)
				}
				if !injected {
					t.Fatal("boundary not reached")
				}
				directory := "backups"
				if kind == "purge" {
					directory = "quarantine"
				}
				if _, err = os.Lstat(filepath.Join(f.root, directory, op.BackupID+".backup")); !os.IsNotExist(err) {
					t.Fatalf("source not removed: %v", err)
				}
				if err = f.db.ReconcileAuthorized(op.ID, f.now); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestRunOnceHoldsLockPastFinalPermissionUntilReconciliation(t *testing.T) {
	f := newPermissionFixture(t)
	f.add(t, f.now.Add(-10*time.Hour))
	op, err := f.db.RequestQuarantine(f.backups[0].ID, "manual", "admin", "concurrent executor", f.now)
	if err != nil {
		t.Fatal(err)
	}
	injected := false
	s := f.service(t, func() {
		injected = true
		f.now = f.now.Add(11 * time.Minute)
		if err := New(f.root, "unused", "second-worker", nil).RunOnce(context.Background()); !errors.Is(err, ErrExecutorActive) {
			t.Errorf("timeout admitted second executor: %v", err)
		}
		if err := f.db.Revoke(f.key); err != nil {
			t.Error(err)
		}
	})
	// The physical operation completes, but its confirmation loses the expired
	// protocol lease. The next cycle must reconcile, not redo or cancel it.
	if err = s.RunOnce(context.Background()); err == nil || !injected {
		t.Fatalf("expired confirmation: injected=%t err=%v", injected, err)
	}
	if _, err = os.Lstat(filepath.Join(f.root, "backups", op.BackupID+".backup")); !os.IsNotExist(err) {
		t.Fatalf("admitted physical work stopped: %v", err)
	}
	if err = s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := f.db.Operation(op.ID)
	if err != nil || stored.State != "confirmed" || !stored.ExecutionCommitted {
		t.Fatalf("completion lost: %+v %v", stored, err)
	}
}

func TestCommittedWorkResumesAfterCrashDespiteRevocation(t *testing.T) {
	for _, kind := range []string{"quarantine", "purge"} {
		for _, phase := range []string{"before-permission", "lost-permission-response", "after-physical", "uncertain"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				f := newPermissionFixture(t)
				op, lease := f.authorize(t, kind)
				if phase != "before-permission" {
					if err := f.db.ValidateOperation(op.ID, op.Ticket, lease, f.now); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "after-physical" {
					if err := New(f.root, "", "", nil).execute(op, f.backups[0]); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "uncertain" {
					source, dest := "backups", "quarantine"
					if kind == "purge" {
						source, dest = dest, source
					}
					if err := os.Link(filepath.Join(f.root, source, op.BackupID+".backup"), filepath.Join(f.root, dest, op.BackupID+".backup")); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.db.Revoke(f.key); err != nil {
					t.Fatal(err)
				}
				// Reopen the DB and let the old protocol lease expire, as after death.
				if err := f.db.Close(); err != nil {
					t.Fatal(err)
				}
				var err error
				f.db, err = store.Open(filepath.Join(f.root, "metadata.db"), false)
				if err != nil {
					t.Fatal(err)
				}
				f.now = f.now.Add(11 * time.Minute)
				s := f.service(t, nil)
				err = s.RunOnce(context.Background())
				if phase == "uncertain" {
					if err == nil {
						t.Fatal("uncertain physical state accepted")
					}
					op, err = f.db.Operation(op.ID)
					if err != nil || op.State != "authorized" || !op.ExecutionCommitted {
						t.Fatalf("lost committed work: %+v %v", op, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				op, err = f.db.Operation(op.ID)
				want := "confirmed"
				if phase == "before-permission" {
					want = "cancelled"
				}
				if err != nil || op.State != want {
					t.Fatalf("state=%+v want=%s err=%v", op, want, err)
				}
				if err = s.RunOnce(context.Background()); err != nil {
					t.Fatal("idempotent retry:", err)
				}
			})
		}
	}
}

func TestExecutorLockReleasedByProcessDeath(t *testing.T) {
	if root := os.Getenv("ONLYBACKUP_LOCK_TEST_ROOT"); root != "" {
		lock, err := New(root, "", "", nil).lockExecution()
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		fmt.Println("locked")
		var b [1]byte
		_, _ = os.Stdin.Read(b[:])
		return
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "maintenance"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutorLockReleasedByProcessDeath$")
	cmd.Env = append(os.Environ(), "ONLYBACKUP_LOCK_TEST_ROOT="+root)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var ready string
	if _, err = fmt.Fscanln(output, &ready); err != nil || ready != "locked" {
		t.Fatalf("child startup: %q %v", ready, err)
	}
	s := New(root, "", "", nil)
	if err = s.RunOnce(context.Background()); !errors.Is(err, ErrExecutorActive) {
		t.Fatalf("second process entered: %v", err)
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := s.lockExecution()
	if err != nil {
		t.Fatal("lock survived process death:", err)
	}
	lock.Close()
}

type permissionFixture struct {
	root       string
	now        time.Time
	db         *store.Store
	key, token string
	backups    []model.Backup
}

func newPermissionFixture(t *testing.T, omit ...int) *permissionFixture {
	t.Helper()
	f := &permissionFixture{root: filepath.Join(t.TempDir(), "state"), now: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
	if err := store.Init(f.root); err != nil {
		t.Fatal(err)
	}
	var err error
	f.db, err = store.Open(filepath.Join(f.root, "metadata.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.db.Close() })
	key, token, err := f.db.CreateKey("audit", "XS")
	if err != nil {
		t.Fatal(err)
	}
	f.key, f.token = key.ID, token
	for day := -14; day < 0; day++ {
		skip := false
		for _, d := range omit {
			if d == day {
				skip = true
			}
		}
		if !skip {
			f.backups = append(f.backups, f.add(t, f.now.AddDate(0, 0, day).Add(-10*time.Hour)))
		}
	}
	if err = f.db.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "a@test", Recipients: "b@test"}, "UTC", f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.db.MarkMailTested(f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.db.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	m := policy.KeyModel{KeyID: f.key, Timezone: "UTC", LearnedAt: f.now.AddDate(0, 0, -14), Reliable: true, Schedule: map[time.Weekday][]policy.Appointment{}}
	for d := time.Sunday; d <= time.Saturday; d++ {
		m.Schedule[d] = []policy.Appointment{{MinuteOfDay: 120, MedianSize: 14}}
	}
	if err = f.db.SaveModel(f.key, 7, "UTC", 7, m, true, false, m.LearnedAt); err != nil {
		t.Fatal(err)
	}
	if err = f.db.SetCheckState(model.MonitoringRegular, 7, f.now); err != nil {
		t.Fatal(err)
	}
	if err = f.db.ResumeRetention(f.now); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *permissionFixture) add(t *testing.T, at time.Time) model.Backup {
	t.Helper()
	data := []byte("review payload")
	b, err := f.db.ReserveIdempotent(f.token, model.Metadata{Description: "audit", OriginalName: "db.bin"}, int64(len(data)), fmt.Sprintf("%x", sha256.Sum256(data)), fmt.Sprintf("audit-%d", at.UnixNano()), at)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(f.root, "backups", b.ID+".backup"), data, 0440); err != nil {
		t.Fatal(err)
	}
	if err = f.db.Complete(b.ID, at); err != nil {
		t.Fatal(err)
	}
	b, err = f.db.Backup(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func (f *permissionFixture) service(t *testing.T, afterValidate func()) *Service {
	t.Helper()
	api := localapi.New(f.db, f.root)
	api.Now = func() time.Time { return f.now }
	h := api.MaintenanceHandler()
	if afterValidate != nil {
		base := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/validate" {
				base.ServeHTTP(w, r)
				return
			}
			rr := httptest.NewRecorder()
			base.ServeHTTP(rr, r)
			if rr.Code == 200 {
				afterValidate()
			}
			for k, vs := range rr.Header() {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(rr.Code)
			_, _ = w.Write(rr.Body.Bytes())
		})
	}
	socket := filepath.Join(f.root, "audit.sock")
	serveUnix(t, socket, h)
	s := New(f.root, socket, "audit-worker", nil)
	s.Now = func() time.Time { return f.now }
	s.Filesystem = func(string) (policy.Filesystem, error) {
		return policy.Filesystem{Blocks: 1000000, Bfree: 900000, Bavail: 900000, BlockSize: 4096}, nil
	}
	return s
}
func (f *permissionFixture) authorize(t *testing.T, kind string) (model.RetentionOperation, model.MaintenanceLease) {
	t.Helper()
	lease, err := f.db.AcquireMaintenanceLease("audit-worker", 0, f.now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.db.RequestQuarantine(f.backups[0].ID, "manual", "admin", "audit", f.now)
	if err != nil {
		t.Fatal(err)
	}
	op, err = f.db.AuthorizeOperation(op.ID, lease, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "purge" {
		if err = os.Rename(filepath.Join(f.root, "backups", op.BackupID+".backup"), filepath.Join(f.root, "quarantine", op.BackupID+".backup")); err != nil {
			t.Fatal(err)
		}
		if err = f.db.ConfirmOperation(op.ID, op.Ticket, true, "", lease, f.now); err != nil {
			t.Fatal(err)
		}
		if err = f.db.ReleaseMaintenanceLease(lease); err != nil {
			t.Fatal(err)
		}
		f.now = f.now.Add(48 * time.Hour)
		lease, err = f.db.AcquireMaintenanceLease("audit-worker", 0, f.now, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		op, err = f.db.RequestPurge(op.BackupID, "automatic", "audit-worker", "audit", f.now)
		if err != nil {
			t.Fatal(err)
		}
		op, err = f.db.AuthorizeOperation(op.ID, lease, f.now)
		if err != nil {
			t.Fatal(err)
		}
	}
	return op, lease
}
