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
{{- $enabled := $nri.enabled | default false -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "metadata-collector.nriPlugin.enabled must be a boolean (true or false), got %s %#v" (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}
{{- if include "metadata-collector.gpuDraEnabled" . -}}
{{- fail "metadata-collector.nriPlugin.enabled and global.gpuDraEnabled are mutually exclusive: GPUCluster (DRA) mode has no Container Toolkit, so there is no NRI plugin to inject the CDI device" -}}
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
