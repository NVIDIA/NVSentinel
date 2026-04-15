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

### [Local Fault Injection Demo](local-fault-injection-demo/)

**What it shows:** GPU failure detection and automated node quarantine

**Requirements:** Docker, kubectl, kind, helm - **no GPU hardware needed**

**Time:** 5-10 minutes

**Best for:** Understanding how NVSentinel detects hardware failures and automatically protects your cluster by cordoning faulty nodes.

### [Local Slinky Drain Demo](local-slinky-drain-demo/)

**What it shows:** Custom drain extensibility using the Slinky Drainer plugin with scheduler integration

**Requirements:** Docker, kubectl, kind, helm, ko, go 1.25+ - **no GPU hardware needed**

**Time:** 5-10 minutes

**Best for:** Understanding how NVSentinel's node-drainer can delegate pod eviction to external controllers for custom drain workflows coordinated with HPC schedulers.


## Coming Soon

- Pod rescheduling and restarting from checkpointing

**Questions?** See the [main README](../README.md) or [open an issue](https://github.com/NVIDIA/NVSentinel/issues).

