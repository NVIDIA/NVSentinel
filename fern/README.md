# NVSentinel Fern Docs

This directory contains the [Fern](https://buildwithfern.com) configuration for NVSentinel documentation.

The `docs/` directory on `main` is the **source of truth**. This `fern/` directory holds site config and theme assets only — never doc content.

## Local preview

```bash
npm install -g fern-api@4.23.0
fern check
HOST=0.0.0.0 fern docs dev   # bind to host IP for remote access
# navigate to http://<host>:3000/nvsentinel/dev
```

All authoring happens in `docs/`; update `docs/index.yml` to add or move pages in the sidebar.
