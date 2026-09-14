# GPU Health Monitor

Health monitor for monitoring the health of GPUs


## Incident reporting

Each GPU and watch reports every distinct error code. Repeated incidents for the same code share one event with their combined messages. Each event retains its code-specific remediation action.

Suppression and debounce apply separately to each code. A healthy event clears the watch only when that GPU has no remaining reported incidents. Cache updates occur after successful delivery.
