.PHONY: dev-cluster dev-cluster-down dev-cluster-check test test-pg test-xfs images image-postgres smoke-postgres image-agent smoke-agent image-operator image-apiserver smoke-binaries load-images deploy-dev undeploy-dev generate envtest test-envtest

CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
SETUP_ENVTEST = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
ENVTEST_K8S_VERSION = 1.37.0

test: ## host tests (PostgreSQL and XFS tests skip)
	go vet ./...
	go test ./...

generate: ## deepcopy code, CRDs and the ClusterRoles of the operator and the API server (controller-gen)
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=consonance-operator paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	$(CONTROLLER_GEN) rbac:roleName=consonance-apiserver paths=./internal/httpapi/... output:rbac:artifacts:config=config/rbac/apiserver

envtest: ## download the envtest API server and etcd into bin/envtest
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest

test-envtest: envtest ## host tests with a Kubernetes API server (envtest) for the API types and the operator
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path)" go test ./...

test-pg: ## all tests in a container on the Podman machine, with PostgreSQL 17 and XFS (envtest packages skip)
	hack/pg-test.sh ./...

test-xfs: ## internal/volume tests on XFS with reflink in a container on the Podman machine
	hack/pg-test.sh -race -count=1 ./internal/volume/... ./internal/testutil/xfstest/...

images: image-postgres image-agent image-operator image-apiserver ## build every image

image-postgres: ## build consonance/postgres:17-dev for the local architecture
	docker build -f images/postgres/Dockerfile -t consonance/postgres:17-dev .

smoke-postgres: image-postgres ## run the postgres image smoke test
	test/image/supervisor-smoke.sh

image-agent: ## build consonance/agent:dev for the local architecture
	docker build -f images/agent/Dockerfile -t consonance/agent:dev .

smoke-agent: image-agent ## run the agent image smoke test
	test/image/agent-smoke.sh

image-operator: ## build consonance/operator:dev (distroless, uid 65532)
	docker build -f images/operator/Dockerfile -t consonance/operator:dev .

image-apiserver: ## build consonance/apiserver:dev (distroless, uid 65532)
	docker build -f images/apiserver/Dockerfile -t consonance/apiserver:dev .

smoke-binaries: image-operator image-apiserver ## run the operator and API server image smoke test
	test/image/binary-smoke.sh

load-images: ## import the four images into the dev cluster's containerd (run make images first)
	hack/load-images.sh

dev-cluster: ## k3s on the Podman machine with StorageClass consonance-xfs; kubeconfig in bin/dev/kubeconfig
	hack/dev-cluster.sh up

dev-cluster-down: ## remove the dev cluster container (volumes stay; hack/dev-cluster.sh down --purge deletes them)
	hack/dev-cluster.sh down

dev-cluster-check: ## prove consonance-xfs volumes are on XFS with reflink
	hack/dev-cluster.sh check

deploy-dev: ## install CRDs, RBAC, the operator and the API server in the dev cluster (admin key in bin/dev/admin-key)
	hack/deploy-dev.sh up

undeploy-dev: ## remove the operator and the API server (CRDs, RBAC, namespace and key stay)
	hack/deploy-dev.sh down
