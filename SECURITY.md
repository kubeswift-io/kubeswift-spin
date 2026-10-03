# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub security advisories:
https://github.com/kubeswift-io/kubeswift-spin/security/advisories/new

Please include the affected version or commit, a description of the issue
and its impact, and steps to reproduce. Do not open a public issue.

We aim to acknowledge reports within five working days and to agree on a
disclosure timeline with the reporter.

## Scope

In scope: the kubeswift-spin controller, the runtime image and its
entrypoint, the Helm chart, the release workflows, and the example
applications in this repository.

Vulnerabilities in KubeSwift, Spin, Spin Operator, Cloud Hypervisor or
Kubernetes should be reported to those projects. If you are unsure where an
issue belongs, report it here and we will help route it.

## Supported versions

kubeswift-spin has not had a release yet. Until v1.0, security fixes are
made on `main` and in the latest release only.

## Security model

The design, trust boundaries and known limitations are described in
[docs/security-model.md](docs/security-model.md).
