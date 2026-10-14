# NVSentinel Adopters

This is a non-comprehensive list of organizations and projects that publicly use —
or build on — NVSentinel.

💡 **Want to be listed?** Open a pull request adding a row to the table below with
your organization's name, a link, and a brief description of how you use or build
on NVSentinel. Entries are ordered alphabetically. See [CONTRIBUTING.md](CONTRIBUTING.md)
for the PR process.

## Adopters

| Organization | Success Story |
|:-------------|:--------------|
| [SK Telecom](https://petasus.ai/) | Uses NVSentinel in the GPU clusters of **Petasus AI Cloud**, a KubeVirt-based GPU cloud in which every Kubernetes cluster is provisioned with Cluster API. Built and open-sourced **[nvsentinel-capi-remediator](https://github.com/petasus-ai/nvsentinel-capi-remediator)**: one controller in the management cluster consumes the `ExternalRemediationRequest`s of every workload cluster, has Cluster API restart the VM or replace the Machine as NVSentinel recommends, and reports each restart's outcome, or why it declined, through `ExternalRemediationComplete`. GPU resets stay with NVSentinel's janitor. |
