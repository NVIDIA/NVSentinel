# ADR-027: Kubernetes Data Store (CRD) for HealthEvent

Author: Avinash Reddy Yeddula, Igor Velichkovich

## Context

NVSentinel currently persists and watches health events using MongoDB through a datastore client. While MongoDB provides rich query capabilities and watch semantics, reliance on an external database makes NVSentinel harder to deploy in Kubernetes-native or constrained environments and prevents exposing health events through standard Kubernetes APIs.

A broader datastore abstraction and multi-backend strategy is defined in the NVSentinel Datastore Abstraction Design document. This ADR narrows the scope to the Kubernetes Custom Resource (CR) datastore option and formalizes the design of the Kubernetes HealthEvent API and its operational constraints.

## Decision

We will introduce a Kubernetes-native HealthEvent API and implement a Kubernetes Custom Resource–backed datastore for NVSentinel that supports core event ingestion, status updates, and watch-driven remediation workflows.

In the initial phase, Kubernetes CRs are considered suitable only for NVSentinel’s core, watch-oriented workflows and not for analytics or historical querying.
## Motivations

- **Avoid new database dependencies:** Deploying NVSentinel currently requires provisioning and managing an external datastore (MongoDB, PostgreSQL, etc.). A Kubernetes CR backend allows operators to run NVSentinel without introducing additional database systems.
- **Stay Kubernetes-native:** NVSentinel should leverage Kubernetes as the primary platform. CRs make health events first-class Kubernetes resources, observable through standard Kubernetes APIs and tools.
- **Preserve multi-backend flexibility:** This design maintains the existing datastore abstraction, allowing operators to choose their preferred backend based on their needs.
## Implementation

### API Definition

- API Group/Version: `nvsentinel.dgxc.nvidia.com/v1alpha1`
- Kind: `HealthEvent`
- `spec`: contains immutable event data (from HealthEvent)
- `status`: tracks mutable workflow state (from HealthEventStatus)

### Directory Structure and CRD Generation

- CRD generation strategy: we intend to use a proto-first generation approach (for example, [`protoc-gen-crd`](https://github.com/yandex/protoc-gen-crd)) to produce Kubernetes CRD YAML and the corresponding Go types for the `HealthEvent` `spec` directly from the `.proto` definitions. This keeps the authoritative shape of `spec` in the `.proto` files and reduces drift between proto, generated types, and handwritten types.

- Spec is the `HealthEvent` proto: the `spec` of the `HealthEvent` CR should be the `HealthEvent` protobuf message. To keep generation coherent, the `status` subresource (`HealthEventStatus`) should also be defined in the `.proto` if it is expected to be generated into the CRD and accompanying types.

- Move `HealthEventStatus` into proto: currently `HealthEventStatus` exists as a handwritten Go struct in `data-models/pkg/model/health_event_extentions.go`. For proto-first CRD generation, move the `HealthEventStatus` definition into the `.proto` file so the generator emits a single, consistent CRD with both `spec` and `status` schemas. If `HealthEventStatus` remains only as a handwritten Go type, generation will produce `spec`-related types from proto while `status` remains a separate handwritten type — this creates a mix-and-match and generally forces an extra wrapper or conversion layer to assemble a complete CR type for controllers.

   - Implementation note: after moving `HealthEventStatus` into the `.proto`, run the proto-to-CRD generator to produce the CRD and generated Go types. Existing conversion helpers should be adapted to map between the protobuf messages, generated CRD types, and any internal model types. The event flow and responsibilities (health monitors send events to platform-connectors, which persist to the configured backend) remain unchanged.

- **Future consideration — API versioning:** Generating CRDs directly from proto objects means the proto types will eventually need to follow Kubernetes API versioning rules (e.g., storage versions, conversion webhooks, backward compatibility guarantees). This is acceptable for `v1alpha1` where we can iterate freely, but as the API stabilizes toward `v1`, we should plan for more formal API versioning practices. This does not block the current proto-first approach but should be part of the v1 graduation plan.

> See **Alternatives Considered** for the approach where CRD structs and conversion tests are hand-managed in Go instead of generated from proto.

### Operational Workflow

**Event Flow and CRD Integration:**

1. Kubernetes CR as a pluggable datastore backend: The Kubernetes CR backend is one datastore implementation option alongside MongoDB, PostgreSQL, and others. Operators select which backend to use via Helm configuration at deployment time. Only one backend is active per deployment.

2. Responsibilities and data flow with Kubernetes CR backend:
   - **Health Monitors → Platform Connectors → Datastore:** Health monitors detect events and forward them to the `platform-connectors` component (gRPC). The `platform-connectors` service is responsible for validating and persisting events to the configured datastore backend. When the Kubernetes CR backend is selected, `platform-connectors` will create/update `HealthEvent` CRs. This preserves the current responsibility model — health monitors do not write directly to the datastore. This is the current data flow (Health Monitors → Platform Connectors → Datastore) and will remain unchanged by the CR backend introduction.
   - **FaultRemediationReconciler and controllers:** Consume HealthEvent CRs via watches. Controllers use the CR `spec` as immutable event data and update the CR `status` subresource to reflect mutable workflow state (node quarantine status, pod eviction progress, remediation timestamps).

3. Operators or additional controllers can act on the CR data to determine remediation actions independently, providing flexibility and separation between event ingestion and execution.

### Integration with Existing Tooling and Processes

- **Kubernetes CR as a datastore backend:** The CR backend is a pluggable datastore option. Operators choose which backend to deploy via Helm configuration — either Kubernetes CRs or MongoDB (or another backend). Only one backend is active per deployment.
- **Existing datastore abstraction pattern:** The multi-datastore pattern is already established in NVSentinel (e.g., PostgreSQL and MongoDB backends). The Kubernetes CR backend follows the same abstraction approach. Implementation requires implementing the datastore interface (see [#456](https://github.com/NVIDIA/NVSentinel/pull/456) as a reference for adding a new datastore backend).
- **No MongoDB-specific changes:** The existing MongoDB-backed implementation remains unchanged and fully functional.
   - **Health Monitors and Controllers:** When using the Kubernetes CR backend, health monitors send events to `platform-connectors`, which create CRs with immutable `spec`. Controllers consume CRs via watches and update CR `status` to track workflow state. When using MongoDB, the flow remains as described above.
- **Existing remediation logic:** Log collection, state management, and remediation remain functionally the same regardless of which backend is deployed.
- **Proto migration coordination:** Moving `HealthEventStatus` from `data-models/pkg/model/health_event_extentions.go` into the `.proto` definition requires regenerating the proto files and updating all components (health monitors, controllers, store-client, converters, etc.) that currently depend on the old struct. This change must be coordinated across all affected modules to ensure they consume the new generated types.

### CR Cleanup / Garbage Collection:
To prevent uncontrolled growth of HealthEvent CRs and reduce load on the Kubernetes API server, the Kubernetes store implementation includes:
- Optional TTL / age-based deletion – CRs older than a configured threshold can be automatically removed if still unresolved, preventing indefinite growth.
- Enforcement of configurable per-node or per-cluster CR limits – ensures the number of CRs remains manageable even during bursty failure periods or in large clusters.

## Rationale

- **Operational simplicity:** Using Kubernetes CRs leverages the existing control plane and standard Kubernetes mechanisms, eliminating the need for a separate database deployment and management.
- **No additional dependencies:** Without a Kubernetes CR backend, consumers must deploy and manage an external datastore (MongoDB, PostgreSQL, etc.). Using Kubernetes CRs as the backing store removes this external dependency burden.
- **Native observability and integration:** CRs can be watched, queried (to a limited extent), and annotated using Kubernetes-native APIs, making integration with controllers, dashboards, and CI/CD pipelines easier.
- **Developer experience and flexibility:** The CRD abstraction allows developers to work with Kubernetes-native resources while keeping the underlying datastore pluggable, enabling future enhancements or alternative datastore implementations.

## Consequences

### Positive

- Kubernetes-native storage: Events are persisted directly in the cluster, reducing external dependencies.
- Core workflows supported: Watch-oriented health event processing (quarantine, remediation, basic updates) works without modification.
- Historic view via kube-state-metrics: Health events stored as CRs can be scraped and exposed through Kubernetes-native monitoring tools like kube-state-metrics, enabling historical analysis and observability without additional infrastructure.

### Negative

- Limited querying capabilities: Complex ad-hoc queries and full-featured historical analysis capabilities (beyond basic time-series metrics) are not feasible using only CRs.
- Control plane load: High-frequency or bursty events can increase the number of CR objects, potentially impacting the API server in large clusters.

### Mitigations

- Event deduplication and rate limiting: Only create CRs for unique unhealthy events or updates to existing events.
- Resource cleanup and garbage collection: Automatically remove CRs when events are remediated or when cluster/node limits are reached.
- Phased approach: Core watch-based workflows are enabled first; advanced features can be added later.

## Alternatives Considered

### Hand-Managed CRD Structs and Conversion Tests
**Under Consideration** — needs further review:
- Data Models: All new structs for the Kubernetes HealthEvent CRD (HealthEventSpec, HealthEventStatus, BehaviourOverrides, Entity, etc.) are added to `./data-models` as handwritten Go types.
- CRD Generation: `controller-gen` is used to generate the Kubernetes CRD YAML manifests from these Go structs (spec) while `HealthEventStatus` remains in `data-models/pkg/model/health_event_extentions.go`.
- Conversion APIs: The conversion functions between internal NVSentinel models and CRD types are implemented in `pkg/conversion/healthevent_conversion.go` with corresponding unit tests in `pkg/conversion/healthevent_conversion_test.go`. These tests ensure that the mapping between internal models, protobufs, and CRD structs is correct and that there is no drift between the data models.
- Helm Chart Packaging: The generated CRD manifests are packaged in a new Helm chart at `./distros/kubernetes/nvsentinel/charts/kubernetes-store`.
- **Trade-off:** This approach keeps `HealthEventStatus` as a handwritten struct, which requires an additional wrapper/conversion layer to compose a complete CR type for controllers. However, it may offer more flexibility in certain scenarios and should be evaluated further before final decision.

## Notes

- The initial phase only supports core, watch-driven workflows; analytics and historical queries are deferred.
- The system is designed to allow multiple operators or controllers to act independently on CR data.

## References

- [NVSentinel Datastore Abstraction Design Document](https://docs.google.com/document/d/1iD6qhWDapfb7CMCAY7sr3FB6WngdlwmSMewUDQ8gxJo/edit?tab=t.0#heading=h.lolo8bj6u6g2)
- [K8s API Data store WIP PR](https://github.com/NVIDIA/NVSentinel/pull/640)
