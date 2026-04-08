# NVSentinel Fern Docs

This directory contains the [Fern](https://buildwithfern.com) configuration for publishing NVSentinel documentation to [docs.nvidia.com/nvsentinel](https://docs.nvidia.com/nvsentinel).

The `docs/` directory on `main` is the **source of truth**. This `fern/` directory holds site config and theme assets only — never doc content. Contributors not involved in docs publishing can ignore this directory.

## Local preview

```bash
npm install -g fern-api@4.23.0
fern check
HOST=0.0.0.0 fern docs dev   # bind to host IP for remote access
```

Navigate to `http://<host>:3000/nvsentinel/dev`.

## Automated publishing (optional)

Publishing is automated via GitHub Actions CI (`.github/workflows/fern-docs.yml`). CI is optional — the `fern/` configuration is complete on its own and can be used for local preview without it.

| Trigger | What happens |
|---|---|
| PR touching `docs/**` or `fern/**` | `fern check` + broken-link scan; preview URL posted as PR comment |
| Merge to `main` | Syncs `docs/` to `docs-website` branch, publishes dev docs |
| Tag `vX.Y.Z` | Snapshots current docs as a versioned release, publishes |

The `docs-website` branch is CI-managed — never edit it by hand. All authoring happens in `docs/`; update `docs/index.yml` to add or move pages in the sidebar.

## Releasing a new version

Push a semver tag — the CI workflow handles the rest:

```bash
git tag v1.0.0
git push origin v1.0.0
```

## One-time setup for repo owners (required to enable CI publishing)

The following steps require repo owner permissions. They are independent of merging this PR — the `fern/` configuration is valid without them.

**Versioned releases:** if doc snapshots tied to `vX.Y.Z` tags are needed, decide this before the first publish run — snapshots cannot be created retroactively. Confirm the existing tag convention matches `vX.Y.Z`.

### 1. Create the `docs-website` branch

```bash
git checkout --orphan docs-website
git rm -rf .
git clean -fdx
git commit --allow-empty -m "chore: init docs-website branch"
git push origin docs-website
git checkout main
```

### 2. Register the project with Fern

Register under the NVIDIA org via NVIDIA's Fern account. Coordinate with the docs infrastructure team to get the project registered and the `docs.nvidia.com/nvsentinel` route provisioned.

### 3. Add the `FERN_TOKEN` secret

In the repo: **Settings → Secrets and variables → Actions → New repository secret**

- Name: `FERN_TOKEN`
- Value: token from the NVIDIA Fern account
