package localclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/meiome/onlybackup/internal/model"
)

func TestSnapshotPagesFailWithoutPublishingPartialCatalogue(t *testing.T) {
	for _, mode := range []string{"request_error", "cancelled", "bad_cursor", "changed_boundary", "success", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			firstID, lastID := strings.Repeat("a", 32), strings.Repeat("b", 32)
			calls := 0
			c := &Client{http: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Query().Get("paged") != "1" {
					t.Fatal("unbounded snapshot requested")
				}
				var value any
				if calls == 1 {
					initial := Snapshot{Backups: []model.Backup{{Receipt: model.Receipt{ID: firstID}, StartedAt: 1}}, NextBackupID: firstID, BackupsThrough: 2}
					if mode == "legacy" {
						initial.NextBackupID, initial.BackupsThrough = "", 0
					}
					value = initial
				} else {
					if calls > 2 || r.URL.Query().Get("after") != firstID || r.URL.Query().Get("through") != "2" {
						t.Fatalf("unexpected page request: %s", r.URL)
					}
					switch mode {
					case "request_error":
						return nil, errors.New("writer unavailable")
					case "cancelled":
						return nil, context.Canceled
					}
					page := model.BackupPage{Backups: []model.Backup{{Receipt: model.Receipt{ID: lastID}, StartedAt: 2}}, Through: 2}
					if mode == "bad_cursor" {
						page.Next = firstID
					}
					if mode == "changed_boundary" {
						page.Through = 3
					}
					value = page
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				header := make(http.Header)
				header.Set(protocolHeader, protocolVersion)
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})}}
			target := Snapshot{Backups: []model.Backup{{Receipt: model.Receipt{ID: "previous-snapshot"}}}}
			err := c.Do(context.Background(), "GET", "/v1/snapshot", nil, &target)
			if mode == "success" {
				if err != nil || len(target.Backups) != 2 || target.Backups[0].ID != lastID || target.Backups[1].ID != firstID {
					t.Fatalf("complete sorted snapshot: %+v, %v", target, err)
				}
			} else if mode == "legacy" {
				if err != nil || calls != 1 || len(target.Backups) != 1 || target.Backups[0].ID != firstID {
					t.Fatalf("legacy writer compatibility: %+v, %v", target, err)
				}
			} else if err == nil || len(target.Backups) != 1 || target.Backups[0].ID != "previous-snapshot" {
				t.Fatalf("partial catalogue exposed after page failure: %+v, %v", target, err)
			}
		})
	}
}
