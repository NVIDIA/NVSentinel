# ADR-023: Architecture — Health Event Transformer Pipeline

## Context

Platform Connector receives health events from monitors and needs to transform them before storage and propagation. Currently, it applies metadata augmentation that adds node labels and provider info from Kubernetes.

[ADR-021](./021-health-event-property-overrides.md) introduces a second transformation: property overrides using CEL-based rules to modify `isFatal`, `isHealthy`, `recommendedAction`. This is being implemented as a separate processor with its own interface, following the existing pattern used by the metadata augmentor.

As we add more transformations (filtering, enrichment, audit logging), this ad-hoc approach creates maintenance burden and makes ordering/configuration difficult.

## Problem

The current implementation (with [ADR-021](./021-health-event-property-overrides.md) additions) couples the server logic to specific processor interfaces:

```go
if p.Processor != nil {
    p.Processor.AugmentHealthEvent(ctx, he.Events[i])
}

if p.OverrideProcessor != nil {
    p.OverrideProcessor.ApplyOverrides(ctx, he.Events[i])
}
```

With only one transformer this was acceptable, but the pattern doesn't scale. Each new transformation requires:
- Server struct modification
- New initialization code in `main.go`
- Hardcoded ordering in the handler
- Different interface methods (`AugmentHealthEvent`, `ApplyOverrides`, etc.)

There's no way to configure which transformers run or their order without code changes.

## Decision

Introduce a unified transformer pipeline with a common interface. All transformations implement `Transformer`, and a `Pipeline` executes them in configured order.

### Transformer Interface

```go
type Transformer interface {
    Transform(ctx context.Context, event *pb.HealthEvent) error
    Name() string
}
```

### Pipeline Execution

```go
type Pipeline struct {
    transformers []Transformer
}

func (p *Pipeline) Process(ctx context.Context, event *pb.HealthEvent) error {
    for _, t := range p.transformers {
        if err := t.Transform(ctx, event); err != nil {
            slog.Warn("Transformer failed", 
                "transformer", t.Name(), 
                "error", err)
            // Continue with remaining transformers
        }
    }
    return nil
}
```

### Server Integration

```go
type PlatformConnectorServer struct {
    Pipeline *pipeline.Pipeline
}

func (p *PlatformConnectorServer) HealthEventOccurredV1(ctx context.Context, 
    he *pb.HealthEvents) (*empty.Empty, error) {
    
    for i := range he.Events {
        p.Pipeline.Process(ctx, he.Events[i])
    }
    // ... enqueue to ring buffers
}
```

## Implementation

### Package Structure

```plaintext
platform-connectors/pkg/
├── pipeline/
│   ├── pipeline.go       # Pipeline and Transformer interface
│   ├── factory.go        # Transformer factory and registry
│   └── config.go         # Configuration loading
└── transformers/
    ├── metadata/         # Metadata augmentor
    │   └── factory.go    # Registers with pipeline
    └── overrides/        # Property overrides (ADR-021)
        └── factory.go    # Registers with pipeline
```

### Factory Pattern

Transformers are instantiated via factory functions registered by name:

```go
// pkg/pipeline/factory.go

type TransformerFactory func(cfg *TransformerConfig) (Transformer, error)

type TransformerConfig struct {
    Name    string
    Enabled bool
    Config  string  // Path to config file
}

var registry = map[string]TransformerFactory{}

func Register(name string, factory TransformerFactory) {
    registry[name] = factory
}

func Create(cfg *TransformerConfig) (Transformer, error) {
    factory, ok := registry[cfg.Name]
    if !ok {
        return nil, fmt.Errorf("unknown transformer: %s", cfg.Name)
    }
    return factory(cfg)
}

func NewPipelineFromConfig(configs []TransformerConfig) (*Pipeline, error) {
    var transformers []Transformer
    for _, cfg := range configs {
        if !cfg.Enabled {
            continue
        }
        t, err := Create(&cfg)
        if err != nil {
            return nil, err
        }
        transformers = append(transformers, t)
    }
    return NewPipeline(transformers...), nil
}
```

### Transformer Registration

Each transformer package registers its factory and implements the `Transformer` interface directly:

```go
// pkg/transformers/metadata/metadata.go

type MetadataAugmentor struct {
    client kubernetes.Interface
}

func New(client kubernetes.Interface) *MetadataAugmentor {
    return &MetadataAugmentor{client: client}
}

func (m *MetadataAugmentor) Transform(ctx context.Context, event *pb.HealthEvent) error {
    // Augment with node metadata (labels, provider info)
    node, err := m.client.CoreV1().Nodes().Get(ctx, event.NodeName, metav1.GetOptions{})
    if err != nil {
        return err
    }
    // Add labels to event.Metadata...
    return nil
}

func (m *MetadataAugmentor) Name() string { 
    return "MetadataAugmentor" 
}

// pkg/transformers/metadata/factory.go

func init() {
    pipeline.Register("MetadataAugmentor", newFromConfig)
}

func newFromConfig(cfg *pipeline.TransformerConfig) (pipeline.Transformer, error) {
    client, err := kubernetes.NewForConfig(...)
    if err != nil {
        return nil, err
    }
    return New(client), nil
}
```


### Configuration

```yaml
platformConnector:
  transformers:
    - name: MetadataAugmentor
      enabled: true
      config: /etc/nvsentinel/metadata.toml
```

The factory creates transformers from config. Order in YAML determines execution order. main.go imports transformer packages (triggering `init()` registration) then calls `NewPipelineFromConfig()`.

### Error Handling

Transformer failures log warnings but don't block the pipeline. Events with transformation errors still reach storage/Kubernetes. This matches current behavior where augmentation/override failures are non-fatal.

## Consequences

### Positive

- **Extensibility**: New transformers require no server changes, just factory registration
- **Configuration**: Enable/disable/reorder via Helm values
- **Testability**: Test transformers independently, mock registry in tests
- **Consistency**: Single interface for all transformations

### Negative

- **Refactoring cost**: Existing `nodemetadata.Processor` needs interface change
- **Complexity**: Adds indirection through factory and registry

## Alternatives

### 1. Keep current ad-hoc approach

Continue adding processors with custom interfaces.

Rejected: Technical debt accumulates quickly. [ADR-021](./021-health-event-property-overrides.md) adds the second transformer; more are coming (audit, filtering). Better to establish the pattern now than refactor later.

## Notes

- Pipeline processes events in-place (mutates `*pb.HealthEvent`)
- Transformers can access full event context including metadata from previous transformers
- First transformation must be metadata augmentation (override rules depend on node labels)
- Factory pattern maps config string names to implementations via registry
- Transformers register themselves via `init()` - no explicit wiring in main.go
- Implements alongside [ADR-021](./021-health-event-property-overrides.md) (both will be merged together)

