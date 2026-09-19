# GPU Health Monitor

Health monitor for monitoring the health of GPUs


## Incident reporting

Each GPU and watch reports every distinct error code. Repeated incidents for the same code share one event with their combined messages. Each event retains its code-specific remediation action.

Suppression and debounce apply separately to each code. When a previously reported code disappears, a healthy event clears that code while preserving other faults on the same GPU or NVSwitch and watch. When no incidents remain, an empty-code healthy event clears the whole entity/watch. New faults are published before recoveries, and cache updates occur after successful delivery.

Missing or incomplete health data does not clear existing faults. When DCGM reaches its error limit, the response may leave out other errors. The monitor therefore waits for complete data before clearing faults.

The monitor loses its error history when it restarts. An earlier fault may then stay active while other errors remain for the same GPU or NVSwitch watch. A complete check with no errors for that device and watch clears those faults.
