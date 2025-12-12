# NVSentinel Demos

Interactive demonstrations of NVSentinel's core capabilities that run locally on your laptop.

## Available Demos

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

