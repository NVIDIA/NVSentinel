# Contributing to NVSentinel

Thank you for your interest in contributing! We welcome contributions from the community.

## Getting Started

Before contributing:

1. Read the [README.md](README.md) to understand the project
2. Check existing [issues](https://github.com/NVIDIA/NVSentinel/issues) to avoid duplicates
3. Browse [discussions](https://github.com/NVIDIA/NVSentinel/discussions) for questions
4. Review the [security policy](SECURITY.md) for security-related contributions

## How to Contribute

Ways to contribute:

- 🐛 Report bugs via GitHub issues
- 💡 Suggest features through feature requests
- 📝 Improve documentation
- 🧪 Add tests to increase coverage
- 🔧 Fix issues with code contributions
- 💬 Help others in discussions

## Reporting Issues

When reporting issues:

1. Use the issue templates when available
2. Provide clear reproduction steps
3. Include environment details (OS, Kubernetes version, etc.)
4. Add relevant logs or error messages
5. Search existing issues first to avoid duplicates

## Submitting Pull Requests

1. Fork the repository and create a feature branch
2. Follow the coding standards and existing patterns
3. Write or update tests for your changes
4. Update documentation if needed
5. Sign your commits (see DCO section below)
6. Submit a pull request with a clear description

**Pull Request Guidelines**:
- Keep PRs focused on a single issue or feature
- Write clear, descriptive commit messages
- Include tests for new functionality
- Ensure all CI checks pass
- Be responsive to feedback and code review

## Community Guidelines

- Be respectful and inclusive in all interactions
- Follow the [Code of Conduct](https://docs.nvidia.com/cuda/eula/index.html)
- Help maintain a welcoming environment
- Focus on constructive feedback in reviews

## Development Setup

**Prerequisites**:
- Go 1.25+ (see `.versions.yaml` for exact version)
- Kubernetes cluster (for testing)
- Docker (for container builds)
- Make (for build targets)

**Quick Setup**:

1. Clone the repository:
   ```bash
   git clone https://github.com/NVIDIA/NVSentinel.git
   cd NVSentinel
   ```

2. Install dependencies:
   ```bash
   make dev-env-setup
   ```

3. Run tests:
   ```bash
   make test
   ```

4. Run linting:
   ```bash
   make lint
   ```

For detailed development instructions, see [DEVELOPMENT.md](DEVELOPMENT.md).

## Developer Certificate of Origin

All commits must be signed off to certify the Developer Certificate of Origin (DCO) from [developercertificate.org](http://developercertificate.org/).

**To sign off, add this line to every commit message**:

```
Signed-off-by: Joe Smith <joe.smith@email.com>
```

Use your real name (no pseudonyms or anonymous contributions).

**Automatic sign-off**:
```bash
git config user.name "Your Name"
git config user.email "your.email@example.com"
git commit -s  # Automatically adds sign-off
```

**DCO Summary**: By signing off, you certify that:
- (a) You created the contribution and have the right to submit it under the project's open source license
- (b) The contribution is based on previous work covered by an appropriate license
- (c) The contribution was provided to you by someone who certified (a) or (b)
- (d) You understand the contribution is public and will be maintained indefinitely
