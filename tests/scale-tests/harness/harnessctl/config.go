/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// Config holds harness settings. Values come from defaultConfig() and CLI flags.
type Config struct {
	NVSNamespace         string
	MonitoringNamespace  string
	JanitorNamespace     string
	CertManagerNamespace string
	KWOKNamespace        string

	NVSChartVersion string
	NVSChart        string

	NVSentinelValuesFiles []string
	MonitoringValuesFiles []string

	KWOKVersion          string
	CertManagerVersion   string
	MetricsServerVersion string
	KPSChartVersion      string

	NodeCount        int
	NodePrefix       string
	NodeBatch        int
	GPUCount         int
	NodeCPU          string
	NodeMemory       string
	NodeMaxPods      int
	ProviderIDScheme string

	NodeReadyTO int

	JobCompleteDelay int

	MonPromSvc  string
	MonPromPort string

	ResultsDir string
}

func defaultConfig() Config {
	return Config{
		NVSNamespace:         "nvsentinel",
		MonitoringNamespace:  "prometheus",
		JanitorNamespace:     "dgxc-janitor-system",
		CertManagerNamespace: "cert-manager",
		KWOKNamespace:        "kube-system",
		NVSChart:             defaultNVSChart,

		NodePrefix:  "kwok-gpu",
		NodeBatch:   500,
		GPUCount:    8,
		NodeCPU:     "192",
		NodeMemory:  "2048Gi",
		NodeMaxPods: 110,

		NodeReadyTO: 1800,

		JobCompleteDelay: 30,

		MonPromSvc:  "prometheus-prometheus",
		MonPromPort: "9090",
		// Outside the checkout by default so local runs never drop artifacts in-tree.
		ResultsDir: filepath.Join(os.TempDir(), "nvsentinel-harness", "results"),
	}
}

func bindNvsNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NVSNamespace, "nvs-namespace", c.NVSNamespace, "NVSentinel namespace")
}

func bindMonitoringNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.MonitoringNamespace, "monitoring-namespace", c.MonitoringNamespace, "kube-prometheus-stack namespace")
}

func bindKwokNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.KWOKNamespace, "kwok-namespace", c.KWOKNamespace, "KWOK controller namespace")
}

func bindJanitorNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.JanitorNamespace, "janitor-namespace", c.JanitorNamespace, "janitor controller namespace (detection only; install is in-chart with NVSentinel)")
}

func bindCertManagerNamespaceFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.CertManagerNamespace, "cert-manager-namespace", c.CertManagerNamespace, "cert-manager namespace")
}

func bindResultsFlag(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.ResultsDir, "results-dir", c.ResultsDir, "directory for JSON/JUnit artifacts (default: $TMPDIR/nvsentinel-harness/results)")
}

func bindPromFlags(fs *flag.FlagSet, c *Config) {
	bindMonitoringNamespaceFlag(fs, c)
	fs.StringVar(&c.MonPromSvc, "prom-service", c.MonPromSvc, "Prometheus service name")
	fs.StringVar(&c.MonPromPort, "prom-port", c.MonPromPort, "Prometheus service port")
}

func bindNodeReadyFlag(fs *flag.FlagSet, c *Config) {
	fs.IntVar(&c.NodeReadyTO, "node-ready-timeout", c.NodeReadyTO, "seconds to wait for nodes to become Ready")
}

func bindVersionFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NVSChart, "nvs-chart", c.NVSChart, "NVSentinel Helm chart reference (OCI or repo/chart)")
	fs.StringVar(&c.NVSChartVersion, "nvs-chart-version", c.NVSChartVersion, "NVSentinel version (required for stack bringup)")
	fs.StringVar(&c.KWOKVersion, "kwok-version", c.KWOKVersion, "KWOK version (required for stack bringup)")
	fs.StringVar(&c.CertManagerVersion, "cert-manager-version", c.CertManagerVersion, "cert-manager version (required for stack bringup)")
	fs.StringVar(&c.MetricsServerVersion, "metrics-server-version", c.MetricsServerVersion, "metrics-server version (required for stack bringup)")
	fs.StringVar(&c.KPSChartVersion, "kps-chart-version", c.KPSChartVersion, "kube-prometheus-stack chart version (required for stack bringup)")
}

type valuesFilesFlag []string

func (v *valuesFilesFlag) String() string { return strings.Join(*v, ",") }
func (v *valuesFilesFlag) Set(s string) error {
	*v = append(*v, s)
	return nil
}

func bindValuesFilesFlag(fs *flag.FlagSet, c *Config) {
	fs.Var((*valuesFilesFlag)(&c.NVSentinelValuesFiles), "nvsentinel-values", "NVSentinel Helm values YAML (required, repeatable; deep-merged in order)")
	fs.Var((*valuesFilesFlag)(&c.MonitoringValuesFiles), "monitoring-values", "kube-prometheus-stack Helm values YAML (required, repeatable; deep-merged in order)")
}

func bindNodeShapeFlags(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.NodePrefix, "node-prefix", c.NodePrefix, "simulated node name prefix")
	fs.IntVar(&c.NodeBatch, "node-batch", c.NodeBatch, "node creation batch size")
	fs.IntVar(&c.GPUCount, "gpu-count", c.GPUCount, "GPUs advertised per simulated node")
	fs.StringVar(&c.NodeCPU, "node-cpu", c.NodeCPU, "CPU advertised per simulated node")
	fs.StringVar(&c.NodeMemory, "node-memory", c.NodeMemory, "memory advertised per simulated node")
	fs.IntVar(&c.NodeMaxPods, "node-max-pods", c.NodeMaxPods, "max pods advertised per simulated node")
	fs.StringVar(&c.ProviderIDScheme, "provider-id-scheme", c.ProviderIDScheme, "spec.providerID scheme for KWOK nodes (empty = none; set on managed clusters)")
}

func bindJobCompleteDelayFlag(fs *flag.FlagSet, c *Config) {
	fs.IntVar(&c.JobCompleteDelay, "job-complete-delay", c.JobCompleteDelay, "seconds before KWOK completes janitor Job pods")
}
