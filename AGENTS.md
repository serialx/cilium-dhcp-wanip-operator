# Repository Guidelines

## Project Structure & Module Organization
The controller entrypoint is in `cmd/main.go`, with reconciliation logic under `internal/` (`internal/controller`, `internal/routers`). API types and generated code live in `api/v1alpha1`; CRDs, RBAC, and deployment overlays sit in `config/`. Development scaffolding and boilerplates are in `hack/`, while integration fixtures and helpers live in `test/e2e`. Unit tests stay next to their source files.

## Build, Test, and Development Commands
`make build` compiles the manager binary to `bin/manager`; `make run` launches it against the kubeconfig on your machine. Use `make test` for unit tests and coverage (`cover.out`) and `make lint` or `make lint-fix` to enforce formatting and lint rules. `make test-e2e` provisions the Kind cluster defined by `KIND_CLUSTER`, runs the Ginkgo suite in `test/e2e`, and cleans it up. Produce a single install manifest with `make build-installer`, or ship an image with `make docker-build`.

## Coding Style & Naming Conventions
Stick to `go fmt` output; let imports be organized automatically. Package names should remain short and lowercase; exported identifiers use UpperCamelCase, internal helpers use lowerCamelCase. Follow Go filename norms such as `public_ip_claim_controller.go` and `*_test.go`. The configured `golangci-lint` stack (revive, staticcheck, ginkgolinter, etc.) must be clean before committing.

## Testing Guidelines
Unit tests rely on the standard library plus controller-runtime fakes; name them `TestXxx` and scope helpers per package. The Ginkgo e2e suite uses `Describe`/`It` blocks and `-tags=e2e`; reuse `make setup-test-e2e` when you need a long-lived Kind cluster. Inspect coverage with `go tool cover -html=cover.out` and keep `coverage_full.out` up to date for regression comparisons.

## Commit & Pull Request Guidelines
History follows a `<type>: description` subject (`docs:`, `test:`, `feat:`) capped near 72 characters, often referencing PR numbers like `(#2)`. Summaries should explain the change, note testing commands, and link related issues. Pull requests are expected to mention generated artifacts touched, attach relevant logs or screenshots, and confirm lint plus unit tests ran.

## Deployment & Configuration Tips
`make install` or `make deploy` apply manifests via the `config/default` kustomize overlay; never edit generated YAML under `config/crd/bases` by hand. Override the default `IMG` or `KIND_CLUSTER` by exporting environment variables when building images or running e2e tests. Regenerate client code with `make manifests` and `make generate` whenever API types change.
