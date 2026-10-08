.PHONY: test test-vm test-xfs vm-setup images image-postgres smoke-postgres image-agent smoke-agent

test: ## host tests (PostgreSQL and XFS tests skip)
	go vet ./...
	go test ./...

vm-setup: ## one-time: start the Lima VM, install Go and PostgreSQL 17, mount the XFS test volume
	hack/vm-setup.sh

test-vm: ## all tests inside the Lima VM
	hack/vm-test.sh ./...

test-xfs: ## internal/volume tests on XFS with reflink inside the Lima VM
	hack/vm-test.sh -race ./internal/volume/... ./internal/testutil/xfstest/...

images: image-postgres image-agent ## build every image

image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the postgres image smoke test
	test/image/supervisor-smoke.sh

image-agent: ## build consonance/agent:dev for the local architecture
	docker build -f images/agent/Dockerfile -t consonance/agent:dev .

smoke-agent: image-agent ## run the agent image smoke test
	test/image/agent-smoke.sh
