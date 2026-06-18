#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# Auto-scaling setup for goboxd
#
# Two layers of scaling:
#   Layer 1 – ECS Service Auto Scaling (task count: 0 → 10)
#             Metric: CPUUtilization (scales tasks on busy containers)
#   Layer 2 – EC2 ASG (instance count: 0 → 10)
#             Managed automatically by ECS Capacity Provider (step 7 above)
#
# FUTURE: when you add SQS job queue, replace the CPU metric with
#         SQS ApproximateNumberOfMessagesVisible for more responsive scaling.
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

ACCOUNT="936344984906"
REGION="ap-south-1"
CLUSTER="goboxd-cluster"
SERVICE="goboxd-service"
RESOURCE="service/${CLUSTER}/${SERVICE}"

echo "▶ Step 1 — Register ECS service as an Application Auto Scaling target"
aws application-autoscaling register-scalable-target \
  --service-namespace ecs \
  --scalable-dimension ecs:service:DesiredCount \
  --resource-id       "$RESOURCE" \
  --min-capacity 0 \
  --max-capacity 10 \
  --region "$REGION"

echo "▶ Step 2 — Scale-OUT policy (CPU > 60%  →  add tasks)"
SCALE_OUT_ARN=$(aws application-autoscaling put-scaling-policy \
  --service-namespace ecs \
  --scalable-dimension ecs:service:DesiredCount \
  --resource-id       "$RESOURCE" \
  --policy-name       goboxd-scale-out \
  --policy-type       StepScaling \
  --step-scaling-policy-configuration '{
    "AdjustmentType":        "ChangeInCapacity",
    "Cooldown":              60,
    "MetricAggregationType": "Average",
    "StepAdjustments": [
      {"MetricIntervalLowerBound": 0,  "MetricIntervalUpperBound": 20, "ScalingAdjustment": 1},
      {"MetricIntervalLowerBound": 20, "MetricIntervalUpperBound": 40, "ScalingAdjustment": 2},
      {"MetricIntervalLowerBound": 40,                                 "ScalingAdjustment": 3}
    ]
  }' \
  --region "$REGION" \
  --query 'PolicyARN' --output text)
echo "  Scale-out policy ARN: $SCALE_OUT_ARN"

echo "▶ Step 3 — CloudWatch alarm → scale OUT (triggers the policy above)"
aws cloudwatch put-metric-alarm \
  --alarm-name        goboxd-cpu-high \
  --alarm-description "goboxd CPU above 60% - add tasks" \
  --namespace         AWS/ECS \
  --metric-name       CPUUtilization \
  --dimensions        Name=ClusterName,Value="$CLUSTER" Name=ServiceName,Value="$SERVICE" \
  --statistic         Average \
  --period            60 \
  --evaluation-periods 2 \
  --threshold         60 \
  --comparison-operator GreaterThanThreshold \
  --alarm-actions     "$SCALE_OUT_ARN" \
  --region "$REGION"

echo "▶ Step 4 — Scale-IN policy (CPU < 20%  →  remove tasks)"
SCALE_IN_ARN=$(aws application-autoscaling put-scaling-policy \
  --service-namespace ecs \
  --scalable-dimension ecs:service:DesiredCount \
  --resource-id       "$RESOURCE" \
  --policy-name       goboxd-scale-in \
  --policy-type       StepScaling \
  --step-scaling-policy-configuration '{
    "AdjustmentType":        "ChangeInCapacity",
    "Cooldown":              120,
    "MetricAggregationType": "Average",
    "StepAdjustments": [
      {"MetricIntervalUpperBound": 0, "ScalingAdjustment": -1}
    ]
  }' \
  --region "$REGION" \
  --query 'PolicyARN' --output text)

echo "▶ Step 5 — CloudWatch alarm → scale IN"
aws cloudwatch put-metric-alarm \
  --alarm-name        goboxd-cpu-low \
  --alarm-description "goboxd CPU below 20% - remove tasks" \
  --namespace         AWS/ECS \
  --metric-name       CPUUtilization \
  --dimensions        Name=ClusterName,Value="$CLUSTER" Name=ServiceName,Value="$SERVICE" \
  --statistic         Average \
  --period            300 \
  --evaluation-periods 3 \
  --threshold         20 \
  --comparison-operator LessThanThreshold \
  --alarm-actions     "$SCALE_IN_ARN" \
  --region "$REGION"

echo ""
echo "✅ Auto-scaling configured."
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "  LATER — SQS-based scaling (swap CPU for queue depth)"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "  When you add SQS integration, replace the CPU alarms above"
echo "  with this CloudWatch alarm:"
echo ""
echo "  aws cloudwatch put-metric-alarm \\"
echo "    --alarm-name goboxd-queue-deep \\"
echo "    --namespace AWS/SQS \\"
echo "    --metric-name ApproximateNumberOfMessagesVisible \\"
echo "    --dimensions Name=QueueName,Value=goboxd-jobs \\"
echo "    --threshold 5 \\"
echo "    --comparison-operator GreaterThanThreshold \\"
echo "    --alarm-actions <scale-out-policy-arn>"
echo ""
echo "  Scale in when queue is empty (threshold=0, LessThanOrEqualToThreshold)"
