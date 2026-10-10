// Package localclient is the client-only side of the fixed writer protocol.
// It deliberately has no dependency on SQLite or the writer server package.
package localclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/meiome/onlybackup/internal/model"
)

const protocolVersion = "1"
const protocolHeader = "X-OnlyBackup-Protocol"
const maxLocalResponseBytes = 64 << 20

type Snapshot struct {
	Status                       model.AutomationStatus     `json:"status"`
	Settings                     model.MailSettings         `json:"mail_settings"`
	Keys                         []model.Key                `json:"keys"`
	Quotas                       []model.Quota              `json:"quotas"`
	Backups                      []model.Backup             `json:"backups"`
	Operations                   []model.RetentionOperation `json:"operations"`
	Anomalies                    []model.Anomaly            `json:"anomalies"`
	Exclusions                   []model.AnomalyExclusion   `json:"anomaly_exclusions"`
	AcknowledgedMissingBackupIDs []string                   `json:"acknowledged_missing_backup_ids"`
	Models                       map[string]json.RawMessage `json:"models"`
	NextBackupID                 string                     `json:"next_backup_id,omitempty"`
	BackupsThrough               int64                      `json:"backups_through,omitempty"`
}

type Client struct{ http *http.Client }

func New(socket string, timeout time.Duration) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: timeout}}
}

func (c *Client) Do(ctx context.Context, method, path string, request, response any) error {
	if snapshot, ok := response.(*Snapshot); ok && snapshot != nil && method == "GET" && path == "/v1/snapshot" {
		return c.readSnapshot(ctx, snapshot)
	}
	return c.do(ctx, method, path, request, response)
}

// Build a complete snapshot only after every bounded page succeeds. A timeout,
// cancellation or invalid cursor must never expose a partial catalogue to
// monitoring or retention decisions.
func (c *Client) readSnapshot(ctx context.Context, target *Snapshot) error {
	var snapshot Snapshot
	if err := c.do(ctx, "GET", "/v1/snapshot?paged=1", nil, &snapshot); err != nil {
		return err
	}
	for snapshot.NextBackupID != "" {
		after := snapshot.NextBackupID
		if !model.ValidID(after) || snapshot.BackupsThrough <= 0 {
			return errors.New("cursore catalogo del writer non valido")
		}
		var page model.BackupPage
		path := fmt.Sprintf("/v1/snapshot?paged=1&after=%s&through=%d", after, snapshot.BackupsThrough)
		if err := c.do(ctx, "GET", path, nil, &page); err != nil {
			return err
		}
		if page.Through != snapshot.BackupsThrough || (page.Next != "" && (!model.ValidID(page.Next) || page.Next <= after)) {
			return errors.New("pagina catalogo del writer non valida")
		}
		snapshot.Backups = append(snapshot.Backups, page.Backups...)
		snapshot.NextBackupID = page.Next
	}
	// Preserve the ordering exposed by the original unpaged snapshot.
	sort.Slice(snapshot.Backups, func(i, j int) bool {
		if snapshot.Backups[i].StartedAt == snapshot.Backups[j].StartedAt {
			return snapshot.Backups[i].ID > snapshot.Backups[j].ID
		}
		return snapshot.Backups[i].StartedAt > snapshot.Backups[j].StartedAt
	})
	*target = snapshot
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, request, response any) error {
	var body io.Reader
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://onlybackup"+path, body)
	if err != nil {
		return err
	}
	r.Header.Set(protocolHeader, protocolVersion)
	if request != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	client := c.http
	if path == "/v1/reconcile" || path == "/v1/confirm" {
		// These authenticated local commands hash whole archives. Their caller's
		// context governs cancellation; short control calls retain their timeout.
		longClient := *client
		longClient.Timeout = 0
		client = &longClient
	}
	result, err := client.Do(r)
	if err != nil {
		return err
	}
	defer result.Body.Close()
	if result.Header.Get(protocolHeader) != protocolVersion {
		return errors.New("writer con protocollo locale incompatibile")
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, maxLocalResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxLocalResponseBytes {
		return errors.New("risposta del writer locale troppo grande")
	}
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		var apiError struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &apiError) == nil && apiError.Error != "" {
			return errors.New(apiError.Error)
		}
		return fmt.Errorf("writer locale: HTTP %d", result.StatusCode)
	}
	if response != nil && len(data) != 0 {
		return json.Unmarshal(data, response)
	}
	return nil
}
