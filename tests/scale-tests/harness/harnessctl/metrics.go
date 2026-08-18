/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

func (c *clients) nodeMetricsAvailable(ctx context.Context) error {
	mc, err := metricsclient.NewForConfig(c.rest)
	if err != nil {
		return err
	}
	_, err = mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("node metrics: %w", err)
	}
	return nil
}
