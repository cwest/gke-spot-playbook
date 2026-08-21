.PHONY: test test-go test-python test-shell test-manifests tools check-tools

# Pinned so CI and a laptop validate against the same schema logic.
KUBECONFORM_VERSION ?= v0.8.0

test: check-tools test-go test-python test-shell test-manifests

## check-tools: fail with instructions, not `command not found`.
check-tools:
	@missing=0; \
	for t in go shellcheck kubeconform python3; do \
	  command -v $$t >/dev/null 2>&1 || { echo "missing: $$t"; missing=1; }; \
	done; \
	if [ $$missing -ne 0 ]; then \
	  echo ""; \
	  echo "Install the missing tools, then re-run \`make test\`:"; \
	  echo "  make tools     # installs kubeconform $(KUBECONFORM_VERSION) via go install"; \
	  echo "  brew install shellcheck kubeconform   # macOS/Linuxbrew alternative"; \
	  echo ""; \
	  echo "See CONTRIBUTING.md for the full prerequisite list."; \
	  exit 1; \
	fi

## tools: install the pinned Go-based dev tooling into $(go env GOPATH)/bin.
tools:
	go install github.com/yannh/kubeconform/cmd/kubeconform@$(KUBECONFORM_VERSION)
	@echo "installed kubeconform $(KUBECONFORM_VERSION) -> $$(go env GOPATH)/bin"
	@command -v kubeconform >/dev/null 2>&1 || \
	  echo "note: add $$(go env GOPATH)/bin to your PATH to pick it up."

test-go:
	cd advisor && go test ./...
	if [ -f workloads/01-queue/worker/go.mod ]; then cd workloads/01-queue/worker && go test ./...; fi
	if [ -f workloads/05-agents/agent/go.mod ]; then cd workloads/05-agents/agent && go test ./...; fi

test-python:
	@for w in workloads/02-embeddings/worker workloads/03-finetune/trainer; do \
		if [ -f $$w/requirements-test.txt ]; then \
			(cd $$w && { [ -d .venv ] || python3 -m venv .venv; } && \
			./.venv/bin/pip install -q -r requirements-test.txt && \
			./.venv/bin/python -m pytest -q tests); \
		fi; \
	done

test-shell:
	shellcheck infra/*.sh infra/lib/*.sh infra/tests/*.sh demo/*.sh demo/act1/*.sh demo/act2/*.sh demo/act5/*.sh demo/cost/*.sh
	./infra/tests/test_region_portability.sh
	./infra/tests/test_migrate_region.sh
	./infra/tests/test_reconciler_manifests.sh

test-manifests:
	if [ -d workloads/01-queue/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/01-queue/manifests/; fi
	if [ -d workloads/02-embeddings/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/02-embeddings/manifests/; fi
	if [ -d workloads/03-finetune/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/03-finetune/manifests/; fi
	if [ -d workloads/05-agents/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/05-agents/manifests/; fi
	if [ -d workloads/06-serve/manifests ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary workloads/06-serve/manifests/; fi
	if [ -d infra/kueue ]; then \
	  kubeconform -strict -ignore-missing-schemas -summary infra/kueue/; fi
	if [ -d infra/reconciler ]; then \
	  sed -e 's|PROJECT_PLACEHOLDER_ID|p|g' -e 's|PROJECT_PLACEHOLDER|g@p.iam.gserviceaccount.com|g' \
	      -e 's|REGION_PLACEHOLDER|us-central1|g' -e 's|CLUSTER_PLACEHOLDER|spot-demo|g' \
	      -e 's|IMAGE_PLACEHOLDER|img:v1|g' \
	      infra/reconciler/*.yaml \
	  | kubeconform -strict -ignore-missing-schemas -summary -; fi
