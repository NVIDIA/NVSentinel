# NVSentinel Fern Docs

This directory contains the [Fern](https://buildwithfern.com) configuration for publishing NVSentinel documentation to a hosted docs site.

The `docs/` directory on `main` is the **source of truth**. This `fern/` directory holds site config and theme assets only — never doc content.

## How publishing works

- **Authoring**: edit markdown files in `docs/`, update `docs/index.yml` for navigation
- **CI on merge to main**: syncs `docs/` to the `docs-website` branch and runs `fern generate --docs`
- **CI on `vX.Y.Z` tag**: creates a versioned snapshot and publishes it alongside `dev`
- **PRs**: `fern check` + broken-link scan run automatically; a preview URL is posted as a PR comment

The `docs-website` branch is CI-managed — never edit it by hand.

## One-time setup (required before CI can publish)

1. **Register the project with Fern** at [buildwithfern.com](https://buildwithfern.com), create an organization (use `nvidia` or your own), and obtain a `FERN_TOKEN`. Note that publishing to a custom domain (e.g. `docs.nvidia.com/nvsentinel`) requires a paid Fern plan; the `buildwithfern.com` preview URL is available on the free tier.

2. **Update org/URL** if the project slug differs from `nvsentinel`:
   - `fern/fern.config.json` → `organization`
   - `fern/docs.yml` → `instances[0].url` (and `custom-domain` if applicable)

3. **Create the `docs-website` branch**:
   ```bash
   git checkout --orphan docs-website
   git commit --allow-empty -m "chore: init docs-website branch"
   git push origin docs-website
   git checkout main
   ```

4. **Add `FERN_TOKEN` as a GitHub Actions secret** in repo Settings → Secrets and variables → Actions.

5. **Validate locally**:
   ```bash
   npm install -g fern-api
   fern check
   ```

## Local preview

```bash
npm install -g fern-api
fern docs dev
```

## Releasing a new version

Push a semver tag — the workflow handles the rest:

```bash
git tag v1.0.0
git push origin v1.0.0
```
