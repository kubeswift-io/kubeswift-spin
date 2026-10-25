{{- define "kubeswift-spin.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kubeswift-spin.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "kubeswift-spin.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "kubeswift-spin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kubeswift-spin.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kubeswift-spin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "kubeswift-spin.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kubeswift-spin.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* image reference: repository@digest when a digest is set, else repository:tag */}}
{{- define "kubeswift-spin.imageRef" -}}
{{- $img := index . 0 -}}
{{- $appVersion := index . 1 -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $img.repository (default $appVersion $img.tag) -}}
{{- end -}}
{{- end -}}

{{/* Compiled-cache flags shared by the controller and the cache manager. */}}
{{- define "kubeswift-spin.compiledCacheArgs" -}}
{{- $_ := required "compiledCache.enabled needs runtimeImage.digest: compiled caches are keyed by the runtime image digest" .Values.runtimeImage.digest -}}
- --compiled-cache-repository={{ required "compiledCache.repository is required when compiledCache.enabled is true" .Values.compiledCache.repository }}
- --compiled-cache-namespace={{ .Release.Namespace }}
- --compiled-cache-architecture={{ .Values.compiledCache.architecture }}
- --compiled-cache-runtimes={{ join "," (prepend .Values.compiledCache.extraRuntimes (printf "%s@%s" .Values.runtimeImage.repository .Values.runtimeImage.digest)) }}
{{- end -}}

{{/* Local application flags shared by the controller and the cache manager
(ADR 0015). */}}
{{- define "kubeswift-spin.localApplicationArgs" -}}
{{- $_ := required "localApplication.enabled needs runtimeImage.digest: only digest-pinned runtime images run local applications" .Values.runtimeImage.digest -}}
{{- if not .Values.localApplication.nodeCacheBounded -}}
{{- fail "localApplication.enabled needs localApplication.nodeCacheBounded=true: bound KubeSwift's node artifact cache first (KubeSwift chart sandboxMaterialize.artifactCache.maxBytes or maxPercent); see docs/local-application.md" -}}
{{- end }}
- --local-application
- --local-application-runtimes={{ join "," (prepend .Values.localApplication.extraRuntimes (printf "%s@%s" .Values.runtimeImage.repository .Values.runtimeImage.digest)) }}
{{- with .Values.localApplication.registries }}
- --local-application-registries={{ join "," . }}
{{- end }}
- --local-application-max-placements-per-namespace={{ .Values.localApplication.maxPlacementsPerNamespace }}
- --local-application-max-new-per-hour={{ .Values.localApplication.maxNewPerHour }}
- --local-application-retention={{ .Values.localApplication.retention }}
{{- end -}}

{{/* How replicas and placements read signed caches: the controller and the
cache manager build the same artifact from these. */}}
{{- define "kubeswift-spin.compiledCacheDeliveryArgs" -}}
- --compiled-cache-fetch-timeout={{ .Values.compiledCache.fetchTimeout }}
{{- with .Values.compiledCache.pullSecretName }}
- --compiled-cache-pull-secret={{ . }}
{{- end }}
{{- end -}}

{{/* Flags that shape how the controller and the cache manager read executors. */}}
{{- define "kubeswift-spin.profileArgs" -}}
- --runtime-image={{ include "kubeswift-spin.imageRef" (list .Values.runtimeImage (required "runtimeImage.tag or runtimeImage.digest is required" (.Values.runtimeImage.tag | default .Values.runtimeImage.digest))) }}
- --default-cpu={{ .Values.controller.defaultCPU }}
- --default-memory={{ .Values.controller.defaultMemory }}
- --min-memory={{ .Values.controller.minMemory }}
- --max-memory={{ .Values.controller.maxMemory }}
- --max-vcpus={{ .Values.controller.maxVCPUs }}
- --max-replicas={{ .Values.controller.maxReplicas }}
{{- with .Values.controller.watchNamespaces }}
- --watch-namespaces={{ join "," . }}
{{- end }}
{{- end -}}

{{- define "kubeswift-spin.cacheManagerName" -}}
{{- printf "%s-cache-manager" (include "kubeswift-spin.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* The cache manager's pods carry their own app.kubernetes.io/name, so the
controller's Deployment and metrics Service never select them. */}}
{{- define "kubeswift-spin.cacheManagerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kubeswift-spin.name" . }}-cache-manager
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
