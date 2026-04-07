# NVSentinel Fern Docs

This directory contains the [Fern](https://buildwithfern.com) configuration for publishing NVSentinel documentation to a hosted docs site.

The `docs/` directory on `main` is the **source of truth**. This `fern/` directory holds site config and theme assets only — never doc content.

## How publishing works

| Trigger | What happens |
|---|---|
| PR touching `docs/**` or `fern/**` | `fern check` + broken-link scan; preview URL posted as PR comment |
| Merge to `main` | Syncs `docs/` to `docs-website` branch, publishes dev docs |
| Tag `vX.Y.Z` | Snapshots current docs as a versioned release, publishes |

The `docs-website` branch is CI-managed — never edit it by hand. All authoring happens in `docs/`; update `docs/index.yml` to add or move pages in the sidebar.

## One-time setup (required before CI can publish)

### 1. Register with Fern

Go to [buildwithfern.com](https://buildwithfern.com), create an account, and register the project. The free tier gives a preview URL at `nvsentinel.docs.buildwithfern.com`; a paid plan is required for a custom domain (e.g. `docs.nvidia.com/nvsentinel`).

### 2. Update org/URL if needed

If the org or project slug differs from `nvidia` / `nvsentinel`, update:
- `fern/fern.config.json` → `organization`
- `fern/docs.yml` → `instances[0].url` (and `custom-domain` if applicable)

### 3. Create the `docs-website` branch

This branch accumulates versioned doc snapshots over time and must exist before CI can push to it. Create it once:

```bash
git checkout --orphan docs-website
git commit --allow-empty -m "chore: init docs-website branch"
git push origin docs-website
git checkout main
```

### 4. Add the `FERN_TOKEN` secret

In the repo: **Settings → Secrets and variables → Actions → New repository secret**

- Name: `FERN_TOKEN`
- Value: token obtained from your Fern account

### 5. Validate locally

```bash
npm install -g fern-api
fern check
```

## Local preview

```bash
npm install -g fern-api
HOST=0.0.0.0 fern docs dev   # bind to host IP for remote access
# or
fern docs dev                  # localhost only
```

## Releasing a new version

Push a semver tag — the workflow handles the rest:

```bash
git tag v1.0.0
git push origin v1.0.0
```
