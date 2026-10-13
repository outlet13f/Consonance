.PHONY: test test-vm test-xfs vm-setup images image-postgres smoke-postgres image-agent smoke-agent generate envtest test-envtest

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
ENVTEST_K8S_VERSION = 1.37.0

test: ## host tests (PostgreSQL and XFS tests skip)
	go vet ./...
	go test ./...

generate: ## deepcopy code and CRD manifests from api/ (controller-gen)
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases

envtest: ## download the envtest API server and etcd into bin/envtest
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest

test-envtest: envtest ## host tests with a Kubernetes API server (envtest) for the API types and the operator
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path)" go test ./...

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
