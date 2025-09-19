# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load('ext://helm_resource', 'helm_resource', 'helm_repo')
load('ext://namespace', 'namespace_create', 'namespace_inject')

update_settings(k8s_upsert_timeout_secs=600)

helm_repo('jetstack', 'https://charts.jetstack.io')
helm_resource(
    'cert-manager',
    chart='jetstack/cert-manager',
    namespace='cert-manager',
    flags=[
        '--create-namespace',
        '--set=installCRDs=true',
    ],
)

helm_repo('prometheus-community', 'https://prometheus-community.github.io/helm-charts')
helm_resource(
    'prometheus-operator',
    chart='prometheus-community/kube-prometheus-stack',
    namespace='monitoring',
    flags=[
        '--create-namespace',
        '--set=prometheus.enabled=false',
        '--set=alertmanager.enabled=false',
        '--set=grafana.enabled=false',
        '--set=kubeStateMetrics.enabled=false',
        '--set=nodeExporter.enabled=false',
        '--set=prometheusOperator.enabled=true',
    ],
)

namespace_create('nvsentinel')

include('./fault-quarantine-module/Tiltfile')
include('./fault-remediation-module/Tiltfile')
include('./node-drainer-module/Tiltfile')
include('./platform-connectors/Tiltfile')
include('./health-monitors/gpu-health-monitor/Tiltfile')
include('./health-monitors/nvswitch-health-monitor/Tiltfile')
include('./health-monitors/syslog-health-monitor/Tiltfile')

yaml = helm(
    './distros/kubernetes/nvsentinel',
    name='nvsentinel',
    namespace='nvsentinel',
    values=['./distros/kubernetes/nvsentinel/values.yaml', './distros/kubernetes/nvsentinel/values-tilt.yaml'],
)
k8s_yaml(yaml)

k8s_resource(
    new_name='cert-manager-resources',
    objects=[
        'mongo-root-ca:certificate',
        'mongo-ca-issuer:issuer', 
        'selfsigned-ca-issuer:issuer',
        'mongo-server-cert-0:certificate',
        'mongo-server-cert-1:certificate',
        'mongo-server-cert-2:certificate',
        'mongo-app-client-cert:certificate',
        'mongo-dgxcops-client-cert:certificate'
    ],
    resource_deps=['cert-manager'],
)
k8s_resource(
    new_name='prometheus-resources',
    objects=['nvsentinel-pod-monitor:podmonitor'],
    resource_deps=['prometheus-operator'],
)
