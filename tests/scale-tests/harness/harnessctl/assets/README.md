# Embedded KWOK assets

Only KWOK stages are baked into the `harnessctl` binary via `go:embed`.
`stack bringup` reads them from the binary.

Helm values are **not** embedded. Pass them at runtime via the mandatory
`--nvsentinel-values` / `--monitoring-values` flags:

- Multi-node clusters: `../../values/`
- Local Kind cluster: `../../kind/`
