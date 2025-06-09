{{/*
Define the is-dcgm-reachable.sh script template
*/}}
{{- define "is-dcgm-reachable.sh.tpl" -}}
#!/bin/bash

DCGM_HOST="{{ .Values.global.dcgm.service.endpoint }}"
DCGM_PORT={{ .Values.global.dcgm.service.port }}
MAX_ATTEMPTS=60
INTERVAL=60

check_dcgm() {
    output=$(nc -zv $DCGM_HOST $DCGM_PORT 2>&1)
    if [ $? -eq 0 ]; then
    if echo "$output" | grep -q "open"; then
        echo "DCGM service is reachable"
        echo "Details: $output"
        return 0
    else
        echo "Unexpected output from netcat: $output"
        return 1
    fi
    else
    echo "Unable to connect to DCGM service"
    echo "Error: $output"
    return 1
    fi
}

check_local_dcgm() {
    output=$(nc -zv localhost $DCGM_PORT 2>&1)
    if [ $? -eq 0 ]; then
    if echo "$output" | grep -q "open"; then
        echo "DCGM service is reachable"
        echo "Details: $output"
        return 0
    else
        echo "Unexpected output from netcat: $output"
        return 1
    fi
    else
    echo "Unable to connect to DCGM service"
    echo "Error: $output"
    return 1
    fi
}

for attempt in $(seq 1 $MAX_ATTEMPTS); do
    echo "Attempt $attempt of $MAX_ATTEMPTS"
    {{- if .Values.global.gpuHealthMonitor.useHostNetworking }}
    if check_local_dcgm; then
        echo "DCGM service is reachable. Exiting successfully."
        exit 0
    fi
    {{- else }}
    if check_dcgm; then
        echo "DCGM service is reachable. Exiting successfully."
        exit 0
    fi
    {{- end }}
    if [ $attempt -lt $MAX_ATTEMPTS ]; then
    echo "Waiting $INTERVAL seconds before next attempt."
    sleep $INTERVAL
    fi
done

echo "Max attempts reached. DCGM service is not reachable."
exit 1
{{- end }} 