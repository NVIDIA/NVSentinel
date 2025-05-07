### NVSentinel Tests

## Overview
This directory contains test cases for validating NVSentinel functionality. The tests cover various aspects of NVSentinel including:

### Deployment Tests
- Verifies NVSentinel pods are correctly deployed in the expected namespace
- Validates daemonset deployments for components like:
    - NIC health monitor
    - NVSwitch health monitor 
    - Platform connector
    - GPU health monitor
- Checks pod states and counts match expectations

### Health Monitor Tests
- GPU Health Monitor
    - Monitors GPU hardware errors and health metrics
    - Detects and reports GPU failures and performance issues
    - Validates GPU temperature, power, memory and other vital parameters

- NIC Health Monitor
    - Monitors network interface health and connectivity
    - Tracks network throughput and error rates
    - Validates NIC firmware and driver status

- NVSwitch Health Monitor
    - Monitors NVSwitch fabric health and performance
    - Tracks inter-GPU communication paths
    - Validates NVSwitch temperatures and error counters

### Fault Quarantine Tests
- GPU Health Fatal Error Recovery
    - Tests node quarantine behavior when fatal GPU errors occur
    - Validates node taints, annotations and recovery
  
- GPU Health Non-Fatal Error
    - Verifies proper handling of non-fatal GPU errors
    - Ensures nodes aren't unnecessarily quarantined

- Complex Ruleset Testing
    - Tests priority-based fault handling rules
    - Validates different quarantine behaviors based on error types
  
- GPU Power Watch Error Handling
    - Tests exclusion of power watch errors from node cordoning
    - Validates node conditions and health monitoring

### Node Drainer Module Tests
- Eviction timeout mode test
    - Tests whether the pods are evicted after eviction timeout

- Immedidate eviction mode test
    - Tests whether the pods are moved to another node when the eviction mode is set to Immedidate

- AllowCompletion eviction mode test
    - Verifies that the pod in namespace with eviction mode set to AllowCompletion are not evicted after injecting a fatal error, but pods in the namespace with Immediate mode of eviction are evicted.

### Fault Remediation Tests
- Fault remediation module tests
    - Verifies creation maintainence CR upon injection of fatal XID error

### Metrics tests

Contains tests that around verifying metrics that are based on certain events like failure of reach DCGM

The test suite uses a base test class `TestNVSentinelCaseBase` that provides common utilities and setup for NVSentinel testing. Tests validate both the monitoring and remediation aspects of NVSentinel's node health management capabilities.

