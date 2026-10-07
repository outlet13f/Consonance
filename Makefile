.PHONY: test test-vm vm-setup

test: ## host tests (PostgreSQL tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM and install Go + PostgreSQL 17
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...
