#
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# GitHub OIDC Identity Provider
resource "aws_iam_openid_connect_provider" "github" {
  url = var.oidc_provider_url

  client_id_list = [
    var.oidc_audience,
  ]

  thumbprint_list = [
    "6938fd4d98bab03faadb97b34396831e3780aea1", # GitHub Actions OIDC thumbprint
    "1c58a3a8518e8759bf075b76b750d4f2df264fcd"  # Backup thumbprint
  ]

  tags = {
    Name        = "github-actions-oidc-provider"
    Environment = "ci"
    ManagedBy   = "terraform"
  }
}

# Trust policy for GitHub Actions
data "aws_iam_policy_document" "github_actions_assume_role_policy" {
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    effect  = "Allow"

    condition {
      test     = "StringEquals"
      variable = "${replace(var.oidc_provider_url, "https://", "")}:aud"
      values   = [var.oidc_audience]
    }

    condition {
      test     = "StringLike"
      variable = "${replace(var.oidc_provider_url, "https://", "")}:sub"
      values   = [
        "repo:${var.git_repo}:*"
      ]
    }

    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
  }
}

# IAM Role for GitHub Actions
resource "aws_iam_role" "github_actions" {
  name               = var.github_actions_role_name
  assume_role_policy = data.aws_iam_policy_document.github_actions_assume_role_policy.json

  tags = {
    Name        = "github-actions-role"
    Environment = "ci"
    ManagedBy   = "terraform"
    Repository  = var.git_repo
  }
}

# IAM Policy for EKS and EC2 permissions
data "aws_iam_policy_document" "github_actions_permissions" {
  # STS permissions
  statement {
    sid    = "STSPermissions"
    effect = "Allow"
    actions = ["sts:*"]
    resources = ["*"]
  }

  # IAM permissions for EKS service roles
  statement {
    sid    = "IAMPermissions"
    effect = "Allow"
    actions = ["iam:*"]
    resources = ["*"]
  }

  # SSM permissions for EKS nodes
  statement {
    sid    = "SSMNodePermissions"
    effect = "Allow"
    actions = ["ssm:*"]
    resources = ["*"]
  }

  # EKS Cluster permissions
  statement {
    sid    = "EKSClusterPermissions"
    effect = "Allow"
    actions = [
      "eks:*"
    ]
    resources = ["*"]
  }

  # EC2 permissions for EKS
  statement {
    sid    = "EC2Permissions"
    effect = "Allow"
    actions = ["ec2:*"]
    resources = ["*"]
  }

  # CloudFormation permissions (EKS uses CloudFormation)
  statement {
    sid    = "CloudFormationPermissions"
    effect = "Allow"
    actions = ["cloudformation:*"]
    resources = ["*"]
  }

  # Auto Scaling permissions for EKS node groups
  statement {
    sid    = "AutoScalingPermissions"
    effect = "Allow"
    actions = ["autoscaling:*"]
    resources = ["*"]
  }
}

# IAM Policy for GitHub Actions
resource "aws_iam_policy" "github_actions" {
  name        = "${var.github_actions_role_name}-policy"
  description = "Policy for GitHub Actions to manage EKS clusters"
  policy      = data.aws_iam_policy_document.github_actions_permissions.json

  tags = {
    Name        = "${var.github_actions_role_name}-policy"
    Environment = "ci"
    ManagedBy   = "terraform"
  }
}

# Attach policy to role
resource "aws_iam_role_policy_attachment" "github_actions" {
  policy_arn = aws_iam_policy.github_actions.arn
  role       = aws_iam_role.github_actions.name
}