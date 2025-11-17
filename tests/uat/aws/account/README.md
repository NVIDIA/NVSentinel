# AWS Account OIDC Setup for GitHub Actions

This Terraform configuration sets up AWS IAM OIDC federation for GitHub Actions, enabling secure authentication without long-lived credentials.

## Prerequisites

- [Terraform](https://www.terraform.io/downloads.html) >= 1.13
- [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2.html) configured with appropriate credentials
- AWS account with necessary permissions to create IAM roles, policies, and OIDC providers

## What This Creates

- **OIDC Identity Provider**: Configures GitHub as a trusted identity provider
- **IAM Role**: `github-actions-role` with permissions for EKS cluster management
- **IAM Policy**: Comprehensive permissions for EKS, EC2, IAM, and related services
- **Trust Relationship**: Allows GitHub Actions from the specified repository to assume the role

## Usage

1. **Initialize Terraform:**
   ```bash
   terraform init
   ```

2. **Configure variables (optional):**
   ```bash
   cp terraform.tfvars.example terraform.tfvars
   # Edit terraform.tfvars with your values
   ```

3. **Preview changes:**
   ```bash
   terraform plan
   ```

4. **Create the resources:**
   ```bash
   terraform apply
   ```

5. **Note the outputs** for use in GitHub Actions:
   ```bash
   terraform output
   ```

## Configuration Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `aws_region` | AWS region for resources | `us-west-2` |
| `git_repo` | GitHub repository (format: owner/repo) | `NVIDIA/NVSentinel` |
| `github_actions_role_name` | Name for the IAM role | `github-actions-role` |
| `oidc_provider_url` | GitHub OIDC provider URL | `https://token.actions.githubusercontent.com` |
| `oidc_audience` | OIDC audience | `sts.amazonaws.com` |

## Permissions Granted

The GitHub Actions role includes permissions for:

- **EKS**: Full cluster and node group management
- **EC2**: VPC, subnet, security group, and instance management
- **IAM**: Role and instance profile management for EKS service roles
- **Auto Scaling**: For EKS managed node groups
- **Elastic Load Balancing**: For Kubernetes services
- **CloudFormation**: EKS uses CloudFormation internally

## GitHub Actions Integration

After running this Terraform, use the outputs in your GitHub Actions workflow:

```yaml
permissions:
  id-token: write
  contents: read

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - name: Configure AWS credentials
        uses: aws-actions/configure-aws-credentials@v4
        with:
          role-to-assume: ${{ secrets.AWS_ROLE_ARN }}  # From terraform output
          aws-region: ${{ secrets.AWS_REGION }}        # From terraform output
          role-session-name: GitHubActions
```

## Security Features

- **No long-lived credentials**: Uses OIDC for temporary credentials
- **Repository restriction**: Only allows access from the specified GitHub repository
- **Branch restrictions**: Configured for main branch and any branch/PR from the repo
- **Least privilege**: Permissions scoped to EKS and related services only

## Trust Policy Details

The trust policy allows:
- GitHub Actions from the specified repository
- Main branch access
- All branches and pull requests from the repository

## Cleanup

To remove all resources:
```bash
terraform destroy
```

## Troubleshooting

### Common Issues

1. **OIDC Provider already exists**: If you get an error about the OIDC provider already existing, you can import it:
   ```bash
   terraform import aws_iam_openid_connect_provider.github arn:aws:iam::ACCOUNT-ID:oidc-provider/token.actions.githubusercontent.com
   ```

2. **Permission denied**: Ensure your AWS credentials have permissions to create IAM resources

3. **Invalid thumbprint**: The GitHub OIDC thumbprints are included, but if they change, update the `thumbprint_list` in `federation.tf`

## References

- [AWS IAM OIDC Documentation](https://docs.aws.amazon.com/IAM/latest/UserGuide/id_roles_providers_create_oidc.html)
- [GitHub Actions OIDC](https://docs.github.com/en/actions/deployment/security-hardening-your-deployments/about-security-hardening-with-openid-connect)
- [AWS Actions for GitHub](https://github.com/aws-actions/configure-aws-credentials)