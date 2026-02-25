# NVSentinel Governance

This document describes the governance model for the NVSentinel project. It defines the roles, responsibilities, and decision-making processes that help maintain the long-term health and sustainability of the project.

## Overview

NVSentinel follows a hierarchical governance model similar to other Kubernetes ecosystem projects. This structure ensures that contributions are properly reviewed, architectural decisions are well-considered, and the project maintains high quality standards while remaining accessible to new contributors.

## Roles and Responsibilities

The NVSentinel project has the following roles, listed in order of increasing scope of responsibility:

### Contributors

**Who they are**: Anyone who contributes to the project in any form (code, documentation, issues, discussions, etc.).

**Responsibilities**:
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md)
- Sign the [Developer Certificate of Origin](CONTRIBUTING.md#developer-certificate-of-origin) (DCO) for all contributions
- Follow project coding standards and guidelines
- Respond to feedback on their contributions

**How to become one**: Simply contribute! Open a pull request, file an issue, or participate in discussions.

### Reviewers

**Who they are**: Contributors who have demonstrated consistent, high-quality contributions and have been granted review privileges for specific areas of the codebase.

**Responsibilities**:
- Review pull requests in their area of expertise
- Provide constructive feedback on code quality, design, and testing
- Ensure contributions follow project standards
- Help triage and categorize issues
- Mentor new contributors

**Privileges**:
- Can review and comment on pull requests
- Can request changes on pull requests
- Can be assigned to review pull requests

**How to become one**:
1. Demonstrate consistent, high-quality contributions over time
2. Show expertise in specific areas of the codebase
3. Be nominated by an existing Reviewer or Approver
4. Be approved by a majority of Approvers in the relevant area

### Approvers

**Who they are**: Reviewers who have demonstrated deep expertise and excellent judgment in their areas. They have the authority to approve pull requests for merging.

**Responsibilities**:
- All responsibilities of Reviewers
- Approve pull requests that meet project standards
- Make decisions on technical design within their area
- Participate in architectural discussions
- Help maintain code quality and project health
- Mentor Reviewers and Contributors

**Privileges**:
- All privileges of Reviewers
- Can approve pull requests (with appropriate reviews)
- Can merge approved pull requests
- Can participate in release planning and decisions

**How to become one**:
1. Be an active Reviewer for at least 3 months
2. Demonstrate excellent technical judgment and code review quality
3. Show ability to make sound architectural decisions
4. Deliver a feature end to end
5. Be nominated by an existing Approver or Maintainer
6. Be approved by a majority of Maintainers

### Maintainers

**Who they are**: Approvers who have demonstrated exceptional commitment to the project and have broad responsibility for its overall health and direction.

**Responsibilities**:
- All responsibilities of Approvers
- Make architectural and design decisions
- Participate in release planning and management
- Resolve conflicts and disputes
- Maintain project documentation and governance
- Represent the project in the community
- Onboard new Approvers and Reviewers

**Privileges**:
- All privileges of Approvers
- Can make architectural decisions
- Can approve new Reviewers and Approvers
- Can participate in project-wide decisions
- Can manage project settings and repositories

**How to become one**:
1. Be an active Approver for at least 6 months
2. Demonstrate exceptional commitment and leadership
3. Show ability to guide project direction
4. Be nominated by an existing Maintainer
5. Be approved by a supermajority (2/3) of existing Maintainers

### Technical Leads / Project Chairs

**Who they are**: Maintainers who provide overall technical leadership and strategic direction for the project.

**Responsibilities**:
- All responsibilities of Maintainers
- Set long-term technical vision and roadmap
- Make final decisions on major architectural changes
- Resolve disputes that cannot be resolved by Maintainers
- Represent the project to external stakeholders
- Coordinate with other projects and organizations

**Privileges**:
- All privileges of Maintainers
- Final authority on technical decisions
- Can make emergency decisions when needed

**How to become one**:
- Appointed by the project sponsor (NVIDIA) in consultation with existing Technical Leads
- Requires exceptional technical leadership and commitment

## Decision-Making Process

### Code Changes

1. **Pull Request Process**:
   - All pull requests require at least one review from a Reviewer
   - All pull requests require approval from at least one Approver
   - Maintainers can approve their own changes, but should seek review from another Maintainer for significant changes
   - All CI checks must pass before merging

2. **Review Requirements**:
   - Small changes (documentation, typo fixes): 1 Reviewer approval
   - Standard changes: 1 Reviewer + 1 Approver approval
   - Significant changes (new features, major refactoring): 2 Reviewers + 1 Approver approval
   - Architectural changes: 2 Approvers + 1 Maintainer approval

### Design and Architecture Decisions

1. **Proposal Process**:
   - Create a GitHub Discussion or issue with the `design` label
   - Tag relevant Maintainers and Approvers
   - Allow at least 1 week for community feedback
   - Document the decision and rationale

2. **Decision Authority**:
   - Minor design decisions: Any Approver
   - Significant design decisions: Majority of Maintainers
   - Major architectural changes: Supermajority (2/3) of Maintainers or Technical Leads

### Conflict Resolution

1. **Technical Disagreements**:
   - Discuss in pull request comments or GitHub Discussions
   - If unresolved, escalate to Maintainers
   - Maintainers will facilitate discussion and make a decision
   - Final appeal to Technical Leads if needed

2. **Code of Conduct Issues**:
   - Report to GitHub_Conduct@nvidia.com (as per [Code of Conduct](CODE_OF_CONDUCT.md))
   - Maintainers will investigate and take appropriate action

## Areas of Ownership

The project is organized into several areas, each with their own Reviewers and Approvers:

- **Core Infrastructure**: Platform connectors, store client, data models
- **Health Monitors**: GPU, syslog, CSP, and Kubernetes object monitors
- **Fault Management**: Fault quarantine, node drainer, fault remediation
- **Supporting Services**: Janitor, labeler, metadata collector, log collector
- **Distribution**: Helm charts, Kubernetes manifests, deployment tooling
- **Documentation**: All project documentation

Maintainers have cross-cutting responsibilities across all areas.

## Becoming a Reviewer or Approver

### Path to Reviewer

1. **Contribute consistently**: Make at least 5-10 meaningful contributions
2. **Demonstrate expertise**: Show deep understanding of specific areas
3. **Review contributions**: Provide helpful reviews on others' pull requests
4. **Get nominated**: Be nominated by an existing Reviewer or Approver
5. **Get approved**: Receive approval from a majority of Approvers in the relevant area

### Path to Approver

1. **Be an active Reviewer**: Review consistently for at least 3 months
2. **Show judgment**: Demonstrate ability to make sound technical decisions
3. **Mentor others**: Help onboard new contributors
4. **Get nominated**: Be nominated by an existing Approver or Maintainer
5. **Get approved**: Receive approval from a majority of Maintainers

## Maintaining Status

All roles require ongoing participation:

- **Reviewers**: Should review at least 1 pull request per month
- **Approvers**: Should review and approve at least 2 pull requests per month
- **Maintainers**: Should participate in project discussions and decisions regularly

If a person is inactive for 6+ months, their status may be reviewed. Exceptions can be made for known circumstances (e.g., sabbatical, parental leave).

## Release Management

- **Release Planning**: Maintainers coordinate release planning and scheduling
- **Release Approval**: Releases require approval from at least 2 Maintainers
- **Release Process**: See [RELEASE.md](RELEASE.md) for detailed release procedures

## Modifications to Governance

Changes to this governance document require:
- A pull request with the proposed changes
- Discussion period of at least 1 week
- Approval from a supermajority (2/3) of Maintainers

## Contact

For questions about governance or to nominate someone for a role:
- Open a GitHub Discussion with the `governance` label
- Contact the project Maintainers directly

---

*This governance model is inspired by the Kubernetes project and other CNCF projects, adapted for the needs of NVSentinel.*
