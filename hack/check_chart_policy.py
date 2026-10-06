"""Security policy checks for rendered kubeswift-spin chart manifests.

Reads multi-document YAML on stdin; see hack/check-chart.sh.

RBAC is checked against an exact allowlist: every Role and ClusterRole the
chart renders must match one of the expected rule sets below, so any new
permission fails the check until it is reviewed and added here (and to
docs/security-model.md).
"""
import sys

import yaml


def norm(rules):
    """Order-independent representation of a rule list."""
    out = set()
    for r in rules:
        for group in r.get("apiGroups", [None]):
            for res in r.get("resources", [None]):
                for verb in r.get("verbs", []):
                    out.add(("api", group, res, verb))
        for url in r.get("nonResourceURLs", []):
            for verb in r.get("verbs", []):
                out.add(("url", url, verb))
    return frozenset(out)


EXPECTED = {
    "controller": norm([
        {"apiGroups": ["core.spinkube.dev"], "resources": ["spinapps", "spinappexecutors"], "verbs": ["get", "list", "watch"]},
        {"apiGroups": ["core.spinkube.dev"], "resources": ["spinapps/status"], "verbs": ["get", "patch"]},
        {"apiGroups": ["core.spinkube.dev"], "resources": ["spinapps/finalizers"], "verbs": ["update"]},
        {"apiGroups": ["sandbox.kubeswift.io"], "resources": ["swiftsandboxes"], "verbs": ["get", "list", "watch", "create", "delete"]},
        {"apiGroups": ["sandbox.kubeswift.io"], "resources": ["swiftsandboxpools"], "verbs": ["get", "list", "watch"]},
        {"apiGroups": ["events.k8s.io"], "resources": ["events"], "verbs": ["create", "patch"]},
    ]),
    "leader-election": norm([
        {"apiGroups": ["coordination.k8s.io"], "resources": ["leases"], "verbs": ["get", "create", "update"]},
        {"apiGroups": [""], "resources": ["events"], "verbs": ["create", "patch"]},
    ]),
    "metrics-auth": norm([
        {"apiGroups": ["authentication.k8s.io"], "resources": ["tokenreviews"], "verbs": ["create"]},
        {"apiGroups": ["authorization.k8s.io"], "resources": ["subjectaccessreviews"], "verbs": ["create"]},
    ]),
    "metrics-reader": norm([
        {"nonResourceURLs": ["/metrics"], "verbs": ["get"]},
    ]),
}

# The kind each approved rule set must have: leader election is confined to
# the release namespace; everything else is cluster-wide by design.
EXPECTED_KIND = {"controller": "ClusterRole", "leader-election": "Role", "metrics-auth": "ClusterRole", "metrics-reader": "ClusterRole"}

docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
errors = []
# Bindings may only grant the chart's own roles to the chart's own
# ServiceAccount, the one the Deployment runs as.
roles = {(d["kind"], d["metadata"]["name"]) for d in docs if d.get("kind") in ("ClusterRole", "Role")}
accounts = {(d["metadata"]["name"], d["metadata"].get("namespace")) for d in docs if d.get("kind") == "ServiceAccount"}
pod_accounts = {d["spec"]["template"]["spec"].get("serviceAccountName") for d in docs if d.get("kind") == "Deployment"}
for d in docs:
    kind, name = d.get("kind"), d.get("metadata", {}).get("name")
    if kind in ("ClusterRole", "Role"):
        rules = norm(d.get("rules", []))
        if rules not in EXPECTED.values():
            extra = sorted(str(x) for x in rules - frozenset().union(*EXPECTED.values()))
            errors.append(f"{kind}/{name}: rules are not an approved set; unexpected: {extra or 'combination'}")
        else:
            role_set = next(k for k, v in EXPECTED.items() if v == rules)
            if kind != EXPECTED_KIND[role_set]:
                errors.append(f"{kind}/{name}: the {role_set} rules must be a {EXPECTED_KIND[role_set]}")
            if kind == "Role" and d["metadata"].get("namespace") not in {ns for _, ns in accounts}:
                errors.append(f"{kind}/{name}: must be in the controller ServiceAccount's namespace")
        if d.get("aggregationRule"):
            errors.append(f"{kind}/{name}: aggregationRule is not allowed")
    if kind in ("ClusterRoleBinding", "RoleBinding"):
        ref = (d["roleRef"].get("kind"), d["roleRef"].get("name"))
        if kind == "ClusterRoleBinding" and ref[0] != "ClusterRole":
            errors.append(f"{kind}/{name}: must reference a ClusterRole")
        if kind == "RoleBinding" and d["metadata"].get("namespace") not in {ns for _, ns in accounts}:
            errors.append(f"{kind}/{name}: must be in the controller ServiceAccount's namespace")
        if ref not in roles:
            errors.append(f"{kind}/{name}: roleRef {ref[0]}/{ref[1]} is not a role rendered by the chart")
        for sub in d.get("subjects") or []:
            if sub.get("kind") != "ServiceAccount" or (sub.get("name"), sub.get("namespace")) not in accounts \
                    or sub.get("name") not in pod_accounts:
                errors.append(f"{kind}/{name}: subject {sub.get('kind')}/{sub.get('name')} is not the controller ServiceAccount")
        if not d.get("subjects"):
            errors.append(f"{kind}/{name}: has no subjects")
    if kind == "Deployment":
        pod = d["spec"]["template"]["spec"]
        psc = pod.get("securityContext", {})
        if psc.get("seccompProfile", {}).get("type") != "RuntimeDefault":
            errors.append("Deployment: pod seccompProfile is not RuntimeDefault")
        if not psc.get("runAsNonRoot"):
            errors.append("Deployment: pod runAsNonRoot is not true")
        for c in pod["containers"]:
            sc = c.get("securityContext", {})
            if sc.get("allowPrivilegeEscalation") is not False:
                errors.append(f"container {c['name']}: allowPrivilegeEscalation is not false")
            if sc.get("readOnlyRootFilesystem") is not True:
                errors.append(f"container {c['name']}: readOnlyRootFilesystem is not true")
            if sc.get("privileged"):
                errors.append(f"container {c['name']}: privileged")
            if "ALL" not in sc.get("capabilities", {}).get("drop", []):
                errors.append(f"container {c['name']}: capabilities do not drop ALL")
            if sc.get("capabilities", {}).get("add"):
                errors.append(f"container {c['name']}: adds capabilities")
            image = c["image"]
            if image.endswith(":latest") or (":" not in image.split("/")[-1] and "@" not in image):
                errors.append(f"container {c['name']}: image {image} is not pinned")
            for a in c.get("args", []):
                if a.startswith("--runtime-image=") and a.endswith(":latest"):
                    errors.append(f"runtime image {a} uses latest")
        if pod.get("hostNetwork") or pod.get("hostPID") or pod.get("hostIPC"):
            errors.append("Deployment: uses host namespaces")
        if any(v.get("hostPath") for v in pod.get("volumes", []) or []):
            errors.append("Deployment: mounts a hostPath volume")
    if kind == "SpinAppExecutor":
        if d["spec"].get("createDeployment") is not False:
            errors.append(f"SpinAppExecutor/{name}: createDeployment is not false")
        if d["metadata"].get("labels", {}).get("spin.kubeswift.io/managed-by") != "kubeswift-spin":
            errors.append(f"SpinAppExecutor/{name}: missing managed-by label")
if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print(f"chart policy: {len(docs)} objects checked")
