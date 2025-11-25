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

In looking at the `mongodb_op_counters_total`:

| Activity Level | Mean Ops/Min | Events/Sec | Number of Nodes |
|---------|--------------|------------|----------------|
| Peak  | 531 | **8.85** | 23 |
| Moderate  | 256 | **4.27** | 573 |
| Low  | 127 | **2.12** | 19 |
| Minimal  | 9.56 |  **0.16**| 375 |
| Steady* | 5-50 | **0.1-0.8** | ~15-600 |

*\*Steady activity represents 50+ additional production clusters with low, steady-state activity*

**Note:** The amount of MongoDB/API server load is more closely correlated with health events happening on the cluster than the number of nodes directly. We include number of nodes to demonstrate this.

## Recommended Scale Test Scenarios

Based on production data, we plan to do scale testing of a 1500-node cluster at these event rates based on extrapolate from production data of smaller clusters:

**MongoDB Event Rates:**

| Test Scenario | Target Event Rate | Scale Factor | Description |
|--------------|------------------|--------------|-------------|
| **Light Load** | 30 events/sec | 3.4× peak | Conservative baseline for 1500 nodes |
| **Medium Load** | 100 events/sec | 11× peak | Moderate stress test |
| **Heavy Load** | 300 events/sec | 34× peak | High stress test |
---

**Note:** API server impact will be measured in scale tests at production event rates. Production clusters do not consistently expose API server metrics, but NVSentinel's API operations (node condition updates, tainting, cordoning) will be validated on test clusters.

*Data collected: October 2025*  
*Analysis based on 7-day MongoDB operations monitoring*

