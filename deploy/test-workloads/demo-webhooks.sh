#!/bin/bash
# Send a random firing alert from Alertmanager to the bot webhook.
# If no alerts are firing, prints a message and exits.
#
# Usage:
#   ./deploy/test-workloads/demo-webhooks.sh          # random alert
#   ./deploy/test-workloads/demo-webhooks.sh --all     # all firing alerts
#
# Prerequisites:
#   - Bot running with webhook.enabled: true (default :8080)
#   - Alertmanager port-forwarded: kubectl -n monitoring port-forward svc/alertmanager 9093:9093

BOT_URL="${BOT_URL:-http://localhost:8080}"
AM_URL="${AM_URL:-http://localhost:9093}"

# Check bot health
if ! curl -sf "$BOT_URL/healthz" > /dev/null 2>&1; then
  echo "❌ Bot not reachable at $BOT_URL/healthz"
  echo "   Make sure bot is running with: make run"
  exit 1
fi

# Fetch firing alerts from Alertmanager
ALERTS_JSON=$(curl -sf "$AM_URL/api/v2/alerts?silenced=false&inhibited=false&active=true" 2>/dev/null)
if [ $? -ne 0 ] || [ -z "$ALERTS_JSON" ]; then
  echo "❌ Can't reach Alertmanager at $AM_URL"
  echo "   Port-forward first: kubectl -n monitoring port-forward svc/alertmanager 9093:9093 &"
  exit 1
fi

# Filter firing alerts only
FIRING=$(echo "$ALERTS_JSON" | jq '[.[] | select(.status.state == "active")]')
COUNT=$(echo "$FIRING" | jq 'length')

if [ "$COUNT" = "0" ] || [ "$COUNT" = "null" ]; then
  echo "✅ No firing alerts. Cluster looks healthy!"
  exit 0
fi

echo "🔥 $COUNT firing alert(s) in Alertmanager"
echo ""

if [ "$1" = "--all" ]; then
  # Send all firing alerts
  SELECTED="$FIRING"
  echo "Sending all $COUNT alerts..."
else
  # Pick one random alert
  IDX=$((RANDOM % COUNT))
  SELECTED=$(echo "$FIRING" | jq ".[$IDX:$IDX+1]")
  ALERT_NAME=$(echo "$SELECTED" | jq -r '.[0].labels.alertname')
  POD=$(echo "$SELECTED" | jq -r '.[0].labels.pod // "n/a"')
  NS=$(echo "$SELECTED" | jq -r '.[0].labels.namespace // "n/a"')
  echo "Picked: $ALERT_NAME (ns=$NS, pod=$POD)"
fi

# Build Alertmanager webhook payload
PAYLOAD=$(echo "$SELECTED" | jq '{
  version: "4",
  status: "firing",
  receiver: "lazy-diagnose",
  groupLabels: {alertname: .[0].labels.alertname},
  commonLabels: .[0].labels,
  commonAnnotations: (.[0].annotations // {}),
  alerts: [.[] | {
    status: "firing",
    labels: .labels,
    annotations: (.annotations // {}),
    startsAt: .startsAt,
    endsAt: .endsAt,
    generatorURL: (.generatorURL // ""),
    fingerprint: .fingerprint
  }]
}')

# Send to bot
echo ""
HTTP_CODE=$(curl -s -o /tmp/demo-webhook-response.txt -w "%{http_code}" \
  -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD")

if [ "$HTTP_CODE" = "200" ]; then
  echo "✅ Sent to bot → check Telegram"
else
  echo "❌ Bot returned HTTP $HTTP_CODE"
  cat /tmp/demo-webhook-response.txt
fi
