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
}

type Client struct{ http *http.Client }

func New(socket string, timeout time.Duration) *Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &Client{http: &http.Client{Transport: transport, Timeout: timeout}}
}

func (c *Client) Do(ctx context.Context, method, path string, request, response any) error {
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
	result, err := c.http.Do(r)
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
