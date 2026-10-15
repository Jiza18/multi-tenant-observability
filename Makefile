# Common tasks. Requires Docker; Go is only needed for `make test` and
# `make generate` outside CI.

SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

COMPOSE   := docker compose --project-directory deploy/compose -f deploy/compose/compose.yaml
GATEWAY   := http://127.0.0.1:8080
KIND_NAME := observability

PROMTOOL_IMAGE    := prom/prometheus:v3.13.4
MIMIRTOOL_IMAGE   := grafana/mimirtool:3.2.2
KUBECONFORM_IMAGE := ghcr.io/yannh/kubeconform:v0.8.0
DOCKER_USER       := --user $(shell id -u):$(shell id -g)

##@ Demo

.PHONY: up
up: ## Start the platform (Mimir x3, S3, gateway, Grafana)
	$(COMPOSE) up -d --build --wait

.PHONY: demo
demo: ## Start the platform with simulated tenants and sync their rules
	$(COMPOSE) --profile demo up -d --build --wait
	$(MAKE) rules-sync
	@echo
	@echo "Grafana:  http://localhost:3000  (alerts show up within ~2 minutes)"
	@echo "Gateway:  $(GATEWAY)"

.PHONY: smoke-test
smoke-test: ## Run end-to-end checks against the running demo
	scripts/smoke-test.sh

.PHONY: down
down: ## Stop the demo and delete its data
	$(COMPOSE) --profile demo down -v

##@ Development

.PHONY: generate
generate: ## Render Mimir overrides, Grafana datasources and the rule plan from tenants.yaml
	go run ./cmd/tenantctl render

.PHONY: test
test: test-go test-rules ## Run all unit tests

.PHONY: test-go
test-go: ## Run Go tests with the race detector
	go test -race ./...

.PHONY: test-rules
test-rules: ## Check and unit-test rules (promtool, mimirtool)
	docker run --rm $(DOCKER_USER) -v $(CURDIR)/rules:/rules -w /rules --entrypoint promtool $(PROMTOOL_IMAGE) \
		check rules $$(cd rules && ls *.yaml tenants/*.yaml)
	docker run --rm $(DOCKER_USER) -v $(CURDIR)/rules:/rules -w /rules/tests --entrypoint promtool $(PROMTOOL_IMAGE) \
		test rules $$(cd rules/tests && ls *.test.yaml)
	docker run --rm -v $(CURDIR)/rules:/rules -w /rules $(MIMIRTOOL_IMAGE) \
		rules check $$(cd rules && ls *.yaml tenants/*.yaml)

.PHONY: lint
lint: ## Lint Go code (golangci-lint) and shell scripts (shellcheck)
	golangci-lint run
	docker run --rm -v $(CURDIR):/mnt -w /mnt koalaman/shellcheck:stable scripts/*.sh

.PHONY: validate
validate: ## Validate the Kubernetes overlays against their schemas
	@for overlay in deploy/kubernetes/overlays/*/; do \
		echo "==> $$overlay"; \
		kubectl kustomize "$$overlay" | docker run --rm -i $(KUBECONFORM_IMAGE) -strict -summary \
			-schema-location default \
			-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' -; \
	done

.PHONY: vendor
vendor: ## Refresh vendored upstream dashboards and rules
	scripts/vendor-upstream.sh

##@ Operations

.PHONY: rules-sync
rules-sync: ## Sync rules to Mimir (MIMIR_ADDRESS, MIMIR_AUTH_TOKEN; defaults to the demo)
	MIMIR_ADDRESS=$${MIMIR_ADDRESS:-$(GATEWAY)} MIMIR_AUTH_TOKEN=$${MIMIR_AUTH_TOKEN:-demo-ops} \
		MIMIRTOOL_IMAGE=$(MIMIRTOOL_IMAGE) scripts/sync-rules.sh

.PHONY: rules-diff
rules-diff: ## Show what rules-sync would change
	MIMIR_ADDRESS=$${MIMIR_ADDRESS:-$(GATEWAY)} MIMIR_AUTH_TOKEN=$${MIMIR_AUTH_TOKEN:-demo-ops} \
		MIMIRTOOL_IMAGE=$(MIMIRTOOL_IMAGE) scripts/sync-rules.sh --diff

.PHONY: token
token: ## Issue a token: make token NAME=acme-agents TENANT=acme-prod SCOPES=metrics:write
	go run ./cmd/tenantctl token -name "$(NAME)" -tenant "$(TENANT)" -scopes "$(or $(SCOPES),metrics:write)"

##@ Local Kubernetes (kind)

.PHONY: kind-up
kind-up: ## Create a kind cluster and deploy the local overlay
	kind create cluster --config deploy/kubernetes/overlays/local/kind.yaml
	docker build -t tenant-gateway:dev .
	kind load docker-image tenant-gateway:dev --name $(KIND_NAME)
	kubectl apply --server-side -k deploy/kubernetes/overlays/local
	kubectl -n observability rollout status statefulset/mimir --timeout=5m

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_NAME)

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
