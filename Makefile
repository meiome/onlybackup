GO := ./scripts/go.sh
BINS := onlybackup onlybackup-admin onlybackup-writer onlybackup-receiver onlybackup-vault onlybackup-recover onlybackup-inspect

.PHONY: build test race vet check fmt smoke system-test isolation-test mysql-test
build:
	@mkdir -p bin
	@for name in $(BINS); do $(GO) build -buildvcs=false -trimpath -o bin/$$name ./cmd/$$name || exit; done

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

check: test vet

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
