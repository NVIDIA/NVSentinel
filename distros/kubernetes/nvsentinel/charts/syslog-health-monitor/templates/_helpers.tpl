{{/*
Expand the name of the chart.
*/}}
{{- define "syslog-health-monitor.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "syslog-health-monitor.fullname" -}}
{{- "syslog-health-monitor" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "syslog-health-monitor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "syslog-health-monitor.labels" -}}
helm.sh/chart: {{ include "syslog-health-monitor.chart" . }}
{{ include "syslog-health-monitor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "syslog-health-monitor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "syslog-health-monitor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
DaemonSet template that can be customized for kata or regular mode.
Usage: include "syslog-health-monitor.daemonset" (dict "root" . "kataMode" true)
*/}}
{{- define "syslog-health-monitor.daemonset" }}
{{- $root := .root -}}
{{- $kataMode := .kataMode -}}
{{- $suffix := ternary "kata" "regular" $kataMode -}}
{{- $kataLabel := ternary "true" "false" $kataMode -}}
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: {{ include "syslog-health-monitor.fullname" $root }}-{{ $suffix }}
  labels:
    {{- include "syslog-health-monitor.labels" $root | nindent 4 }}
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 5%
  selector:
    matchLabels:
      {{- include "syslog-health-monitor.selectorLabels" $root | nindent 6 }}
      nvsentinel.dgxc.nvidia.com/kata: {{ $kataLabel | quote }}
  template:
    metadata:
      {{- with $root.Values.podAnnotations }}
      annotations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      labels:
        {{- include "syslog-health-monitor.selectorLabels" $root | nindent 8 }}
        nvsentinel.dgxc.nvidia.com/kata: {{ $kataLabel | quote }}
    spec:
      {{- with $root.Values.global.imagePullSecrets }}
      imagePullSecrets:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- if and $root.Values.xidSideCar.enabled (semverCompare ">=1.29-0" $root.Capabilities.KubeVersion.Version) }}
      initContainers:
        - name: xid-analyzer-sidecar
          restartPolicy: Always
          image: {{ $root.Values.xidSideCar.image.repository }}:{{ $root.Values.xidSideCar.image.tag }}
          imagePullPolicy: {{ $root.Values.xidSideCar.image.pullPolicy }}
          ports:
            - name: http-api
              containerPort: 8080
              protocol: TCP
          startupProbe:
            tcpSocket:
              port: 8080
            initialDelaySeconds: 1
            periodSeconds: 2
            failureThreshold: 15
          resources:
            requests:
              memory: "256Mi"
              cpu: "100m"
            limits:
              memory: "512Mi"
              cpu: "500m"
          env:
            - name: PORT
              value: "8080"
      {{- end }}
      containers:
        - name: syslog-health-monitor
          securityContext:
            runAsUser: 0
            capabilities:
              add: ["SYSLOG", "SYS_ADMIN"]
          image: "{{ $root.Values.image.repository }}:{{ $root.Values.image.tag | default (($root.Values.global).image).tag | default $root.Chart.AppVersion }}"
          imagePullPolicy: {{ $root.Values.image.pullPolicy }}
          args:
            - "--polling-interval"
            - "15s"
            - "--metrics-port"
            - "{{ $root.Values.global.metricsPort }}"
            - "--kata-enabled"
            - {{ $kataLabel | quote }}
            {{- if $root.Values.xidSideCar.enabled }}
            - "--xid-analyser-endpoint"
            - "http://localhost:8080"
            {{- end }}
            - "--checks"
            - "{{ join "," $root.Values.enabledChecks }}"
            - "--metadata-path"
            - "{{ $root.Values.global.metadataPath }}"
            - "--processing-strategy"
            - "{{ $root.Values.processingStrategy }}"
            - "--nic-driver-config"
            - "/etc/syslog-health-monitor/nic-driver.toml"
            - "--sysfs-root"
            - "/nvsentinel/sys"
            - "--boot-lookback-window"
            {{- if hasKey $root.Values "bootLookbackWindow" }}
            - {{ $root.Values.bootLookbackWindow | quote }}
            {{- else }}
            - "2h"
            {{- end }}
            - "--cancellations-config"
            - "/etc/syslog-health-monitor/cancellations.toml"
            {{- if include "syslog-health-monitor.pcAuth.enabled" $root }}
            {{- /* Node-local monitor: it reports only its own node, and the token
                 lets platform-connector confirm that placement. */}}
            - "--platform-connector-token-path={{ include "syslog-health-monitor.pcAuth.tokenPath" $root }}"
            {{- end }}
          resources:
            {{- toYaml $root.Values.resources | nindent 12 }}
          ports:
            - name: metrics
              containerPort: {{ $root.Values.global.metricsPort }}
          livenessProbe:
            httpGet:
              path: /healthz
              port: {{ $root.Values.global.metricsPort }}
            initialDelaySeconds: 30
            periodSeconds: 30
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /metrics
              port: {{ $root.Values.global.metricsPort }}
            initialDelaySeconds: 10
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          env: 
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  apiVersion: v1
                  fieldPath: spec.nodeName
            - name: LOG_LEVEL
              value: "{{ $root.Values.logLevel }}"
          volumeMounts:
            - name: var-run-vol
              mountPath: /var/run/
            - name: syslog-state-vol
              mountPath: /var/run/syslog_health_monitor
            - name: metadata-vol
              mountPath: /var/lib/nvsentinel
              readOnly: true
            {{- if $kataMode }}
            # Kata mode: Mount systemd journal for accessing host logs
            - name: host-journal
              mountPath: /nvsentinel/var/log/journal
              readOnly: true
            - name: host-systemd
              mountPath: /run/systemd/journal
              readOnly: true
            - name: host-machine-id
              mountPath: /etc/machine-id
              readOnly: true
            {{- else }}
            # Regular mode: Mount journal from user-defined host path
            - name: var-log-vol
              mountPath: /nvsentinel/var/log
              readOnly: true
            {{- end }}
            - name: proc-vol
              mountPath: /nvsentinel/proc
              readOnly: true
            - name: sys-vol
              mountPath: /nvsentinel/sys
              readOnly: true
            - name: syslog-health-monitor-config
              mountPath: /etc/syslog-health-monitor
              readOnly: true
            {{- if include "syslog-health-monitor.pcAuth.enabled" $root }}
            {{- include "syslog-health-monitor.pcAuth.volumeMount" $root | nindent 12 }}
            {{- end }}
        {{- if and $root.Values.xidSideCar.enabled (not (semverCompare ">=1.29-0" $root.Capabilities.KubeVersion.Version)) }}
        - name: xid-analyzer-sidecar
          image: {{ $root.Values.xidSideCar.image.repository }}:{{ $root.Values.xidSideCar.image.tag }}
          imagePullPolicy: {{ $root.Values.xidSideCar.image.pullPolicy }}
          ports:
            - name: http-api
              containerPort: 8080
              protocol: TCP
          resources:
            requests:
              memory: "256Mi"
              cpu: "100m"
            limits:
              memory: "512Mi"
              cpu: "500m"
          env:
            - name: PORT
              value: "8080"
        {{- end }}
      volumes:
        - name: var-run-vol
          hostPath:
            path: /var/run/nvsentinel
            type: DirectoryOrCreate
        - name: syslog-state-vol
          hostPath:
            path: /var/run/syslog_health_monitor
            type: DirectoryOrCreate
        - name: metadata-vol
          hostPath:
            path: /var/lib/nvsentinel
            type: DirectoryOrCreate
        {{- if $kataMode }}
        # Kata mode: Systemd journal volumes for host log access
        - name: host-journal
          hostPath:
            path: /var/log/journal
            type: Directory
        - name: host-systemd
          hostPath:
            path: /run/systemd/journal
            type: Directory
        - name: host-machine-id
          hostPath:
            path: /etc/machine-id
            type: File
        {{- else }}
        # Regular mode: Mount journal from user-defined host path
        - name: var-log-vol
          hostPath:
            path: {{ $root.Values.journalHostPath }}
            type: Directory
        {{- end }}
        - name: sys-vol
          hostPath:
            path: /sys
            type: Directory
        - name: syslog-health-monitor-config
          configMap:
            name: {{ include "syslog-health-monitor.fullname" $root }}-config
        - name: proc-vol
          hostPath:
            path: /proc
            type: Directory
        {{- if include "syslog-health-monitor.pcAuth.enabled" $root }}
        {{- include "syslog-health-monitor.pcAuth.volume" $root | nindent 8 }}
        {{- end }}
      nodeSelector:
        nvsentinel.dgxc.nvidia.com/driver.installed: "true"
        nvsentinel.dgxc.nvidia.com/kata.enabled: {{ $kataLabel | quote }}
        {{- with ($root.Values.global.nodeSelector | default $root.Values.nodeSelector) }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with ($root.Values.global.affinity | default $root.Values.affinity) }}
      affinity:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      {{- with ($root.Values.global.tolerations | default $root.Values.tolerations) }}
      tolerations:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end -}}

{{/*
Chart-local copies of the umbrella chart's nvsentinel.pcAuth.* helpers, so this
chart still renders on its own (`helm template` of this directory alone loads
the subchart without its parent, which `make helm-lint` does for every chart).

They are deliberately named syslog-health-monitor.* rather than nvsentinel.*:
Helm template names are GLOBAL across the whole chart tree, and a subchart
definition wins over the parent's, so reusing the nvsentinel.* names here would
silently override the umbrella's helpers for every other chart — making later
edits to nvsentinel/templates/_helpers.tpl take no effect at all.

They read the same global values as the umbrella versions AND must apply the
same strictness. An earlier copy skipped the boolean check here, so a quoted
"false" was refused by the umbrella but silently ENABLED token injection when
this chart was rendered standalone. Keep the checks identical to that file
until the shared helpers move into a proper Helm library chart; the chart test
in tests/pc_auth_strictness_test.yaml is what catches the two drifting again.
*/}}
{{- define "syslog-health-monitor.pcAuth.enabled" -}}
{{- $auth := ((.Values.global).platformConnectorAuth) | default dict -}}
{{- $enabled := $auth.enabled -}}
{{- if not (kindIs "bool" $enabled) -}}
{{- fail (printf "global.platformConnectorAuth.enabled must be a boolean (true or false), got %s %#v. Quoted strings, null and numbers are refused because they would silently enable or disable authentication." (kindOf $enabled) $enabled) -}}
{{- end -}}
{{- if $enabled -}}true{{- end -}}
{{- end -}}

{{- define "syslog-health-monitor.pcAuth.mountPath" -}}
{{- required "global.platformConnectorAuth.tokenMountPath is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).tokenMountPath) -}}
{{- end -}}

{{- define "syslog-health-monitor.pcAuth.tokenPath" -}}
{{- printf "%s/token" (include "syslog-health-monitor.pcAuth.mountPath" .) -}}
{{- end -}}

{{- define "syslog-health-monitor.pcAuth.volume" -}}
- name: platform-connector-token
  projected:
    sources:
      - serviceAccountToken:
          audience: {{ required "global.platformConnectorAuth.audience is required when platform-connector auth is enabled" (((.Values.global).platformConnectorAuth).audience) | quote }}
          expirationSeconds: {{ include "syslog-health-monitor.pcAuth.expirationSeconds" . }}
          path: token
{{- end -}}

{{- define "syslog-health-monitor.pcAuth.volumeMount" -}}
- name: platform-connector-token
  mountPath: {{ include "syslog-health-monitor.pcAuth.mountPath" . }}
  readOnly: true
{{- end -}}

{{- define "syslog-health-monitor.pcAuth.expirationSeconds" -}}
{{- $v := (((.Values.global).platformConnectorAuth)).tokenExpirationSeconds -}}
{{- if kindIs "invalid" $v -}}
{{- fail "global.platformConnectorAuth.tokenExpirationSeconds is required when platform-connector auth is enabled" -}}
{{- end -}}
{{- if not (or (kindIs "float64" $v) (kindIs "int" $v) (kindIs "int64" $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be an integer, got %s %#v." (kindOf $v) $v) -}}
{{- end -}}
{{- /*
YAML numbers reach templates as float64, so a fractional value passes a bare
numeric check and then renders into an integer Kubernetes field, which the API
server rejects when the pod is created.
*/ -}}
{{- if ne (float64 $v) (floor (float64 $v)) -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds must be a whole number of seconds, got %v." $v) -}}
{{- end -}}
{{- /*
Kubernetes rejects a projected ServiceAccount token lifetime below 10 minutes or
above 2^32 seconds (core validation, volume projection). Out-of-range values
render fine and are then refused by the API server when the pod is created, so
the workload never starts and the reason is a long way from the values file.
*/ -}}
{{- if lt (float64 $v) 600.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime under 600 seconds (10 minutes)." $v) -}}
{{- end -}}
{{- if gt (float64 $v) 4294967296.0 -}}
{{- fail (printf "global.platformConnectorAuth.tokenExpirationSeconds is %v, but Kubernetes rejects a projected token lifetime over 2^32 seconds." $v) -}}
{{- end -}}
{{- int64 $v -}}
{{- end -}}
