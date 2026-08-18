/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// component is one installable stack piece (detect + install).
type component struct {
	id string
	// required: failed install aborts bring-up (monitoring/metrics-server are optional).
	required bool
	detect   func(ctx context.Context, c *clients, cfg Config) (present bool, detail string)
	install  func(ctx context.Context, c *clients, cfg Config) error
}

// stackComponents is the bringup install order.
func stackComponents() []component {
	return []component{
		{"kube-prometheus-stack", false, detectMonitoring, installMonitoring},
		{"metrics-server", false, detectMetricsServer, installMetricsServer},
		{"kwok", true, detectKwok, installKWOK},
		{"cert-manager", true, detectCertManager, installCertManager},
		{"nvsentinel", true, detectNVSentinel, installNVSentinel},
	}
}

// deployPresent reports whether any of the named Deployments exists in ns.
func (c *clients) deployPresent(ctx context.Context, ns string, names ...string) (bool, string) {
	for _, n := range names {
		if _, err := c.kube.AppsV1().Deployments(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			return true, fmt.Sprintf("deploy/%s in %s", n, ns)
		}
	}
	return false, ""
}

// dsPresent reports whether any of the named DaemonSets exists in ns.
func (c *clients) dsPresent(ctx context.Context, ns string, names ...string) (bool, string) {
	for _, n := range names {
		if _, err := c.kube.AppsV1().DaemonSets(ns).Get(ctx, n, metav1.GetOptions{}); err == nil {
			return true, fmt.Sprintf("ds/%s in %s", n, ns)
		}
	}
	return false, ""
}

// detectMonitoring reports PRESENT only when the Helm chart version matches --kps-chart-version.
func detectMonitoring(ctx context.Context, c *clients, cfg Config) (bool, string) {
	// fullnameOverride=prometheus → prometheus-operator; other names = stock install.
	present, detail := c.deployPresent(ctx, cfg.MonitoringNamespace,
		"prometheus-operator", "prometheus-kube-prometheus-operator", "kube-prometheus-stack-operator")
	if !present {
		return false, ""
	}
	installed, err := helmChartVersion("prometheus", cfg.MonitoringNamespace)
	if err != nil {
		warnf("  kube-prometheus-stack: could not read Helm chart version: %v", err)
	}
	if installed == "" {
		// ArgoCD / external Helm: release may not be in local helm storage.
		installed = c.deployLabel(ctx, cfg.MonitoringNamespace, "prometheus-operator", "app.kubernetes.io/version")
		if installed == "" {
			if chart := c.deployLabel(ctx, cfg.MonitoringNamespace, "prometheus-operator", "chart"); strings.HasPrefix(chart, "kube-prometheus-stack-") {
				installed = strings.TrimPrefix(chart, "kube-prometheus-stack-")
			}
		}
	}
	return gateVersion("kube-prometheus-stack", cfg.KPSChartVersion, installed, detail)
}

func detectKwok(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.KWOKNamespace, "kwok-controller")
	if !present {
		return false, ""
	}
	return gateVersion("kwok", cfg.KWOKVersion,
		c.deployImageTag(ctx, cfg.KWOKNamespace, "kwok-controller"), detail)
}

// detectMetricsServer looks for metrics-server in kube-system.
func detectMetricsServer(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, "kube-system", "metrics-server")
	if !present {
		return false, ""
	}
	return gateVersion("metrics-server", cfg.MetricsServerVersion,
		c.deployImageTag(ctx, "kube-system", "metrics-server"), detail)
}

func detectCertManager(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.CertManagerNamespace, "cert-manager-webhook", "cert-manager")
	if !present {
		return false, ""
	}
	installed, err := helmChartVersion("cert-manager", cfg.CertManagerNamespace)
	if err != nil {
		warnf("  cert-manager: could not read Helm chart version: %v", err)
	}
	if installed == "" {
		installed = c.deployImageTag(ctx, cfg.CertManagerNamespace, "cert-manager", "cert-manager-controller", "cert-manager-webhook")
	}
	return gateVersion("cert-manager", cfg.CertManagerVersion, installed, detail)
}

// detectNVSentinel reports PRESENT only when installed chart/image version matches --nvs-chart-version.
func detectNVSentinel(ctx context.Context, c *clients, cfg Config) (bool, string) {
	present, detail := c.deployPresent(ctx, cfg.NVSNamespace,
		"health-events-analyzer", "fault-quarantine", "node-drainer", "fault-remediation")
	if !present {
		if ok, d := c.dsPresent(ctx, cfg.NVSNamespace, "platform-connectors"); ok {
			present, detail = true, d
		}
	}
	if !present {
		return false, ""
	}
	installed, err := helmChartVersion("nvsentinel", cfg.NVSNamespace)
	if err != nil {
		warnf("  nvsentinel: could not read Helm chart version: %v", err)
	}
	if installed == "" {
		installed = c.deployImageTag(ctx, cfg.NVSNamespace,
			"fault-quarantine", "health-events-analyzer", "node-drainer", "fault-remediation")
	}
	return gateVersion("nvsentinel", cfg.NVSChartVersion, installed, detail)
}

// gateVersion reports PRESENT only when installed matches the required target version.
func gateVersion(id, target, installed, detail string) (bool, string) {
	target = strings.TrimSpace(target)
	installed = strings.TrimSpace(installed)
	if installed != "" && versionEqual(installed, target) {
		return true, detail + ", version " + installed
	}
	if installed == "" {
		warnf("  %s present but version unknown (want %s) -> will install/upgrade", id, target)
	} else {
		warnf("  %s present at %s but target is %s -> will upgrade", id, installed, target)
	}
	return false, ""
}

func versionEqual(a, b string) bool {
	if a == b {
		return true
	}
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// deployLabel returns a Deployment metadata label value, or "".
func (c *clients) deployLabel(ctx context.Context, namespace, name, key string) string {
	d, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return d.Labels[key]
}

// deployImageTag returns the container image tag of the first of the named
// Deployments that exists in namespace, or "" if none is found / all untagged.
func (c *clients) deployImageTag(ctx context.Context, namespace string, names ...string) string {
	for _, name := range names {
		d, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		fallback := ""
		for _, ct := range d.Spec.Template.Spec.Containers {
			tag := imageTag(ct.Image)
			if tag == "" {
				continue
			}
			if ct.Name == name {
				return tag
			}
			if fallback == "" {
				fallback = tag
			}
		}
		if fallback != "" {
			return fallback
		}
	}
	return ""
}

// imageTag returns the tag portion of a container image ref (after the final
// ':', ignoring a registry port and any @digest), or "" if untagged.
func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}

func detectJanitor(ctx context.Context, c *clients, cfg Config) (bool, string) {
	if ok, d := c.deployPresent(ctx, cfg.NVSNamespace, "janitor"); ok {
		return true, d
	}
	// Platform-managed janitor may live in its own namespace.
	return c.deployPresent(ctx, cfg.JanitorNamespace, "dgxc-janitor-controller-manager")
}

// runBringup detects the stack, installs anything missing/mismatched, then prints node inventory.
func runBringup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack bringup", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindMonitoringNamespaceFlag(fs, &cfg)
	bindCertManagerNamespaceFlag(fs, &cfg)
	bindKwokNamespaceFlag(fs, &cfg)
	bindJanitorNamespaceFlag(fs, &cfg)
	bindVersionFlags(fs, &cfg)
	bindValuesFilesFlag(fs, &cfg)
	bindJobCompleteDelayFlag(fs, &cfg)
	_ = fs.Parse(args)
	if err := requireBringupFlags(cfg); err != nil {
		return err
	}

	c, err := newClients()
	if err != nil {
		return err
	}

	stepf("bring-up: verifying cluster reachability")
	ver, err := c.kube.Discovery().ServerVersion()
	if err != nil {
		return fmt.Errorf("cannot reach cluster: %w", err)
	}
	infof("cluster reachable: kubernetes %s", ver.GitVersion)

	stepf("bring-up: detecting components")
	comps := stackComponents()
	present := make([]bool, len(comps))
	for i, comp := range comps {
		ok, detail := comp.detect(ctx, c, cfg)
		present[i] = ok
		if ok {
			infof("  %-22s PRESENT  (%s)", comp.id, detail)
		} else {
			infof("  %-22s MISSING", comp.id)
		}
		// Janitor is in-chart; show under nvsentinel.
		if comp.id == "nvsentinel" {
			if jok, jd := detectJanitor(ctx, c, cfg); jok {
				infof("    - janitor (in-chart)  PRESENT  (%s)", jd)
			} else {
				infof("    - janitor (in-chart)  MISSING")
			}
		}
	}

	var todo []component
	for i, comp := range comps {
		if !present[i] {
			todo = append(todo, comp)
		}
	}
	if len(todo) == 0 {
		infof("nothing to install; all stack components already at requested versions")
	} else {
		stepf("bring-up: installing %d component(s)", len(todo))
		for _, comp := range todo {
			if err := comp.install(ctx, c, cfg); err != nil {
				if comp.required {
					return fmt.Errorf("%s: %w", comp.id, err)
				}
				warnf("optional component %s failed to install: %v — continuing", comp.id, err)
			}
		}
	}

	stepf("bring-up: verifying nodes")
	real, kwok, err := c.nodeInventory(ctx)
	if err != nil {
		return err
	}
	infof("nodes: %d real, %d kwok", real, kwok)
	if real < 1 {
		warnf("no real (non-kwok) nodes detected; check the 'type' label convention")
	}
	infof("bring-up complete")
	return nil
}

// nodeInventory returns the count of real vs. simulated (KWOK) nodes.
func (c *clients) nodeInventory(ctx context.Context) (real, kwok int, err error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes.Items {
		if n.Labels["type"] == "kwok" {
			kwok++
		} else {
			real++
		}
	}
	return real, kwok, nil
}

func runScaleNodes(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nodes scale", flag.ExitOnError)
	cfg := defaultConfig()
	bindResultsFlag(fs, &cfg)
	bindPromFlags(fs, &cfg)
	bindNodeReadyFlag(fs, &cfg)
	bindNodeShapeFlags(fs, &cfg)
	count := fs.Int("count", cfg.NodeCount, "target KWOK node count (required, e.g. --count 10000)")
	_ = fs.Parse(args)
	if *count <= 0 {
		return fmt.Errorf("--count is required: pass the target KWOK node count, e.g. --count 10000")
	}
	cfg.NodeCount = *count

	c, err := newClients()
	if err != nil {
		return err
	}
	rs := newResultSet(cfg.ResultsDir)
	res := checkScaleNodes(ctx, c, cfg)
	rs.add(res)
	_ = rs.write()
	if !res.passed() {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

// checkScaleNodes creates nodes, waits Ready, and records the ceiling.
func checkScaleNodes(ctx context.Context, c *clients, cfg Config) CheckResult {
	started := time.Now()
	stepf("nodes scale: simulated nodes to %d", cfg.NodeCount)

	created, existing, failed := c.scaleNodes(ctx, cfg)
	infof("scale: created=%d existing=%d failed=%d", created, existing, failed)

	ready, ok := c.waitNodesReady(ctx, cfg.NodeCount, time.Duration(cfg.NodeReadyTO)*time.Second,
		func() error { return c.restartKwokController(ctx, cfg) })
	elapsed := time.Since(started)

	p99, p99ok := c.promInstantQuery(ctx, cfg, apiserverP99Query)
	util := c.clusterNodeUtil(ctx)
	clusterDetail := util.summary()

	res := CheckResult{
		ID:       "nodes-scale",
		Name:     "node ceiling",
		Started:  started,
		Finished: time.Now(),
		Metrics: map[string]any{
			"target_nodes":            cfg.NodeCount,
			"ready_nodes":             ready,
			"created":                 created,
			"failed":                  failed,
			"time_to_ready_seconds":   elapsed.Seconds(),
			"apiserver_p99_seconds":   fmtFloat(p99, p99ok),
			"cluster_cpu_pct":         util.CPUPct,
			"cluster_mem_pct":         util.MemPct,
			"cluster_cpu_used_cores":  util.CPUUsedCores,
			"cluster_mem_used_mi":     util.MemUsedMi,
			"cluster_real_nodes":      util.RealNodes,
			"cluster_metrics_present": util.OK,
		},
	}
	writeArtifact(cfg.ResultsDir, "node-ceiling.json", res.Metrics)
	infof("cluster resources: %s", clusterDetail)

	if !ok {
		res.Verdict = "FAIL"
		res.Message = fmt.Sprintf("only %d/%d nodes Ready within %ds — attribute what saturated first (kwok controller vs api server/etcd)",
			ready, cfg.NodeCount, cfg.NodeReadyTO)
	} else {
		res.Verdict = "PASS"
		res.Message = fmt.Sprintf("ready=%d/%d in %.0fs, apiserver p99=%s s, %s",
			ready, cfg.NodeCount, elapsed.Seconds(), fmtFloat(p99, p99ok), clusterDetail)
	}
	return res
}
