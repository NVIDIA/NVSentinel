# Production Baseline Analysis


## Overview

This document analyzes production NVSentinel deployments to establish realistic baseline event rates for scale testing. 

We are using production data as a baseline for these scale tests.  We analyzed clusters ranging in size from  ~15-600 nodes.  

## Data Collection

**Data Source:** Production Operations Grafana  
**Metric:** `mongodb_op_counters_total` (1-minute rate)
**Query:** `sum(rate(mongodb_op_counters_total{env="prod", type!="command"}[1m])) by (cluster, type)`
**Time Period:** 7-day observation (peak 1-minute rates shown)
**Clusters Analyzed:** 50+ production clusters across multiple regions and clouds

### MongoDB Production Event Rates

In looking at the `mongodb_op_counters_total`, we identified these representative production clusters:

| Example Cluster | Nodes | Activity Level | Peak Events/Sec | Avg Events/Sec | Peak Ops/Min |
|----------------|-------|----------------|-----------------|----------------|--------------|
| Cluster A | 65 | Peak activity | **4190.1** | 51.0 | 251406 |
| Cluster B | 37 | High activity | **1845.2** | 77.6 | 110714 |
| Cluster C | 27 | High activity | **1304.8** | 414.5 | 78288 |
| Cluster D | 573 | Moderate activity | **294.0** | 14.3 | 17640 |
| Cluster E | 118 | Moderate activity | **231.8** | 14.0 | 13908 |
| Cluster F | 375 | Low activity | **86.3** | 11.2 | 5178 |
| 50+ other clusters | ~15-600 | Steady baseline | **0.1-0.8** | 0.1-0.8 | 5-50 |

**Key Observations:**
- **Peak vs Average:** Production clusters show significant differences between peak burst events and sustained average loads. Most clusters maintain low average event rates (11-77 events/sec) during normal operation, with occasional bursts reaching much higher peaks (86-4,190 events/sec). Cluster C appears to have sustained higher activity levels (414 events/sec average), suggesting ongoing cluster issues during the monitoring period.
- **Node Count vs Load:** The amount of MongoDB/API server load is more closely correlated with health events happening on the cluster than the number of nodes directly. We include number of nodes to demonstrate this.

*Data collected: October 2025*  
*Analysis based on 7-day MongoDB operations monitoring*
