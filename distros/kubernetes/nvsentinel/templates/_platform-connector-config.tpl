# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

{{/*
nvsentinel.platformConnector.configFiles renders the platform connector's
config files for one role, as ConfigMap data entries: config.json (the
connector toggles and settings, the node-binding auth settings and the
pipeline) and the transformer TOML files. Both roles read the same keys; the
dict passed in selects what differs between them:

  context:             the chart context
  k8sQps, k8sBurst:    the Kubernetes client rate limit (connector writes and
                       node metadata reads)
  cacheSize, cacheTTL: the node metadata cache, cacheTTL as a duration string;
                       metadata.toml is rendered only when the MetadataAugmentor
                       transformer is configured
  deployment:          the deployment role's tunables, rendered as the
                       "deployment" object; absent for the DaemonSet
*/}}
{{- define "nvsentinel.platformConnector.configFiles" -}}
{{- $files := . -}}
{{- with .context -}}
config.json: |
  {
    "enableK8sPlatformConnector": "{{ .Values.platformConnector.k8sConnector.enabled }}",
    "K8sConnectorQps": {{ printf "%.2f" ($files.k8sQps | float64) }},
    "K8sConnectorBurst": {{ $files.k8sBurst }},
    "K8sConnectorMaxRetries": {{ .Values.platformConnector.k8sConnector.maxRetries }},
    "K8sConnectorMaxRetryDuration": {{ .Values.platformConnector.k8sConnector.maxRetryDuration | quote }},
    "MaxNodeConditionMessageLength": {{ .Values.platformConnector.k8sConnector.maxNodeConditionMessageLength }},
    "CompactedHealthEventMsgLen": {{ .Values.platformConnector.k8sConnector.compactedHealthEventMsgLen }},
    "StoreConnectorMaxRetries": {{ .Values.platformConnector.mongodbStore.maxRetries }},
    "enableMongoDBStorePlatformConnector": "{{ or .Values.global.mongodbStore.enabled (and .Values.global.datastore (eq (default "" .Values.global.datastore.provider) "mongodb")) }}",
    "enablePostgresDBStorePlatformConnector": {{ if and .Values.global.datastore .Values.global.datastore.provider }}{{ eq .Values.global.datastore.provider "postgresql" | quote }}{{ else }}"false"{{ end }}
    ,"enableNodeBindingAuth": "{{ include "nvsentinel.pcAuth.enabled" . | default "false" }}"
    ,"AuthAudience": "{{ include "nvsentinel.pcAuth.audience" . }}"
    ,"AuthCrossNodeServiceAccounts": {{ include "nvsentinel.pcAuth.crossNodeUsernames" . }}
    ,"AuthMode": "{{ include "nvsentinel.pcAuth.mode" . }}"
    ,"AuthFailOpenOnUnavailable": "{{ include "nvsentinel.pcAuth.failOpenOnUnavailable" . | default "false" }}"
    ,"enableGRPCSinkConnector": "{{ .Values.platformConnector.grpcSinkConnector.enabled }}"
    ,"GRPCSinkTarget": "{{ .Values.platformConnector.grpcSinkConnector.target }}"
    ,"GRPCSinkConnectorMaxRetries": {{ .Values.platformConnector.grpcSinkConnector.maxRetries }}
    ,"GRPCSinkTokenPath": "{{ .Values.platformConnector.grpcSinkConnector.tokenPath }}"
    ,"enablePromPlatformConnector": "{{ .Values.platformConnector.promConnector.enabled }}"
    {{- with $files.deployment }}
    ,"deployment": {
      "TokenReviewQps": {{ .TokenReviewQps }},
      "TokenReviewBurst": {{ .TokenReviewBurst }},
      "TokenCacheSize": {{ .TokenCacheSize }},
      "ConditionUpdateTimeout": {{ .ConditionUpdateTimeout | quote }},
      "MaxConnectionAge": {{ .MaxConnectionAge | quote }},
      "MaxConnectionIdle": {{ .MaxConnectionIdle | quote }},
      "GrpcReadBufferBytes": {{ .GrpcReadBufferBytes }},
      "GrpcWriteBufferBytes": {{ .GrpcWriteBufferBytes }}
    }
    {{- end }}
    {{- $pipeline := (.Values.platformConnector.pipeline | default list) }}
    {{- with .Values.platformConnector.dedup }}
    {{- $pipeline = append $pipeline (dict "name" "Deduplicator" "enabled" .enabled "config" "/etc/config/dedup.toml") }}
    {{- end }}
    ,"pipeline": {{ $pipeline | toJson }}
  }
{{- if .Values.platformConnector.dedup }}
dedup.toml: |
  suppressionWindow = "{{ .Values.platformConnector.dedup.suppressionWindow }}"
  cleanupInterval = "{{ .Values.platformConnector.dedup.cleanupInterval }}"
  includeChecks = {{ .Values.platformConnector.dedup.includeChecks | default list | toJson }}
{{- end }}
{{- with .Values.platformConnector.transformers.MetadataAugmentor }}
metadata.toml: |
  cacheSize = {{ $files.cacheSize }}
  cacheTTL = "{{ $files.cacheTTL }}"
  {{- with .allowedLabels }}
  allowedLabels = {{ . | toJson }}
  {{- end }}
  {{- with .skipNodeLabel }}
  skipNodeLabel = {{ . | quote }}
  {{- end }}
{{- end }}
{{- if .Values.platformConnector.transformers.OverrideTransformer }}
overrides.toml: |
  {{- $overrideEnabled := false }}
  {{- range .Values.platformConnector.pipeline }}
    {{- if eq .name "OverrideTransformer" }}
      {{- $overrideEnabled = .enabled }}
    {{- end }}
  {{- end }}
  enabled = {{ $overrideEnabled }}
  {{- range .Values.platformConnector.transformers.OverrideTransformer.rules }}

  [[rules]]
  name = {{ .name | quote }}
  when = {{ .when | quote }}
  override = { {{- $pairs := list -}}
    {{- if hasKey .override "isFatal" -}}
      {{- $pairs = append $pairs (printf "isFatal = %v" .override.isFatal) -}}
    {{- end -}}
    {{- if hasKey .override "isHealthy" -}}
      {{- $pairs = append $pairs (printf "isHealthy = %v" .override.isHealthy) -}}
    {{- end -}}
    {{- if .override.recommendedAction -}}
      {{- $pairs = append $pairs (printf "recommendedAction = %q" .override.recommendedAction) -}}
    {{- end -}}
    {{- $pairs | join ", " -}}
  }
  {{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
nvsentinel.platformConnector.datastoreCertArgs: the datastore client
certificate flags both roles pass to the binary (commons
RegisterDatabaseCertFlags): the PostgreSQL client certificate directory, the
MongoDB one, or TLS explicitly off when the datastore has no certificate.
*/}}
{{- define "nvsentinel.platformConnector.datastoreCertArgs" -}}
{{- $mongoPC := include "nvsentinel.platformConnector.mongoCertMountPath" . -}}
{{- if and .Values.global.datastore (eq .Values.global.datastore.provider "postgresql") -}}
{{- if eq (include "nvsentinel.datastore.postgresClientCertEnabled" .) "true" -}}
- "--database-client-cert-mount-path={{ .Values.platformConnector.postgresqlStore.clientCertMountPath }}"
{{- else -}}
- "--database-client-cert-mount-path="
{{- end -}}
{{- else if $mongoPC -}}
- "--mongo-client-cert-mount-path={{ $mongoPC }}"
{{- else -}}
{{- /* Explicit TLS off when no certificate is mounted (e.g. Tilt with MongoDB TLS disabled). */ -}}
- "--tls-enabled=false"
{{- end -}}
{{- end }}
