"""Security policy checks for rendered kubeswift-spin chart manifests.

Reads multi-document YAML on stdin; see hack/check-chart.sh.
"""
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
errors = []
forbidden = {"secrets", "configmaps", "pods", "pods/exec", "serviceaccounts/token"}
for d in docs:
    kind, name = d.get("kind"), d.get("metadata", {}).get("name")
    if kind in ("ClusterRole", "Role"):
        for r in d.get("rules", []):
            for field in ("verbs", "resources", "apiGroups"):
                if "*" in r.get(field, []):
                    errors.append(f"{kind}/{name}: wildcard in {field}")
            for res in r.get("resources", []):
                if res in forbidden:
                    errors.append(f"{kind}/{name}: grants access to {res}")
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
            if c["image"].endswith(":latest") or ":" not in c["image"].split("/")[-1] and "@" not in c["image"]:
                errors.append(f"container {c['name']}: image {c['image']} is not pinned")
            for a in c.get("args", []):
                if a.startswith("--runtime-image=") and a.endswith(":latest"):
                    errors.append(f"runtime image {a} uses latest")
        if pod.get("hostNetwork") or pod.get("hostPID") or pod.get("hostIPC"):
            errors.append("Deployment: uses host namespaces")
    if kind == "SpinAppExecutor":
        if d["spec"].get("createDeployment") is not False:
            errors.append(f"SpinAppExecutor/{name}: createDeployment is not false")
        if d["metadata"].get("labels", {}).get("spin.kubeswift.io/managed-by") != "kubeswift-spin":
            errors.append(f"SpinAppExecutor/{name}: missing managed-by label")
if errors:
    print("\n".join(errors), file=sys.stderr)
    sys.exit(1)
print(f"chart policy: {len(docs)} objects checked")
