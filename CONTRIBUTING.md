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

The sign-off is a simple signature at the end of the description for the patch.
Your signature certifies that you wrote the patch or otherwise have the right
to pass it on as an open-source patch.

The rules are pretty simple, and sign-off means that you certify the DCO below
(from [developercertificate.org](http://developercertificate.org/)):

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.
1 Letterman Drive
Suite D4700
San Francisco, CA, 94129

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.

Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

To sign off, you just add the following line to every git commit message:

```
    Signed-off-by: Joe Smith <joe.smith@email.com>
```

> Note: You must use your real name (sorry, no pseudonyms or anonymous contributions).

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
