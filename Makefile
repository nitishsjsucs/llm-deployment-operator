# Image to build and deploy.
IMG ?= controller:latest

CHART_DIR ?= charts/llm-deployment-operator

# Tool versions. All tools are installed with `go install` into ./bin.
CONTROLLER_TOOLS_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION ?= v2.5.0
KUSTOMIZE_VERSION ?= v5.7.1

LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
KUSTOMIZE ?= $(LOCALBIN)/kustomize
KUBECTL ?= kubectl

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRDs and RBAC, and copy them into the Helm chart.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/
	cp config/rbac/role.yaml $(CHART_DIR)/files/role.yaml

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet ## Run the unit and fake-client tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: verify-generated
verify-generated: manifests generate ## Fail if generated files are out of date.
	git diff --exit-code -- api config $(CHART_DIR)

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager and packsim binaries.
	go build -o bin/manager ./cmd/manager
	go build -o bin/packsim ./cmd/packsim

.PHONY: run
run: manifests generate fmt vet ## Run the manager against the current kubeconfig.
	go run ./cmd/manager

.PHONY: packsim
packsim: ## Print the static MIG packing comparison for the sample fleet.
	go run ./cmd/packsim

.PHONY: docker-build
docker-build: ## Build the manager image.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the manager image.
	docker push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install the CRDs into the current cluster.
	$(KUBECTL) apply --server-side -k config/crd

.PHONY: uninstall
uninstall: ## Remove the CRDs from the current cluster.
	$(KUBECTL) delete --ignore-not-found -k config/crd

.PHONY: deploy
deploy: manifests kustomize ## Deploy the operator with kustomize, using IMG.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply --server-side -f -

.PHONY: undeploy
undeploy: kustomize ## Remove the operator.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found -f -

##@ Tools

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Install controller-gen.
	test -x $(CONTROLLER_GEN) && $(CONTROLLER_GEN) --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN) ## Install golangci-lint.
	test -x $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: kustomize
kustomize: $(LOCALBIN) ## Install kustomize.
	test -x $(KUSTOMIZE) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)
