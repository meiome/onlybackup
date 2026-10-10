package localapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/meiome/onlybackup/internal/model"
	"github.com/meiome/onlybackup/internal/store"
)

func pendingVerificationFixture(t *testing.T) (*API, *store.Store, model.RetentionOperation, model.MaintenanceLease, time.Time) {
	t.Helper()
	api, s := apiFixture(t)
	now := api.Now()
	_, token, err := s.CreateKey("long-verification", "XS")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{42}, 3<<20)
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	var old model.Backup
	for _, day := range []int{-10, -1} {
		at := now.AddDate(0, 0, day)
		b, reserveErr := s.Reserve(token, model.Metadata{Description: "verification", OriginalName: "archive"}, int64(len(data)), digest, at)
		if reserveErr != nil {
			t.Fatal(reserveErr)
		}
		if err = os.WriteFile(filepath.Join(api.Root, "archives", "backups", b.ID+".backup"), data, 0440); err != nil {
			t.Fatal(err)
		}
		if err = s.Complete(b.ID, at); err != nil {
			t.Fatal(err)
		}
		if day == -10 {
			old = b
		}
	}
	if err = s.SetupAutomation(store.MailSettings{Host: "smtp.test", Port: 25, From: "backup@test", Recipients: "admin@test"}, "UTC", now); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkMailTested(now); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableAutomation(); err != nil {
		t.Fatal(err)
	}
	if err = s.SetCheckState(model.MonitoringRegular, 7, now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResumeRetention(now); err != nil {
		t.Fatal(err)
	}
	op, err := s.RequestQuarantine(old.ID, "manual", "admin", "verification lifecycle", now)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease("verification-worker", 0, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.AuthorizeOperation(op.ID, lease, now)
	if err != nil {
		t.Fatal(err)
	}
	return api, s, op, lease, now
}

// Hook a real multi-chunk file read through the cancellation interface. The
// third Err check occurs after its first chunk, not before verification starts.
type duringVerificationContext struct {
	context.Context
	checks          atomic.Int32
	afterFirstChunk func()
}

func (c *duringVerificationContext) Err() error {
	if c.checks.Add(1) == 3 {
		c.afterFirstChunk()
	}
	return c.Context.Err()
}

func assertVerificationDidNotChangeState(t *testing.T, s *store.Store, op model.RetentionOperation) {
	t.Helper()
	current, err := s.Operation(op.ID)
	if err != nil || current.State != "authorized" || current.ExecutionCommitted {
		t.Fatalf("verification changed pending operation: %+v %v", current, err)
	}
	active, err := s.Anomalies(true)
	if err != nil || len(active) != 0 {
		t.Fatalf("verification invented anomalies: %+v %v", active, err)
	}
}

func TestLeaseExpiryDuringFullHashPreventsReconciliation(t *testing.T) {
	api, s, op, lease, now := pendingVerificationFixture(t)
	currentTime := now
	api.Now = func() time.Time { return currentTime }
	ctx := &duringVerificationContext{Context: context.Background(), afterFirstChunk: func() { currentTime = now.Add(2 * time.Minute) }}
	if _, err := api.reconcileContext(ctx, op.ID, now, lease); !errors.Is(err, model.ErrLease) {
		t.Fatalf("expired lease accepted after hash: %v", err)
	}
	if ctx.checks.Load() < 3 {
		t.Fatal("expiry happened before reading archive")
	}
	assertVerificationDidNotChangeState(t, s, op)
}

func TestWriterBaseContextCancellationInterruptsActiveHash(t *testing.T) {
	api, s, op, lease, _ := pendingVerificationFixture(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	var interrupted atomic.Bool
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := &duringVerificationContext{Context: r.Context(), afterFirstChunk: func() { interrupted.Store(true); cancel() }}
		api.MaintenanceHandler().ServeHTTP(w, r.WithContext(ctx))
	}))
	server.Config.BaseContext = func(net.Listener) context.Context { return base }
	server.Start()
	defer server.Close()
	payload, err := json.Marshal(map[string]any{"id": op.ID, "owner": lease.Owner, "generation": lease.Generation})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", server.URL+"/v1/reconcile", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(protocolHeader, ProtocolVersion)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !interrupted.Load() || response.StatusCode != 409 || !strings.Contains(string(body), context.Canceled.Error()) {
		t.Fatalf("active hash did not stop on writer context cancellation: %d %s", response.StatusCode, body)
	}
	assertVerificationDidNotChangeState(t, s, op)
}
