// Package system_test checks real binaries. Set ONLYBACKUP_BIN to their absolute
// directory. ONLYBACKUP_ISOLATION=1 requires an isolated container running as root.
package system_test

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type running struct {
	cmd  *exec.Cmd
	done chan error
	log  *os.File
}

func (p *running) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		if err != nil {
			t.Errorf("process %s: %v", p.cmd.Path, err)
		}
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Errorf("process did not drain: %s", p.cmd.Path)
	}
	p.log.Close()
	p.cmd = nil
}

func TestProtectedDepositAndRecovery(t *testing.T) {
	bin := os.Getenv("ONLYBACKUP_BIN")
	if bin == "" {
		t.Skip("set ONLYBACKUP_BIN after make build")
	}
	isolated := os.Getenv("ONLYBACKUP_ISOLATION") == "1"
	if isolated && os.Geteuid() != 0 {
		t.Fatal("isolation test needs container root")
	}
	root, err := os.MkdirTemp("", "ob-system-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err = os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "archive")
	writerRun := filepath.Join(root, "writer-run")
	writerSock := filepath.Join(writerRun, "writer.sock")
	var writerID, receiverID *syscall.Credential
	if isolated {
		writerID = &syscall.Credential{Uid: 12001, Gid: 12004}
		receiverID = &syscall.Credential{Uid: 12005, Gid: 12004}
	}
	dir := func(path string, mode os.FileMode, owner *syscall.Credential) {
		t.Helper()
		if err := os.Mkdir(path, mode); err != nil {
			t.Fatal(err)
		}
		if owner != nil {
			if err := os.Chown(path, int(owner.Uid), int(owner.Gid)); err != nil {
				t.Fatal(err)
			}
		}
	}
	dir(state, 0700, writerID)
	dir(writerRun, 0750, writerID)
	command := func(owner *syscall.Credential, name string, args ...string) *exec.Cmd {
		c := exec.Command(filepath.Join(bin, name), args...)
		if owner != nil {
			c.SysProcAttr = &syscall.SysProcAttr{Credential: owner}
		}
		return c
	}
	run := func(owner *syscall.Credential, name string, args ...string) []byte {
		t.Helper()
		out, err := command(owner, name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return out
	}
	run(writerID, "onlybackup-admin", "--state", state, "init")
	sendKey := filepath.Join(state, "send.key")
	keyOut := run(writerID, "onlybackup-admin", "--state", state, "keys", "create", "--name", "system", "--profile", "XS", "--out", sendKey)
	var keyInfo struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(keyOut, &keyInfo); err != nil {
		t.Fatal(err)
	}
	cert, key := createCertificate(t, root)
	if receiverID != nil {
		if err := os.Chown(key, int(receiverID.Uid), int(receiverID.Gid)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(cert, 0644); err != nil {
			t.Fatal(err)
		}
	}
	start := func(owner *syscall.Credential, name string, args ...string) *running {
		t.Helper()
		log, err := os.CreateTemp(root, name+"-*.log")
		if err != nil {
			t.Fatal(err)
		}
		c := command(owner, name, args...)
		c.Stdout, c.Stderr = log, log
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		p := &running{cmd: c, done: make(chan error, 1), log: log}
		go func() { p.done <- c.Wait() }()
		t.Cleanup(func() {
			p.stop(t)
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Logf("%s\n%s", name, data)
			}
		})
		return p
	}
	waitSocket := func(path string) {
		t.Helper()
		for i := 0; i < 200; i++ {
			if info, e := os.Stat(path); e == nil && info.Mode()&os.ModeSocket != 0 {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("socket did not start: %s", path)
	}
	writer := start(writerID, "onlybackup-writer", "--state", state, "--socket", writerSock, "--reserve-free", "0")
	waitSocket(writerSock)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	receiver := start(receiverID, "onlybackup-receiver", "--listen", addr, "--socket", writerSock, "--tls-cert", cert, "--tls-key", key)
	pool := x509.NewCertPool()
	certData, _ := os.ReadFile(cert)
	pool.AppendCertsFromPEM(certData)
	hc := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}}}
	defer hc.CloseIdleConnections()
	url := "https://" + addr
	ready := false
	for i := 0; i < 200; i++ {
		resp, e := hc.Get(url + "/v1/backups")
		if e == nil {
			resp.Body.Close()
			if resp.StatusCode == 405 {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("HTTPS receiver did not start")
	}
	input := filepath.Join(root, "database.sql")
	data := bytes.Repeat([]byte("sensitive database content 0123456789\n"), 12000)
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(root, "recovery.key")
	var recipient struct {
		Recipient string `json:"recipient"`
	}
	if err := json.Unmarshal(run(nil, "onlybackup-recover", "keygen", "--out", identity), &recipient); err != nil || recipient.Recipient == "" {
		t.Fatalf("keygen: %v", err)
	}
	ids := make(map[string]string)
	for _, format := range []string{"plain", "age-v1"} {
		args := []string{"send", "--quiet", "--url", url, "--key-file", sendKey, "--ca-file", cert, "--description", "system " + format}
		if format == "age-v1" {
			args = append(args, "--encrypt-to", recipient.Recipient)
		} else {
			args = append(args, "--plaintext")
		}
		args = append(args, input)
		var receipt struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(run(nil, "onlybackup", args...), &receipt); err != nil || receipt.Status != "complete" {
			t.Fatalf("receipt: %v", err)
		}
		ids[format] = receipt.ID
		stored, err := os.ReadFile(filepath.Join(state, "backups", receipt.ID+".backup"))
		if err != nil {
			t.Fatal(err)
		}
		if format == "plain" && !bytes.Equal(stored, data) {
			t.Fatal("explicit plaintext upload changed")
		}
		if format == "age-v1" && (bytes.Equal(stored, data) || bytes.Contains(stored, []byte("sensitive database content"))) {
			t.Fatal("encrypted backup contains plaintext")
		}
		output := filepath.Join(root, format+".recovered")
		recoverArgs := []string{"--state", state, "--id", receipt.ID, "--out", output}
		if format == "age-v1" {
			recoverArgs = append(recoverArgs, "--identity-file", identity)
		}
		run(nil, "onlybackup-recover", recoverArgs...)
		got, _ := os.ReadFile(output)
		if !bytes.Equal(got, data) {
			t.Fatal("recovered bytes differ")
		}
		if err := command(nil, "onlybackup-recover", recoverArgs...).Run(); err == nil {
			t.Fatal("recovery overwrote existing output")
		}
	}
	for _, method := range []string{"GET", "DELETE", "PUT", "PATCH"} {
		req, _ := http.NewRequest(method, url+"/v1/backups", nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Fatalf("method %s accepted: %d", method, resp.StatusCode)
		}
	}
	if isolated {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		probe := exec.Command(exe, "-test.run=^TestArchiveDenied$", "-test.v")
		probe.SysProcAttr = &syscall.SysProcAttr{Credential: receiverID}
		probe.Env = append(os.Environ(), "ONLYBACKUP_PROBE="+state, "ONLYBACKUP_PROBE_ID="+ids["plain"])
		out, err := probe.CombinedOutput()
		if err != nil {
			t.Fatalf("receiver isolation: %v\n%s", err, out)
		}
		t.Log("receiver cannot read, chmod, replace or delete archive/catalogue")
	}
	run(writerID, "onlybackup-admin", "--state", state, "keys", "revoke", "--id", keyInfo.ID)
	if err := command(nil, "onlybackup", "send", "--quiet", "--url", url, "--key-file", sendKey, "--ca-file", cert, "--description", "revoked", input).Run(); err == nil {
		t.Fatal("revoked key accepted")
	}
	receiver.stop(t)
	writer.stop(t)
	// Take a cold copy, including any remaining WAL, and recover only from it.
	copyState := filepath.Join(root, "restored-archive")
	if err := copyTree(state, copyState); err != nil {
		t.Fatal(err)
	}
	for format, id := range ids {
		out := filepath.Join(root, "cold-"+format)
		args := []string{"--state", copyState, "--id", id, "--out", out}
		if format == "age-v1" {
			args = append(args, "--identity-file", identity)
		}
		run(nil, "onlybackup-recover", args...)
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, data) {
			t.Fatal("cold restore differs")
		}
	}
	// A fresh writer must reconcile the original archive without losing old data.
	writer2 := start(writerID, "onlybackup-writer", "--state", state, "--socket", writerSock, "--reserve-free", "0")
	waitSocket(writerSock)
	writer2.stop(t)
	for _, id := range ids {
		run(nil, "onlybackup-recover", "--state", state, "--id", id)
	}
	t.Log("clear/encrypted deposits, revocation, restart and cold catalogue recovery passed")
}

func TestArchiveDenied(t *testing.T) {
	state := os.Getenv("ONLYBACKUP_PROBE")
	if state == "" {
		t.Skip("only executed as unprivileged subprocess")
	}
	file := filepath.Join(state, "backups", os.Getenv("ONLYBACKUP_PROBE_ID")+".backup")
	for _, path := range []string{state, filepath.Join(state, "backups"), file, filepath.Join(state, "metadata.db")} {
		if err := os.Chmod(path, 0777); err == nil {
			t.Fatalf("chmod permitted: %s", path)
		}
	}
	for _, path := range []string{file, filepath.Join(state, "metadata.db"), filepath.Join(state, "metadata.db-wal")} {
		if f, err := os.Open(path); err == nil {
			f.Close()
			t.Fatalf("read permitted: %s", path)
		}
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600); err == nil {
			f.Close()
			t.Fatalf("write permitted: %s", path)
		}
		if err := os.Remove(path); err == nil {
			t.Fatalf("unlink permitted: %s", path)
		}
		if err := os.Rename(path, path+".moved"); err == nil {
			t.Fatalf("rename permitted: %s", path)
		}
	}
	source, err := os.CreateTemp("", "ob-probe-")
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	defer os.Remove(source.Name())
	if err := os.Rename(source.Name(), file); err == nil {
		t.Fatal("replacement permitted")
	}
}

func createCertificate(t *testing.T, root string) (string, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	cert, key := filepath.Join(root, "tls.crt"), filepath.Join(root, "tls.key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.Mkdir(target, info.Mode().Perm())
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected archive entry %s", path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if err == nil {
			err = out.Sync()
		}
		ce := out.Close()
		if err == nil {
			err = ce
		}
		return err
	})
}
