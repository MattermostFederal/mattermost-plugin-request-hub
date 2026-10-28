GO ?= $(shell command -v go 2> /dev/null)
NPM ?= $(shell command -v npm 2> /dev/null)
CURL ?= $(shell command -v curl 2> /dev/null)
MM_DEBUG ?=
GOPATH ?= $(shell go env GOPATH)
GO_TEST_FLAGS ?= -race
GO_BUILD_FLAGS ?=
DEFAULT_GOOS := $(shell go env GOOS)
DEFAULT_GOARCH := $(shell go env GOARCH)

export GO111MODULE=on

# We need to export GOBIN to allow it to be set
# for processes spawned from the Makefile
export GOBIN ?= $(PWD)/bin

# You can include assets this directory into the bundle. This can be e.g. used to include profile pictures.
ASSETS_DIR ?= assets

# Verify environment, and define PLUGIN_ID, PLUGIN_VERSION, HAS_SERVER and HAS_WEBAPP as needed.
include build/setup.mk

BUNDLE_NAME ?= $(PLUGIN_ID)-$(PLUGIN_VERSION).tar.gz

# ====================================================================================
# Build-time URL discovery
# ====================================================================================
# PLUGIN_REPO_URL resolves to "where this code lives in GitHub right now".
# Used to substitute __PLUGIN_REPO_URL__ / __PLUGIN_RELEASE_URL__ /
# __PLUGIN_VERSION__ placeholders in plugin.json's homepage_url,
# support_url, and settings_schema.header. Result: fork or repo rename
# flows into MM's System Console rendering without a source edit.
#
# Cascade (most authoritative first):
#   1. CI: $(GITHUB_SERVER_URL)/$(GITHUB_REPOSITORY) — set by GitHub Actions.
#   2. Local: parse `git remote get-url origin`, normalize SSH->HTTPS,
#      strip trailing `.git`. Correct for normal dev clones AND forks.
#   3. Hardcoded fallback PLUGIN_REPO_URL_DEFAULT — catches the
#      "source tarball, no git, no CI" build case. Update this string
#      if the upstream repo is permanently moved.
# ====================================================================================

PLUGIN_REPO_URL_DEFAULT := https://github.com/MattermostFederal/mattermost-plugin-request-hub

ifneq ($(GITHUB_REPOSITORY),)
PLUGIN_REPO_URL := $(GITHUB_SERVER_URL)/$(GITHUB_REPOSITORY)
else
PLUGIN_REPO_URL := $(shell git remote get-url origin 2>/dev/null | sed -e 's|^git@github.com:|https://github.com/|' -e 's|\.git$$||')
endif

ifeq ($(strip $(PLUGIN_REPO_URL)),)
PLUGIN_REPO_URL := $(PLUGIN_REPO_URL_DEFAULT)
endif

PLUGIN_RELEASE_URL := $(PLUGIN_REPO_URL)/releases/tag/v$(PLUGIN_VERSION)

# Include custom makefile, if present
ifneq ($(wildcard build/custom.mk),)
	include build/custom.mk
endif

ifneq ($(MM_DEBUG),)
	GO_BUILD_GCFLAGS = -gcflags "all=-N -l"
else
	GO_BUILD_GCFLAGS =
endif

# ====================================================================================
# Default Target
# ====================================================================================

.PHONY: default
default: all

# ====================================================================================
# Build Targets
# ====================================================================================

## Checks the code style, tests, builds and bundles the plugin.
.PHONY: all
all: check-style test dist

## Pre-release checks: git status and changelog validation.
.PHONY: release-check
release-check:
	@echo "Running pre-release checks..."
	@if [ -n "$$(git status --porcelain -- . ':!webapp/package-lock.json')" ]; then \
		echo "ERROR: Working directory has uncommitted changes."; \
		echo "Please commit or stash changes before building a release."; \
		git status --short -- . ':!webapp/package-lock.json'; \
		exit 1; \
	fi
	@if [ ! -f CHANGELOG.md ]; then \
		echo "ERROR: CHANGELOG.md not found."; \
		exit 1; \
	fi
	@if ! grep -q "## \[Unreleased\]" CHANGELOG.md && ! grep -q "## \[$(PLUGIN_VERSION)\]" CHANGELOG.md; then \
		echo "WARNING: CHANGELOG.md may not be updated for version $(PLUGIN_VERSION)."; \
	fi
	@echo "Pre-release checks passed."

## Generate SHA256 checksum for the release bundle.
.PHONY: release-checksum
release-checksum:
	@echo "Generating SHA256 checksum..."
	@cd dist && shasum -a 256 $(BUNDLE_NAME) > $(BUNDLE_NAME).sha256
	@echo "Checksum: $$(cat dist/$(BUNDLE_NAME).sha256)"

## Include SBOMs and CodeQL results in the release bundle and repackage.
.PHONY: release-bundle
release-bundle:
	@echo "Including SBOMs and security reports in release bundle..."
	@if [ -d dist/sbom ]; then \
		cp -r dist/sbom dist/$(PLUGIN_ID)/; \
		echo "SBOMs included in bundle"; \
	else \
		echo "WARNING: No SBOMs found to include"; \
	fi
	@mkdir -p dist/$(PLUGIN_ID)/security
	@if [ -f dist/codeql-go.sarif ]; then \
		cp dist/codeql-go.sarif dist/$(PLUGIN_ID)/security/; \
		echo "Go CodeQL results included"; \
	fi
	@if [ -f dist/codeql-js.sarif ]; then \
		cp dist/codeql-js.sarif dist/$(PLUGIN_ID)/security/; \
		echo "JavaScript CodeQL results included"; \
	fi
	@rm -f dist/$(BUNDLE_NAME)
	@if [ "$$(uname)" = "Darwin" ]; then \
		cd dist && tar --disable-copyfile -cvzf $(BUNDLE_NAME) $(PLUGIN_ID); \
	else \
		cd dist && tar -cvzf $(BUNDLE_NAME) $(PLUGIN_ID); \
	fi

## Sign the plugin bundle with GPG (requires PLUGIN_SIGNING_KEY env var).
.PHONY: release-sign
release-sign:
	@if [ -n "$(PLUGIN_SIGNING_KEY)" ]; then \
		echo "Signing plugin bundle with GPG key $(PLUGIN_SIGNING_KEY)..."; \
		gpg -u $(PLUGIN_SIGNING_KEY) --verbose --personal-digest-preferences SHA256 --detach-sign dist/$(BUNDLE_NAME); \
		echo "Signature: dist/$(BUNDLE_NAME).sig"; \
	else \
		echo "PLUGIN_SIGNING_KEY not set, skipping signing."; \
		echo "To sign, set PLUGIN_SIGNING_KEY to your GPG key ID."; \
	fi

## Create a git tag for the release version.
.PHONY: release-tag
release-tag:
	@echo "Creating git tag v$(PLUGIN_VERSION)..."
	@if git rev-parse "v$(PLUGIN_VERSION)" >/dev/null 2>&1; then \
		echo "ERROR: Tag v$(PLUGIN_VERSION) already exists."; \
		exit 1; \
	fi
	git tag -a "v$(PLUGIN_VERSION)" -m "Release v$(PLUGIN_VERSION)"
	@echo "Tag v$(PLUGIN_VERSION) created. Push with: git push origin v$(PLUGIN_VERSION)"

## Full release build: clean, checks, style, tests, build, SBOM audit, govulncheck, CodeQL, bundle with SBOMs, sign, and checksum.
.PHONY: release
release: release-check clean all sbom-audit govulncheck codeql-analyze security-gate release-bundle virus-scan release-sign release-checksum
	@echo ""
	@echo "=========================================="
	@echo "Release build complete!"
	@echo "Bundle:   dist/$(BUNDLE_NAME)"
	@echo "Checksum: dist/$(BUNDLE_NAME).sha256"
	@if [ -f dist/$(BUNDLE_NAME).sig ]; then echo "Signature: dist/$(BUNDLE_NAME).sig"; fi
	@echo "SBOMs included in bundle"
	@echo ""
	@echo "To tag this release: make release-tag"
	@echo "=========================================="

## Ensures the plugin manifest is valid
.PHONY: manifest-check
manifest-check:
	./build/bin/manifest check

## Propagates plugin manifest information into the server/ and webapp/ folders.
.PHONY: apply
apply:
	./build/bin/manifest apply

## Builds the server, if it exists, for all supported architectures, unless MM_SERVICESETTINGS_ENABLEDEVELOPER is set.
.PHONY: server
server:
ifneq ($(HAS_SERVER),)
ifneq ($(MM_DEBUG),)
	$(info DEBUG mode is on; to disable, unset MM_DEBUG)
endif
	mkdir -p server/dist;
ifneq ($(MM_SERVICESETTINGS_ENABLEDEVELOPER),)
	@echo Building plugin only for $(DEFAULT_GOOS)-$(DEFAULT_GOARCH) because MM_SERVICESETTINGS_ENABLEDEVELOPER is enabled
	cd server && env CGO_ENABLED=0 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_GCFLAGS) -trimpath -o dist/plugin-$(DEFAULT_GOOS)-$(DEFAULT_GOARCH);
else
	cd server && env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_GCFLAGS) -trimpath -o dist/plugin-linux-amd64;
	cd server && env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_GCFLAGS) -trimpath -o dist/plugin-linux-arm64;
endif
endif

## Ensures NPM dependencies are installed without having to run this all the time.
webapp/node_modules: $(wildcard webapp/package.json)
ifneq ($(HAS_WEBAPP),)
	cd webapp && $(NPM) install
	touch $@
endif

## Builds the webapp, if it exists.
.PHONY: webapp
webapp: webapp/node_modules
ifneq ($(HAS_WEBAPP),)
ifeq ($(MM_DEBUG),)
	cd webapp && $(NPM) run build;
else
	cd webapp && $(NPM) run debug;
endif
endif

## Generates a tar bundle of the plugin for install.
.PHONY: bundle
bundle:
	rm -rf dist/
	mkdir -p dist/$(PLUGIN_ID)
	./build/bin/manifest dist
	# Post-process the plugin.json that manifest just staged into
	# dist/$(PLUGIN_ID)/plugin.json to substitute the URL/version
	# placeholders. sed delimiter `|` because URLs contain slashes.
	@if [ -f dist/$(PLUGIN_ID)/plugin.json ]; then \
		sed -i.bak \
			-e 's|__PLUGIN_RELEASE_URL__|$(PLUGIN_RELEASE_URL)|g' \
			-e 's|__PLUGIN_REPO_URL__|$(PLUGIN_REPO_URL)|g' \
			-e 's|__PLUGIN_VERSION__|$(PLUGIN_VERSION)|g' \
			dist/$(PLUGIN_ID)/plugin.json && \
		rm -f dist/$(PLUGIN_ID)/plugin.json.bak; \
	fi
ifneq ($(wildcard $(ASSETS_DIR)/.),)
	cp -r $(ASSETS_DIR) dist/$(PLUGIN_ID)/
endif
ifneq ($(HAS_PUBLIC),)
	cp -r public dist/$(PLUGIN_ID)/
	# Bundle doc screenshots for /mattermost-request-hub docs (public/help/docs.html).
	mkdir -p dist/$(PLUGIN_ID)/public/help/images
	if [ -d docs/images ]; then cp -R docs/images/. dist/$(PLUGIN_ID)/public/help/images/; fi
endif
ifneq ($(HAS_SERVER),)
	mkdir -p dist/$(PLUGIN_ID)/server
	cp -r server/dist dist/$(PLUGIN_ID)/server/
endif
ifneq ($(HAS_WEBAPP),)
	mkdir -p dist/$(PLUGIN_ID)/webapp
	cp -r webapp/dist dist/$(PLUGIN_ID)/webapp/
endif
ifeq ($(shell uname),Darwin)
	cd dist && tar --disable-copyfile -cvzf $(BUNDLE_NAME) $(PLUGIN_ID)
else
	cd dist && tar -cvzf $(BUNDLE_NAME) $(PLUGIN_ID)
endif

	@echo plugin built at: dist/$(BUNDLE_NAME)

## Builds and bundles the plugin.
.PHONY: dist
dist: apply server webapp bundle

# ====================================================================================
# Quality Targets
# ====================================================================================

## golangci-lint release binary, checked against its published SHA-256.
GOLANGCI_LINT_VERSION := 2.14.0
GOLANGCI_LINT_SHA256_linux_amd64 := ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab
GOLANGCI_LINT_SHA256_linux_arm64 := ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae
GOLANGCI_LINT_SHA256_darwin_amd64 := a5667c1c3536be1740133213e1e822bfb8f0d98ea12903174d6d5f635e4ed68d
GOLANGCI_LINT_SHA256_darwin_arm64 := 5ef5f36a7147e91dc58ef9ef4d11bb7bad5ead0c76eb6c01327a73c641d1dcc3

## Install go tools
install-go-tools:
	@echo Installing go tools
	@set -e; \
	if "$(GOBIN)/golangci-lint" version 2>/dev/null | grep -q "version $(GOLANGCI_LINT_VERSION) "; then \
		echo "golangci-lint $(GOLANGCI_LINT_VERSION) already installed"; \
	else \
		os=$$($(GO) env GOHOSTOS); arch=$$($(GO) env GOHOSTARCH); \
		case "$$os-$$arch" in \
			linux-amd64) sum=$(GOLANGCI_LINT_SHA256_linux_amd64) ;; \
			linux-arm64) sum=$(GOLANGCI_LINT_SHA256_linux_arm64) ;; \
			darwin-amd64) sum=$(GOLANGCI_LINT_SHA256_darwin_amd64) ;; \
			darwin-arm64) sum=$(GOLANGCI_LINT_SHA256_darwin_arm64) ;; \
			*) echo "no golangci-lint checksum for $$os-$$arch" >&2; exit 1 ;; \
		esac; \
		name=golangci-lint-$(GOLANGCI_LINT_VERSION)-$$os-$$arch; \
		tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		curl -sSfL -o "$$tmp/$$name.tar.gz" https://github.com/golangci/golangci-lint/releases/download/v$(GOLANGCI_LINT_VERSION)/$$name.tar.gz; \
		if command -v sha256sum >/dev/null; then \
			echo "$$sum  $$tmp/$$name.tar.gz" | sha256sum -c -; \
		else \
			echo "$$sum  $$tmp/$$name.tar.gz" | shasum -a 256 -c -; \
		fi; \
		tar -xzf "$$tmp/$$name.tar.gz" -C "$$tmp"; \
		mkdir -p "$(GOBIN)"; \
		install -m 0755 "$$tmp/$$name/golangci-lint" "$(GOBIN)/golangci-lint"; \
	fi
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GO) install gotest.tools/gotestsum@v1.13.0

## Runs eslint and golangci-lint
.PHONY: check-style
check-style: manifest-check apply webapp/node_modules install-go-tools
	@echo Checking for style guide compliance

ifneq ($(HAS_WEBAPP),)
	cd webapp && npm run lint
	cd webapp && npm run check-types
endif

# It's highly recommended to run go-vet first
# to find potential compile errors that could introduce
# weird reports at golangci-lint step
ifneq ($(HAS_SERVER),)
	@echo Running golangci-lint
	$(GO) vet ./server/...
	$(GOBIN)/golangci-lint run ./server/...
endif

## Runs any lints and unit tests defined for the server and webapp, if they exist.
.PHONY: test
test: apply webapp/node_modules install-go-tools
ifneq ($(HAS_SERVER),)
	$(GOBIN)/gotestsum -- -v ./...
endif
ifneq ($(HAS_WEBAPP),)
	cd webapp && $(NPM) run test;
endif

## Runs any lints and unit tests defined for the server and webapp, if they exist, optimized for a CI environment.
.PHONY: test-ci
test-ci: apply webapp/node_modules install-go-tools
ifneq ($(HAS_SERVER),)
	$(GOBIN)/gotestsum --format standard-verbose --junitfile report.xml -- ./...
endif
ifneq ($(HAS_WEBAPP),)
	cd webapp && $(NPM) run test;
endif

## Builds the bundle and runs the Playwright e2e suite against Mattermost in Docker (needs Node from e2e/.nvmrc).
.PHONY: e2e
e2e: dist
	cd e2e && $(NPM) ci && npx playwright install chromium && $(NPM) test

## Prints Go code coverage summary to terminal.
.PHONY: coverage-backend
coverage-backend: apply
ifneq ($(HAS_SERVER),)
	$(GO) test $(GO_TEST_FLAGS) -coverprofile=server/coverage.txt ./server/...
	$(GO) tool cover -func=server/coverage.txt
endif

## Prints frontend code coverage summary to terminal.
.PHONY: coverage-frontend
coverage-frontend: apply webapp/node_modules
ifneq ($(HAS_WEBAPP),)
	cd webapp && $(NPM) run test:coverage
	cd webapp && $(NPM) run test:pw-ct-coverage
	@echo ""
	@echo "=== Merged Coverage (unit + component) ==="
	cd webapp && $(NPM) run test:coverage-merged
endif

## Prints code coverage summary for both backend and frontend.
.PHONY: coverage
coverage: coverage-backend coverage-frontend

## Clean removes all build artifacts (but preserves build tools).
.PHONY: clean
clean:
	rm -fr dist/
ifneq ($(HAS_SERVER),)
	rm -fr server/coverage.txt
	rm -fr server/dist
endif
ifneq ($(HAS_WEBAPP),)
	rm -fr webapp/junit.xml
	rm -fr webapp/dist
	rm -fr webapp/node_modules
	rm -fr webapp/coverage
	rm -fr webapp/coverage-ct
	rm -fr webapp/.v8-ct-coverage
	rm -fr webapp/coverage-merged webapp/.v8-unit-coverage webapp/.v8-merged-coverage
endif

## Nuke everything: Docker containers, data, and all build artifacts
.PHONY: nuke
nuke: docker-kill-orphans
	@echo "Nuking everything..."
	@$(DOCKER_COMPOSE) down -v 2>/dev/null || true
	@rm -rf docker/postgres-data docker/mattermost
	@rm -fr dist/
	@rm -fr server/coverage.txt server/dist
	@rm -fr webapp/junit.xml webapp/dist webapp/node_modules
	@rm -fr webapp/coverage webapp/coverage-ct webapp/.v8-ct-coverage
	@rm -fr webapp/coverage-merged webapp/.v8-unit-coverage webapp/.v8-merged-coverage
	@rm -fr build/bin/
	@echo "Everything removed. Run 'make docker-setup' to start fresh."

## Generate mocks
.PHONY: mock
mock:
ifneq ($(HAS_SERVER),)
	go install go.uber.org/mock/mockgen@v0.6.0
	@echo "No mocks configured for this plugin. Add your mockgen commands here."
endif

# ====================================================================================
# Docker Development Environment
# ====================================================================================
DOCKER_COMPOSE := docker compose -f docker-compose.dev.yml
MM_PORT ?= 8065

## Start Mattermost and PostgreSQL containers
.PHONY: docker-start
docker-start:
	@echo "Starting Mattermost Enterprise Edition..."
	@mkdir -p docker/mattermost/{config,data,logs,plugins,client-plugins}
	@mkdir -p docker/postgres-data
	@$(DOCKER_COMPOSE) up -d

## Stop containers (preserves data)
.PHONY: docker-stop
docker-stop:
	@$(DOCKER_COMPOSE) stop

## Stop and remove containers
.PHONY: docker-down
docker-down:
	@$(DOCKER_COMPOSE) down

## Remove containers and all data
.PHONY: docker-clean
docker-clean:
	@$(DOCKER_COMPOSE) down -v
	@rm -rf docker/postgres-data docker/mattermost
	@echo "Containers and data removed"

## Kill orphaned Docker containers on the MM port (useful after deleting a worktree)
.PHONY: docker-kill-orphans
docker-kill-orphans:
	@project=$$(docker ps --filter "publish=$(MM_PORT)" \
		--format '{{.Label "com.docker.compose.project"}}' | head -1); \
	if [ -z "$$project" ]; then \
		echo "No containers found on port $(MM_PORT)"; \
	else \
		echo "Stopping compose project: $$project"; \
		docker compose -p $$project down -v; \
		echo "Project $$project removed"; \
	fi

## View Mattermost container logs
.PHONY: docker-logs
docker-logs: docker-check
	@$(DOCKER_COMPOSE) logs -f mattermost

## First-time setup: start containers and create admin user
.PHONY: docker-setup
docker-setup: docker-start
	@echo "Waiting for Mattermost to be ready..."
	@until curl -sf http://localhost:$(MM_PORT)/api/v4/system/ping >/dev/null 2>&1; do \
		sleep 2; \
		echo "Waiting..."; \
	done
	@echo "Creating admin user..."
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local user create \
		--email admin@example.com \
		--username admin \
		--password 'password' \
		--system-admin 2>/dev/null || echo "Admin user already exists"
	@echo "Creating default team..."
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local team create \
		--name test \
		--display-name "Test" 2>/dev/null || echo "Team 'Test' already exists"
	@echo "Adding admin to Test team..."
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local team users add test admin 2>/dev/null || echo "Admin already in Test team"
	@echo ""
	@echo "=========================================="
	@echo "Mattermost ready at http://localhost:$(MM_PORT)"
	@echo "Login: admin / password"
	@echo "Team: Test"
	@echo "=========================================="

## Check if Mattermost container is running
.PHONY: docker-check
docker-check:
	@if ! $(DOCKER_COMPOSE) ps --status running 2>/dev/null | grep -q mattermost; then \
		echo "Error: Mattermost container is not running."; \
		echo "Run 'make docker-setup' first to start the environment."; \
		exit 1; \
	fi

## Build and deploy plugin to Docker Mattermost
.PHONY: docker-deploy
docker-deploy: docker-check dist
	@echo "Deploying plugin to Docker Mattermost..."
	@$(DOCKER_COMPOSE) cp dist/$(BUNDLE_NAME) mattermost:/tmp/$(BUNDLE_NAME)
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin add /tmp/$(BUNDLE_NAME) --force
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin enable $(PLUGIN_ID)
	@echo "Plugin $(PLUGIN_ID) deployed and enabled"

## Disable and re-enable plugin in Docker
.PHONY: docker-reset
docker-reset: docker-check
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin disable $(PLUGIN_ID)
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin enable $(PLUGIN_ID)
	@echo "Plugin $(PLUGIN_ID) reset"

## Disable plugin in Docker
.PHONY: docker-disable
docker-disable: docker-check
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin disable $(PLUGIN_ID)

## Enable plugin in Docker
.PHONY: docker-enable
docker-enable: docker-check
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin enable $(PLUGIN_ID)

## List installed plugins in Docker
.PHONY: docker-plugin-list
docker-plugin-list: docker-check
	@$(DOCKER_COMPOSE) exec -T mattermost mmctl --local plugin list

## Convenience alias: deploy plugin to Docker
.PHONY: deploy
deploy: docker-deploy

# ====================================================================================
# SBOM & Vulnerability Scanning
# ====================================================================================

CYCLONEDX_GOMOD_VERSION ?= v1.12.0
CYCLONEDX_NPM_VERSION ?= 6.0.1
GRYPE_VERSION ?= v0.120.1

## Install SBOM generation tools
.PHONY: install-sbom-tools
install-sbom-tools:
	@echo "Installing SBOM generation tools..."
	$(GO) install github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@$(CYCLONEDX_GOMOD_VERSION)

## Install Grype vulnerability scanner
.PHONY: install-grype
install-grype:
	@if $(GO) version -m "$(GOBIN)/grype" 2>/dev/null | grep -q "^[[:space:]]*mod[[:space:]]*github.com/anchore/grype[[:space:]]*$(GRYPE_VERSION)[[:space:]]"; then \
		echo "Grype $(GRYPE_VERSION) already installed"; \
	else \
		echo "Installing Grype $(GRYPE_VERSION) via go install (cross-platform, no anchore install.sh)..."; \
		mkdir -p $(GOBIN); \
		GOBIN=$(GOBIN) $(GO) install github.com/anchore/grype/cmd/grype@$(GRYPE_VERSION); \
	fi

## Generate Software Bill of Materials (SBOM) in CycloneDX JSON format
.PHONY: sbom
sbom: install-sbom-tools
	@mkdir -p dist/sbom
ifneq ($(HAS_SERVER),)
	@echo "Generating Go SBOM..."
	$(GOBIN)/cyclonedx-gomod mod -json -output dist/sbom/server-sbom.json
endif
ifneq ($(HAS_WEBAPP),)
	@echo "Generating Node.js SBOM..."
	# --omit dev: the plugin bundle ships only production deps; the dev/build
	# toolchain (eslint, webpack, babel) is never in webapp/dist, so its CVEs
	# must not gate releases. Grype still blocks HIGH/CRITICAL in shipped deps.
	cd webapp && npx --yes @cyclonedx/cyclonedx-npm@$(CYCLONEDX_NPM_VERSION) --omit dev --ignore-npm-errors --output-file ../dist/sbom/webapp-sbom.json
endif
	@echo "SBOMs generated in dist/sbom/"
	@ls -la dist/sbom/

## Scan SBOMs for vulnerabilities using Grype (fails on high or critical)
.PHONY: sbom-scan
sbom-scan: install-grype
	@if [ ! -d dist/sbom ]; then \
		echo "No SBOMs found. Run 'make sbom' first."; \
		exit 1; \
	fi
ifneq ($(HAS_SERVER),)
	@echo "Scanning Go dependencies for vulnerabilities..."
	$(GOBIN)/grype sbom:dist/sbom/server-sbom.json --output table --fail-on high
endif
ifneq ($(HAS_WEBAPP),)
	@echo "Scanning Node.js dependencies for vulnerabilities..."
	$(GOBIN)/grype sbom:dist/sbom/webapp-sbom.json --output table --fail-on high
endif

## Generate SBOMs and scan for vulnerabilities
.PHONY: sbom-audit
sbom-audit: sbom sbom-scan

GOVULNCHECK_VERSION ?= v1.9.0

## Scan Go code for known vulnerabilities it actually calls (fails on any finding)
.PHONY: govulncheck
govulncheck: apply
ifneq ($(HAS_SERVER),)
	@echo "Running govulncheck..."
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	GOTOOLCHAIN=$$($(GO) env GOVERSION) $(GOBIN)/govulncheck ./...
endif

# ====================================================================================
# CodeQL Security Analysis
# ====================================================================================

## CodeQL bundle, checked against its published SHA-256.
CODEQL_VERSION := 2.20.1
CODEQL_SHA256_linux64 := 790f5c109f15d26cf67d266591868e21fd6357211bbdbc4601921291a490b164
CODEQL_SHA256_osx64 := d97cf7ae727338dd9f09068386c612f386ebb8bf63327c300b7e370a269c88c9
CODEQL_DIR := $(PWD)/build/codeql
CODEQL := $(CODEQL_DIR)/codeql/codeql
CODEQL_DB_DIR := $(PWD)/build/codeql-db

## Install CodeQL CLI
.PHONY: install-codeql
install-codeql:
	@set -e; \
	if [ ! -f "$(CODEQL)" ]; then \
		echo "Installing CodeQL CLI v$(CODEQL_VERSION)..."; \
		mkdir -p $(CODEQL_DIR); \
		if [ "$$(uname)" = "Darwin" ]; then \
			CODEQL_PLATFORM="osx64"; sum=$(CODEQL_SHA256_osx64); \
		else \
			CODEQL_PLATFORM="linux64"; sum=$(CODEQL_SHA256_linux64); \
		fi; \
		tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
		curl -sSfL -o "$$tmp/codeql.tar.gz" "https://github.com/github/codeql-action/releases/download/codeql-bundle-v$(CODEQL_VERSION)/codeql-bundle-$$CODEQL_PLATFORM.tar.gz"; \
		if command -v sha256sum >/dev/null; then \
			echo "$$sum  $$tmp/codeql.tar.gz" | sha256sum -c -; \
		else \
			echo "$$sum  $$tmp/codeql.tar.gz" | shasum -a 256 -c -; \
		fi; \
		tar -xzf "$$tmp/codeql.tar.gz" -C "$$tmp"; \
		rm -rf $(CODEQL_DIR)/codeql; \
		mv "$$tmp/codeql" $(CODEQL_DIR)/; \
		echo "CodeQL CLI installed"; \
	else \
		echo "CodeQL CLI already installed"; \
	fi

## Run CodeQL analysis on Go code
.PHONY: codeql-go
codeql-go: install-codeql
ifneq ($(HAS_SERVER),)
	@echo "Running CodeQL analysis on Go code..."
	@rm -rf $(CODEQL_DB_DIR)/go
	@mkdir -p $(CODEQL_DB_DIR)/go
	$(CODEQL) database create $(CODEQL_DB_DIR)/go --language=go --source-root=server --overwrite
	$(CODEQL) database analyze $(CODEQL_DB_DIR)/go --format=sarif-latest --output=dist/codeql-go.sarif -- codeql/go-queries
	@echo "Go CodeQL results: dist/codeql-go.sarif"
endif

## Run CodeQL analysis on JavaScript/TypeScript code
.PHONY: codeql-js
codeql-js: install-codeql webapp/node_modules
ifneq ($(HAS_WEBAPP),)
	@echo "Running CodeQL analysis on JavaScript/TypeScript code..."
	@rm -rf $(CODEQL_DB_DIR)/js
	@mkdir -p $(CODEQL_DB_DIR)/js
	$(CODEQL) database create $(CODEQL_DB_DIR)/js --language=javascript --source-root=webapp --overwrite
	$(CODEQL) database analyze $(CODEQL_DB_DIR)/js --format=sarif-latest --output=dist/codeql-js.sarif -- codeql/javascript-queries
	@echo "JavaScript/TypeScript CodeQL results: dist/codeql-js.sarif"
endif

## Run CodeQL analysis on all code
.PHONY: codeql-analyze
codeql-analyze: codeql-go codeql-js
	@echo "CodeQL analysis complete. Results in dist/codeql-*.sarif"

## Check CodeQL SARIF reports for critical/high severity issues (level=error in SARIF)
.PHONY: security-gate
security-gate:
	@echo "Checking security scan results for critical/high issues..."
	@failed=0; \
	for sarif in dist/codeql-*.sarif; do \
		[ -f "$$sarif" ] || continue; \
		count=$$(python3 -c "import json,sys;data=json.load(open(sys.argv[1]));print(sum(1 for run in data.get('runs',[]) for result in run.get('results',[]) if result.get('level')=='error'))" "$$sarif"); \
		if [ "$$count" -gt 0 ]; then \
			echo "ERROR: $$sarif contains $$count critical/high severity issue(s)."; \
			failed=1; \
		else \
			echo "OK: $$sarif has no critical/high severity issues."; \
		fi; \
	done; \
	if [ "$$failed" -eq 1 ]; then \
		echo ""; \
		echo "Security gate FAILED: Critical or high severity issues found."; \
		echo "Review the SARIF files in dist/ for details."; \
		exit 1; \
	fi
	@echo "Security gate passed."

# ====================================================================================
# Virus Scanning
# ====================================================================================

## Install ClamAV antivirus scanner
.PHONY: install-clamav
install-clamav:
	@if ! command -v clamscan >/dev/null 2>&1; then \
		echo "Installing ClamAV..."; \
		if [ "$$(uname)" = "Darwin" ]; then \
			brew install clamav; \
		else \
			sudo apt-get update && sudo apt-get install -y clamav; \
		fi; \
	else \
		echo "ClamAV already installed"; \
	fi
	@echo "Updating virus definitions..."
	@if [ "$$(uname)" = "Darwin" ]; then \
		if [ ! -f /opt/homebrew/etc/clamav/freshclam.conf ] && [ -f /opt/homebrew/etc/clamav/freshclam.conf.sample ]; then \
			cp /opt/homebrew/etc/clamav/freshclam.conf.sample /opt/homebrew/etc/clamav/freshclam.conf; \
			sed -i '' 's/^Example/#Example/' /opt/homebrew/etc/clamav/freshclam.conf; \
		fi; \
	else \
		sudo systemctl stop clamav-freshclam 2>/dev/null || true; \
	fi
	@sudo freshclam || freshclam

## Scan dist/ for viruses using ClamAV (fails if any detected)
.PHONY: virus-scan
virus-scan: install-clamav
	@if [ ! -d dist ]; then \
		echo "No dist/ directory found. Run 'make dist' first."; \
		exit 1; \
	fi
	@echo "Scanning release artifacts for viruses..."
	clamscan --recursive --infected --alert-broken dist/
	@echo "Virus scan passed."

# ====================================================================================
# Help
# ====================================================================================

help:
	@cat Makefile build/*.mk | grep -v '\.PHONY' |  grep -v '\help:' | grep -B1 -E '^[a-zA-Z0-9_.-]+:.*' | sed -e "s/:.*//" | sed -e "s/^## //" |  grep -v '\-\-' | sed '1!G;h;$$!d' | awk 'NR%2{printf "\033[36m%-30s\033[0m",$$0;next;}1' | sort
