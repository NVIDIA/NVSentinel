/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Chart URL default only — install versions are never defaulted; pass --*-version when installing.
const defaultNVSChart = "oci://ghcr.io/nvidia/nvsentinel"

// ---------------------------------------------------------------------------
// kube-prometheus-stack
// ---------------------------------------------------------------------------

func installMonitoring(ctx context.Context, c *clients, cfg Config) error {
	stepf("monitoring: kube-prometheus-stack")
	if err := c.ensureNamespace(ctx, cfg.MonitoringNamespace); err != nil {
		return err
	}
	if err := helmRepoAddUpdate(ctx, "prometheus-community", "https://prometheus-community.github.io/helm-charts"); err != nil {
		return err
	}
	vals, err := mergeValuesFiles(cfg.MonitoringValuesFiles)
	if err != nil {
		return err
	}

	ver := cfg.KPSChartVersion
	infof("installing/upgrading kube-prometheus-stack %s", ver)
	if _, err := helmUpgradeInstall(ctx,
		"prometheus", "prometheus-community/kube-prometheus-stack",
		cfg.MonitoringNamespace, ver, vals, false, 15*time.Minute,
	); err != nil {
		return err
	}
	if err := c.waitDeployRollout(ctx, cfg.MonitoringNamespace, "prometheus-operator", 5*time.Minute); err != nil {
		return err
	}
	if err := c.waitDeployRollout(ctx, cfg.MonitoringNamespace, "prometheus-kube-state-metrics", 5*time.Minute); err != nil {
		return err
	}
	infof("monitoring stack ready in namespace %s", cfg.MonitoringNamespace)
	return nil
}

// ---------------------------------------------------------------------------
// metrics-server
// ---------------------------------------------------------------------------

func installMetricsServer(ctx context.Context, c *clients, cfg Config) error {
	ver := cfg.MetricsServerVersion
	stepf("metrics-server: install %s", ver)
	manifest := fmt.Sprintf("https://github.com/kubernetes-sigs/metrics-server/releases/download/%s/components.yaml", ver)
	infof("applying metrics-server %s", ver)
	if err := c.applyYAMLURL(ctx, manifest); err != nil {
		return err
	}

	// Self-signed kubelet certs (Kind/bare): --kubelet-insecure-tls once.
	d, err := c.kube.AppsV1().Deployments("kube-system").Get(ctx, "metrics-server", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get metrics-server: %w", err)
	}
	if len(d.Spec.Template.Spec.Containers) > 0 {
		args := d.Spec.Template.Spec.Containers[0].Args
		has := false
		for _, a := range args {
			if a == "--kubelet-insecure-tls" {
				has = true
				break
			}
		}
		if !has {
			infof("enabling --kubelet-insecure-tls (self-signed kubelet serving certs)")
			patch := `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]`
			if _, err := c.kube.AppsV1().Deployments("kube-system").Patch(ctx, "metrics-server", types.JSONPatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
				return fmt.Errorf("patch metrics-server args: %w", err)
			}
		}
	}

	if err := c.waitDeployRollout(ctx, "kube-system", "metrics-server", 3*time.Minute); err != nil {
		return err
	}
	infof("waiting for metrics.k8s.io to serve node metrics")
	if err := waitUntil(ctx, 2*time.Minute, 10*time.Second, "metrics.k8s.io node metrics", func() error {
		return c.nodeMetricsAvailable(ctx)
	}); err != nil {
		return err
	}
	infof("metrics-server ready in namespace kube-system")
	return nil
}

// ---------------------------------------------------------------------------
// KWOK
// ---------------------------------------------------------------------------

func installKWOK(ctx context.Context, c *clients, cfg Config) error {
	ver := cfg.KWOKVersion
	stepf("KWOK: controller + stages (%s)", ver)
	base := "https://github.com/kubernetes-sigs/kwok/releases/download/" + ver

	infof("applying KWOK CRDs + controller")
	if err := c.applyYAMLURL(ctx, base+"/kwok.yaml"); err != nil {
		return err
	}
	infof("applying KWOK upstream default stages")
	if err := c.applyYAMLURL(ctx, base+"/stage-fast.yaml"); err != nil {
		return err
	}
	if err := c.waitDeployRollout(ctx, cfg.KWOKNamespace, "kwok-controller", 5*time.Minute); err != nil {
		return err
	}

	raw, err := readEmbedded("kwok/stages-custom.yaml")
	if err != nil {
		return err
	}
	delayMS := cfg.JobCompleteDelay * 1000
	if delayMS <= 0 {
		delayMS = 30_000
	}
	rendered := bytes.ReplaceAll(raw, []byte("__JOB_COMPLETE_DELAY__"), []byte(strconv.Itoa(delayMS)))
	infof("applying harness custom stages (job-complete delay %dms)", delayMS)
	if err := c.applyYAMLBytes(ctx, rendered, false); err != nil {
		return err
	}

	// Stub RuntimeClass: GPUReset Jobs set runtimeClassName=nvidia.
	infof("ensuring 'nvidia' RuntimeClass exists (GPUReset reset Job requires it)")
	rc := &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia"},
		Handler:    "nvidia",
	}
	if _, err := c.kube.NodeV1().RuntimeClasses().Create(ctx, rc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create RuntimeClass/nvidia: %w", err)
	}

	infof("KWOK ready")
	return nil
}

// ---------------------------------------------------------------------------
// cert-manager
// ---------------------------------------------------------------------------

func installCertManager(ctx context.Context, c *clients, cfg Config) error {
	ver := cfg.CertManagerVersion
	stepf("cert-manager: helm install %s", ver)
	if err := c.ensureNamespace(ctx, cfg.CertManagerNamespace); err != nil {
		return err
	}
	if err := helmRepoAddUpdate(ctx, "jetstack", "https://charts.jetstack.io"); err != nil {
		return err
	}
	vals := map[string]interface{}{}
	if err := mergeSet(vals, "crds.enabled=true"); err != nil {
		return err
	}
	infof("installing/upgrading cert-manager %s (with CRDs)", ver)
	if _, err := helmUpgradeInstall(ctx,
		"cert-manager", "jetstack/cert-manager",
		cfg.CertManagerNamespace, ver, vals, false, 10*time.Minute,
	); err != nil {
		return err
	}
	for _, d := range []string{"cert-manager", "cert-manager-webhook", "cert-manager-cainjector"} {
		if err := c.waitDeployRollout(ctx, cfg.CertManagerNamespace, d, 5*time.Minute); err != nil {
			return err
		}
	}
	infof("verifying the cert-manager webhook is admitting requests")
	if err := c.probeCertManagerWebhook(ctx, cfg.CertManagerNamespace, 2*time.Minute); err != nil {
		return err
	}
	infof("cert-manager ready in namespace %s", cfg.CertManagerNamespace)
	return nil
}

// dryRunCertManagerIssuer dry-runs a self-signed Issuer (webhook readiness probe).
func (c *clients) dryRunCertManagerIssuer(ctx context.Context, ns string) error {
	manifest := fmt.Sprintf(`apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: nvs-harness-cert-manager-probe
  namespace: %s
spec:
  selfSigned: {}
`, ns)
	return c.applyYAMLBytes(ctx, []byte(manifest), true)
}

// probeCertManagerWebhook waits until the webhook admits a dry-run Issuer.
func (c *clients) probeCertManagerWebhook(ctx context.Context, ns string, timeout time.Duration) error {
	return waitUntil(ctx, timeout, 5*time.Second, "cert-manager webhook admitting Issuers", func() error {
		return c.dryRunCertManagerIssuer(ctx, ns)
	})
}

// ---------------------------------------------------------------------------
// NVSentinel
// ---------------------------------------------------------------------------

func installNVSentinel(ctx context.Context, c *clients, cfg Config) error {
	stepf("NVSentinel: helm install")
	if err := c.ensureNamespace(ctx, cfg.NVSNamespace); err != nil {
		return err
	}
	if err := c.ensureCertManagerWebhookHealthy(ctx, cfg); err != nil {
		warnf("%v", err)
	}
	if err := c.ensureEventExporterOIDCSecret(ctx, cfg); err != nil {
		return err
	}

	chart := cfg.NVSChart
	ver := cfg.NVSChartVersion

	vals, err := mergeValuesFiles(cfg.NVSentinelValuesFiles)
	if err != nil {
		return err
	}

	if err := c.unlockPendingHelmRelease(ctx, "nvsentinel", cfg.NVSNamespace); err != nil {
		warnf("%v", err)
	}

	upgrade := func() (string, error) {
		infof("installing NVSentinel %s from %s", ver, chart)
		return helmUpgradeInstall(ctx, "nvsentinel", chart, cfg.NVSNamespace, ver, vals, true, 20*time.Minute)
	}

	infof("helm upgrade --install (attempt 1)")
	out, err := upgrade()
	if err != nil {
		if looksImmutableFieldError(out + err.Error()) {
			infof("immutable field change detected; recreating affected MongoDB objects")
			c.recreateMongoObjects(ctx, cfg.NVSNamespace)
			infof("helm upgrade --install (attempt 2, post-recovery)")
			if _, err2 := upgrade(); err2 != nil {
				return fmt.Errorf("helm upgrade failed after self-healing retry: %w", err2)
			}
		} else {
			return err
		}
	}

	for _, d := range []string{"fault-quarantine", "node-drainer", "fault-remediation"} {
		if err := c.waitDeployRollout(ctx, cfg.NVSNamespace, d, 5*time.Minute); err != nil {
			warnf("%s not ready (may be disabled or named differently in this chart version): %v", d, err)
		}
	}
	infof("NVSentinel installed in namespace %s", cfg.NVSNamespace)
	return nil
}

// ensureCertManagerWebhookHealthy probes the webhook; heals expired CA if needed.
func (c *clients) ensureCertManagerWebhookHealthy(ctx context.Context, cfg Config) error {
	ns := cfg.CertManagerNamespace
	if _, err := c.kube.AppsV1().Deployments(ns).Get(ctx, "cert-manager-webhook", metav1.GetOptions{}); err != nil {
		return nil // not installed here; nothing to heal
	}
	if c.dryRunCertManagerIssuer(ctx, ns) == nil {
		infof("cert-manager webhook healthy")
		return nil
	}
	warnf("cert-manager webhook not functional (commonly an expired webhook CA); regenerating CA + restarting webhook")
	_ = c.kube.CoreV1().Secrets(ns).Delete(ctx, "cert-manager-webhook-ca", metav1.DeleteOptions{})
	if err := c.rolloutRestart(ctx, ns, "cert-manager-webhook"); err != nil {
		warnf("restart cert-manager-webhook: %v", err)
	}
	_, _ = c.waitRolloutComplete(ctx, ns, "cert-manager-webhook", 2*time.Minute)

	for i := 0; i < 18; i++ {
		if c.dryRunCertManagerIssuer(ctx, ns) == nil {
			infof("cert-manager webhook healthy after regeneration")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("cert-manager webhook still failing after heal attempt; NVSentinel install may fail on cert-manager-gated resources")
}

// ensureEventExporterOIDCSecret seeds a placeholder so the pod can mount and Ready.
func (c *clients) ensureEventExporterOIDCSecret(ctx context.Context, cfg Config) error {
	const name = "event-exporter-oidc-secret"
	_, err := c.kube.CoreV1().Secrets(cfg.NVSNamespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		infof("event-exporter OIDC secret present; leaving as-is")
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	warnf("event-exporter OIDC secret %q missing; creating a PLACEHOLDER (harness only — real event egress will not function)", name)
	_, err = c.kube.CoreV1().Secrets(cfg.NVSNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.NVSNamespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{"oidc-client-secret": "placeholder"},
	}, metav1.CreateOptions{})
	return err
}

// unlockPendingHelmRelease rolls back a stuck pending-* release.
func (c *clients) unlockPendingHelmRelease(ctx context.Context, release, ns string) error {
	status, err := helmStatus(ctx, release, ns)
	if err != nil || status == "" {
		return nil // no release yet
	}
	if !strings.HasPrefix(status, "pending-") {
		return nil
	}
	warnf("release %s stuck in %q; rolling back to last deployed revision", release, status)
	if err := helmRollback(ctx, release, ns); err != nil {
		return fmt.Errorf("rollback failed; attempting the upgrade anyway: %w", err)
	}
	return nil
}

func looksImmutableFieldError(out string) bool {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "updates to statefulset"):
		return true
	case strings.Contains(low, "is immutable"):
		return true
	case strings.Contains(low, "cannot patch") && strings.Contains(low, "job"):
		return true
	case strings.Contains(low, "forbidden"):
		return true
	default:
		return false
	}
}

// recreateMongoObjects deletes MongoDB StatefulSets/Jobs so helm can recreate after immutable conflicts.
func (c *clients) recreateMongoObjects(ctx context.Context, ns string) {
	stsList, err := c.kube.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, sts := range stsList.Items {
			if !strings.Contains(strings.ToLower(sts.Name), "mongo") {
				continue
			}
			infof("  delete sts/%s --cascade=orphan", sts.Name)
			pol := metav1.DeletePropagationOrphan
			_ = c.kube.AppsV1().StatefulSets(ns).Delete(ctx, sts.Name, metav1.DeleteOptions{PropagationPolicy: &pol})
		}
	}
	jobs, err := c.kube.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, job := range jobs.Items {
			if !strings.Contains(strings.ToLower(job.Name), "mongo") {
				continue
			}
			infof("  delete job/%s", job.Name)
			_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, job.Name, metav1.DeleteOptions{})
		}
	}
}
