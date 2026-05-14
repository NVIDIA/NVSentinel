# NVSentinel MCP Server

A read-only [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server that exposes
NVSentinel GPU health and fault data to AI assistants (Claude, Cursor, MCP-capable hosts).

This component is donated from `ArangoGutierrez/k8s-gpu-mcp-server`
(Apache-2.0) — see the donation source pin in `.agents/plans/donation-source.md`.

The donation reshapes the original project to fit NVSentinel's idioms: it
becomes a thin, stateless Deployment that consumes the existing event store
(via `store-client`) rather than collecting its own data on each node. All
GPU-side data collection stays in NVSentinel's existing monitors.

## Status

This is a work-in-progress merge. See [`AUDIT.md`](AUDIT.md) for the per-tool
Working/Stub matrix and the architectural adaptations from the original
design.

## Documentation

- [Audit](AUDIT.md) — monitor-surface audit, per-tool data sources, spec
  divergences, architectural adaptations
- Top-level NVSentinel `docs/` for project-wide design and architecture
  records

## License

Apache-2.0 (see top-level `LICENSE`).
