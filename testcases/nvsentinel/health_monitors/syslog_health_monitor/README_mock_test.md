# Syslog Health Monitor Error Injection Tests

This directory contains comprehensive unit tests for testing the NVSentinel Syslog Health Monitor's ability to detect various system errors using logger-based error injection.

## Overview

The tests follow a simple 5-step process to test error detection:

1. **Create debug pod** on target node
2. **Inject error messages** using logger command
3. **Wait for monitoring cycle** to detect errors
4. **Check node conditions** are updated
5. **Cleanup** debug pod and temporary files

## Files

### Test Files
- `test_bmc_health_mock_error_injection.py` - BMC Health error injection test
- `test_hca_fw_error_mock_injection.py` - HCA Firmware error injection test  
- `test_ib_firmware_bug_mock_injection.py` - IB Firmware bug error injection test
- `test_mce_errors_mock_injection.py` - MCE (Machine Check Exception) error injection test
- `test_syslog_mock_service_error_injection.py` - Fabricmanager and Persistenced tests (uses mock services)

### Utility Files
- `syslog_mock_test_utils.py` - Shared utility module with base class for all tests
- `README_mock_test.md` - This documentation

## Test Architecture

### Modular Design

All tests inherit from `SyslogMockTestBase` class which provides:
- Common setup/teardown functionality
- Debug pod creation and management
- Logger-based error message injection
- Node condition verification

This modular approach ensures:
- **Code reusability** across different error types
- **Consistent behavior** for all tests
- **Easy maintenance** and updates
- **Simplified test creation** for new error types

### Logger-Based Error Injection

The tests use the `logger` command to inject error messages directly into the system journal:

```bash
chroot /host logger -p daemon.err "Error message here"
```

This approach:
- **No configuration changes** - Uses existing ConfigMap as-is
- **Direct journal injection** - Messages appear immediately in systemd journal
- **Realistic testing** - Error patterns match actual system log formats
- **Simple cleanup** - Only requires debug pod removal

### Error Types Tested

#### 1. BMC Health Errors (`SysLogsBMCHealth`)
**Pattern:** `BMC returned incorrect response`
**Count:** 4 (requires multiple occurrences)
**Case:** Insensitive

```python
error_messages = [
    "BMC returned incorrect response for sensor reading",
    "BMC returned incorrect response during temperature check", 
    "BMC returned incorrect response for fan speed",
    "BMC returned incorrect response on power status",
    "BMC returned incorrect response for voltage monitoring"
]
```

#### 2. HCA Firmware Errors (`SysLogshcaFwError`)
**Pattern:** `Health issue observed, firmware internal error`
**Count:** 0 (triggers immediately)
**Case:** Insensitive

```python
error_messages = [
    "mlx5_core 0000:17:00.0: Health issue observed, firmware internal error detected on device",
    "HCA firmware encountered critical error: Health issue observed, firmware internal error during operation"
]
```

#### 3. IB Firmware Bug Errors (`SysLogsIbFirmwareBug`)
**Pattern:** `Skipping wait for vf pages stage`
**Count:** 0 (triggers immediately)
**Case:** Sensitive

```python
error_messages = [
    "mlx5_core 0000:3b:00.0: Skipping wait for vf pages stage due to firmware bug",
    "IB driver: Skipping wait for vf pages stage - firmware initialization issue"
]
```

#### 4. MCE Errors (`SysLogsMceErrors`)
**Pattern:** `Machine check events logged`
**Count:** 20 (requires multiple occurrences)
**Case:** Insensitive

```python
error_messages = [
    "kernel: mce: Machine check events logged - CPU 0 Bank 0 Error 0x0001",
    "kernel: mce: Machine check events logged - CPU 1 Bank 1 Error 0x0002",
    # ... 8 different messages, repeated 3 times each = 24 total
]
```

#### 5. Fabricmanager/Persistenced Tests
These tests use mock systemd services for more complex testing scenarios.

## Usage

### Using pytest

```bash
# Run from testcases directory
cd /path/to/nvsentinel/testcases

# Run all syslog health monitor tests
pytest nvsentinel/health_monitors/syslog_health_monitor/ -v

# Run specific test files
pytest nvsentinel/health_monitors/syslog_health_monitor/test_bmc_health_mock_error_injection.py -v
pytest nvsentinel/health_monitors/syslog_health_monitor/test_hca_fw_error_mock_injection.py -v
pytest nvsentinel/health_monitors/syslog_health_monitor/test_ib_firmware_bug_mock_injection.py -v
pytest nvsentinel/health_monitors/syslog_health_monitor/test_mce_errors_mock_injection.py -v

# Run specific test methods
pytest nvsentinel/health_monitors/syslog_health_monitor/test_bmc_health_mock_error_injection.py::TestBMCHealthMockErrorInjection::test_bmc_health_mock_error_injection -v

# Run with specific markers
pytest -m "sysloghealthmonitor" -v

# Run with detailed output
pytest nvsentinel/health_monitors/syslog_health_monitor/ -v -s
```

### Running Individual Tests

```bash
# BMC Health Error Test
pytest nvsentinel/health_monitors/syslog_health_monitor/test_bmc_health_mock_error_injection.py::TestBMCHealthMockErrorInjection::test_bmc_health_mock_error_injection -v

# HCA Firmware Error Test
pytest nvsentinel/health_monitors/syslog_health_monitor/test_hca_fw_error_mock_injection.py::TestHCAFwErrorMockErrorInjection::test_hca_fw_error_mock_error_injection -v

# IB Firmware Bug Test
pytest nvsentinel/health_monitors/syslog_health_monitor/test_ib_firmware_bug_mock_injection.py::TestIBFirmwareBugMockErrorInjection::test_ib_firmware_bug_mock_error_injection -v

# MCE Errors Test
pytest nvsentinel/health_monitors/syslog_health_monitor/test_mce_errors_mock_injection.py::TestMCEErrorsMockErrorInjection::test_mce_errors_mock_error_injection -v
```

## Prerequisites

### Environment Requirements

1. **Kubernetes cluster** with NVSentinel deployed
2. **nvsentinel namespace** exists with syslog-health-monitor running
3. **kubectl access** to the cluster
4. **Python 3.7+** with required dependencies:
   - `kubernetes`
   - `pytest`
   - `PyYAML`

### Permissions

The tests require:
- **Cluster admin** or permissions to:
  - Create debug pods with privileged access
  - Access node filesystem via hostPath mounts

### Node Requirements

- **systemd-based** Linux nodes (Ubuntu, RHEL, etc.)
- **logger** command available
- **chroot** command available in debug pod

## Expected Test Flow

### Step-by-Step Execution

1. **Pre-verification**
   - ✅ Check nvsentinel-syslog-health-monitor pods exist
   - ✅ Select target node for testing

2. **Debug Pod Setup**
   - 🐳 Create privileged debug pod on target node
   - ⏳ Wait for pod to be ready

3. **Error Injection**
   - 📝 Inject error messages using logger command
   - 🔄 Messages appear in systemd journal immediately

4. **Monitoring and Verification**
   - ⏳ Wait 60 seconds for monitoring cycle
   - 🔍 Check node conditions for error updates
   - 📊 Verify expected error patterns detected

5. **Cleanup**
   - 🗑️ Delete debug pod
   - 🧹 Remove temporary files

### Expected Node Conditions

After successful error injection, node conditions should include:

```yaml
# BMC Health Errors
conditions:
- type: "SysLogsBMCHealth"
  status: "False" 
  reason: "ErrorDetected"
  message: "Found matching patterns: BMC returned incorrect response"

# HCA Firmware Errors  
- type: "SysLogshcaFwError"
  status: "False"
  reason: "ErrorDetected"
  message: "Found matching patterns: Health issue observed, firmware internal error"

# IB Firmware Bug Errors
- type: "SysLogsIbFirmwareBug"  
  status: "False"
  reason: "ErrorDetected"
  message: "Found matching patterns: Skipping wait for vf pages stage"

# MCE Errors
- type: "SysLogsMceErrors"
  status: "False"
  reason: "ErrorDetected"
  message: "Found matching patterns: Machine check events logged"
```

## Test Characteristics

### Error Pattern Matching

| Test | Pattern | Count | Case Sensitive | Trigger Threshold |
|------|---------|-------|----------------|-------------------|
| BMC Health | `BMC returned incorrect response` | 4 | No | 4+ occurrences |
| HCA Firmware | `Health issue observed, firmware internal error` | 0 | No | Immediate |
| IB Firmware Bug | `Skipping wait for vf pages stage` | 0 | Yes | Immediate |
| MCE Errors | `Machine check events logged` | 20 | No | 20+ occurrences |

### Logger Injection Features

- **Direct journal access** via logger command
- **Realistic error patterns** matching actual system logs
- **Configurable message counts** for threshold testing
- **Immediate availability** in systemd journal
- **No system configuration changes** required

## Troubleshooting

### Common Issues

#### 1. No syslog-health-monitor pods found
```bash
# Check if NVSentinel is deployed
kubectl get pods -n nvsentinel

# Check if syslog-health-monitor is enabled
kubectl get configmap -n nvsentinel
```

#### 2. Permission denied creating debug pod
```bash
# Check your cluster permissions
kubectl auth can-i create pods --as=system:serviceaccount:default:default

# Use cluster admin context if needed
```

#### 3. Logger command not working
```bash
# Test logger in debug pod
kubectl exec -n nvsentinel <debug-pod> -- chroot /host logger -p daemon.err "Test message"

# Check if message appears in journal
kubectl exec -n nvsentinel <debug-pod> -- chroot /host journalctl -n 10 | grep "Test message"
```

#### 4. Node conditions not updated
```bash
# Check syslog-health-monitor logs
kubectl logs -n nvsentinel <syslog-health-monitor-pod>

# Verify ConfigMap is using the expected patterns
kubectl get configmap -n nvsentinel nvsentinel-syslog-health-monitor -o yaml
```

#### 5. Test cleanup issues
```bash
# Manual cleanup if needed
kubectl delete pod -n nvsentinel <debug-pod-name>
```

## Best Practices

### Test Development

1. **Use the base class** `SyslogMockTestBase` for new error type tests
2. **Match exact patterns** from the ConfigMap configuration
3. **Generate appropriate error counts** based on the threshold configuration
4. **Use realistic error messages** that match actual system patterns
5. **Test both threshold and immediate trigger scenarios**

### Test Execution

1. **Run tests individually** first to verify functionality
2. **Check cluster resources** before running multiple tests
3. **Monitor cleanup** to ensure no test artifacts remain
4. **Verify existing ConfigMap** contains the expected error patterns

### Adding New Tests

1. Create new test file following naming pattern: `test_<error_type>_mock_injection.py`
2. Inherit from `SyslogMockTestBase`
3. Define appropriate error messages that match ConfigMap patterns
4. Call `run_logger_error_injection_test()` with correct parameters
5. Update this README with the new test information

### Logger Command Tips

- Use appropriate log levels (`daemon.err`, `kern.err`, etc.)
- Include realistic timestamps and system identifiers
- Match case sensitivity requirements from ConfigMap
- Test message injection manually before automating

This simplified architecture makes it easy to add new error type tests while maintaining consistency and avoiding complex system configuration changes.

## Advantages of Logger-Based Approach

✅ **No ConfigMap changes** - Uses existing configuration  
✅ **No service management** - No systemd service files to create/cleanup  
✅ **Immediate injection** - Messages appear in journal instantly  
✅ **Simple cleanup** - Only debug pod needs to be removed  
✅ **Realistic testing** - Direct journal injection like real system errors  
✅ **Easy debugging** - Can manually verify message injection  
✅ **Faster execution** - No service startup/failure delays 