{{/*
Expand the name of the chart.
*/}}
{{- define "metadata-collector.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "metadata-collector.fullname" -}}
{{- "metadata-collector" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "metadata-collector.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "metadata-collector.labels" -}}
helm.sh/chart: {{ include "metadata-collector.chart" . }}
{{ include "metadata-collector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "metadata-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "metadata-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}


{{/*
GPU Operator installation mode from global.gpuDraEnabled. Defaults to
false;
*/}}
{{- define "metadata-collector.gpuDraEnabled" -}}
{{- $enabled := (.Values.global | default dict).gpuDraEnabled | default false -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.gpuDraEnabled must be a boolean (true or false), got %s %#v" (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end }}

{{/*
GPU Operator NRI plugin mode from nriPlugin.enabled. Defaults to false. The
NRI plugin runs in the Container Toolkit, which GPUCluster (DRA) mode does not
deploy, so the two modes cannot be combined.
*/}}
{{- define "metadata-collector.nriPluginEnabled" -}}
{{- $nri := .Values.nriPlugin | default dict -}}
{{- $enabled := false -}}
{{- if hasKey $nri "enabled" -}}
{{- $enabled = $nri.enabled -}}
{{- end -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "metadata-collector.nriPlugin.enabled must be a boolean (true or false), got %s %#v" (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}
{{- if include "metadata-collector.gpuDraEnabled" . -}}
{{- fail "metadata-collector.nriPlugin.enabled and global.gpuDraEnabled are mutually exclusive: GPUCluster (DRA) mode has no Container Toolkit, so there is no NRI plugin to inject the CDI device" -}}
{{- end -}}
{{- if and $nri.cdiDevice (not (kindIs "string" $nri.cdiDevice)) -}}
{{- fail (printf "metadata-collector.nriPlugin.cdiDevice must be a string, got %s %#v" (kindOf $nri.cdiDevice) $nri.cdiDevice) -}}
{{- end -}}
{{- if not $nri.cdiDevice -}}
{{- fail "metadata-collector.nriPlugin.cdiDevice must be set when nriPlugin.enabled is true" -}}
{{- end -}}
true
{{- end -}}
{{- end }}

{{/*
Whether the Prometheus metrics endpoint is enabled, as a template-truthy string.

Must be a real YAML boolean. Go-template truthiness would otherwise decide it for us: the string
"false" is truthy and would leave the endpoint ENABLED, binding a port on the node because this
DaemonSet is hostNetwork. Fail the render instead, matching nvsentinel.pcAuth.enabled.
*/}}
{{- define "metadata-collector.metricsEnabled" -}}
{{- $enabled := (.Values.metrics | default dict).enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "metadata-collector.metrics.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable the endpoint, which binds a host port here." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{/*
Kubelet's --root-dir on the host. Nil/empty → /var/lib/kubelet.
*/}}
{{- define "metadata-collector.kubeletRootDir" -}}
{{- $dir := (.Values.global | default dict).kubeletRootDir -}}
{{- if or (kindIs "invalid" $dir) (eq ($dir | toString) "") -}}
{{- $dir = "/var/lib/kubelet" -}}
{{- end -}}
{{- $trimmed := "" -}}
{{- if kindIs "string" $dir -}}
{{- $trimmed = regexReplaceAll "/+$" (trim $dir) "" -}}
{{- end -}}
{{- if or (not (hasPrefix "/" $trimmed)) (hasSuffix "/pod-resources" $trimmed) -}}
{{- fail (printf "global.kubeletRootDir must be kubelet's --root-dir as an absolute path, such as /var/lib/kubelet, not its pod-resources subdirectory; got %s %#v" (kindOf $dir) $dir) -}}
{{- end -}}
{{- $trimmed -}}
{{- end -}}

{{/*
Pod affinity: the user's affinity plus a required rule on the GPU Operator's
nvidia.com/gpu.deploy.client label, so this NVML client leaves the node while
the GPU Operator unloads the driver or changes the MIG layout.

GPU Operator 26.7+ sets the label to "true" on GPU nodes (state_manager.go).
k8s-driver-manager v0.12.0 (maybeSetPaused) changes it to
"paused-for-driver-upgrade", and mig-parted v0.15.0 to "paused-for-mig-change".
They set it back to "true" when they finish. Older GPU Operator releases never
set the label, so an absent label must still schedule the pod.

The two terms are ORed: the pod runs when the label is "true" or absent, and
the DaemonSet controller removes it for any other value. Each user
nodeSelectorTerm is ANDed with both terms, so a user rule still applies.
*/}}
{{- define "metadata-collector.affinity" -}}
{{- $affinity := deepCopy (.Values.global.affinity | default .Values.affinity | default dict) -}}
{{- $nodeAffinity := $affinity.nodeAffinity | default dict -}}
{{- $required := $nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution | default dict -}}
{{- $clientRules := list
  (dict "key" "nvidia.com/gpu.deploy.client" "operator" "In" "values" (list "true"))
  (dict "key" "nvidia.com/gpu.deploy.client" "operator" "DoesNotExist") -}}
{{- $terms := list -}}
{{- range $userTerm := ($required.nodeSelectorTerms | default (list (dict))) -}}
{{- range $rule := $clientRules -}}
{{- $term := deepCopy $userTerm -}}
{{- $_ := set $term "matchExpressions" (append ($term.matchExpressions | default list) $rule) -}}
{{- $terms = append $terms $term -}}
{{- end -}}
{{- end -}}
{{- $_ := set $required "nodeSelectorTerms" $terms -}}
{{- $_ = set $nodeAffinity "requiredDuringSchedulingIgnoredDuringExecution" $required -}}
{{- $_ = set $affinity "nodeAffinity" $nodeAffinity -}}
{{- toYaml $affinity -}}
{{- end -}}
