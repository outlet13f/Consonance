.PHONY: test test-vm vm-setup image-postgres smoke-postgres

test: ## host tests (PostgreSQL tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM and install Go + PostgreSQL 17
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...

image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the image smoke test
	test/image/supervisor-smoke.sh
