# NVSentinel Demos

Interactive demonstrations of NVSentinel's core capabilities.

## Demo Videos

<table>
<tr>
<td align="center" width="50%">
<a href="https://youtu.be/6HHYMF-YfqY">
<img src="https://img.youtube.com/vi/6HHYMF-YfqY/hqdefault.jpg" alt="End-to-End Fault Detection & Remediation" width="100%"/>
<br/><b>End-to-End Fault Detection & Remediation</b>
</a>
<br/>Full pipeline: health monitoring, fault detection, quarantine, drain, and breakfix
</td>
<td align="center" width="50%">
<a href="https://youtu.be/0qmrHUmxNPQ">
<img src="https://img.youtube.com/vi/0qmrHUmxNPQ/hqdefault.jpg" alt="Custom Health Monitors" width="100%"/>
<br/><b>Custom Health Monitors</b>
</a>
<br/>Building your own GPU health monitor using the gRPC interface
</td>
</tr>
<tr>
<td align="center" width="50%">
<a href="https://youtu.be/G1j4NV5IMkY">
<img src="https://img.youtube.com/vi/G1j4NV5IMkY/hqdefault.jpg" alt="Custom Drain Plugins" width="100%"/>
<br/><b>Custom Drain Plugins</b>
</a>
<br/>Slinky integration for coordinated drain of HPC workloads
</td>
<td align="center" width="50%">
<a href="https://youtu.be/VVAtON7ERHQ">
<img src="https://img.youtube.com/vi/VVAtON7ERHQ/hqdefault.jpg" alt="Extensible Remediation" width="100%"/>
<br/><b>Extensible Remediation</b>
</a>
<br/>Bringing your own breakfix system or remediation operator
</td>
</tr>
<tr>
<td align="center" width="50%">
<a href="https://youtu.be/kwWnC0SEFEI">
<img src="https://img.youtube.com/vi/kwWnC0SEFEI/hqdefault.jpg" alt="Health Events Analyzer" width="100%"/>
<br/><b>Health Events Analyzer</b>
</a>
<br/>Identifying and removing bad GPU nodes from the cluster
</td>
<td></td>
</tr>
</table>

## Interactive Demos

Run these locally on your laptop — no GPU hardware needed.

Each one installs the current NVSentinel release, resolved from the chart's OCI repository at
run time, so a clone of this repo stays current however old the clone is. Pin with
`NVSENTINEL_CHART_VERSION=v1.23.0` if you need a specific one.

> These are two node KIND clusters, which is smaller than anything NVSentinel is tuned for.
> Each demo therefore disables fault quarantine's circuit breaker: it measures itself against
> GPU-labelled nodes, and cordoning 1 node out of 2 is already its default trip threshold.
> Those overrides exist for the demo cluster only; keep the defaults on real clusters.

### [Local Fault Injection Demo](local-fault-injection-demo/)

**What it shows:** The full pipeline — GPU fault detection, node quarantine, workload drain, repair request, and automatic recovery

**Requirements:** Docker, kubectl, kind, helm, jq, curl - **no GPU hardware needed**

**Time:** 10-15 minutes

**Best for:** Seeing what NVSentinel actually does end to end. A workload is running when the GPU breaks, and you watch it get moved off, the node repaired, and the node returned to service with nothing typed in between.

### [Local Slinky Drain Demo](local-slinky-drain-demo/)

**What it shows:** Custom drain extensibility — node-drainer hands the eviction to an external scheduler instead of doing it itself

**Requirements:** Docker, kubectl, kind, helm, ko, go 1.25+, jq, curl - **no GPU hardware needed**

**Time:** 10-15 minutes

**Best for:** Understanding how NVSentinel delegates pod eviction to external controllers, so an HPC scheduler decides when a job can be interrupted. Injects a real XID into a fake DCGM hostengine, so detection runs on the production code path.


### [Local Custom Remediation Demo](local-custom-remediation-demo/)

**What it shows:** Custom remediation actions — a custom health monitor reports a fault NVSentinel has no check for, and a third-party controller repairs it

**Requirements:** Docker, kubectl, kind, helm, go 1.25+, jq, curl - **no GPU hardware needed**

**Time:** 10-15 minutes

**Best for:** Understanding how to extend NVSentinel beyond GPU faults to any hardware or system fault — writing a health monitor, routing `CUSTOM` actions to your own CRD, and what a controller has to write back.

## Coming Soon

- Pod rescheduling and restarting from checkpointing

**Questions?** See the [main README](../README.md) or [open an issue](https://github.com/NVIDIA/NVSentinel/issues).

