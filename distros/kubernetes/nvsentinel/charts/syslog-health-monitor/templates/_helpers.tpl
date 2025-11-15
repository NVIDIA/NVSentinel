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
          resources:
            {{- toYaml $root.Values.resources | nindent 12 }}
          ports:
            - name: metrics
              containerPort: {{ $root.Values.global.metricsPort }}
          livenessProbe:
            httpGet:
              path: /metrics
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
            # Regular mode: Mount var/log directly
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
        {{- if $root.Values.xidSideCar.enabled }}
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
        # Regular mode: Direct var/log mount
        - name: var-log-vol
          hostPath:
            path: /var/log
            type: Directory
        {{- end }}
        - name: sys-vol
          hostPath:
            path: /sys
            type: Directory
        - name: proc-vol    
          hostPath:
            path: /proc
            type: Directory
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
