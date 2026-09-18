// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package central

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/auth"
)

// deploymentAuthSettings reads the node-binding settings from the shared
// config.json and checks the one the deployment role cannot do without: node
// binding must be on, since every caller must present a token and there is no
// local node to pin a tokenless caller to.
func deploymentAuthSettings(raw map[string]any) (auth.Settings, error) {
	settings, err := auth.SettingsFromConfig(raw)
	if err != nil {
		return auth.Settings{}, fmt.Errorf("node-binding auth settings: %w", err)
	}

	if !settings.Enabled {
		return auth.Settings{}, errors.New("enableNodeBindingAuth must be true for the deployment platform " +
			"connector: every caller presents a token (set global.platformConnectorAuth.enabled)")
	}

	return settings, nil
}

// newValidator builds the TokenReview validator for the audience the shared
// config.json names, with the builder the DaemonSet uses, sized for a call on
// the path of every batch of the fleet rather than one node's callers.
func newValidator(cfg *config, audience string) (*grpcauth.Validator, error) {
	// Every batch of the fleet authenticates; a line per success would be
	// most of the log.
	opts := []grpcauth.ValidatorOption{grpcauth.WithSuccessLogLevel(slog.LevelDebug)}

	if cfg.tokenCacheSize > 0 {
		opts = append(opts, grpcauth.WithCacheSize(cfg.tokenCacheSize))
	}

	return auth.NewTokenReviewValidator("", audience, cfg.tokenReviewQPS, cfg.tokenReviewBurst, opts...)
}

// newAuthInterceptor builds caller authentication for the deployment role
// from the node-binding interceptor the DaemonSet uses, in its configuration
// without a local node: every caller must present a pod-bound token, its
// events are pinned to the node the token claims, and the listed cross-node
// publishers may name any node. AuthMode audit and AuthFailOpenOnUnavailable
// are socket settings: with no local node to fall back on the deployment role
// always enforces. The interceptor validates the cross-node list: a malformed
// username refuses to start.
func newAuthInterceptor(
	ctx context.Context, settings auth.Settings, validator auth.TokenValidator,
) (grpc.UnaryServerInterceptor, error) {
	if settings.Mode == auth.ModeAudit || settings.FailOpenOnUnavailable {
		slog.WarnContext(ctx, "AuthMode audit and AuthFailOpenOnUnavailable apply to the node-local role only; "+
			"the deployment platform connector enforces node binding")
	}

	interceptor, err := auth.NewNodeBindingInterceptor(auth.Config{
		Validator:                validator,
		CrossNodeServiceAccounts: settings.CrossNodeServiceAccounts,
		Mode:                     auth.ModeEnforce,
	})
	if err != nil {
		return nil, fmt.Errorf("AuthCrossNodeServiceAccounts: %w", err)
	}

	return interceptor, nil
}
