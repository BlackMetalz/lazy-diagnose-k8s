#!/bin/bash
# Generates alertmanager-config ConfigMap with the correct host IP.
# Usage: ./gen-alertmanager-config.sh <HOST_IP> | kubectl apply -f -
# Optional: ./gen-alertmanager-config.sh <HOST_IP> <KUBECTL_CONTEXT>

HOST_IP="${1:?Usage: $0 <HOST_IP> [kubectl-context]}"
CONTEXT="${2:-}"

cat <<EOF | kubectl ${CONTEXT:+--context "$CONTEXT"} apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: alertmanager-config
  namespace: monitoring
data:
  alertmanager.yml: |
    global:
      resolve_timeout: 5m
    route:
      receiver: lazy-diagnose
      group_by: [alertname, namespace, pod]
      group_wait: 15s
      group_interval: 1m
      repeat_interval: 4h
    receivers:
      - name: lazy-diagnose
        webhook_configs:
          - url: "http://${HOST_IP}:8080/webhook/alertmanager"
            send_resolved: true
EOF
