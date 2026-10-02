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
