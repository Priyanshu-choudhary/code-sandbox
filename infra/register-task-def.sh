#!/usr/bin/env bash
set -euo pipefail

REGION="ap-south-1"
CLUSTER="goboxd-cluster"
SERVICE="goboxd-service"
ACCOUNT="936344984906"

echo "▶ Creating CloudWatch log group..."
aws logs create-log-group \
  --log-group-name /ecs/goboxd \
  --region "$REGION" 2>/dev/null || echo "  (already exists)"

aws logs put-retention-policy \
  --log-group-name /ecs/goboxd \
  --retention-in-days 30 \
  --region "$REGION"

echo "▶ Registering ECS task definition..."
TASK_ARN=$(aws ecs register-task-definition \
  --cli-input-json file://task-def.json \
  --region "$REGION" \
  --query 'taskDefinition.taskDefinitionArn' \
  --output text)
echo "  Registered: $TASK_ARN"

echo "▶ Creating ECS service..."
aws ecs create-service \
  --cluster        "$CLUSTER" \
  --service-name   "$SERVICE" \
  --task-definition goboxd \
  --desired-count  1 \
  --launch-type    EC2 \
  --deployment-configuration '{
    "deploymentCircuitBreaker": {"enable": true, "rollback": true},
    "maximumPercent":   200,
    "minimumHealthyPercent": 50
  }' \
  --region "$REGION" 2>/dev/null || \
  echo "  (service already exists — use 'aws ecs update-service' to update it)"

echo ""
echo "✅ Service created. Check status:"
echo "   aws ecs describe-services --cluster $CLUSTER --services $SERVICE --region $REGION"
