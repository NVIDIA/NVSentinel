# NVSentinel Fern Docs

This directory contains the [Fern](https://buildwithfern.com) configuration for NVSentinel documentation.

The `docs/` directory on `main` is the **source of truth**. This `fern/` directory holds site config and theme assets only — never doc content.

## Local preview

```bash
npm install -g fern-api@4.42.1
fern check
HOST=0.0.0.0 fern docs dev   # bind to host IP for remote access
# navigate to http://<host>:3000/nvsentinel/dev
```

All authoring happens in `docs/`; update `docs/index.yml` to add or move pages in the sidebar.

## CI workflows

| Workflow | Trigger | Purpose |
|---|---|---|
| `fern-docs-ci.yml` | PR touching `docs/**` or `fern/**` | `fern check` + broken-links validation |
| `fern-docs-preview-build.yml` | PR touching `docs/**` or `fern/**` | Collects sources as artifact (no secrets) |
| `fern-docs-preview-comment.yml` | After preview build completes | Generates preview URL and posts PR comment |
| `publish-fern-docs.yml` | `docs/v*` tag push or manual dispatch | Publishes to production |

Preview and publish workflows require `DOCS_FERN_TOKEN` — a repo secret provisioned by the docs infrastructure team for this project.

## Publishing

Push a `docs/v*` tag to trigger a publish:

```bash
git tag docs/v1.0.0
git push origin docs/v1.0.0
```

Or trigger manually from the **Actions** tab → **Publish Fern Docs** → **Run workflow**.
