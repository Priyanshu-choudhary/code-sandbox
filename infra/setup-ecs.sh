#!/usr/bin/env bash
# ─────────────────────────────────────────────────────────────────────────────
# One-time ECS EC2 infrastructure setup for goboxd
# Run from a machine that has AWS CLI configured (yadi_laptop_clint or equivalent)
# Region: ap-south-1   Account: 936344984906
# ─────────────────────────────────────────────────────────────────────────────
set -euo pipefail

ACCOUNT="936344984906"
REGION="ap-south-1"
CLUSTER="goboxd-cluster"
SERVICE="goboxd-service"
VPC="vpc-07d0920bd2c3a86cb"
SUBNETS="subnet-03a2c3924ebb31e0d,subnet-073d48106a18ec157"

echo "▶ Step 1 — EC2 instance role (ECS agent needs this to register)"
aws iam create-role \
  --role-name ecsInstanceRole-goboxd \
  --assume-role-policy-document '{
    "Version":"2012-10-17",
    "Statement":[{
      "Effect":"Allow",
      "Principal":{"Service":"ec2.amazonaws.com"},
      "Action":"sts:AssumeRole"
    }]
  }' 2>/dev/null || echo "  (role already exists)"

aws iam attach-role-policy \
  --role-name ecsInstanceRole-goboxd \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonEC2ContainerServiceforEC2Role

aws iam create-instance-profile \
  --instance-profile-name ecsInstanceProfile-goboxd 2>/dev/null || true
aws iam add-role-to-instance-profile \
  --instance-profile-name ecsInstanceProfile-goboxd \
  --role-name ecsInstanceRole-goboxd 2>/dev/null || true

echo "▶ Step 2 — Security group for goboxd EC2 instances"
SG_ID=$(aws ec2 create-security-group \
  --group-name goboxd-ec2-sg \
  --description "goboxd ECS EC2 instances" \
  --vpc-id "$VPC" \
  --query 'GroupId' --output text 2>/dev/null || \
  aws ec2 describe-security-groups \
    --filters "Name=group-name,Values=goboxd-ec2-sg" "Name=vpc-id,Values=$VPC" \
    --query 'SecurityGroups[0].GroupId' --output text)

# Allow outbound (compilers need to... nothing, actually – nsjail blocks network)
# Allow inbound from within VPC only (ALB or CFC backend calls it)
aws ec2 authorize-security-group-ingress \
  --group-id "$SG_ID" \
  --protocol tcp --port 8080 \
  --cidr 10.0.0.0/8 2>/dev/null || true

echo "  Security group: $SG_ID"

echo "▶ Step 3 — Fetch latest ECS-optimised Amazon Linux 2 AMI"
AMI_ID=$(aws ssm get-parameter \
  --name /aws/service/ecs/optimized-ami/amazon-linux-2/recommended/image_id \
  --region "$REGION" \
  --query 'Parameter.Value' --output text)
echo "  AMI: $AMI_ID"

echo "▶ Step 4 — EC2 Launch Template"
# User data registers the instance with the ECS cluster on boot
USER_DATA=$(cat <<EOF | base64 -w0
#!/bin/bash
echo ECS_CLUSTER=${CLUSTER} >> /etc/ecs/ecs.config
echo ECS_ENABLE_CONTAINER_METADATA=true >> /etc/ecs/ecs.config
EOF
)

LT_ID=$(aws ec2 create-launch-template \
  --launch-template-name goboxd-lt \
  --launch-template-data "{
    \"ImageId\":           \"${AMI_ID}\",
    \"InstanceType\":      \"t3.medium\",
    \"IamInstanceProfile\":{\"Name\":\"ecsInstanceProfile-goboxd\"},
    \"SecurityGroupIds\":  [\"${SG_ID}\"],
    \"UserData\":          \"${USER_DATA}\",
    \"BlockDeviceMappings\":[{
      \"DeviceName\":\"/dev/xvda\",
      \"Ebs\":{\"VolumeSize\":30,\"VolumeType\":\"gp3\",\"DeleteOnTermination\":true}
    }]
  }" \
  --query 'LaunchTemplate.LaunchTemplateId' --output text 2>/dev/null || \
  aws ec2 describe-launch-templates \
    --filters "Name=launch-template-name,Values=goboxd-lt" \
    --query 'LaunchTemplates[0].LaunchTemplateId' --output text)

echo "  Launch template: $LT_ID"

echo "▶ Step 5 — Auto Scaling Group (0 → 10 t3.medium instances)"
aws autoscaling create-auto-scaling-group \
  --auto-scaling-group-name goboxd-asg \
  --launch-template "LaunchTemplateId=${LT_ID},Version=\$Latest" \
  --min-size 0 \
  --max-size 10 \
  --desired-capacity 1 \
  --vpc-zone-identifier "${SUBNETS}" \
  --capacity-rebalance \
  --tags "Key=Name,Value=goboxd-ecs-ec2,PropagateAtLaunch=true" \
         "Key=Project,Value=code-sandbox,PropagateAtLaunch=true" \
  2>/dev/null || echo "  (ASG already exists)"

echo "▶ Step 6 — ECS Cluster"
aws ecs create-cluster \
  --cluster-name "$CLUSTER" \
  --settings name=containerInsights,value=enabled \
  2>/dev/null || echo "  (cluster already exists)"

echo "▶ Step 7 — ECS Capacity Provider (links ASG → cluster)"
aws ecs create-capacity-provider \
  --name goboxd-cap-provider \
  --auto-scaling-group-provider "{
    \"autoScalingGroupArn\": \"arn:aws:autoscaling:${REGION}:${ACCOUNT}:autoScalingGroup:*:autoScalingGroupName/goboxd-asg\",
    \"managedScaling\": {
      \"status\":              \"ENABLED\",
      \"targetCapacity\":      80,
      \"minimumScalingStepSize\": 1,
      \"maximumScalingStepSize\": 5
    },
    \"managedTerminationProtection\": \"DISABLED\"
  }" 2>/dev/null || echo "  (capacity provider already exists)"

aws ecs put-cluster-capacity-providers \
  --cluster "$CLUSTER" \
  --capacity-providers goboxd-cap-provider \
  --default-capacity-provider-strategy \
    capacityProvider=goboxd-cap-provider,weight=1,base=0

echo "▶ Step 8 — ECS Task Execution Role (same pattern as CFC backend)"
aws iam create-role \
  --role-name ecsTaskExecutionRole-goboxd \
  --assume-role-policy-document '{
    "Version":"2012-10-17",
    "Statement":[{
      "Effect":"Allow",
      "Principal":{"Service":"ecs-tasks.amazonaws.com"},
      "Action":"sts:AssumeRole"
    }]
  }' 2>/dev/null || echo "  (role already exists)"

aws iam attach-role-policy \
  --role-name ecsTaskExecutionRole-goboxd \
  --policy-arn arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy

echo ""
echo "✅ Infrastructure ready. Next: register task definition then create the service."
echo "   Run:  bash register-task-def.sh"
