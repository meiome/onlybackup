GO := ./scripts/go.sh
BINS := onlybackup onlybackup-admin onlybackup-writer onlybackup-receiver onlybackup-recover onlybackup-inspect

.PHONY: build windows-client test race vet vuln check fmt smoke system-test isolation-test mysql-test
build:
	@mkdir -p bin
	@for name in $(BINS); do $(GO) build -buildvcs=false -trimpath -o bin/$$name ./cmd/$$name || exit; done

windows-client:
	@mkdir -p bin/windows-amd64
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o bin/windows-amd64/onlybackup.exe ./cmd/onlybackup
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -buildvcs=false -trimpath -o bin/windows-amd64/onlybackup-recover.exe ./cmd/onlybackup-recover

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

check: test vet vuln

fmt:
	$(GO) fmt ./...

smoke: build
	./scripts/smoke-test.sh

system-test: build
	ONLYBACKUP_BIN="$(CURDIR)/bin" $(GO) test -count=1 -v ./tests/system

isolation-test: build
	$(GO) test -c -o bin/onlybackup-system.test ./tests/system
	./scripts/isolation-test.sh

mysql-test: build
	./scripts/mysql-restore-test.sh
