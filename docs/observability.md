# Observability

## Controller metrics

The controller serves Prometheus metrics on `--metrics-bind-address`
(chart default `:8080`, Service `<release>-metrics`, port `metrics`), next to
the standard `controller_runtime_*`, `workqueue_*` and Go runtime metrics.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `kubeswift_spin_reconciliations_total` | counter | `result` (`success`, `error`) | SpinApp reconciliations |
| `kubeswift_spin_reconcile_errors_total` | counter | none | reconciliations that returned an error |
| `kubeswift_spin_apps` | gauge | `state` (`available`, `progressing`, `blocked`, `unavailable`) | SpinApps realized by kubeswift-spin |
| `kubeswift_spin_ready_replicas` | gauge | none | sum of `readyReplicas` |
| `kubeswift_spin_desired_replicas` | gauge | none | sum of desired replicas |
| `kubeswift_spin_sandbox_creations_total` | counter | none | SwiftSandboxes created |
| `kubeswift_spin_sandbox_deletions_total` | counter | `reason` (`scale_down`, `rollout`, `failed`, `stale`, `executor_change`) | SwiftSandboxes deleted |
| `kubeswift_spin_sandbox_failures_total` | counter | none | sandboxes observed `Completed` or `Failed` |
| `kubeswift_spin_unsupported_configuration_total` | counter | `field` (a SpinApp spec field name) | SpinApp generations found to use unsupported configuration |

Labels are limited to the fixed sets above. Namespaces, application names,
UIDs, images and error strings are never label values; per-app detail is in
SpinApp status and Events. `state=blocked` means unsupported configuration,
an invalid or missing executor, a warm-pool mismatch or a name conflict;
`state=unavailable` includes every app that runs but cannot be exposed
(`NetworkUnavailable`), which with KubeSwift v0.15.1 is every running app.

Useful queries:

```promql
sum by (state) (kubeswift_spin_apps)
rate(kubeswift_spin_sandbox_failures_total[10m])
sum by (field) (increase(kubeswift_spin_unsupported_configuration_total[1h]))
```

### Scraping

With `metrics.serviceMonitor.enabled=true` the chart creates a
ServiceMonitor for the Prometheus Operator. With `metrics.secure=true` the
endpoint is served over HTTPS and requires a bearer token authorized for
`get` on the non-resource URL `/metrics`; the chart creates a
`<release>-metrics-reader` ClusterRole to bind to the Prometheus service
account. controller-runtime then serves a self-signed certificate, so the
generated ServiceMonitor skips certificate verification and Prometheus sends
its token without authenticating the server (see
[security-model.md](security-model.md#metrics-endpoint)).

## Logs

The controller logs JSON through zap (`--zap-log-level`, chart value
`controller.logLevel`). Reconcile log lines carry `spinapp`, `namespace`
and `executor`, plus `sandbox` and `revision` or `reason` for create and
delete actions. Objects, variable values and runtime configuration are
never logged.

## Events and status

Per-application state is in the SpinApp: `kubectl describe spinapp <name>`
shows the `Available` and `Progressing` conditions and the Events listed in
[executor-contract.md](executor-contract.md#events). The SwiftSandboxes
(`kubectl get swiftsandbox -l spin.kubeswift.io/app=<name>`) carry
KubeSwift's own conditions and Events.

## Spin output

Spin's console output, including component `println!` output, goes to the
guest console. Read it with KubeSwift's CLI:

```bash
swiftctl -n <namespace> sandbox logs <app>-0
```

Spin does not write log files in the sandbox (see
[executor-contract.md](executor-contract.md#spin-command-line)).

## Application telemetry

Spin exports traces, metrics and logs with OpenTelemetry when the standard
`OTEL_EXPORTER_OTLP_*` variables are set. kubeswift-spin sets them from the
executor's `spec.deploymentConfig.otel`, the same SpinKube field Spin
Operator uses:

```yaml
apiVersion: core.spinkube.dev/v1alpha1
kind: SpinAppExecutor
metadata:
  name: kubeswift-open
  labels:
    spin.kubeswift.io/managed-by: kubeswift-spin
  annotations:
    spin.kubeswift.io/network-mode: open
spec:
  createDeployment: false
  deploymentConfig:
    otel:
      exporter_otlp_endpoint: http://otel-collector.observability.svc:4318
```

An in-cluster collector is a cluster address, so it is unreachable from the
default `restricted` network mode; use an `open` executor or a collector
with a public address. This path has been verified only at the level of the
generated sandbox environment, not by exporting data from a sandbox.
