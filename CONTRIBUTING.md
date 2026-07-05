# Contributing

Thanks for your interest in improving keenetic-operator!

## Development environment

- Go 1.26+
- Kubebuilder v4.15+ (`kubebuilder`, only needed to scaffold new APIs)
- A kubeconfig for a test cluster (kind/minikube is fine); envtest is used for unit tests

## Workflow

```bash
make manifests generate   # regenerate CRDs/RBAC/deepcopy after changing api/*
make test                 # unit + envtest
golangci-lint run         # lint; must be clean
make build                # build the manager
```

Please make sure `golangci-lint run` and `make test` are green before opening a
merge request — CI enforces both.

## Commit messages

Use short, imperative summaries (e.g. `add drift re-assertion for host records`).
Conventional Commit prefixes (`feat:`, `fix:`, `docs:`, `ci:`) are welcome.

## Merge requests

- Keep changes focused; one logical change per MR.
- Update the README/CHANGELOG when behaviour or configuration changes.
- Describe how you tested the change.
