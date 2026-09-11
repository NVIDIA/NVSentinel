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
