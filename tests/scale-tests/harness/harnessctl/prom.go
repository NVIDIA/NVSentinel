/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// promInstantQuery runs a Prometheus instant query via the API server service
// proxy. Returns (0, false) when there is no usable sample.
func (c *clients) promInstantQuery(ctx context.Context, cfg Config, query string) (float64, bool) {
	raw, err := c.kube.CoreV1().
		Services(cfg.MonitoringNamespace).
		ProxyGet("http", cfg.MonPromSvc, cfg.MonPromPort, "/api/v1/query", map[string]string{"query": query}).
		DoRaw(ctx)
	if err != nil {
		warnf("prometheus query failed (%s:%s in %s): %v", cfg.MonPromSvc, cfg.MonPromPort, cfg.MonitoringNamespace, err)
		return 0, false
	}

	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		warnf("prometheus response parse error: %v", err)
		return 0, false
	}
	if resp.Status != "success" || len(resp.Data.Result) == 0 {
		return 0, false
	}
	val := resp.Data.Result[0].Value
	if len(val) != 2 {
		return 0, false
	}
	s, ok := val[1].(string)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

const apiserverP99Query = `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])) by (le))`

func fmtFloat(f float64, ok bool) string {
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%.3f", f)
}
