# 🚀 NVSentinel Development Guide

Welcome to the NVSentinel development guide! This document will walk you through the complete development workflow from start to finish. We've designed this process to ensure high-quality code, thorough testing, and smooth collaboration across the team.

## 📋 Quick Overview

NVSentinel is a GPU node resilience system that monitors and automatically remediates hardware and software faults in Kubernetes GPU clusters. As a developer, you'll be working with a modular microservices architecture that includes health monitors, core processing modules, and integration components.

---

## 🎯 Development Workflow

### 1. 🎫 Start with a JIRA Ticket

Before writing any code, ensure you have a JIRA ticket in the **KACE project**. This helps us track work, understand requirements, and maintain proper project management.

**What to check:**
- ✅ Ticket is well-defined with clear acceptance criteria
- ✅ Requirements are understood and scope is clear
- ✅ Any dependencies or blockers are identified

**💡 Important:** If the ticket lacks details or clarity, reach out directly to the **ticket requestor** for more information. Don't assume requirements - get clear specifications before starting development!

### 2. 🌿 Create Your Feature Branch

Create a new branch following our naming convention:

```bash
git checkout main
git pull origin main
git checkout -b <your-username>/<jira-ticket>-<short-description>
```

**Example:**
```bash
git checkout -b johnsmith/KACE-1234-gpu-health-monitor-improvements
```

**Branch naming best practices:**
- Keep the description short but descriptive
- Use kebab-case for the description
- Include the JIRA ticket number for easy traceability

### 3. 💻 Make Your Code Changes

Now for the fun part - coding! Here are some guidelines to keep in mind:

**Code Quality:**
- Follow Go best practices and conventions
- Keep functions focused and testable
- Add clear comments for complex logic
- Ensure your code is readable and maintainable

**Architecture Considerations:**
- NVSentinel uses a modular architecture with gRPC communication
- Health monitors are independent and communicate via the platform connector
- Core modules watch MongoDB change streams for event-driven processing
- Follow the existing patterns in the codebase

### 4. 🔍 Run Lint and Tests Locally

Before pushing any code, make sure everything passes the same linting checks used in our CI pipeline:

**For Go modules:**
```bash
# Run the same linters used in CI
go vet ./...
golangci-lint run

# Run tests
go test ./...
```

**For Python modules (like GPU Health Monitor):**
```bash
# Install dependencies and run formatting/linting
cd health-monitors/gpu-health-monitor
poetry install
poetry run black --check .  # Code formatting check
poetry run black .          # Auto-format code

# Run tests
poetry run pytest
```

**Our CI uses these specific linters:**
- **Go**: golangci-lint with 25+ enabled rules including errcheck, gosimple, govet, staticcheck, gosec, gofmt, goimports, and many others
- **Python**: Black code formatter with 120 character line length

**💡 Pro tip:** Set up your IDE to run these automatically on save!

### 5. 🐳 Build and Test Containers Locally

Verify that your changes work in a containerized environment:

```bash
# Build the container for your module
cd <your-module-directory>
docker build -t nvsentinel-<module-name>:local .
```

**For integration testing:**
- Test with real MongoDB connections
- Verify gRPC endpoints work as expected
- Use Helm for deploying to test clusters

### 6. 🧪 Write Unit Tests

Quality unit tests are essential! Make sure to:

**Test Coverage Guidelines:**
- Aim for good test coverage of your new code
- Test both happy paths and error conditions
- Mock external dependencies (MongoDB, Kubernetes API, etc.)
- Use table-driven tests for multiple scenarios

**Example test structure:**
```go
func TestYourFunction(t *testing.T) {
    tests := []struct {
        name     string
        input    YourInput
        expected YourOutput
        wantErr  bool
    }{
        // Your test cases here
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Test implementation
        })
    }
}
```

### 7. 📝 Open a Merge Request (MR)

Time to share your work! Create an MR with these requirements:

**MR Title:**
- Follow [Conventional Commits](https://www.conventionalcommits.org/) format
- Examples:
  - `feat(gpu-monitor): add XID error classification`
  - `fix(node-drainer): resolve timeout handling bug`
  - `docs: update API documentation for platform connector`

**MR Description:**
- **Strongly recommended:** Use the existing MR template
- Provide clear details about what changes were made
- Include the motivation and context for the changes
- **Link to your JIRA ticket:** `Fixes KACE-1234` or `Closes KACE-1234`

**🚨 MANDATORY: AI-Generated Code Review:**
If **ANY** part of your MR contains AI-generated code (ChatGPT, GitHub Copilot, etc.), you **MUST**:

1. **Thoroughly review ALL AI-generated code yourself first**
2. **Add a comment in the MR description stating**: "I have reviewed all AI-generated code in this MR and verified it follows our coding standards, error handling patterns, and includes appropriate tests."
3. **Ensure the AI code** properly handles errors, follows our architectural patterns, and includes comprehensive tests

**⚠️ REJECTION POLICY:** MRs containing AI-generated code without the required self-review comment will be **automatically rejected** without review. This is non-negotiable for code quality and security.

### 8. 🧪 Test on Clusters

Test your changes across different environments:

**Testing Requirements:**
- ✅ **All CSPs (Cloud Service Providers):** AWS, GCP, Azure
- ✅ **Forge environments**
- ✅ Verify functionality in real cluster conditions
- ✅ Test both positive and negative scenarios

**Deployment Requirements:**
- **Disable ArgoCD** on your dev cluster before testing
- **Use Helm** to install the NVSentinel chart:
  ```bash
  # Install using Helm
  helm install nvsentinel ./distros/kubernetes/nvsentinel
  ```

**What to verify:**
- Health monitors detect issues correctly
- Events are properly persisted to MongoDB
- Core modules respond to events as expected
- No resource leaks or performance degradation

### 9. 🤖 Address Code Rabbit Feedback

Our automated code review tool (Code Rabbit) will analyze your MR. Please:

- Review all suggestions carefully
- Address valid concerns and suggestions
- Respond to comments explaining your decisions when you disagree
- Be open to learning from the feedback!

**💡 Tip:** Code Rabbit often catches things humans might miss, like potential bugs, performance issues, or style inconsistencies.

### 10. ⏳ Pipeline and Conflicts

Wait for the CI/CD pipeline to complete successfully:

**Pipeline Requirements:**
- ✅ All tests pass
- ✅ Linting passes
- ✅ Security scans pass
- ✅ Container builds succeed

**If there are conflicts:**
- Rebase your branch on the latest main
- Resolve any merge conflicts
- Re-run tests to ensure everything still works
- Push the updated branch

```bash
git checkout main
git pull origin main
git checkout your-branch
git rebase main
# Resolve conflicts if any
git push --force-with-lease origin your-branch
```

### 11. 📢 Request Team Review

Post in the **#dgxc-gpu-node-resilience** Slack channel:

```
Hi team! 👋 I have an MR ready for review:
[Link to your MR]

Summary: [Brief description of changes]
JIRA: KACE-1234

@dgxc-k8s-gpu-node-resilience could someone please review when you have a chance? Thanks! 🙏
```

**Review expectations:**
- Be responsive to feedback
- Ask questions if something isn't clear
- Engage constructively in discussions

### 12. 🔧 Add Integration Tests

While your MR is being reviewed, add comprehensive integration tests:

**Integration Test Guidelines:**
- Test end-to-end workflows
- Use real or realistic test data
- Verify cross-module communication
- Test failure scenarios and recovery

**Where to add tests:**
- Check the `testcases/` directory for existing patterns
- Follow the established testing framework
- Ensure tests can run in CI/CD environments

### 13. 🤝 Collaborate on Feedback

Work closely with your reviewer(s):

**Best practices:**
- Respond to feedback promptly
- Ask for clarification when needed
- Be open to suggestions and alternative approaches
- Update code based on feedback
- Re-request review after making significant changes

**Remember:** The review process is collaborative and helps everyone learn!

### 14. ✅ Merge Only After Approval

**Important:** Only merge your MR after:
- ✅ Reviewer has explicitly approved the MR
- ✅ All pipeline checks pass
- ✅ All feedback has been addressed
- ✅ Any required documentation is updated

### 15. 🏷️ Create Semantic Version Tag

After merging to main, manually create a semantic version tag:

```bash
git checkout main
git pull origin main

# Create and push the tag
git tag v1.2.3  # Follow semantic versioning
git push origin v1.2.3
```

**Semantic versioning guidelines:**
- **MAJOR** (v2.0.0): Breaking changes
- **MINOR** (v1.2.0): New features, backwards compatible
- **PATCH** (v1.1.1): Bug fixes, backwards compatible

### 16. 📧 Monitor Generated MRs

After tagging, GitLab will automatically create MRs in related repositories:

**Repositories to watch:**
- Components repo
- SBOM repo  
- Image sync repos

**Your responsibilities:**
- ✅ Monitor for the automated MR emails from GitLab
- ✅ Review each generated MR to ensure correctness
- ✅ Merge the generated MRs (no additional review needed unless you add new content)
- ✅ Watch for any failures and address them promptly

**💡 Note:** These are typically automated updates, but always verify they look correct before merging.

### 17. 🎉 Announce Completion

Once all automated MRs are merged, update the Slack thread:

```
✅ Update: All automated MRs have been reviewed and merged successfully!

The feature is now ready for the release train. 🚂

MRs merged:
- Components repo: [link]
- SBOM repo: [link]  
- Image sync repos: [links]

JIRA ticket: KACE-1234 ✅
```

---

## 🆘 Getting Help

**Stuck? Here's how to get help:**

1. **Check existing documentation:** README, code comments, and existing tests
2. **Ask in Slack:** #dgxc-gpu-node-resilience channel
3. **Pair programming:** Reach out to team members for collaboration
4. **Team meetings:** Bring up questions in standup or team meetings

Remember: We're all here to help each other succeed! 🤝

---

**Happy coding! 🎉**

*This guide is a living document. If you find areas for improvement or have suggestions, please update it as part of your MR or create a separate improvement ticket.*
