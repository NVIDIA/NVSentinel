# Maintainers

NVSentinel is maintained by the [@NVIDIA/dgxc-nvsentinel-maintainers](https://github.com/orgs/NVIDIA/teams/dgxc-nvsentinel-maintainers) GitHub team. That team is the canonical, always-current list of maintainers — this document describes what maintainers do and how the areas of the project map to them.

See [GOVERNANCE.md](GOVERNANCE.md) for the full role definitions (Contributor, Reviewer, Approver, Maintainer, Technical Lead), how decisions are made, and how to be nominated for a role.

## Contacting the maintainers

- **Reviews and technical questions**: mention `@NVIDIA/dgxc-nvsentinel-maintainers` on the relevant issue or pull request
- **General questions**: open a [discussion](https://github.com/NVIDIA/NVSentinel/discussions)
- **Governance questions or role nominations**: open a discussion with the `governance` label
- **Security vulnerabilities**: do not use GitHub — follow [SECURITY.md](SECURITY.md)
- **Code of Conduct concerns**: GitHub_Conduct@nvidia.com, per [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)

## Areas of ownership

Maintainers hold cross-cutting responsibility for the whole project. The areas below (defined in [GOVERNANCE.md](GOVERNANCE.md#areas-of-ownership)) determine which Reviewers and Approvers are pulled into a given change.

| Area | Scope |
|---|---|
| Core Infrastructure | [platform-connectors/](platform-connectors/), [store-client/](store-client/), [data-models/](data-models/), [commons/](commons/), [api/](api/) |
| Health Monitors | [health-monitors/](health-monitors/), [health-events-analyzer/](health-events-analyzer/) |
| Fault Management | [fault-quarantine/](fault-quarantine/), [node-drainer/](node-drainer/), [fault-remediation/](fault-remediation/), [gpu-reset/](gpu-reset/) |
| Supporting Services | [janitor/](janitor/), [janitor-provider/](janitor-provider/), [labeler/](labeler/), [metadata-collector/](metadata-collector/), [log-collector/](log-collector/), [event-exporter/](event-exporter/) |
| Preflight | [preflight/](preflight/), [preflight-checks/](preflight-checks/) |
| Distribution | [distros/](distros/), [docker/](docker/), [lifecycle-manager/](lifecycle-manager/), release workflows |
| Documentation | [docs/](docs/), [fern/](fern/), top-level project documentation |

## Maintainer responsibilities

Maintainers are expected to:

- Review and approve pull requests according to the requirements in [GOVERNANCE.md](GOVERNANCE.md#code-changes)
- Triage incoming issues and keep the backlog meaningful
- Make architectural and design decisions, and record the rationale
- Participate in release planning and approve releases (2 maintainer approvals required)
- Uphold the [Code of Conduct](CODE_OF_CONDUCT.md) and respond to reports
- Mentor contributors and onboard new Reviewers and Approvers

Activity expectations and the review cadence for each role are described in [Maintaining Status](GOVERNANCE.md#maintaining-status).

## Becoming a maintainer

Maintainers are drawn from active Approvers. The path — and the nomination and approval thresholds — is documented in [GOVERNANCE.md](GOVERNANCE.md#maintainers). In short: contribute consistently, review others' work, and get nominated by an existing maintainer.

## Emeritus maintainers

Maintainers who step back from day-to-day work move to emeritus status rather than being dropped, and can be restored without repeating the full nomination process. See [Emeritus Process](GOVERNANCE.md#emeritus-process).

_No emeritus maintainers yet._
