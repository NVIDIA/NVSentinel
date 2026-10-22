# Deprecations

This directory lists the NVSentinel features that are deprecated. A deprecated feature still works, but a later release removes it. Each page gives the reason, the impact, the action to take, and the timeline.

## How NVSentinel Announces a Deprecation

1. A page in this directory describes the deprecation.
2. The release notes of the first release that carries the page get a "Deprecations" section.
3. The documentation of the affected component shows a deprecation warning until the removal ships.
4. Users can object in a GitHub issue during the feedback window on the page. The maintainers decide whether an objection blocks the removal.
5. The release notes of the release that removes the feature list the removal.

## Active Deprecations

| Feature | Component | Removal | Details |
|---|---|---|---|
| DCGM 3.x support | GPU Health Monitor | No later than v1.32.0 | [GPU Health Monitor: DCGM 3.x Support Removal](gpu-health-monitor-dcgm-3-support-removal.md) |
