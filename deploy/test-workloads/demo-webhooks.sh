#!/bin/bash
# Send test alerts to the bot webhook.
#
# Usage:
#   ./deploy/test-workloads/demo-webhooks.sh          # show menu
#   ./deploy/test-workloads/demo-webhooks.sh 1         # send OOMKilled alert
#   ./deploy/test-workloads/demo-webhooks.sh 3         # send ImagePullError alert
#   ./deploy/test-workloads/demo-webhooks.sh all       # send all alerts
#
# Prerequisites:
#   - Bot running with webhook.enabled: true (default :8080)

BOT_URL="${BOT_URL:-http://localhost:8080}"
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# Alert definitions: ID|NAME|SEVERITY|NAMESPACE|DEPLOYMENT|POD|CONTAINER|SUMMARY
ALERTS=(
  "1|ContainerOOMKilled|critical|demo-prod|checkout|checkout-6bbbc4d46f-sqsnh|checkout|Container checkout was terminated due to OOM. Check memory limits."
  "2|KubePodCrashLooping|critical|demo-staging|api-config-missing|api-config-missing-5d49d5cdf9-74hf7|api|Container api has been in CrashLoopBackOff for more than 1 minute."
  "3|KubePodImagePullError|warning|demo-infra|api-bad-image|api-bad-image-678d69b6bf-6zhck|api|Container api is stuck in ErrImagePull. Check image name and pull secrets."
  "4|KubePodCrashLooping|critical|demo-staging|api-dependency-fail|api-dependency-fail-657f8d6478-th79s|api|Container api has been in CrashLoopBackOff. Connection refused to dependency."
  "5|KubePodNotReady|warning|demo-prod|worker|worker-7649cd76c-dvnxb|worker|Pod has been in Pending phase for more than 2 minutes. Insufficient resources."
  "6|KubePodCrashLooping|warning|demo-infra|api-probe-fail|api-probe-fail-59d9c55d9f-hq5rk|api|Container killed by liveness probe failure."
  "7|KubePodNotReady|warning|demo-infra|ml-worker-taint|ml-worker-taint-b557dbc56-zpv9r|worker|Pod stuck in Pending. No node matches nodeSelector."
  "8|KubePodNotReady|warning|demo-infra|db-pvc-pending|db-pvc-pending-858956f4ff-g69n4|db|Pod stuck in Pending. PVC not bound."
  "9|KubeDeploymentReplicasMismatch|warning|demo-prod|payment|payment-bad-revision-xxx|payment|Deployment payment has mismatched replicas after rollout."
)

show_menu() {
  echo "╔══════════════════════════════════════════════════════════════╗"
  echo "║              lazy-diagnose-k8s — Demo Alerts                ║"
  echo "╠══════════════════════════════════════════════════════════════╣"
  echo "║                                                              ║"
  for alert in "${ALERTS[@]}"; do
    IFS='|' read -r id name severity ns deploy pod container summary <<< "$alert"
    printf "║  %s. %-28s %-10s %s\n" "$id" "$name" "[$severity]" "$ns/$deploy"
  done
  echo "║                                                              ║"
  echo "╠══════════════════════════════════════════════════════════════╣"
  echo "║  Usage:                                                      ║"
  echo "║    make demo-alerts NUM=1        Send OOMKilled alert        ║"
  echo "║    make demo-alerts NUM=3        Send ImagePullError         ║"
  echo "║    make demo-alerts NUM=all      Send all alerts             ║"
  echo "╚══════════════════════════════════════════════════════════════╝"
}

send_alert() {
  local alert="$1"
  IFS='|' read -r id name severity ns deploy pod container summary <<< "$alert"

  PAYLOAD=$(cat <<EOF
{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "$name"},
  "commonLabels": {"alertname": "$name", "namespace": "$ns"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "$name",
        "severity": "$severity",
        "namespace": "$ns",
        "pod": "$pod",
        "container": "$container",
        "deployment": "$deploy"
      },
      "annotations": {
        "summary": "$summary",
        "description": "$summary"
      },
      "startsAt": "$NOW",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "demo-$id-$name"
    }
  ]
}
EOF
)

  echo -n "  [$id] $name ($ns/$deploy)... "

  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "$BOT_URL/webhook/alertmanager" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD")

  if [ "$HTTP_CODE" = "200" ]; then
    echo "✅ sent"
  else
    echo "❌ HTTP $HTTP_CODE"
  fi
}

# Check bot health
if ! curl -sf "$BOT_URL/healthz" > /dev/null 2>&1; then
  echo "❌ Bot not reachable at $BOT_URL"
  echo "   Run: make run"
  exit 1
fi

# Parse argument
CHOICE="${1:-menu}"

case "$CHOICE" in
  menu|help|"")
    show_menu
    ;;
  all)
    echo "Sending all ${#ALERTS[@]} alerts..."
    echo ""
    for alert in "${ALERTS[@]}"; do
      send_alert "$alert"
      sleep 1
    done
    echo ""
    echo "Done. Check Telegram."
    ;;
  [1-9])
    IDX=$((CHOICE - 1))
    if [ $IDX -lt ${#ALERTS[@]} ]; then
      echo "Sending alert #$CHOICE..."
      echo ""
      send_alert "${ALERTS[$IDX]}"
      echo ""
      echo "Check Telegram."
    else
      echo "❌ Invalid number. Choose 1-${#ALERTS[@]}"
    fi
    ;;
  *)
    echo "❌ Invalid argument: $CHOICE"
    echo "   Use: 1-${#ALERTS[@]}, 'all', or no argument for menu"
    ;;
esac
