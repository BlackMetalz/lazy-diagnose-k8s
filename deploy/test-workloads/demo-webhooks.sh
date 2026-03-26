#!/bin/bash
# Demo webhook requests to test Alertmanager → Bot integration
# Usage: ./deploy/test-workloads/demo-webhooks.sh
#
# Prerequisites: bot must be running with webhook.enabled: true (default :8080)

BOT_URL="${BOT_URL:-http://localhost:8080}"

echo "=== Sending demo alerts to $BOT_URL/webhook/alertmanager ==="
echo ""

# 1. CrashLoopBackOff alert (critical)
echo "1. KubePodCrashLooping — checkout OOMKilled"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "KubePodCrashLooping"},
  "commonLabels": {"alertname": "KubePodCrashLooping", "namespace": "prod"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "KubePodCrashLooping",
        "severity": "critical",
        "namespace": "prod",
        "pod": "checkout-6bbbc4d46f-n4nzd",
        "container": "checkout",
        "deployment": "checkout"
      },
      "annotations": {
        "summary": "Pod prod/checkout-6bbbc4d46f-n4nzd is CrashLooping",
        "description": "Container checkout in pod checkout-6bbbc4d46f-n4nzd has been in CrashLoopBackOff for more than 1 minute."
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "abc123-crashloop"
    }
  ]
}'
echo ""
echo ""
sleep 2

# 2. Pod Pending alert (warning)
echo "2. KubePodNotReady — worker Pending"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "KubePodNotReady"},
  "commonLabels": {"alertname": "KubePodNotReady", "namespace": "prod"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "KubePodNotReady",
        "severity": "warning",
        "namespace": "prod",
        "pod": "worker-7649cd76c-9t7dw",
        "deployment": "worker",
        "phase": "Pending"
      },
      "annotations": {
        "summary": "Pod prod/worker-7649cd76c-9t7dw is not ready",
        "description": "Pod worker-7649cd76c-9t7dw has been in Pending phase for more than 2 minutes."
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "def456-pending"
    }
  ]
}'
echo ""
echo ""
sleep 2

# 3. OOMKilled alert (critical)
echo "3. ContainerOOMKilled — checkout"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "ContainerOOMKilled"},
  "commonLabels": {"alertname": "ContainerOOMKilled", "namespace": "prod"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "ContainerOOMKilled",
        "severity": "critical",
        "namespace": "prod",
        "pod": "checkout-6bbbc4d46f-n4nzd",
        "container": "checkout",
        "deployment": "checkout"
      },
      "annotations": {
        "summary": "Container checkout in prod/checkout-6bbbc4d46f-n4nzd was OOMKilled",
        "description": "Container checkout was terminated due to OOM. Check memory limits."
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "ghi789-oom"
    }
  ]
}'
echo ""
echo ""
sleep 2

# 4. ImagePullBackOff alert (warning)
echo "4. KubePodImagePullError — api-bad-image"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "KubePodImagePullError"},
  "commonLabels": {"alertname": "KubePodImagePullError", "namespace": "prod"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "KubePodImagePullError",
        "severity": "warning",
        "namespace": "prod",
        "pod": "api-bad-image-678d69b6bf-6zhck",
        "container": "api",
        "deployment": "api-bad-image"
      },
      "annotations": {
        "summary": "Pod prod/api-bad-image-678d69b6bf-6zhck has image pull errors",
        "description": "Container api is stuck in ImagePullBackOff. Check image name and pull secrets."
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "jkl012-imagepull"
    }
  ]
}'
echo ""
echo ""
sleep 2

# 5. Deployment replicas mismatch (rollout regression)
echo "5. KubeDeploymentReplicasMismatch — payment"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "firing",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "KubeDeploymentReplicasMismatch"},
  "commonLabels": {"alertname": "KubeDeploymentReplicasMismatch", "namespace": "prod"},
  "alerts": [
    {
      "status": "firing",
      "labels": {
        "alertname": "KubeDeploymentReplicasMismatch",
        "severity": "warning",
        "namespace": "prod",
        "deployment": "payment"
      },
      "annotations": {
        "summary": "Deployment prod/payment has mismatched replicas",
        "description": "Deployment payment has desired replicas but not all are ready after recent rollout."
      },
      "startsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "mno345-replicas"
    }
  ]
}'
echo ""
echo ""
sleep 2

# 6. Resolved alert (should NOT trigger diagnosis)
echo "6. Resolved alert — should be ignored"
curl -s -X POST "$BOT_URL/webhook/alertmanager" \
  -H "Content-Type: application/json" \
  -d '{
  "version": "4",
  "status": "resolved",
  "receiver": "lazy-diagnose",
  "groupLabels": {"alertname": "KubePodCrashLooping"},
  "commonLabels": {"alertname": "KubePodCrashLooping", "namespace": "prod"},
  "alerts": [
    {
      "status": "resolved",
      "labels": {
        "alertname": "KubePodCrashLooping",
        "severity": "critical",
        "namespace": "prod",
        "pod": "checkout-6bbbc4d46f-n4nzd",
        "deployment": "checkout"
      },
      "annotations": {
        "summary": "Pod is no longer CrashLooping"
      },
      "startsAt": "'$(date -u -v-10M +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "10 minutes ago" +%Y-%m-%dT%H:%M:%SZ)'",
      "endsAt": "'$(date -u +%Y-%m-%dT%H:%M:%SZ)'",
      "generatorURL": "http://vmalert:8880/alerts",
      "fingerprint": "abc123-crashloop"
    }
  ]
}'
echo ""
echo ""

# 7. Single alert test (quick)
echo "=== Quick single alert test ==="
echo "curl -s -X POST $BOT_URL/webhook/alertmanager -H 'Content-Type: application/json' -d @-"
echo ""
echo "Usage examples:"
echo "  # Send all demo alerts:"
echo "  ./deploy/test-workloads/demo-webhooks.sh"
echo ""
echo "  # Send single alert:"
echo '  curl -X POST http://localhost:8080/webhook/alertmanager -H "Content-Type: application/json" -d '"'"'{"version":"4","status":"firing","alerts":[{"status":"firing","labels":{"alertname":"KubePodCrashLooping","severity":"critical","namespace":"prod","deployment":"checkout"},"annotations":{"summary":"checkout is crashing"}}]}'"'"''
echo ""
echo "  # Health check:"
echo "  curl http://localhost:8080/healthz"
echo ""
echo "=== Done ==="
