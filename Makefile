# scootless

BINARY := bin/scootlessd
UNIT   := scootlessd.service
UNITDIR := $(HOME)/.config/systemd/user

.PHONY: all build test vet fmt run once clean install restart status logs

all: build

build:
	go build -o $(BINARY) ./cmd/scootlessd

test:
	go test ./... -count=1

# The live API and feed behaviour, exercised for real. Kept out of `test` so
# the default suite stays offline and deterministic.
test-live:
	SCOOTLESS_LIVE=1 go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	./$(BINARY)

once: build
	./$(BINARY) --once

# Rebuild before restarting. Doing these separately is how a stale binary ends
# up running under a fresh unit, looking for all the world like a config that
# did not take effect.
install: build
	install -d $(UNITDIR)
	install -m 0644 deploy/$(UNIT) $(UNITDIR)/$(UNIT)
	systemctl --user daemon-reload
	systemctl --user restart $(UNIT)
	systemctl --user is-active $(UNIT)

restart: build
	systemctl --user restart $(UNIT)
	systemctl --user is-active $(UNIT)

status:
	systemctl --user status $(UNIT) --no-pager

logs:
	journalctl --user -u $(UNIT) -f

clean:
	rm -rf bin
