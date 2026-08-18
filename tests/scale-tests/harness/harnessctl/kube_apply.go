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
	"io"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
)

// applyYAML applies multi-doc YAML via dynamic client (SSA, create/update fallback).
func (c *clients) applyYAML(ctx context.Context, r io.Reader, dryRun bool) error {
	mapper, err := c.restMapper()
	if err != nil {
		return err
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for {
		var raw map[string]interface{}
		if err := dec.Decode(&raw); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode yaml: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		obj := &unstructured.Unstructured{Object: raw}
		if err := c.applyUnstructured(ctx, mapper, obj, dryRun); err != nil {
			return err
		}
	}
}

// applyYAMLBytes is applyYAML for an in-memory buffer.
func (c *clients) applyYAMLBytes(ctx context.Context, b []byte, dryRun bool) error {
	return c.applyYAML(ctx, bytes.NewReader(b), dryRun)
}

const yamlFetchTimeout = 30 * time.Second

// applyYAMLURL fetches a remote manifest URL and applies it.
func (c *clients) applyYAMLURL(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: yamlFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("fetch %s: HTTP %s: %s", url, resp.Status, bytes.TrimSpace(body))
	}
	return c.applyYAML(ctx, resp.Body, false)
}

func (c *clients) restMapper() (meta.RESTMapper, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(c.rest)
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)), nil
}

func (c *clients) applyUnstructured(ctx context.Context, mapper meta.RESTMapper, obj *unstructured.Unstructured, dryRun bool) error {
	gvk := obj.GroupVersionKind()
	if gvk.Empty() {
		return fmt.Errorf("object missing apiVersion/kind")
	}
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return fmt.Errorf("rest mapping %s: %w", gvk.String(), err)
	}
	var dr dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		dr = c.dynamic.Resource(mapping.Resource).Namespace(ns)
	} else {
		dr = c.dynamic.Resource(mapping.Resource)
	}

	name := obj.GetName()
	data, err := obj.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	patchOpts := metav1.PatchOptions{
		FieldManager: "harnessctl",
		Force:        &force,
	}
	if dryRun {
		patchOpts.DryRun = []string{metav1.DryRunAll}
	}

	_, err = dr.Patch(ctx, name, types.ApplyPatchType, data, patchOpts)
	if err == nil {
		if !dryRun {
			infof("applied %s %s", gvk.Kind, namespacedName(obj))
		}
		return nil
	}

	if dryRun {
		_, createErr := dr.Create(ctx, obj, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if createErr == nil || apierrors.IsAlreadyExists(createErr) {
			return nil
		}
		return createErr
	}

	existing, getErr := dr.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(getErr) {
		_, err = dr.Create(ctx, obj, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create %s %s: %w", gvk.Kind, namespacedName(obj), err)
		}
		infof("created %s %s", gvk.Kind, namespacedName(obj))
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("apply %s %s: patch failed (%v); get failed (%w)", gvk.Kind, namespacedName(obj), err, getErr)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = dr.Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("update %s %s: %w", gvk.Kind, namespacedName(obj), err)
	}
	infof("updated %s %s", gvk.Kind, namespacedName(obj))
	return nil
}

func namespacedName(obj *unstructured.Unstructured) string {
	if obj.GetNamespace() != "" {
		return obj.GetNamespace() + "/" + obj.GetName()
	}
	return obj.GetName()
}
