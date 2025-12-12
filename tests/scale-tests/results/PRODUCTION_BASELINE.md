# Production Baseline Analysis


## Overview

This document analyzes production NVSentinel deployments to establish realistic baseline event rates for scale testing. 

We are using production data as a baseline for these scale tests.  For this experiment, we analyzed production clusters ranging in size from ~15-600 nodes.  

## Data Collection

* **Data Source:** Production Operations Grafana
* **Metric:** `mongodb_op_counters_total` (1-minute rate)
* **Query:** `sum(rate(mongodb_op_counters_total{env="prod", type!="command"}[1m])) by (cluster, type)`
* **Time Period:** 1-month observation (peak 1-minute rates shown)
* **Clusters Analyzed:** 50+ production clusters across multiple regions and clouds

### MongoDB Production Event Rates

In looking at the `mongodb_op_counters_total`, we identified these representative production clusters:

| Example Cluster | Nodes | Activity Level | Peak Events/Sec | Avg Events/Sec |
|----------------|-------|----------------|-----------------|----------------|
| Cluster A | 27 | Peak activity | **2033** | 239 |
| Cluster B | 48 | Peak activity | **1739** | 373 |
| Cluster C | 573 | Moderate activity | **371** | 14 |
| Cluster D | 118 | Moderate activity | **327** | 20 |
| Cluster E | 37 | Moderate activity | **307** | 5 |
| Cluster F | 65 | Low activity | **160** | 8 |
| Cluster G | 375 | Low activity | **67** | 9 |
| 50+ other clusters | ~15-600 | Steady baseline | **0-10** | 0-10 |

**Key Observations:**
- **Peak vs Average:** Production clusters show significant differences between peak burst events and sustained average loads. Most clusters maintain low average event rates (5-239 events/sec) during normal operation, with occasional bursts reaching much higher peaks (67-2,033 events/sec). However, there are exceptions, such as Cluster B which shows sustained higher activity levels (373 events/sec average) showing ongoing health events.
- **Node Count vs Load:** The amount of MongoDB/API server load is more closely correlated with health events happening on the cluster than the number of nodes directly.

### Production Incident Pattern Example

Real MongoDB event spikes during a production incident (Cluster A, 27 nodes):

| Time | Events/sec | Pattern |
|------|------------|---------|
| 2:23 | 1,089 | High spike |
| 2:24 | 81 | Recovery |
| 2:25 | 1,866 | High spike |
| 2:26 | 34 | Recovery |
| 2:27 | 470 | Medium load |
| 2:28 | 894 | High spike |
| 2:29 | 82 | Recovery |
| 2:30 | 1,995 | High spike |
| 2:31 | 39 | Recovery |
| 2:32 | 546 | Medium load |

**Pattern Analysis:** Production bursts are typically 1-2 minutes in duration with immediate recovery periods between spikes. This spike-and-recovery pattern is characteristic of incident-driven load, contrasting with sustained high-load scenarios used in stress testing.

Analysis based on November 2025 (entire month) MongoDB operations monitoring
