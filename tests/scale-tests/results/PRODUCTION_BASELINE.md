# Production Baseline Analysis


## Overview

This document analyzes production NVSentinel deployments to establish realistic baseline event rates for scale testing. 

We are using production data as a baseline for these scale tests.  We analyzed clusters ranging in size from  ~15-600 nodes.  

## Data Collection

**Data Source:** Production Operations Grafana  
**Metric:** MongoDB insert operations (proxy for health event rate)  
**Time Period:** 7-day analysis  
**Clusters Analyzed:** 50+ production clusters across multiple regions and clouds

### MongoDB Production Event Rates

In looking at the `mongodb_op_counters_total`, we identified these representative production clusters:

| Example Cluster | Nodes | Activity Level | Events/Sec | Ops/Min |
|----------------|-------|----------------|------------|---------|
| Cluster A | 23 | Peak activity | **8.85** | 531 |
| Cluster B | 573 | Moderate activity | **4.27** | 256 |
| Cluster C | 19 | Low activity | **2.12** | 127 |
| Cluster D | 375 | Minimal activity | **0.16** | 9.56 |
| 50+ other clusters | ~15-600 | Steady baseline | **0.1-0.8** | 5-50 |

**Note:** The amount of MongoDB/API server load is more closely correlated with health events happening on the cluster than the number of nodes directly. We include number of nodes to demonstrate this.

*Data collected: October 2025*  
*Analysis based on 7-day MongoDB operations monitoring*
