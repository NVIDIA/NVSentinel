# SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: LicenseRef-NvidiaProprietary
#
# NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
# property and proprietary rights in and to this material, related
# documentation and any modifications thereto. Any use, reproduction,
# disclosure or distribution of this material and related documentation
# without an express license agreement from NVIDIA CORPORATION or
# its affiliates is strictly prohibited.
import logging
import os
import re
import shlex
import subprocess
import threading
import time
import shutil
import yaml
import json
from typing import Any, Dict, List, Optional, TypeVar, Generic
from datetime import datetime
from kubernetes import client, config
from kubernetes.client.rest import ApiException
from kubernetes.stream import stream, portforward
import pytest

LOGGER = logging.getLogger(__name__)

# Define type variables for CommonResult, used for function return signature.
T = TypeVar("T")
E = TypeVar("E")


class CommonResult(Generic[T, E]):
    """
    A class to represent a common return value that behaves like a tuple.

    Attributes
    ----------
    values : tuple
        A tuple containing the values stored in the Result object.

    Methods
    -------
    __getitem__(index):
        Returns the value at the specified index.
    __len__():
        Returns the number of elements in the Result object.
    __iter__():
        Returns an iterator over the values.
    __repr__():
        Returns a string representation of the Result object.
    __eq__(other):
        Compares the Result object with another Result object for equality.
    __ne__(other):
        Compares the Result object with another Result object for inequality.
    __bool__():
        Allows direct boolean evaluation of the result. Returns True if operation was successful (no error message and data exists), False otherwise.
        Example:
            result = client.list_namespaces()
            if result:
                namespaces = result.data# List[client.V1Namespace]
            else:
                print(f"Failed to list namespaces: {result.error_msg}")
            # or
            if not result:
                print(f"Failed to list namespaces: {result.error_msg}")
    __str__():
        Returns a string representation of the Result object.
        Example:
            result = client.list_namespaces()
            print(result)
            # Output:
            # Result: Success (items: 1, preview: ['default'])
            # or
            # Result: Failed (Error: Failed to list namespaces: [Errno 2] No such file or directory: '~/.kube/config')
    """

    def __init__(self, data: T, err_msg: E = None):
        self._values = (data, err_msg)

    def __getitem__(self, index):
        return self._values[index]

    def __len__(self):
        return len(self._values)

    def __iter__(self):
        return iter(self._values)

    def __repr__(self):
        return f"Result{self._values}"

    def __eq__(self, other):
        if isinstance(other, CommonResult):
            return self._values == other._values
        return False

    def __ne__(self, other):
        return not self.__eq__(other)

    @property
    def values(self):
        return self._values

    def __str__(self) -> str:
        """
        String representation of the CommonResult.

        Returns:
            str: A human-readable string showing the result value and any error

        The CommonResult structure has data as the first item and
        error message as the second item.
        """
        # Check if we have an error
        if self.values[1]:
            # If we have an error, show the error message
            error_info = f"Error: {self.values[1]}"
            return f"Result: Failed ({error_info})"
        else:
            # If no error, show information about the data
            data = self.values[0]  # Using direct index since we know structure

            if isinstance(data, (list, tuple)):
                data_count = len(data)
                if data_count > 0:
                    # Show count and a preview of the first few items
                    preview = str(data[:3])
                    if data_count > 3:
                        preview = (
                            preview[:-1] + ", ...]"
                        )  # Replace the closing bracket with ", ...]"
                    data_info = f"items: {data_count}, preview: {preview}"
                else:
                    data_info = "empty list"
            elif data is None:
                data_info = "None"
            elif isinstance(data, (int, float, bool, str)):
                # For simple types, show the actual value
                data_info = f"{type(data).__name__}: {data}"
            else:
                # For complex types, show type and a string representation
                data_info = f"{type(data).__name__}: {str(data)[:100]}"
                if len(str(data)) > 100:
                    data_info += "..."

            return f"Result: Success ({data_info})"

    def __bool__(self) -> bool:
        """
        Allows direct boolean evaluation of the result.

        Returns:
            bool: True if operation was successful (no error message) AND data exists/is truthy,
                  False otherwise

        A result is considered successful if:
        1. There's no error message, AND
        2. The data is not empty (for lists/tuples) or evaluates to True (for other types)

        Empty lists or None data with no error will return False.
        """
        # If there's an error message, return False
        if self.values[1]:
            return False

        # If no error, check if we have data
        data = self.values[0]

        if isinstance(data, (list, tuple)):
            return len(data) > 0
        else:
            return bool(data)


class ResourceInof(object):
    def __init__(self, name, resource_type, namespace="default"):
        self.name = name
        self.namespace = namespace
        self.resource_type = resource_type


class KubernetesClient(object):
    """
    Kubernetes client for managing K8s resources
    """

    def __init__(self, csp=None, kubeconfig=None) -> None:
        """
        Initialize Kubernetes client.

        Args:
            csp (str, optional): Cloud Service Provider. Defaults to None.
            kubeconfig (str, optional): Path to the kubeconfig file.
                If not provided, the following order is checked:
                1. KUBECONFIG environment variable.
                2. Default path at ~/.kube/config.
                3. Any other specified path.
        """
        # Skip actual initialization if called just for library registration
        self._initialize_client()
        self.expected_port_info = {
            "22/tcp": {"state": "filtered", "service": "ssh"},
            "111/tcp": {"state": "filtered", "service": "rpcbind"},
            "987/tcp": {"state": "filtered", "service": "unknown"},
            "990/tcp": {"state": "filtered", "service": "ftps"},
            "2020/tcp": {"state": "filtered", "service": "xinupageserver"},
            "2021/tcp": {"state": "filtered", "service": "servexec"},
            "9100/tcp": {"state": "filtered", "service": "jetdirect"},
        }
        self.current_csp = csp
        self.created_resources = []

    def _get_kubeconfig_path(self, kubeconfig):
        """
        Retrieve the kubeconfig file path.

        Args:
            kubeconfig (str): Specified path for kubeconfig.

        Returns:
            str: The full path to the kubeconfig file.

        Raises:
            FileNotFoundError: When a valid kubeconfig file cannot be found.
        """
        # 1. Use the path specified by the user
        if kubeconfig and os.path.exists(kubeconfig):
            LOGGER.info(f"Current kubeconfig path from path: {kubeconfig}")
            return kubeconfig

        # 2. Check the environment variable
        env_kubeconfig = os.getenv("KUBECONFIG")
        if env_kubeconfig and os.path.exists(env_kubeconfig):
            LOGGER.info(f"Current kubeconfig path from env: {env_kubeconfig}")
            return env_kubeconfig

        # 3. Check the default path (~/.kube/config)
        default_kubeconfig = os.path.expanduser("~/.kube/config")
        if os.path.exists(default_kubeconfig):
            LOGGER.info(f"Current kubeconfig path from default path: {default_kubeconfig}")
            return default_kubeconfig

        # 4. If none are found, raise an exception
        raise FileNotFoundError(
            "Could not find kubeconfig file. Please either specify the path, "
            "set the KUBECONFIG environment variable, or ensure ~/.kube/config exists."
        )

    def _initialize_client(self):
        """
        Initialize the Kubernetes client.
        """
        try:
            config.load_kube_config()
            client.configuration.Configuration._default.verify_ssl = False
            self.coreV1Api = client.CoreV1Api()
            self.appsV1Api = client.AppsV1Api()
            self.apiClient = client.ApiClient()
            self.networkingV1Api = client.NetworkingV1Api()
            self.apiextensionsV1Api = client.ApiextensionsV1Api()
            self.customObjectApi = client.CustomObjectsApi()
            self.batchV1Api = client.BatchV1Api()
            self.rbacV1Api = client.RbacAuthorizationV1Api()
            self.v1PolicyRule = client.V1PolicyRule(
                api_groups=[""], resources=["pods"], verbs=["get", "list", "watch"]
            )
            self.storageV1Api = client.StorageV1Api()
        except config.config_exception.ConfigException as e:
            raise ConnectionError(f"Failed to initialize Kubernetes client: {e}")

    #############################################
    # General Utilities
    #############################################


    def cleanup(self, created_resources: List[ResourceInof] = []):
        if created_resources == []:
            created_resources = reversed(self.created_resources)
        for info in created_resources:
            if info.resource_type == "pod":
                LOGGER.info(
                    f"delete pod: {info.name}, namespace: {info.namespace} when clean up"
                )
                self.delete_pod_by_name(info.name, info.namespace)

    def apiClient_call_api(self, api_path):
        response = None
        try:
            response = self.apiClient.call_api(api_path, method="GET")
        except Exception as e:
            response = e
            LOGGER.info("Response message:")
            LOGGER.info(e)
            pass
        response = str(response)
        return response

    def _remove_resource_info(self, resource_name: str, namespace: str):
        for resource in self.created_resources:
            if resource.name == resource_name and resource.namespace == namespace:
                self.created_resources.remove(resource)
                LOGGER.info(
                    f"Resource {resource_name} in namespace {namespace} has been removed from created_resources list"
                )
                break

    #############################################
    # Namespace Management
    #############################################

    def list_namespaces(self) -> CommonResult[List[client.V1Namespace], str]:
        """
        List all namespaces in the cluster.

        Returns:
            CommonResult: Operation result containing:
                - List[client.V1Namespace]: List of namespace objects
                - str: Error message if an exception occurred

        Example:
            namespaces, err_msg = client.list_namespaces()
            if err_msg:
                print(f"Failed to list namespaces: {err_msg}")
            else:
                print(f"Namespaces: {namespaces}")
        """
        try:
            namespace_list = self.coreV1Api.list_namespace()
            return CommonResult(namespace_list.items)
        except client.ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->list_namespace: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    def create_namespace(self, namespace) -> CommonResult[client.V1Namespace, str]:
        """
        Create a namespace. If namespace exists, delete it first then create a new one.

        Args:
            namespace (str): Name of the namespace to create

        Returns:
            CommonResult: Operation result containing:
                - client.V1Namespace: Created namespace object
                - str: Error message if an exception occurred

        Example:
            namespace_obj, err_msg = client.create_namespace("my-namespace")
            if err_msg:
                print(f"Failed to create namespace: {err_msg}")
            else:
                print(f"Created namespace: {namespace_obj.metadata.name}")
        """
        try:
            existing_namespace = self.coreV1Api.read_namespace(name=namespace)
            if existing_namespace:
                LOGGER.info(f"Namespace {namespace} exists, deleting it first")

                self.delete_namespace(namespace)

                start_time = time.time()
                while time.time() - start_time < 60:
                    try:
                        self.coreV1Api.read_namespace(name=namespace)
                        LOGGER.info("Waiting for namespace deletion...")
                        time.sleep(5)
                    except ApiException as e:
                        if e.status == 404:
                            LOGGER.info(f"Namespace {namespace} deleted successfully")
                            break
                else:
                    return CommonResult(
                        None, f"Timeout waiting for namespace {namespace} deletion"
                    )
        except ApiException as e:
            if e.status != 404:
                return CommonResult(
                    None, f"Failed to delete exist namespace {namespace}: {e}"
                )

        # create new one
        try:
            namespace_object = client.V1Namespace(
                metadata=client.V1ObjectMeta(name=namespace)
            )
            LOGGER.info(f"Created namespace with {namespace_object}")

            namespaceObj = self.coreV1Api.create_namespace(namespace_object)
            LOGGER.info(f"Created namespace {namespace}")

            return CommonResult(namespaceObj)
        except ApiException as e:
            return CommonResult(None, f"Failed to create namespace {namespace}: {e}")

    def delete_namespace(self, namespace) -> CommonResult[client.V1Status, str]:
        """
        Delete a namespace from the cluster.

        Args:
            namespace (str): Name of the namespace to delete

        Returns:
            CommonResult: Operation result containing:
                - client.V1Status: Status of the delete operation
                - str: Error message if an exception occurred

        Example:
            status, err_msg = client.delete_namespace("my-namespace")
            if err_msg:
                print(f"Failed to delete namespace: {err_msg}")
            else:
                print(f"Namespace deletion status: {status.status}")
        """
        try:
            status = self.coreV1Api.delete_namespace(name=namespace)
            return CommonResult(status)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->delete_namespace: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def label_namespace(
        self, namespace, key, value
    ) -> CommonResult[client.V1Namespace, str]:
        """
        Add a label to a namespace.

        Args:
            namespace (str): Name of the namespace to label
            key (str): Label key
            value (str): Label value

        Returns:
            CommonResult: Operation result containing:
                - client.V1Namespace: Updated namespace object
                - str: Error message if an exception occurred

        Example:
            namespace_obj, err_msg = client.label_namespace("my-namespace", "environment", "dev")
            if err_msg:
                print(f"Failed to label namespace: {err_msg}")
            else:
                print(f"Added label to namespace: {namespace_obj.metadata.labels}")
        """
        try:
            # Create the patch object
            patch_body = {"metadata": {"labels": {key: value}}}
            patchedNamespace = self.coreV1Api.patch_namespace(
                name=namespace, body=patch_body
            )
            return CommonResult(patchedNamespace)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->patch_namespace: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def annotate_namespace(
        self, namespace, annotations_key, annotations_value
    ) -> CommonResult[client.V1Namespace, str]:
        """
        Add an annotation to a namespace.

        Args:
            namespace (str): Name of the namespace to annotate
            annotations_key (str): Annotation key
            annotations_value (str): Annotation value

        Returns:
            CommonResult: Operation result containing:
                - client.V1Namespace: Updated namespace object
                - str: Error message if an exception occurred

        Example:
            namespace_obj, err_msg = client.annotate_namespace("my-namespace", "description", "Test namespace")
            if err_msg:
                print(f"Failed to annotate namespace: {err_msg}")
            else:
                print(f"Added annotation to namespace: {namespace_obj.metadata.annotations}")
        """
        try:
            # Create the patch object
            patch_body = {"metadata": {"annotations": {annotations_key: annotations_value}}}
            patchedNamespace = self.coreV1Api.patch_namespace(
                name=namespace, body=patch_body
            )
            return CommonResult(patchedNamespace)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->patch_namespace: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def unlabel_namespace(self, namespace, key) -> CommonResult[client.V1Namespace, str]:
        """
        Remove a label from a namespace.

        Args:
            namespace (str): Name of the namespace
            key (str): Label key to remove

        Returns:
            CommonResult: Operation result containing:
                - client.V1Namespace: Updated namespace object
                - str: Error message if an exception occurred

        Example:
            namespace_obj, err_msg = client.unlabel_namespace("my-namespace", "environment")
            if err_msg:
                print(f"Failed to remove label: {err_msg}")
            else:
                print(f"Removed label from namespace: {namespace_obj.metadata.name}")
        """
        try:
            patch_body = {"metadata": {"labels": {key: None}}}
            patchedNamespace = self.coreV1Api.patch_namespace(
                name=namespace, body=patch_body
            )

            # Wait for a short period to allow changes to propagate
            time.sleep(2)

            updated_namespace_obj = self.coreV1Api.read_namespace(name=namespace)
            updated_labels = updated_namespace_obj.metadata.labels
            if key in updated_labels:
                return CommonResult(
                    None, f"Failed to remove label '{key}' from namespace '{namespace}'"
                )

            return CommonResult(patchedNamespace)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->patch_namespace: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    #############################################
    # Pod Management
    #############################################

    def list_pods(
        self, namespace: str = "default", name_pattern: Optional[str] = None, **kwargs
    ) -> CommonResult[List[client.V1Pod], str]:
        """
        Retrieve all pods within a specified namespace, optionally filtered by name pattern.

        Args:
            namespace (str): Namespace name. Defaults to "default".
            name_pattern (Optional[str]): Regular expression pattern to filter pod names.
            **kwargs: Additional arguments to pass to list_namespaced_pod.

        Returns:
            CommonResult: Operation result containing:
                - List[client.V1Pod]: List of pod objects
                - str: Error message if an exception occurred

        Example:
            pods, error_msg = client.list_pods(namespace="default", name_pattern="my-pod-.*")
            if error_msg:
                print(f"Failed to list pods: {error_msg}")
            else:
                print(f"Pods: {pods}")

        """
        try:
            pod_list = self.coreV1Api.list_namespaced_pod(namespace=namespace, **kwargs)
            pods = pod_list.items

            if name_pattern:
                prog = re.compile(name_pattern)
                pods = [pod for pod in pods if prog.match(pod.metadata.name)]

            LOGGER.debug(f"Found {len(pods)} pods in namespace {namespace}")
            return CommonResult(pods)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->list_namespaced_pod: {e}"
            LOGGER.debug(error_msg)
            return CommonResult([], error_msg)

    def list_pods_status(self, namespace) -> CommonResult[List[str], str]:
        """
        Get status information for all pods in a namespace.

        Args:
            namespace (str): The namespace to list pod statuses from

        Returns:
            CommonResult: Operation result containing:
                - List[str]: List of pod status strings in format "pod_name:phase"
                - str: Error message if an exception occurred

        Example:
            statuses, err_msg = client.list_pods_status("default")
            if err_msg:
                print(f"Failed to get pod statuses: {err_msg}")
            else:
                for status in statuses:
                    print(status)
        """
        pods = []
        for pod in self.coreV1Api.list_namespaced_pod(namespace).items:
            pod_name = pod.metadata.name
            pod_phase = pod.status.phase
            pods.append(f"{pod_name}:{pod_phase}")
        return CommonResult(pods)

    def verify_pod_are_running(self, pod_names: List[str], namespace: str) -> bool:
        """
        Verify if all pods are running
        """
        podstatus, _ = self.list_pods_status(namespace).values
        running_pods = [p for p in podstatus if p.split(":")[1] == "Running" and p.split(":")[0] in pod_names]
        return len(running_pods) == len(pod_names)

    def get_pod_containers(
        self, namespace: str, pod_name: str
    ) -> CommonResult[List[client.V1Container], str]:
        """
        Get container information for a specified pod.

        Args:
            namespace (str): Namespace where the pod exists
            pod_name (str): Name of the pod

        Returns:
            CommonResult: Operation result containing:
                - List[client.V1Container]: List of container objects in the pod
                - str: Error message if an exception occurred

        Example:
            containers, err_msg = client.get_pod_containers("default", "my-pod")
            if err_msg:
                print(f"Failed to get containers: {err_msg}")
            else:
                for container in containers:
                    print(f"Container: {container.name}, Image: {container.image}")
        """
        try:
            pod = self.coreV1Api.read_namespaced_pod(name=pod_name, namespace=namespace)
            return CommonResult(pod.spec.containers)
        except client.ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->read_namespaced_pod: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    def get_pod_logs(
        self, namespace: str, pod_name: str, container_name: Optional[str] = None
    ) -> CommonResult[str, str]:
        """
        Retrieve logs for a specified pod.

        Args:
            namespace (str): Namespace where the pod exists
            pod_name (str): Name of the pod
            container_name (Optional[str]): Name of the container (if pod has multiple containers)

        Returns:
            CommonResult: Operation result containing:
                - str: Logs from the pod/container
                - str: Error message if an exception occurred

        Example:
            logs, err_msg = client.get_pod_logs("default", "my-pod")
            if err_msg:
                print(f"Failed to get logs: {err_msg}")
            else:
                print(logs)
        """
        try:
            log = self.coreV1Api.read_namespaced_pod_log(
                name=pod_name, namespace=namespace, container=container_name
            )
            return CommonResult(log)
        except client.ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->read_namespaced_pod_log: {e}"
            LOGGER.error(error_msg)
            return CommonResult("", error_msg)

    def delete_pod_by_name(
        self, pod_name: str, namespace: str, wait: int = 0
    ) -> CommonResult[None, str]:
        """
        Delete a pod by name.

        Args:
            pod_name (str): Name of the pod to delete
            namespace (str): Namespace where the pod exists
            wait (int): Time in seconds to wait for pod deletion (0 means no wait)

        Returns:
            CommonResult: Operation result containing:
                - None: If the pod is deleted successfully
                - str: Error message if an exception occurred

        Example:
            _, err_msg = client.delete_pod_by_name("my-pod", "default", wait=30)
            if err_msg:
                print(f"Failed to delete pod: {err_msg}")
            else:
                print("Pod deleted successfully")
        """
        LOGGER.info(f"Delete pod name: {pod_name}, namespace: {namespace} ")

        try:
            self.coreV1Api.read_namespaced_pod(pod_name, namespace)
            self.coreV1Api.delete_namespaced_pod(
                pod_name,
                namespace,
                grace_period_seconds=0,
                propagation_policy="Foreground",
            )
        except ApiException as e:
            if e.status == 404:
                LOGGER.info(f"Pod {pod_name}, in namespace {namespace} does not exist")
                return CommonResult(None)
            error_msg = f"Exception when calling CoreV1Api->delete_namespaced_pod: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

        if wait == 0:
            # Remove ResourceInfo from created_resources list
            self._remove_resource_info(pod_name, namespace)
            return CommonResult(None)

        bailout_time = time.time() + wait
        while time.time() < bailout_time:
            try:
                pod = self.coreV1Api.read_namespaced_pod(pod_name, namespace)
                LOGGER.debug("Pod %s phase = %s", pod.metadata.name, pod.status.phase)
                time.sleep(3)
            except ApiException as e:
                if e.status == 404:
                    LOGGER.error("Pod %s has been deleted", pod.metadata.name)
                    self._remove_resource_info(pod_name, namespace)
                    return CommonResult(None)
                error_msg = f"Exception when calling CoreV1Api->read_namespaced_pod: {e}"
                LOGGER.error(error_msg)
                return CommonResult(None, error_msg)
        return CommonResult(None, "TIMEOUT")

    def delete_pod(self, pod, wait: int = 0) -> CommonResult[None, str]:
        """
        Delete a pod object.

        Args:
            pod (kubernetes.client.models.v1_pod.V1Pod): The pod object to delete
            wait (int): Time in seconds to wait for pod deletion (0 means no wait)

        Returns:
            CommonResult: Operation result containing:
                - None: If the pod is deleted successfully
                - str: Error message if an exception occurred

        Example:
            pods, _ = client.list_pods("default")
            if pods:
                _, err_msg = client.delete_pod(pods[0])
                if err_msg:
                    print(f"Failed to delete pod: {err_msg}")
                else:
                    print("Pod deleted successfully")
        """
        return self.delete_pod_by_name(pod.metadata.name, pod.metadata.namespace, wait)

    def create_pod(self, pod_body, wait: int = 0) -> CommonResult[client.V1Pod, str]:
        """
        Create a pod from a pod definition.

        Args:
            pod_body (dict): Pod definition in dictionary format
            wait (int): Time in seconds to wait for pod to reach Running/Succeeded state (0 means no wait)

        Returns:
            CommonResult: Operation result containing:
                - client.V1Pod: The created pod object if successful
                - str: Error message if an exception occurred

        Example:
            pod_manifest = {
                "apiVersion": "v1",
                "kind": "Pod",
                "metadata": {"name": "test-pod", "namespace": "default"},
                "spec": {
                    "containers": [{
                        "name": "test-container",
                        "image": "nginx"
                    }]
                }
            }
            pod, err_msg = client.create_pod(pod_manifest, wait=60)
            if err_msg:
                print(f"Failed to create pod: {err_msg}")
            else:
                print(f"Pod created: {pod.metadata.name}")
        """
        pod_namespace = pod_body["metadata"]["namespace"]
        pod_name = pod_body["metadata"]["name"]

        existing_pods, err_msg = self.list_pods(pod_namespace, name_pattern=pod_name)
        if err_msg:
            LOGGER.error(f"Error when listing pods: {err_msg}")
            return CommonResult(None, err_msg)
        if existing_pods:
            LOGGER.debug(f"Pod {pod_name} already exists in namespace {pod_namespace}")
            pod = existing_pods[-1]
            if wait:
                is_healthy, _ = self.wait_for_pod_healthy(pod, timeout=wait)
                if not is_healthy:
                    return CommonResult(
                        None,
                        f"Existing Pod: {pod.metadata.name} is not in healthy state until timeout:[{wait}] occurred.",
                    )
            return CommonResult(pod)

        try:
            api_response = self.coreV1Api.create_namespaced_pod(
                namespace=pod_namespace, body=pod_body
            )
            LOGGER.debug(f"create pod api_response = {api_response}")
        except ApiException as error_message:
            error_msg = f"Error when create_namespaced_pod: {error_message}\n"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

        pods, err_msg = self.list_pods(pod_namespace, name_pattern=pod_name)
        if err_msg:
            LOGGER.error(f"Error when listing pods: {err_msg}")
            return CommonResult(None, err_msg)
        if not pods:
            LOGGER.error("Failed to create pod")
            return CommonResult(None, "Failed to create pod")

        pod = pods[-1]
        if wait:
            is_healthy, _ = self.wait_for_pod_healthy(pod, timeout=wait)
            if not is_healthy:
                return CommonResult(
                    None,
                    f"Created Pod: {pod.metadata.name} is not in healthy state until timeout:[{wait}] occurred.",
                )
        resource = ResourceInof(name=pod_name, resource_type="pod", namespace=pod_namespace)
        self.created_resources.append(resource)
        return CommonResult(pod)

    def is_pod_healthy(self, pod) -> CommonResult[bool, str]:
        """
        Check if a pod is in a healthy state.

        A pod is considered healthy if:
        - Its phase is "Running" or "Succeeded"
        - All containers are ready
        - All required conditions (Initialized, Ready, ContainersReady, PodScheduled) are True

        Args:
            pod (kubernetes.client.models.v1_pod.V1Pod): The pod to check

        Returns:
            CommonResult: Operation result containing:
                - bool: True if the pod is healthy, False otherwise
                - str: Error message if an exception occurred

        Example:
            pod, _ = client.read_pod("my-pod", "default")
            is_healthy, err_msg = client.is_pod_healthy(pod)
            if err_msg:
                print(f"Error checking pod health: {err_msg}")
            else:
                print(f"Pod is {'healthy' if is_healthy else 'unhealthy'}")
        """
        if not pod:
            LOGGER.error("Pod is not found")
            return CommonResult(False, "Pod is not found")
        retval = True
        pod_name = pod.metadata.name
        if pod.status.phase == "Terminating" or pod.metadata.deletion_timestamp:
            LOGGER.debug("Pod %s is terminating", pod_name)
            return CommonResult(False)
        if pod.status.phase != "Running":
            if pod.status.phase == "Succeeded":
                return CommonResult(True)
            LOGGER.debug("Pod %s phase = %s", pod_name, pod.status.phase)
            return CommonResult(False)
        for container_status in pod.status.container_statuses:
            if not container_status.ready:
                LOGGER.debug(
                    "Pod %s container %s not ready", pod_name, container_status.name
                )
                retval = False
        condition_types = [condition.type for condition in pod.status.conditions]
        for condition_type in ["Initialized", "Ready", "ContainersReady", "PodScheduled"]:
            index = (
                condition_types.index(condition_type)
                if condition_type in condition_types
                else None
            )
            if index is None:
                LOGGER.debug("Pod %s condition %s not exists", pod_name, condition_type)
                continue
            condition = pod.status.conditions[index]
            stauts = condition.status
            if stauts != "True":
                LOGGER.debug("Pod %s condition %s = %s", pod_name, condition_type, stauts)
                retval = False
        if retval:
            LOGGER.debug("Pod %s is healthy", pod_name)
        return CommonResult(retval)

    def wait_for_pod_healthy(self, pod, timeout=60) -> CommonResult[bool, str]:
        """
        Wait for a pod to reach a healthy state within the specified timeout.

        Args:
            pod (kubernetes.client.models.v1_pod.V1Pod): The pod to check
            timeout (int): Maximum time in seconds to wait for the pod to become healthy

        Returns:
            CommonResult: Operation result containing:
                - bool: True if the pod became healthy within the timeout, False otherwise
                - str: Error message if an exception occurred or timeout was reached

        Example:
            pod, _ = client.read_pod("my-pod", "default")
            is_healthy, err_msg = client.wait_for_pod_healthy(pod, timeout=120)
            if err_msg:
                print(f"Error waiting for pod health: {err_msg}")
            elif is_healthy:
                print("Pod is now healthy")
            else:
                print("Pod did not become healthy within the timeout")
        """
        bailout_time = time.time() + timeout
        while time.time() < bailout_time:
            try:
                pod, _ = self.read_pod(pod.metadata.name, pod.metadata.namespace)
                is_healthy, err_msg = self.is_pod_healthy(pod)
                if err_msg:
                    LOGGER.error(err_msg)
                    return CommonResult(False, err_msg)
                if is_healthy:
                    LOGGER.info("pod %s is healthy", pod.metadata.name)
                    return CommonResult(True)
                time.sleep(3)
            except ApiException as e:
                error_msg = f"Exception when calling CoreV1Api->is_pod_healthy: {e}"
                LOGGER.error(error_msg)
                return CommonResult(False, error_msg)
        return CommonResult(
            False,
            f"Pod {pod.metadata.name} is not in healthy state until timeout:[{timeout}] occurred.",
        )

    def read_pod(self, pod_name, namespace) -> CommonResult[client.V1Pod, str]:
        """
        Get detailed information about a specific pod.

        Args:
            pod_name (str): Name of the pod to retrieve
            namespace (str): Namespace where the pod exists

        Returns:
            CommonResult: Operation result containing:
                - client.V1Pod: The pod object if found
                - str: Error message if an exception occurred

        Example:
            pod, err_msg = client.read_pod("my-pod", "default")
            if err_msg:
                print(f"Failed to get pod: {err_msg}")
            else:
                print(f"Pod phase: {pod.status.phase}")-
                print(f"Node: {pod.spec.node_name}")
        """
        try:
            pod = self.coreV1Api.read_namespaced_pod(pod_name, namespace)
            return CommonResult(pod)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->read_namespaced_pod: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def exec_command_in_pod(
        self, pod, command, container=None, timeout=60, max_retries=3, retry_delay=5
    ) -> CommonResult[str, str]:
        """
        Execute a command in a pod container.

        Args:
            pod (kubernetes.client.models.v1_pod.V1Pod): The pod object
            command (list): The command to execute as a list of strings
            container (str, optional): The container name (defaults to first container if None)
            timeout (int): Command execution timeout in seconds
            max_retries (int): Maximum number of retry attempts
            retry_delay (int): Delay between retries in seconds

        Returns:
            CommonResult: Operation result containing:
                - str: Command output if successful
                - str: Error message if an exception occurred

        Example:
            pod, _ = client.read_pod("my-pod", "default")
            output, err_msg = client.exec_command_in_pod(pod, ["ls", "-la", "/"])
            if err_msg:
                print(f"Command execution failed: {err_msg}")
            else:
                print(f"Command output: {output}")
        """
        pod_name = pod.metadata.name
        namespace = pod.metadata.namespace

        if container is None:
            container = pod.spec.containers[0].name

        LOGGER.debug(
            "Executing command %s in container %s of pod %s",
            command,
            container,
            pod_name,
        )

        for attempt in range(max_retries):
            try:
                resp = stream(
                    self.coreV1Api.connect_get_namespaced_pod_exec,
                    pod_name,
                    namespace,
                    container=container,
                    command=command,
                    _request_timeout=timeout,
                    stderr=True,
                    stdin=False,
                    stdout=True,
                    tty=False,
                )
                LOGGER.debug("Response: %s", resp)
                return CommonResult(resp)
            except ApiException as e:
                error_msg = f"Exception when calling CoreV1Api->connect_get_namespaced_pod_exec: {e}"
                LOGGER.error(error_msg)
                if attempt < max_retries - 1:
                    time.sleep(retry_delay)
                else:
                    return CommonResult(None, error_msg)
        return CommonResult(
            None,
            f"Failed to execute command in pod {pod_name} after {max_retries} attempts.",
        )

    def get_pod_running_node_name(self, pod_name, namespace) -> CommonResult[str, str]:
        """
        Get the name of the node where a pod is running.

        Args:
            pod_name (str): Name of the pod
            namespace (str): Namespace where the pod exists

        Returns:
            CommonResult: Operation result containing:
                - str: Node name where the pod is running
                - str: Error message if an exception occurred

        Example:
            node_name, err_msg = client.get_pod_running_node_name("my-pod", "default")
            if err_msg:
                print(f"Failed to get node name: {err_msg}")
            else:
                print(f"Pod is running on node: {node_name}")
        """
        try:
            pod = self.coreV1Api.read_namespaced_pod(pod_name, namespace)
            return CommonResult(pod.spec.node_name)
        except ApiException as e:
            error_msg = f"Exception when calling CoreV1Api->read_namespaced_pod: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def wait_for_pod_disappear(
        self, namespace, regex=None, timeout=60, **kwargs
    ) -> CommonResult[bool, str]:
        """
        Wait for pods matching the criteria to disappear from a namespace.

        Args:
            namespace (str): Namespace to check for pods
            regex (str, optional): Regular expression to filter pod names
            timeout (int): Maximum time in seconds to wait for pods to disappear
            **kwargs: Additional arguments to pass to list_pods

        Returns:
            CommonResult: Operation result containing:
                - bool: True if all matching pods disappeared within timeout, False otherwise
                - str: Error message if an exception occurred

        Example:
            disappeared, err_msg = client.wait_for_pod_disappear("default", regex="test-.*", timeout=120)
            if err_msg:
                print(f"Error waiting for pods to disappear: {err_msg}")
            elif disappeared:
                print("All matching pods have disappeared")
            else:
                print("Pods did not disappear within the timeout")
        """
        start_time = time.time()

        while time.time() - start_time < timeout:
            try:
                pods, _ = self.list_pods(namespace=namespace, name_pattern=regex, **kwargs)

                if len(pods) == 0:
                    LOGGER.debug(
                        f"pod disappeared in about {time.time() - start_time} sec."
                    )
                    return CommonResult(True)
            except ApiException as api_err:
                error_msg = f"Exception when calling list_pods api: {api_err.reason}"
                LOGGER.error(error_msg)
                if api_err.status == 401:
                    # Bearer token is expired
                    self._initialize_client()

            time.sleep(2)

        return CommonResult(False, "Pods did not disappear within the timeout")

    def wait_for_pod_appear(
        self, namespace: str, regex: str, timeout: int = 120
    ) -> CommonResult[bool, str]:
        """
        Wait for a pod to appear in a namespace.

        Args:
            namespace (str): Namespace to check for pods
            pod_name (str): Name of the pod to wait for
            timeout (int): Maximum time in seconds to wait for the pod to appear

        Returns:
            CommonResult: Operation result containing:
                - bool: True if the pod appeared within the timeout, False otherwise
                - str: Error message if an exception occurred or timeout was reached
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            pods, _ = self.list_pods(namespace=namespace, name_pattern=regex)
            if len(pods) > 0:
                return CommonResult(True)
            time.sleep(2)
        return CommonResult(False, "Pod did not appear within the timeout")

    def create_nginx_pod(
        self, namespace="default", timeout=300, tolerations=None
    ) -> CommonResult[client.V1Pod, str]:
        """
        Create a simple nginx pod for testing purposes.

        Args:
            namespace (str): Namespace to create the pod in
            timeout (int): Maximum time in seconds to wait for the pod to start running
            tolerations (list, optional): Custom tolerations to apply to the pod

        Returns:
            CommonResult: Operation result containing:
                - client.V1Pod: The created nginx pod if successful
                - str: Error message if an exception occurred

        Example:
            pod, err_msg = client.create_nginx_pod("test-namespace", timeout=120)
            if err_msg:
                print(f"Failed to create nginx pod: {err_msg}")
            else:
                print(f"Nginx pod created: {pod.metadata.name}")
        """
        LOGGER.info("Create nginx-pod in namespace {}".format(namespace))
        pod_manifest = {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {"labels": {"run": "nginx-pod"}, "name": "nginx-pod"},
            "spec": {
                "containers": [
                    {
                        "image": "nginx",
                        "imagePullPolicy": "Always",
                        "name": "nginx-pod",
                        "ports": [{"containerPort": 80}],
                    }
                ],
                "tolerations": [
                    {
                        "effect": "NoSchedule",
                        "key": "dedicated",
                        "operator": "Equal",
                        "value": "system-workload",
                    },
                    {
                        "effect": "NoExecute",
                        "key": "dedicated",
                        "operator": "Equal",
                        "value": "system-workload",
                    },
                ],
                "restartPolicy": "Never",
            },
        }
        if tolerations is not None:
            pod_manifest["spec"]["tolerations"] = tolerations
        self.coreV1Api.create_namespaced_pod(body=pod_manifest, namespace=namespace)
        # check pod is in running state
        start_time = time.time()
        # cmd = "kubectl get pod {} -n {}".format("nginx-pod", namespace)
        pod_name = "nginx-pod"
        while time.time() - start_time < timeout:
            pod = self.coreV1Api.read_namespaced_pod(name=pod_name, namespace=namespace)
            if pod.status.phase == "Running":
                break
            time.sleep(1)
        else:
            return CommonResult(
                None, f"Pod {pod_name} not in running after {timeout} seconds"
            )

        return CommonResult(pod)

    #############################################
    # Storage Management
    #############################################

    def verify_pvc_deleted(
        self, name: str, namespace: str, timeout: int = 300
    ) -> CommonResult[bool, str]:
        """
        Verify that a PVC has been deleted.

        Args:
            name (str): Name of the PVC to verify
            namespace (str): Namespace where the PVC exists
            timeout (int): Maximum time in seconds to wait for deletion. Defaults to 300.

        Returns:
            CommonResult: Operation result containing:
                - bool: True if PVC is deleted, False otherwise
                - str: Error message if an exception occurred or timeout was reached

        Example:
            deleted, err_msg = client.verify_pvc_deleted("my-pvc", "default", timeout=120)
            if err_msg:
                print(f"Error verifying PVC deletion: {err_msg}")
            elif deleted:
                print("PVC was successfully deleted")
            else:
                print("PVC still exists after timeout")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                self.coreV1Api.read_namespaced_persistent_volume_claim(
                    name=name, namespace=namespace
                )
            except ApiException as e:
                if e.status == 404:
                    LOGGER.info(
                        "PASS: PVC '%s' in namespace '%s' is deleted", name, namespace
                    )
                    return CommonResult(True)
                else:
                    LOGGER.error("Error when reading PVC: %s", e)
                    return CommonResult(False, str(e))  # Return False on API errors
            time.sleep(5)  # Sleep for a while before retrying
        LOGGER.debug(
            'FAIL: PVC "%s" in namespace "%s" still exists after %d seconds',
            name,
            namespace,
            timeout,
        )
        return CommonResult(False, "PVC still exists after timeout")

    def list_pvs(
        self, name_pattern: Optional[str] = None, timeout: int = 300
    ) -> CommonResult[List[client.V1PersistentVolume], str]:
        """
        List Persistent Volumes with optional name filtering.

        Args:
            name_pattern (Optional[str]): Regular expression pattern to filter PV names
            timeout (int): Maximum time in seconds to wait for the operation. Defaults to 300.

        Returns:
            CommonResult: Operation result containing:
                - List[client.V1PersistentVolume]: List of PV objects
                - str: Error message if an exception occurred or timeout was reached

        Example:
            pvs, err_msg = client.list_pvs(name_pattern="pvc-.*")
            if err_msg:
                print(f"Failed to list PVs: {err_msg}")
            else:
                print(f"Found {len(pvs)} PVs")
                for pv in pvs:
                    print(f"PV: {pv.metadata.name}, Status: {pv.status.phase}")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                pv_list = self.coreV1Api.list_persistent_volume()
                if name_pattern:
                    prog = re.compile(name_pattern)
                    return [pv for pv in pv_list.items if prog.match(pv.metadata.name)]
                return CommonResult(pv_list.items)
            except ApiException as e:
                error_msg = f"Error retrieving PV: {e}"
                LOGGER.debug(error_msg)
            time.sleep(10)

        error_msg = f"FAIL: list PV:{name_pattern} timeout"
        LOGGER.debug(error_msg)
        return CommonResult([], error_msg)

    def get_storage_info(self) -> CommonResult[dict, str]:
        """
        Get information about storage resources in the cluster.

        Returns:
            CommonResult: Operation result containing:
                - dict: Storage information including name, storage classes, quota, and usage
                - str: Error message if an exception occurred

        Example:
            storage_info, err_msg = client.get_storage_info()
            if err_msg:
                print(f"Failed to get storage info: {err_msg}")
            else:
                print(f"Storage name: {storage_info['name']}")
                print(f"Storage classes: {storage_info['storage_classes']}")
                print(f"Quota: {storage_info['quota']}")
                print(f"Quota used: {storage_info['quota_used']} ({storage_info['quota_used_pct']}%)")
        """
        group = "runai.dgxc.nvidia.com"
        version = "v1beta1"
        namespace = "dgxc-tenant-cluster-policies"
        plural = "runaidgxcstorages"

        # Mapping of CSP to the corresponding storage name
        csp_to_storage_name = {
            "gcp": "filestore-standard",
            "aws": "amazon-fsx-for-lustre",
            "oci": "fss",
            "azure": "azurefile-csi",
        }

        # Fetch the custom resource across all namespaces
        custom_objects = self.customObjectApi.list_cluster_custom_object(
            group, version, plural
        )

        def extract_storage_info(item, namespace):
            storage_classes = item["spec"].get("storageClasses", [])
            quota_used_pct = item["status"].get("storageUsedPct", None)
            quota = item["status"].get("storageQuota", None)
            quota_used = item["status"].get("storageRequested", None)
            storage_consumed = item["status"].get("storageConsumed", None)

            return {
                "namespace": namespace,
                "name": item["metadata"]["name"],
                "storage_classes": storage_classes,
                "quota_used_pct": quota_used_pct,
                "quota": quota,
                "quota_used": quota_used,
                "storage_consumed": storage_consumed,
            }

        # Get the storage name based on the current CSP
        storage_name = csp_to_storage_name.get(self.current_csp)

        # Iterate through the custom objects to find the matching storage
        for item in custom_objects["items"]:
            if item["metadata"]["name"] == storage_name:
                return CommonResult(extract_storage_info(item, namespace))

        return CommonResult(None, "Storage not found")

    def create_rwo_pvc(
        self,
        pvc_name: str,
        test_namespace: str,
        accessmode: List[str] = ["ReadWriteOnce"],
        size: Optional[str] = None,
        storageclass: Optional[str] = None,
    ) -> CommonResult[bool, str]:
        """
        Create a ReadWriteOnce (RWO) Persistent Volume Claim.

        Args:
            pvc_name (str): Name of the PVC to create
            test_namespace (str): Namespace where to create the PVC
            accessmode (List[str]): List of access modes. Defaults to ["ReadWriteOnce"].
            size (Optional[str]): Size of the PVC (e.g. "100Gi"). Defaults to "100Gi".
            storageclass (Optional[str]): Storage class to use. If None, uses CSP-specific default.

        Returns:
            CommonResult: Operation result containing:
                - bool: True if PVC was created and bound successfully, False otherwise
                - str: Error message if an exception occurred or timeout was reached

        Example:
            created, err_msg = client.create_rwo_pvc("my-pvc", "default", size="50Gi")
            if err_msg:
                print(f"Failed to create PVC: {err_msg}")
            elif created:
                print("PVC created and bound successfully")
            else:
                print("PVC creation failed")
        """
        test_storage = StorageClassConfig.get_by_csp(self.current_csp)
        if self.current_csp == SupportedCSP.GCP.value:
            test_storage_class = test_storage["standard"]
        if self.current_csp == SupportedCSP.AWS.value:
            test_storage_class = test_storage["lustre"]
        if self.current_csp == SupportedCSP.OCI.value:
            test_storage_class = test_storage["fss"]
        if self.current_csp == SupportedCSP.AZURE.value:
            test_storage_class = test_storage["csi"]
        pvc_params = {
            "name": pvc_name,
            "namespace": test_namespace,
            "accessmode": ["ReadWriteOnce"] if accessmode is None else accessmode,
            "size": size if size else "100Gi",
            "storageclass": test_storage_class if storageclass is None else storageclass,
        }
        self.create_pvc(**pvc_params)

        LOGGER.info("Waiting PVC go into bound state.")
        interval = 30
        tries = 30
        for _ in range(tries):
            time.sleep(interval)
            if (
                self.current_csp == SupportedCSP.AWS.value
                or self.current_csp == SupportedCSP.AZURE.value
                or self.current_csp == SupportedCSP.OCI.value
            ):
                pvc_list, err_msg = self.list_pvc(test_namespace, pvc_name)
                if err_msg:
                    continue
                if len(pvc_list) == 0:
                    continue
                pvc_status = pvc_list[0].status.phase
                if pvc_status == "Bound":
                    return CommonResult(True)
            elif self.current_csp == SupportedCSP.GCP.value:
                pvc_list, err_msg = self.list_pvc(test_namespace, pvc_name)
                if err_msg:
                    continue
                if len(pvc_list) == 0:
                    continue
                pvc_status = pvc_list[0].status.phase
                if pvc_status == "Pending":
                    return CommonResult(True)
            LOGGER.info(
                "Current PVC status is %s, check after %d seconds..."
                % (pvc_status, interval)
            )

        return CommonResult(
            False, "PVC status is not Bound after %d seconds" % (tries * interval)
        )

    def create_large_file(
        self,
        test_pod: str,
        mount_path: str,
        file_name: str = "largefile.img",
        file_size: int = 10,
    ) -> CommonResult[bool, str]:
        """
        Create a large file in a pod's mounted volume for testing purposes.

        Args:
            test_pod (str): Name of the pod where to create the file
            mount_path (str): Path where the volume is mounted in the pod
            file_name (str): Name of the file to create. Defaults to "largefile.img".
            file_size (int): Size of the file in GB. Defaults to 10.

        Returns:
            CommonResult: Operation result containing:
                - bool: True if file was created successfully, False otherwise
                - str: Error message if an exception occurred

        Example:
            created, err_msg = client.create_large_file("test-pod", "/mnt/data", file_size=5)
            if err_msg:
                print(f"Failed to create large file: {err_msg}")
            elif created:
                print("Large file created successfully")
            else:
                print("File creation failed")
        """
        large_file = os.path.join(mount_path, file_name)
        command = [
            "/bin/sh",
            "-c",
            f"dd if=/dev/urandom of={large_file} bs=1G count={file_size}",
        ]
        output, err_msg = self.exec_command_in_pod(test_pod, command)
        if err_msg:
            return CommonResult(False, err_msg)
        LOGGER.info(f"create_large_file output large_file = {output}")
        return CommonResult(True)

    def verify_pvc_not_in_use(
        self, namespace: str, pvc_name: str, timeout: int = 120
    ) -> CommonResult[bool, str]:
        """
        Verify that a PVC is not being used by any pod.

        Args:
            namespace (str): Namespace where the PVC exists
            pvc_name (str): Name of the PVC to verify
            timeout (int): Maximum time in seconds to wait for verification. Defaults to 120.

        Returns:
            CommonResult: Operation result containing:
                - bool: True if PVC is not in use, False if it is still in use after timeout
                - str: Error message if an exception occurred or timeout was reached

        Example:
            not_in_use, err_msg = client.verify_pvc_not_in_use("default", "my-pvc")
            if err_msg:
                print(f"Error verifying PVC usage: {err_msg}")
            elif not_in_use:
                print("PVC is not in use by any pod")
            else:
                print("PVC is still in use by one or more pods")
        """
        not_used = False
        start_time = time.time()
        while time.time() - start_time < timeout:
            pods = self.coreV1Api.list_namespaced_pod(namespace)
            in_use = False
            for pod in pods.items:
                for volume in pod.spec.volumes:
                    if (
                        volume.persistent_volume_claim
                        and volume.persistent_volume_claim.claim_name == pvc_name
                    ):
                        in_use = True
                        break
                if in_use:
                    break

            if not in_use:
                not_used = True
                break
            LOGGER.info("PVC is still in use, check after 10s...")
            time.sleep(10)

        return CommonResult(not_used, "PVC is still in use after %d seconds" % (timeout))

    def verify_pv_exist(self, name: str) -> CommonResult[client.V1PersistentVolume, str]:
        """
        Verify if a Persistent Volume exists.

        Args:
            name (str): Name of the PV to verify

        Returns:
            CommonResult: Operation result containing:
                - client.V1PersistentVolume: The PV object if it exists
                - str: Error message if the PV doesn't exist or an exception occurred

        Example:
            pv, err_msg = client.verify_pv_exist("pvc-12345678")
            if err_msg:
                print(f"PV verification failed: {err_msg}")
            else:
                print(f"PV exists: {pv.metadata.name}, Status: {pv.status.phase}")
        """
        try:
            pv = self.coreV1Api.read_persistent_volume(name)
            LOGGER.info("[persistent_volume]:")
            LOGGER.info(pv)
            return CommonResult(pv)
        except ApiException as e:
            if e.status == 404:
                return CommonResult(None, "FAIL: PV does not exist")
            else:
                LOGGER.error(f"Error when reading PV: {e}")
                return CommonResult(None, str(e))

    def verify_pv_deleted(self, name: str, timeout: int = 300) -> CommonResult[bool, str]:
        """
        Verify if a Persistent Volume has been deleted.

        Args:
            name (str): Name of the PV to verify
            timeout (int): Maximum time in seconds to wait for deletion. Defaults to 300.

        Returns:
            CommonResult: Operation result containing:
                - bool: True if PV is deleted, False if it still exists after timeout
                - str: Error message if an exception occurred or timeout was reached

        Example:
            deleted, err_msg = client.verify_pv_deleted("pvc-12345678", timeout=120)
            if err_msg:
                print(f"Error verifying PV deletion: {err_msg}")
            elif deleted:
                print("PV was successfully deleted")
            else:
                print("PV still exists after timeout")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                self.coreV1Api.read_persistent_volume(name)
                LOGGER.info("PV exist now, retry after 10s")
                time.sleep(10)
            except client.exceptions.ApiException as e:
                if e.status == 404:
                    return CommonResult(True)

        return CommonResult(False, "PV still exists after %d seconds" % (timeout))

    def create_pvc(
        self, name, namespace, accessmode, size, storageclass
    ) -> CommonResult[client.V1PersistentVolumeClaim, str]:
        """
        Create a new Persistent Volume Claim.

        Args:
            name (str): Name of the PVC to create
            namespace (str): Namespace where to create the PVC
            accessmode (List[str]): List of access modes (e.g. ['ReadWriteOnce'])
            size (str): Size of the PVC (e.g. '10Gi')
            storageclass (str): Storage class to use

        Returns:
            CommonResult: Operation result containing:
                - client.V1PersistentVolumeClaim: The created PVC object if successful
                - str: Error message if an exception occurred

        Example:
            pvc, err_msg = client.create_pvc("my-pvc", "default", ["ReadWriteOnce"], "10Gi", "standard")
            if err_msg:
                print(f"Failed to create PVC: {err_msg}")
            else:
                print(f"PVC created: {pvc.metadata.name}, Status: {pvc.status.phase}")
        """
        try:
            # Check if PVC already exists
            existing_pvc = self.coreV1Api.read_namespaced_persistent_volume_claim(
                name=name, namespace=namespace
            )
            return CommonResult(existing_pvc)
        except ApiException as e:
            if e.status != 404:
                LOGGER.error(
                    f"Failed to check if PVC '{name}' exists in namespace '{namespace}': {e}"
                )
                return CommonResult(None, str(e))

        pvc_metadata = client.V1ObjectMeta(
            name=name,
            namespace=namespace,
        )

        if self.current_csp == SupportedCSP.GCP:
            pvc_metadata.annotations = {
                "volume.beta.kubernetes.io/storage-provisioner": "filestore.csi.storage.gke.io",
                "volume.kubernetes.io/storage-provisioner": "filestore.csi.storage.gke.io",
            }
            pvc_metadata.finalizers = ["kubernetes.io/pvc-protection"]

        pvc_spec = client.V1PersistentVolumeClaim(
            api_version="v1",
            kind="PersistentVolumeClaim",
            metadata=pvc_metadata,
            spec=client.V1PersistentVolumeClaimSpec(
                access_modes=accessmode,
                resources=client.V1ResourceRequirements(requests={"storage": size}),
                storage_class_name=storageclass,
                volume_mode="Filesystem",
            ),
        )

        try:
            new_pvc = self.coreV1Api.create_namespaced_persistent_volume_claim(
                namespace=namespace, body=pvc_spec
            )
            return CommonResult(new_pvc)
        except ApiException as e:
            LOGGER.error(f"Failed to create PVC '{name}' in namespace '{namespace}': {e}")
            return CommonResult(None, str(e))

    def delete_pvc(self, name, namespace) -> CommonResult[bool, str]:
        """
        Delete a Persistent Volume Claim.

        Args:
            name (str): Name of the PVC to delete
            namespace (str): Namespace where the PVC exists

        Returns:
            CommonResult: Operation result containing:
                - bool: True if PVC was deleted successfully, False otherwise
                - str: Error message if an exception occurred

        Example:
            deleted, err_msg = client.delete_pvc("my-pvc", "default")
            if err_msg:
                print(f"Failed to delete PVC: {err_msg}")
            elif deleted:
                print("PVC deleted successfully")
            else:
                print("PVC deletion failed")
        """
        try:
            self.coreV1Api.delete_namespaced_persistent_volume_claim(name, namespace)
            LOGGER.info(f"PVC '{name}' deleted successfully.")
            return CommonResult(True)
        except ApiException as e:
            if e.status == 404:
                LOGGER.info("PVC has been deleted")
            else:
                LOGGER.error(f"Error deleting PVC: {str(e)}")
                return CommonResult(False, str(e))
        except Exception as e:
            LOGGER.error(f"Error deleting PVC: {str(e)}")
            return CommonResult(False, str(e))

    def list_pvc(
        self, namespace, regex=None, timeout=300
    ) -> CommonResult[List[client.V1PersistentVolumeClaim], str]:
        """
        List Persistent Volume Claims in a namespace with optional name filtering.

        Args:
            namespace (str): Namespace to list PVCs from
            regex (Optional[str]): Regular expression pattern to filter PVC names
            timeout (int): Maximum time in seconds to wait for the operation. Defaults to 300.

        Returns:
            CommonResult: Operation result containing:
                - List[client.V1PersistentVolumeClaim]: List of PVC objects
                - str: Error message if an exception occurred or timeout was reached

        Example:
            pvcs, err_msg = client.list_pvc("default", regex="data-.*")
            if err_msg:
                print(f"Failed to list PVCs: {err_msg}")
            else:
                print(f"Found {len(pvcs)} PVCs")
                for pvc in pvcs:
                    print(f"PVC: {pvc.metadata.name}, Status: {pvc.status.phase}")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                pvc_list = self.coreV1Api.list_namespaced_persistent_volume_claim(
                    namespace=namespace, watch=False
                )
                if regex:
                    prog = re.compile(regex)
                    return CommonResult(
                        [pvc for pvc in pvc_list.items if prog.match(pvc.metadata.name)]
                    )
                return CommonResult(pvc_list.items)
            except ApiException as e:
                LOGGER.info(f"Error retrieving PVC: {str(e)}")
                if e.status == 401:
                    # Bearer token is expired
                    self._initialize_client()

            time.sleep(10)
        return CommonResult([], "FAIL: list pvc timeout")

    def list_storage_classes(self, timeout=300) -> CommonResult[List[dict], str]:
        """
        List available Storage Classes in the cluster.

        Args:
            timeout (int): Maximum time in seconds to wait for the operation. Defaults to 300.

        Returns:
            CommonResult: Operation result containing:
                - List[dict]: List of dictionaries with storage class details
                - str: Error message if an exception occurred or timeout was reached

        Example:
            storage_classes, err_msg = client.list_storage_classes()
            if err_msg:
                print(f"Failed to list storage classes: {err_msg}")
            else:
                print(f"Found {len(storage_classes)} storage classes")
                for sc in storage_classes:
                    print(f"Name: {sc['name']}, Provisioner: {sc['provisioner']}")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            try:
                storage_classes = self.storageV1Api.list_storage_class()
                LOGGER.info(f"kubernetes_utils list_storage_classes: {storage_classes}")
                return CommonResult(
                    [
                        {
                            "name": sc.metadata.name,
                            "provisioner": sc.provisioner,
                            "volumebindingmode": sc.volume_binding_mode,
                        }
                        for sc in storage_classes.items
                    ]
                )
            except ApiException as e:
                LOGGER.error(
                    f"Exception when calling StorageV1Api->list_storage_class: {e}"
                )
            time.sleep(10)
        return CommonResult([], "FAIL: list storage classes timeout")

    def update_pvc_reclaim_policy(
        self,
        pvc_name,
        namespace="runai-qa-automation-test",
        storage_namespace="dgxc-tenant-cluster-policies",
    ) -> CommonResult[str, str]:
        """
        Update the reclaim policy of a PV associated with a PVC from Retain to Delete.

        Args:
            pvc_name (str): Name of the PVC
            namespace (str): Namespace of the PVC. Defaults to "runai-qa-automation-test".
            storage_namespace (str): Namespace of the storage resources. Defaults to "dgxc-tenant-cluster-policies".

        Returns:
            CommonResult: Operation result containing:
                - str: Command output if successful
                - str: Error message if an exception occurred

        Example:
            output, err_msg = client.update_pvc_reclaim_policy("my-pvc", "default")
            if err_msg:
                print(f"Failed to update reclaim policy: {err_msg}")
            else:
                print("Reclaim policy updated successfully")
        """
        try:
            # Get PVC to get PV name
            pvc = self.coreV1Api.read_namespaced_persistent_volume_claim(
                name=pvc_name, namespace=namespace
            )

            if not pvc.spec.volume_name:
                LOGGER.error(f"No PV associated with PVC {pvc_name}")
                return CommonResult(None, "No PV associated with PVC %s" % pvc_name)

            # Get the PV name
            pv_name = pvc.spec.volume_name

            # Get current runaidgxcstorages content
            cmd = f"kubectl get runaidgxcstorages -n {storage_namespace} -o json"
            result = subprocess.run(
                cmd, shell=True, check=True, capture_output=True, text=True
            )
            storage_data = json.loads(result.stdout)

            # Find the storage resource that contains our PV
            for item in storage_data.get("items", []):
                if pv_name in item.get("spec", {}).get("instances", {}):
                    storage_name = item["metadata"]["name"]

                    # Update the reclaim policy
                    patch = f'[{{"op": "replace", "path": "/spec/instances/{pv_name}/persistentVolumeReclaimPolicy", "value": "Delete"}}]'
                    cmd = f"kubectl patch runaidgxcstorages {storage_name} -n {storage_namespace} --type json -p '{patch}'"

                    result = subprocess.run(
                        cmd, shell=True, check=True, capture_output=True, text=True
                    )
                    LOGGER.info(
                        f"Successfully updated reclaim policy for PVC {pvc_name} (PV: {pv_name})"
                    )
                    return CommonResult(result.stdout)

            LOGGER.error(f"PV {pv_name} not found in any runaidgxcstorages resource")

        except (ApiException, subprocess.CalledProcessError) as e:
            LOGGER.error(f"Error updating reclaim policy: {str(e)}")
            return CommonResult(None, str(e))

    def get_pv_from_pvc(
        self, pvc_name, namespace
    ) -> CommonResult[client.V1PersistentVolume, str]:
        """
        Get the Persistent Volume associated with a PVC.

        Args:
            pvc_name (str): Name of the PVC
            namespace (str): Namespace where the PVC exists

        Returns:
            CommonResult: Operation result containing:
                - client.V1PersistentVolume: The associated PV object if found
                - str: Error message if no PV is associated or an exception occurred

        Example:
            pv, err_msg = client.get_pv_from_pvc("my-pvc", "default")
            if err_msg:
                print(f"Failed to get PV: {err_msg}")
            else:
                print(f"Associated PV: {pv.metadata.name}, Capacity: {pv.spec.capacity['storage']}")
        """
        try:
            # First get the PVC
            pvc = self.coreV1Api.read_namespaced_persistent_volume_claim(
                name=pvc_name, namespace=namespace
            )

            if not pvc.spec.volume_name:
                LOGGER.info(f"PVC {pvc_name} has no associated PV yet")
                return CommonResult(None, "PVC %s has no associated PV yet" % pvc_name)

            # Get the associated PV
            pv = self.coreV1Api.read_persistent_volume(name=pvc.spec.volume_name)

            return CommonResult(pv)

        except ApiException as e:
            LOGGER.error(f"Error getting PV for PVC {pvc_name}: {e}")
            return CommonResult(None, str(e))

    def get_pvc_events(
        self, pvc_name, namespace
    ) -> CommonResult[List[client.EventsV1Event], str]:
        """
        Get events related to a specific PVC.

        Args:
            pvc_name (str): Name of the PVC
            namespace (str): Namespace where the PVC exists

        Returns:
            CommonResult: Operation result containing:
                - List[client.EventsV1Event]: List of event objects related to the PVC
                - str: Error message if an exception occurred

        Example:
            events, err_msg = client.get_pvc_events("my-pvc", "default")
            if err_msg:
                print(f"Failed to get PVC events: {err_msg}")
            else:
                print(f"Found {len(events)} events for PVC")
                for event in events:
                    print(f"Event: {event.reason}, Message: {event.message}")
        """
        field_selector = f"involvedObject.name={pvc_name}"
        try:
            events = self.coreV1Api.list_namespaced_event(
                namespace=namespace, field_selector=field_selector
            )
            return CommonResult(events.items)
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->list_namespaced_event: {e}")
            return CommonResult([], str(e))

    def list_dgxc_storage(
        self,
        namespace: str = "dgxc-tenant-cluster-policies",
        name_pattern: Optional[str] = None,
        **kwargs,
    ) -> CommonResult[List[object], str]:
        """
        List DGXC storage resources (runaidgxcstorages.runai.dgxc.nvidia.com) with optional filtering.

        Args:
            namespace: The namespace to list storage resources from (default: "dgxc-tenant-cluster-policies")
            name_pattern: Optional regex pattern to filter storage resource names
            **kwargs: Additional parameters to pass to the API

        Returns:
            CommonResult: A collection of DGXC storage resources as dictionaries.
            The result can be used like a list to iterate through the storage resources.

        Example:
            # Get all DGXC storage resources
            storages, _ = client.list_dgxc_storage()

            # Check if any resources were found
            if len(storages) > 0:
                # Iterate through the resources
                for storage in storages:
                    print(f"Name: {storage['metadata']['name']}")
                    print(f"Storage Class: {storage.get('spec', {}).get('storageClass', 'N/A')}")
                    print(f"Quota: {storage.get('spec', {}).get('quota', 'N/A')}")
                    print(f"Quota Used: {storage.get('status', {}).get('quotaUsed', 'N/A')}")

            # Filter resources by name pattern
            filtered_storages, _ = client.list_dgxc_storage(name_pattern="enterprise")
            print(f"Found {len(filtered_storages)} enterprise storage resources")
        """
        try:
            # List the DGXC storage resources
            storage_list = self.customObjectApi.list_namespaced_custom_object(
                group="runai.dgxc.nvidia.com",
                version="v1beta1",
                namespace=namespace,
                plural="runaidgxcstorages",
                **kwargs,
            )

            # Extract the items
            items = storage_list.get("items", [])

            # Filter by name pattern if specified
            if name_pattern:
                pattern = re.compile(name_pattern)
                items = [
                    item
                    for item in items
                    if pattern.search(item.get("metadata", {}).get("name", ""))
                ]

            return CommonResult(items)
        except client.ApiException as e:
            error_msg = f"Exception when calling CustomObjectsApi->list_namespaced_custom_object for runaidgxcstorages: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = (
                f"Unexpected error listing DGXC storage in namespace {namespace}: {e}"
            )
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    def list_nvstoragelocations(
        self, namespace: str = "default", name_pattern: Optional[str] = None, **kwargs
    ) -> CommonResult[List[object], str]:
        """
        List NVIDIA storage locations (nvstoragelocations custom resources) with optional filtering.

        Args:
            namespace: The namespace to list storage locations from (default: "default")
            name_pattern: Optional regex pattern to filter storage location names
            **kwargs: Additional parameters to pass to the API

        Returns:
            CommonResult: A collection of storage location resources as dictionaries.
            The result can be used like a list to iterate through the storage locations.

        Example:
            # Get all storage locations in the default namespace
            locations, _ = client.list_nvstoragelocations()

            # Check if any locations were found
            if len(locations) > 0:
                # Iterate through the locations
                for location in locations:
                    print(f"Name: {location['metadata']['name']}")
                    print(f"Type: {location.get('spec', {}).get('type', 'N/A')}")
                    print(f"Path: {location.get('spec', {}).get('path', 'N/A')}")

            # Filter locations by name pattern
            filtered_locations, _ = client.list_nvstoragelocations(name_pattern="nfs")
            print(f"Found {len(filtered_locations)} NFS storage locations")

            # Get locations in a specific namespace
            tenant_locations, _ = client.list_nvstoragelocations(namespace="tenant-namespace")
            print(f"Found {len(tenant_locations)} storage locations in tenant namespace")
        """
        try:
            # List the storage location resources
            storage_locations = self.customObjectApi.list_namespaced_custom_object(
                group="storage.dgxc.nvidia.com",
                version="v1beta1",
                namespace=namespace,
                plural="nvstoragelocations",
                **kwargs,
            )

            # Extract the items
            items = storage_locations.get("items", [])

            # Filter by name pattern if specified
            if name_pattern:
                pattern = re.compile(name_pattern)
                items = [
                    item
                    for item in items
                    if pattern.search(item.get("metadata", {}).get("name", ""))
                ]

            return CommonResult(items)
        except client.ApiException as e:
            error_msg = f"Exception when calling CustomObjectsApi->list_namespaced_custom_object for nvstoragelocations: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = (
                f"Unexpected error listing storage locations in namespace {namespace}: {e}"
            )
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    def delete_nvstoragelocation(
        self, name: str, namespace: str
    ) -> CommonResult[bool, str]:
        """
        Delete an NVIDIA storage location custom resource.

        Args:
            name (str): Name of the storage location to delete
            namespace (str): Namespace where the storage location exists

        Returns:
            CommonResult: Operation result containing:
                - bool: True if the storage location was deleted successfully, False otherwise
                - str: Error message if an exception occurred

        Example:
            deleted, err_msg = client.delete_nvstoragelocation("my-storage", "default")
            if err_msg:
                print(f"Failed to delete storage location: {err_msg}")
            elif deleted:
                print("Storage location deleted successfully")
            else:
                print("Storage location deletion failed")
        """
        group = "storage.dgxc.nvidia.com"
        version = "v1beta1"
        plural = "nvstoragelocations"

        try:
            self.customObjectApi.delete_namespaced_custom_object(
                group=group,
                version=version,
                namespace=namespace,
                plural=plural,
                name=name,
                body=client.V1DeleteOptions(),
            )
            return CommonResult(True)
        except ApiException as e:
            LOGGER.error(
                f"Exception when calling CustomObjectsApi->delete_namespaced_custom_object: {e}"
            )
            return CommonResult(False, str(e))

    def get_storage_info_from_kubectl(
        self, namespace="dgxc-tenant-cluster-policies"
    ) -> CommonResult[Dict[str, Dict], str]:
        """
        Get storage information using kubectl command line instead of API.
        Args:
            namespace (str): The namespace where runaidgxcstorages resources are located.
                            Default is "dgxc-tenant-cluster-policies".
        Returns:
            CommonResult: Operation result containing:
                - result: Dictionary containing storage information with storage names as keys
                - error_msg: Error message if an exception occurred
        Example:
            # Get storage information
            storage_result = client.get_storage_info_from_kubectl()
            # Check if operation was successful
            if storage_result:
                storage_info = storage_result.data
                # Access specific storage information
                enterprise_file = storage_info.get("dgxc-enterprise-file", {})
                quota_used = enterprise_file.get("quota_used")
                storage_consumed = enterprise_file.get("storage_consumed")
                print(f"Enterprise file quota used: {quota_used}")
                print(f"Enterprise file storage consumed: {storage_consumed}")
                # Iterate through all storage resources
                for name, info in storage_info.items():
                    print(f"Storage: {name}")
                    print(f"  Quota: {info['quota']}")
                    print(f"  Quota Used: {info['quota_used']}")
                    print(f"  Usage Percentage: {info['quota_used_pct']}")
            else:
                print(f"Error: {storage_result.error_msg}")
        """
        try:
            cmd = f"kubectl get -n {namespace} runaidgxcstorages -o json"
            result = subprocess.run(
                cmd,
                shell=True,
                check=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                universal_newlines=True,
            )
            storage_data = json.loads(result.stdout)
            storage_info = {}
            for item in storage_data.get("items", []):
                name = item["metadata"]["name"]
                spec = item.get("spec", {})
                status = item.get("status", {})
                storage_info[name] = {
                    "name": name,
                    "storage_classes": spec.get("storageClasses", []),
                    "quota_used_pct": status.get("storageUsedPct"),
                    "quota": status.get("storageQuota"),
                    "quota_used": status.get("storageRequested"),
                    "storage_consumed": status.get("storageConsumed"),
                }
            LOGGER.info("Storage information in namespace %s:", namespace)
            LOGGER.info(
                "%-21s %-15s %-12s %-10s %-10s %s",
                "NAME",
                "STORAGECLASSES",
                "QUOTAUSEDPCT",
                "QUOTA",
                "QUOTAUSED",
                "STORAGECONSUMED",
            )
            for name, info in storage_info.items():
                storage_classes = (
                    ",".join(info["storage_classes"]) if info["storage_classes"] else ""
                )
                quota_used_pct = (
                    f"{info['quota_used_pct']}%"
                    if info["quota_used_pct"] is not None
                    else "N/A"
                )
                LOGGER.info(
                    "%-21s %-15s %-12s %-10s %-10s %s",
                    name,
                    storage_classes,
                    quota_used_pct,
                    info["quota"] or "N/A",
                    info["quota_used"] or "N/A",
                    info["storage_consumed"] or "N/A",
                )
            return CommonResult(storage_info)
        except subprocess.CalledProcessError as e:
            error_msg = f"Failed to execute kubectl command: {e.stderr}"
            LOGGER.error(error_msg)
            return CommonResult({}, error_msg)
        except json.JSONDecodeError as e:
            error_msg = f"Failed to parse kubectl output: {e}"
            LOGGER.error(error_msg)
            return CommonResult({}, error_msg)
        except Exception as e:
            error_msg = f"Error getting storage info: {str(e)}"
            LOGGER.error(error_msg)
            return CommonResult({}, error_msg)

    def get_storage_class_by_csp(self, csp: str, testcase_version: str) -> str:
        """
        Get the storage class for a given CSP.

        Args:
            csp (str): The CSP to get the storage class for.
        """

        test_storage = StorageClassConfig.get_by_csp(csp)
        if csp == SupportedCSP.GCP.value and testcase_version == "DGXC_Sprint_1.1":
            return test_storage["standard"]
        if csp == SupportedCSP.AWS.value and testcase_version == "DGXC_Sprint_1.1":
            return test_storage["lustre"]
        if csp == SupportedCSP.OCI.value and testcase_version == "DGXC_Sprint_1.1":
            return test_storage["fss"]
        if csp == SupportedCSP.AZURE.value and testcase_version == "DGXC_Sprint_1.1":
            return test_storage["csi"]
        if csp == SupportedCSP.GCP.value and testcase_version == "DGXC_Sprint_1.2":
            return "dgxc-standard-file"
        if csp == SupportedCSP.AWS.value and testcase_version == "DGXC_Sprint_1.2":
            return "dgxc-enterprise-file"
        return None

    def get_pv_details(self, pv_name: str) -> CommonResult[Dict[str, Any], str]:
        """
        Get detailed information about a Persistent Volume.

        Args:
            pv_name (str): Name of the Persistent Volume

        Returns:
            CommonResult: Operation result containing:
                - Dict[str, Any]: Dictionary with PV details
                - str: Error message if an exception occurred
        """
        try:
            pv = self.coreV1Api.read_persistent_volume(name=pv_name)

            # Extract basic metadata
            pv_details = {}

            # 始终添加reclaim_policy，即使为None
            pv_details["reclaim_policy"] = pv.spec.persistent_volume_reclaim_policy

            # 其他字段只在非空时添加
            if pv.metadata.uid:
                pv_details["uid"] = pv.metadata.uid

            if pv.metadata.name:
                pv_details["name"] = pv.metadata.name

            if pv.metadata.creation_timestamp:
                pv_details["creation_timestamp"] = pv.metadata.creation_timestamp

            if pv.spec.storage_class_name:
                pv_details["storage_class"] = pv.spec.storage_class_name

            pv_details["capacity"] = (
                pv.spec.capacity.get("storage") if pv.spec.capacity else None
            )

            pv_details["access_modes"] = pv.spec.access_modes or []

            if pv.spec.volume_mode:
                pv_details["volume_mode"] = pv.spec.volume_mode

            # Extract namespace from claimRef if available
            if pv.spec.claim_ref:
                if pv.spec.claim_ref.namespace:
                    pv_details["namespace"] = pv.spec.claim_ref.namespace

                if pv.spec.claim_ref.name:
                    pv_details["claim_name"] = pv.spec.claim_ref.name

                if pv.spec.claim_ref.uid:
                    pv_details["claim_uid"] = pv.spec.claim_ref.uid

            # Extract CSI volume handle if available
            if pv.spec.csi:
                if pv.spec.csi.driver:
                    pv_details["csi_driver"] = pv.spec.csi.driver

                if pv.spec.csi.volume_handle:
                    pv_details["volume_handle"] = pv.spec.csi.volume_handle

                pv_details["volume_attributes"] = pv.spec.csi.volume_attributes or {}

            # Extract mount options if available
            if hasattr(pv.spec, "mount_options") and pv.spec.mount_options:
                pv_details["mount_options"] = pv.spec.mount_options

            # Extract labels if available
            if pv.metadata.labels:
                pv_details["labels"] = pv.metadata.labels

            # Extract annotations if available
            if pv.metadata.annotations:
                pv_details["annotations"] = pv.metadata.annotations

            return CommonResult(pv_details)

        except ApiException as e:
            error_msg = f"Error getting PV details for {pv_name}: {str(e)}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error getting PV details for {pv_name}: {str(e)}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    #############################################
    # Node Management
    #############################################

    def get_node_names(self) -> CommonResult[List[str], str]:
        """
        Get all node names in the cluster.

        Returns:
            CommonResult containing:
                - List[str]: Sorted list of node names if successful
                - str: Error message if an exception occurred

        Example:
            node_names, err_msg = client.get_node_names()
            if err_msg:
                print(f"Failed to get node names: {err_msg}")
            else:
                print(f"Found nodes: {node_names}")
        """
        try:
            v1NodeList = self.coreV1Api.list_node()
            nodes = [node for node in v1NodeList.items]
            node_names = [node.metadata.name for node in nodes]
            node_names.sort()
            return CommonResult(node_names)
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->list_node: {e}")
            return CommonResult([], str(e))

    def get_node_names_by_label(
        self, label_selector: str = "nodeGroup=gpu"
    ) -> CommonResult[List[str], str]:
        """
        Get all node names matching a label selector.

        Args:
            label_selector: Kubernetes label selector string (e.g. "nodeGroup=gpu", "environment=prod")

        Returns:
            CommonResult containing:
                - List[str]: Sorted list of matching node names if successful
                - str: Error message if an exception occurred

        Example:
            gpu_nodes, err_msg = client.get_node_names_by_label("nodeGroup=gpu")
            if err_msg:
                print(f"Failed to get GPU nodes: {err_msg}")
            else:
                print(f"Found GPU nodes: {gpu_nodes}")
        """
        try:
            v1NodeList = self.coreV1Api.list_node(label_selector=label_selector)
            nodes = [node for node in v1NodeList.items]
            node_names = [node.metadata.name for node in nodes]
            node_names.sort()
            return CommonResult(node_names)
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->list_node: {e}")
            return CommonResult([], str(e))

    def get_nodes(
        self, node_type: str = "gpu", ready: bool = True
    ) -> CommonResult[List[client.V1Node], str]:
        """
        Get nodes filtered by type and readiness state.

        Args:
            node_type: Type of nodes to get - one of:
                - "gpu": Nodes labeled with nodeGroup=customer-gpu, default
                - "cpu": Nodes labeled with nodeGroup=customer-cpu
                - "system": Nodes labeled with nodeGroup=system-cpu
            ready: If True, only return nodes that are ready and schedulable

        Returns:
            CommonResult containing:
                - List[V1Node]: List of matching node objects if successful
                - str: Error message if an exception occurred

        Example:
            gpu_nodes, err_msg = client.get_nodes("gpu", ready=True)
            if err_msg:
                print(f"Failed to get GPU nodes: {err_msg}")
            else:
                print(f"Found {len(gpu_nodes)} ready GPU nodes")
        """
        node_list = []
        selector = None
        if node_type.lower() == "cpu":
            selector = "nvidia.com/gpu.present!=true"
        elif node_type.lower() == "gpu":
            selector = "nvidia.com/gpu.present=true"

        if selector:
            v1NodeList = self.coreV1Api.list_node(label_selector=selector)
        else:
            return CommonResult([], "Invalid node type")

        for node in v1NodeList.items:
            node_name = node.metadata.name
            node_gpus = (
                node.status.capacity.get("nvidia.com/gpu", 0)
                if node_type.lower() == "gpu"
                else 0
            )
            if ready:
                schedulable = not node.spec.unschedulable
                node_ready = None
                for condition in node.status.conditions:
                    if condition.type == "Ready":
                        node_ready = condition.status == "True"
                LOGGER.info(
                    f"{node_type} node {node_name}: GPU capacity={node_gpus}, ready={node_ready}, "
                    f"schedulable={schedulable}"
                )
                if schedulable and node_ready:
                    node_list.append(node)
            else:
                node_list.append(node)
        return CommonResult(node_list)

    def get_node_ip(self, node_name: str) -> CommonResult[str, str]:
        """
        Get the internal IP address of a node.

        Args:
            node_name: Name of the node to get the IP for

        Returns:
            CommonResult containing:
                - str: Node's internal IP address if found
                - str: Error message if an exception occurred or IP not found

        Example:
            node_ip, err_msg = client.get_node_ip("worker-1")
            if err_msg:
                print(f"Failed to get node IP: {err_msg}")
            else:
                print(f"Node IP: {node_ip}")
        """
        try:
            nodes = self.coreV1Api.list_node().items
            for node in nodes:
                if node.metadata.name == node_name:
                    for item in node.status.addresses:
                        if item.type == "InternalIP":
                            node_ip = item.address
                            return CommonResult(node_ip)
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->list_node: {e}")
            return CommonResult(None, str(e))
        except Exception as e:
            LOGGER.error(f"Unexpected error getting node IP: {e}")
            return CommonResult(None, str(e))

    def get_node_name_ip_list(
        self, node_type: str = "gpu"
    ) -> CommonResult[List[Dict[str, str]], str]:
        """
        Get a list of node names and their IP addresses.

        Args:
            node_type: Type of nodes to list - one of:
                - "gpu": Nodes labeled with nodeGroup=customer-gpu, default
                - "cpu": Nodes labeled with nodeGroup=customer-cpu
                - "system": Nodes labeled with nodeGroup=system-cpu
                - "all": All nodes regardless of type

        Returns:
            CommonResult containing:
                - List[Dict[str, str]]: List of dicts with node info if successful:
                    [
                        {
                            "node_name": "worker-1",
                            "node_ip": "10.0.0.1"
                        },
                        ...
                    ]
                - str: Error message if an exception occurred

        Example:
            nodes, err_msg = client.get_node_name_ip_list("gpu")
            if err_msg:
                print(f"Failed to get node info: {err_msg}")
            else:
                for node in nodes:
                    print(f"Node {node['node_name']} has IP {node['node_ip']}")
        """
        node_ip_list = []
        selector = None
        if node_type.lower() == "cpu":
            selector = "nodeGroup=customer-cpu"
        elif node_type.lower() == "gpu":
            selector = "nodeGroup=customer-gpu"
        elif node_type.lower() == "system":
            selector = "nodeGroup=system-cpu"

        try:
            if selector:
                nodes = self.coreV1Api.list_node(label_selector=selector).items
            else:
                nodes = self.coreV1Api.list_node().items
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->list_node: {e}")
            return CommonResult([], str(e))
        except Exception as e:
            LOGGER.error(f"Unexpected error getting node name and IP list: {e}")
            return CommonResult([], str(e))

        for node in nodes:
            node_name = node.metadata.name
            node_ip = [
                item.address for item in node.status.addresses if item.type == "InternalIP"
            ]
            assert node_ip, "Failed to get node ip info"
            node_ip = node_ip[0]
            LOGGER.info(f"node {node_name}: ip={node_ip}")
            node_ip_list.append({"node_name": node_name, "node_ip": node_ip})
        return CommonResult(node_ip_list)

    def get_node_events(self, node_name) -> CommonResult[List[client.EventsV1Event], str]:
        """
        Get Kubernetes events related to a specific node.

        Args:
            node_name: Name of the node to get events for

        Returns:
            CommonResult containing:
                - List[EventsV1Event]: List of event objects if successful
                - str: Error message if an exception occurred

        Example:
            events, err_msg = client.get_node_events("worker-1")
            if err_msg:
                print(f"Failed to get node events: {err_msg}")
            else:
                for event in events:
                    print(f"Event: {event.message}")
        """
        field_selector = f"involvedObject.kind=Node,involvedObject.name={node_name}"
        try:
            events = self.coreV1Api.list_event_for_all_namespaces(
                field_selector=field_selector
            )
            LOGGER.info(f"Node name: {node_name}")
            for event in events.items:
                LOGGER.info(f"Event: {event}")
            return CommonResult(events.items)
        except ApiException as e:
            LOGGER.error(
                f"Exception when calling CoreV1Api->list_event_for_all_namespaces: {e}"
            )
            return CommonResult([], str(e))
        except Exception as e:
            LOGGER.error(f"Unexpected error getting node events: {e}")
            return CommonResult([], str(e))

    def get_node_by_name(
        self, node_name, node_type="gpu"
    ) -> CommonResult[client.V1Node, str]:
        """
        Get a node object by its name.

        Args:
            node_name: Name of the node to get
            node_type: Type of node to look for - one of:
                - "gpu": Nodes labeled with nodeGroup=customer-gpu, default
                - "cpu": Nodes labeled with nodeGroup=customer-cpu
                - "system": Nodes labeled with nodeGroup=system-cpu

        Returns:
            CommonResult containing:
                - V1Node: Node object if found
                - str: Error message if an exception occurred or node not found

        Example:
            node, err_msg = client.get_node_by_name("worker-1", "gpu")
            if err_msg:
                print(f"Failed to get node: {err_msg}")
            else:
                print(f"Found node: {node.metadata.name}")
        """
        nodes, err_msg = self.get_nodes(node_type=node_type, ready=False)
        if err_msg:
            return CommonResult(None, err_msg)
        matched_node = next(
            (node for node in nodes if node.metadata.name == node_name), None
        )
        if not matched_node:
            return CommonResult(None, f"Node {node_name} not found")
        return CommonResult(matched_node)

    def read_node_condition_by_type(
        self, node_name, condition_type
    ) -> CommonResult[client.V1NodeCondition, str]:
        """
        Get a specific condition from a node's status.

        Args:
            node_name: Name of the node to check
            condition_type: Type of condition to look for (e.g. "Ready", "DiskPressure")

        Returns:
            CommonResult containing:
                - V1NodeCondition: Condition object if found
                - str: Error message if an exception occurred or condition not found

        Example:
            condition, err_msg = client.read_node_condition_by_type("worker-1", "Ready")
            if err_msg:
                print(f"Failed to get condition: {err_msg}")
            else:
                print(f"Ready status: {condition.status}")
        """
        for i in range(3):  # retry to avoid timeout exceptions when reading node frequently
            try:
                retval = self.coreV1Api.read_node(node_name)
                if retval:
                    break
            except Exception:  # pylint: disable=bare-except
                LOGGER.info(f"Exception occurred while reading node, retry: {i+1}")
                time.sleep(10)  # retry after sleeping for 10 seconds
        if not retval:
            return CommonResult(None, "FAIL: Failed to call read_node() API.")
        condition = next(
            (
                condition
                for condition in retval.status.conditions
                if condition.type == condition_type
            ),
            None,
        )
        if not condition:
            return CommonResult(None, f"FAIL: No condition which type is {condition_type}")
        return CommonResult(condition)

    def remove_node_condition(self, node_name, condition_type) -> CommonResult[bool, str]:
        """
        Remove a specific condition from a node's status.

        Args:
            node_name: Name of the node to modify
            condition_type: Type of condition to remove

        Returns:
            CommonResult containing:
                - bool: True if condition was removed successfully
                - str: Error message if an exception occurred

        Example:
            success, err_msg = client.remove_node_condition("worker-1", "DiskPressure")
            if err_msg:
                print(f"Failed to remove condition: {err_msg}")
            elif success:
                print("Condition removed successfully")
        """
        success = False
        for attempt in range(5):
            try:
                # Read the node status
                node = self.coreV1Api.read_node_status(node_name)

                # Remove the specific condition from the node status
                if node.status.conditions:
                    new_conditions = [
                        condition
                        for condition in node.status.conditions
                        if condition.type != condition_type
                    ]
                else:
                    new_conditions = []

                # Create a new node status object with updated conditions
                node.status.conditions = new_conditions

                # Update the node status
                updated_node = self.coreV1Api.replace_node_status(node_name, node)

                # Check if the operation was successful
                if updated_node:
                    LOGGER.info(
                        f"Successfully removed condition {condition_type} from {node_name} on attempt {attempt + 1}."
                    )
                    success = True
                    break
            except ApiException as e:
                LOGGER.error(
                    f"ApiException when calling CoreV1Api->replace_node_status: {e}"
                )
            except Exception as e:
                LOGGER.error(f"Exception when calling CoreV1Api->replace_node_status: {e}")
                return CommonResult(False, str(e))

        if not success:
            return CommonResult(
                False, f"FAIL to remove condition {condition_type} on {node_name}"
            )
        return CommonResult(True)

    def check_taints_on_node(self, node_name, conditions) -> CommonResult[bool, str]:
        """
        Check if specific taints exist on a node.

        Args:
            node_name: Name of the node to check
            conditions: List of dicts specifying taints to check for. Each dict should have:
                - key: Taint key
                - value: Taint value
                - effect: Taint effect (e.g. "NoSchedule", "PreferNoSchedule")

        Returns:
            CommonResult containing:
                - bool: True if all specified taints are found
                - str: Error message if an exception occurred or taints not found

        Example:
            conditions = [
                {
                    "key": "key1",
                    "value": "value1",
                    "effect": "NoSchedule"
                }
            ]
            found, err_msg = client.check_taints_on_node("worker-1", conditions)
            if err_msg:
                print(f"Failed to check taints: {err_msg}")
            elif found:
                print("All specified taints found")
        """
        node_info, err_msg = self.get_node_by_name(node_name)
        if err_msg:
            return CommonResult(False, err_msg)
        taints_dict = {
            (item.key, item.value, item.effect): item for item in node_info.spec.taints
        }
        for condition in conditions:
            key = (condition["key"], condition["value"], condition["effect"])
            if key not in taints_dict:
                return CommonResult(
                    False, f"FAIL: Taint {key} not found on node {node_name}"
                )
        return CommonResult(True)

    def get_annotation_on_node(self, node_name, annotation) -> CommonResult[str, str]:
        """
        Get the value of a specific annotation from a node.

        Args:
            node_name: Name of the node to check
            annotation: Annotation key to look up

        Returns:
            CommonResult containing:
                - str: Annotation value if found, None if not found
                - str: Error message if an exception occurred

        Example:
            value, err_msg = client.get_annotation_on_node("worker-1", "example.com/my-annotation")
            if err_msg:
                print(f"Failed to get annotation: {err_msg}")
            elif value is not None:
                print(f"Annotation value: {value}")
        """
        node_info, err_msg = self.get_node_by_name(node_name)
        if err_msg:
            return CommonResult(None, err_msg)
        annotations = node_info.metadata.annotations
        return CommonResult(annotations.get(annotation))

    def remove_annotation_on_node(self, node_name, annotation) -> CommonResult[bool, str]:
        """
        Remove a specific annotation from a node.

        Args:
            node_name: Name of the node to modify
            annotation: Annotation key to remove

        Returns:
            CommonResult containing:
                - bool: True if annotation was removed successfully
                - str: Error message if an exception occurred

        Example:
            success, err_msg = client.remove_annotation_on_node("worker-1", "example.com/my-annotation")
            if err_msg:
                print(f"Failed to remove annotation: {err_msg}")
            elif success:
                print("Annotation removed successfully")
        """
        body = {"metadata": {"annotations": {annotation: None}}}
        retval = self.coreV1Api.patch_node(name=node_name, body=body)
        if retval.metadata.annotations.get(annotation) is None:
            LOGGER.info(f"Annotation {annotation} removed from node {node_name}.")
            return CommonResult(True)
        return CommonResult(False, f"FAIL to remove annotation {annotation} on {node_name}")

    def remove_label_from_node(self, node_name, label) -> CommonResult[bool, str]:
        """
        Remove a specific label from a node.

        Args:
            node_name: Name of the node to modify
            label: Label key to remove

        Returns:
        """
        body = {"metadata": {"labels": {label: None}}}
        retval = self.coreV1Api.patch_node(name=node_name, body=body)
        if retval.metadata.labels.get(label) is None:
            LOGGER.info(f"Label {label} removed from node {node_name}.")
            return CommonResult(True)
        return CommonResult(False, f"FAIL to remove label {label} on {node_name}")

    def add_label_to_node(self, node_name, label_key, label_value) -> CommonResult[bool, str]:
        """
        Add a specific label to a node.

        Args:
            node_name: Name of the node to modify
            label_key: Key of the label to add
            label_value: Value of the label to add

        Returns:
            CommonResult containing:
                - bool: True if label was added successfully
                - str: Error message if an exception occurred
        """
        retval = self.coreV1Api.patch_node(name=node_name, body={"metadata": {"labels": {label_key: str(label_value)}}})
        if retval.metadata.labels.get(label_key) == label_value:
            LOGGER.info(f"Label {label_key} added to node {node_name}.")
            return CommonResult(True)
        return CommonResult(False, f"FAIL to add label {label_key} to {node_name}")

    def get_label_on_node(self, node_name, label_key) -> CommonResult[str, str]:
        """
        Get the value of a specific label from a node.

        Args:
            node_name: Name of the node to check
            label_key: Key of the label to get

        Returns:
            CommonResult containing:
                - str: Label value if found, None if not found

        Example:
            value, err_msg = client.get_label_on_node("worker-1", "k8saas.nvidia.com/ManagedByNVSentinel")
            if err_msg:
                print(f"Failed to get label: {err_msg}")
            elif value is not None:
                print(f"Label value: {value}")
        """
        node_info, err_msg = self.get_node_by_name(node_name)
        if err_msg:
            return CommonResult(None, err_msg)
        return CommonResult(node_info.metadata.labels.get(label_key))

    def remove_taint_on_node(self, node_name, taint_key) -> CommonResult[bool, str]:
        """
        Remove a specific taint from a node.

        Args:
            node_name: Name of the node to modify
            taint_key: Key of the taint to remove

        Returns:
            CommonResult containing:
                - bool: True if taint was removed successfully
                - str: Error message if an exception occurred

        Example:
            success, err_msg = client.remove_taint_on_node("worker-1", "key1")
            if err_msg:
                print(f"Failed to remove taint: {err_msg}")
            elif success:
                print("Taint removed successfully")
        """
        node, err_msg = self.get_node_by_name(node_name)
        if err_msg:
            return CommonResult(False, err_msg)
        if node.spec.taints:
            new_taints = [taint for taint in node.spec.taints if taint.key != taint_key]
            body = {"spec": {"taints": new_taints}}
            retval = self.coreV1Api.patch_node(name=node_name, body=body)
            if taint_key not in [taint.key for taint in retval.spec.taints]:
                LOGGER.info(f"Taint {taint_key} removed from node {node_name}.")
                return CommonResult(True)
            return CommonResult(False, f"FAIL to remove taint {taint_key} on {node_name}")
        else:
            LOGGER.info(f"No taints found on node {node_name}.")
            return CommonResult(True)

    def uncordon_node(self, node_name) -> CommonResult[bool, str]:
        """
        Make a node schedulable by removing the unschedulable flag.

        Args:
            node_name: Name of the node to uncordon

        Returns:
            CommonResult containing:
                - bool: True if node was uncordoned successfully
                - str: Error message if an exception occurred

        Example:
            success, err_msg = client.uncordon_node("worker-1")
            if err_msg:
                print(f"Failed to uncordon node: {err_msg}")
            elif success:
                print("Node uncordoned successfully")
        """
        LOGGER.info("Uncondorning node: %s", node_name)
        try:
            retval = self.coreV1Api.patch_node(node_name, {"spec": {"unschedulable": None}})
            LOGGER.debug(
                "Result: %s",
                retval.metadata.labels
                if hasattr(retval, "metadata") and hasattr(retval.metadata, "labels")
                else retval,
            )
            if retval.spec.unschedulable is None:
                LOGGER.info(f"Node {node_name} uncordoned successfully.")
                return CommonResult(True)
            return CommonResult(False, f"FAIL to uncordon node {node_name}")
        except ApiException as e:
            LOGGER.error(f"Exception when calling CoreV1Api->patch_node: {e}")
            return CommonResult(False, str(e))
        except Exception as e:
            LOGGER.error(f"Unexpected error uncordoning node: {e}")
            return CommonResult(False, str(e))

    def check_node_ready(self, node_name) -> CommonResult[bool, str]:
        """
        Check if a node is in the Ready state.

        Args:
            node_name: Name of the node to check

        Returns:
            CommonResult containing:
                - bool: True if node is ready, False otherwise
                - str: Error message if an exception occurred

        Example:
            ready, err_msg = client.check_node_ready("worker-1")
            if err_msg:
                print(f"Failed to check node: {err_msg}")
            elif ready:
                print("Node is ready")
            else:
                print("Node is not ready")
        """
        node, err_msg = self.get_node_by_name(node_name)
        if err_msg:
            return CommonResult(False, err_msg)
        for condition in node.status.conditions:
            if condition.type == "Ready" and condition.status == "True":
                return CommonResult(True)
        return CommonResult(False, f"FAIL: Node {node_name} is not ready")

    def check_node_cordoned(
        self, node_name: str, timeout: int = 360
    ) -> CommonResult[bool, str]:
        """
        Check if a node is cordoned (unschedulable) using kubectl command with timeout.

        Args:
            node_name (str): Name of the node to check
            timeout (int): Maximum time to wait for node to be cordoned in seconds

        Returns:
            CommonResult: Operation result containing:
                - bool: True if node is cordoned, False otherwise
                - str: Error message if an exception occurred

        Example:
            is_cordoned, err_msg = client.check_node_cordoned("node-1")
            if err_msg:
                print(f"Failed to check node cordon status: {err_msg}")
            else:
                print(f"Node is cordoned: {is_cordoned}")
        """
        start_time = time.time()
        while time.time() - start_time < timeout:
            node, err_msg = self.get_node_by_name(node_name)
            if err_msg:
                return CommonResult(False, err_msg)
            # Check if node is marked as unschedulable
            if node.spec.unschedulable:
                LOGGER.info(f"Node {node_name} is cordoned.")
                return CommonResult(True)
            LOGGER.info(f"Node {node_name} is not cordoned, waiting for 5 seconds...")
            time.sleep(5)
        return CommonResult(
            False, f"FAIL: Node {node_name} is not cordoned after waiting {timeout} seconds"
        )

    #############################################
    # Workload Management
    #############################################

    def get_deployments(self, namespace="default", regex=None, **kwargs):
        """
        Args:
            namespace(str)
            regex(None/str): Regex pattern
        Raises:
            kubernetes.client.rest.ApiException
        Returns:
            list[kubernetes.client.models.v1_deployment.V1Deployment]
        """
        deployment_list = self.appsV1Api.list_namespaced_deployment(namespace, **kwargs)
        if regex:
            prog = re.compile(regex)
            return [
                deployment
                for deployment in deployment_list.items
                if prog.match(deployment.metadata.name)
            ]
        return deployment_list.items

    def list_daemonset(
        self, namespace: str = "default", name_pattern: Optional[str] = None, **kwargs
    ) -> CommonResult[List[client.V1DaemonSet], str]:
        """
        List DaemonSets in the specified namespace with optional name pattern filtering.

        Args:
            namespace: The namespace to list DaemonSets from
            name_pattern: Optional regex pattern to filter DaemonSet names
            **kwargs: Additional parameters to pass to the API

        Returns:
            CommonResult containing a list of V1DaemonSet objects or error message
        """
        try:
            daemonsets = self.appsV1Api.list_namespaced_daemon_set(namespace, **kwargs)

            if name_pattern:
                pattern = re.compile(name_pattern)
                filtered_daemonsets = [
                    ds for ds in daemonsets.items if pattern.search(ds.metadata.name)
                ]
                return CommonResult(filtered_daemonsets)

            return CommonResult(daemonsets.items)
        except ApiException as e:
            error_msg = f"Failed to list DaemonSets in namespace {namespace}: {e}"
            logging.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = f"Unexpected error listing DaemonSets in namespace {namespace}: {e}"
            logging.error(error_msg)
            return CommonResult([], error_msg)

    def rollout_daemonset(self, daemonset_name, namespace="default"):
        """
        Simulate command 'kubectl rollout restart daemonset <deployment-name> -n <namespace>'
        Args:
            api_client(kubernetes.client.ApiClient)
            daemonset_name(str) : daemonset_name, such as nvsentinel-gpu-health-monitor
            namespace(str)
        Raises:
            kubernetes.client.rest.ApiException
        """
        LOGGER.info(f"rollout restart daemonset {daemonset_name} -n {namespace}")
        daemonset = self.appsV1Api.read_namespaced_daemon_set(
            name=daemonset_name, namespace=namespace
        )
        if daemonset.spec.template.metadata.annotations is None:
            daemonset.spec.template.metadata.annotations = {}
        # restart_at = datetime.now(timezone.utc).replace(tzinfo=None).isoformat() + "Z"
        restart_at = datetime.utcnow().isoformat() + "Z"
        daemonset.spec.template.metadata.annotations[
            "kubectl.kubernetes.io/restartedAt"
        ] = restart_at
        self.appsV1Api.patch_namespaced_daemon_set(
            name=daemonset_name, namespace=namespace, body=daemonset
        )

    def list_deployment(self, namespace="default", regex=None, **kwargs):
        """
        Args:
            api_client(kubernetes.client.ApiClient)
            namespace(str)
            regex(None/str): Regex pattern
        Raises:
            kubernetes.client.rest.ApiException
        Returns:
            list[kubernetes.client.models.v1_deployment.V1Deployment]
        """
        deployment_list = self.appsV1Api.list_namespaced_deployment(namespace, **kwargs)
        if regex:
            prog = re.compile(regex)
            return [
                deployment
                for deployment in deployment_list.items
                if prog.match(deployment.metadata.name)
            ]
        return deployment_list.items

    def rollout_deployment(self, deployment_name, namespace="default"):
        """
        Simulate command 'kubectl rollout restart deployment/<deployment-name> -n <namespace>'
        Args:
            api_client(kubernetes.client.ApiClient)
            deployment_name(str): deployment name
            namespace(str): namespace name
        Raises:
            kubernetes.client.rest.ApiException
        """
        deployment = self.appsV1Api.read_namespaced_deployment(
            name=deployment_name, namespace=namespace
        )
        deployment.spec.template.metadata.annotations = {
            "kubectl.kubernetes.io/restartedAt": str(
                datetime.datetime.utcnow().isoformat("T") + "Z"
            )
        }
        self.appsV1Api.patch_namespaced_deployment(
            name=deployment_name, namespace=namespace, body=deployment
        )

    def scale_deployment(self, deployment_name, replicas, namespace="default"):
        """
        Simulate command 'kubectl scale --replicas=<num> deployment/<deployment-name> -n <namespace>'
        Args:
            api_client(kubernetes.client.ApiClient)
            deployment_name(str): deployment name
            replicas(int): replicas number of this deployment
            namespace(str): namespace name
        Raises:
            kubernetes.client.rest.ApiException
        """
        body = {"spec": {"replicas": replicas}}
        response = self.appsV1Api.patch_namespaced_deployment(
            name=deployment_name, namespace=namespace, body=body
        )

        LOGGER.debug(f"patch config reponse: {response}")
        time.sleep(10)

        for _ in range(30):
            deployment = self.appsV1Api.read_namespaced_deployment(
                deployment_name, namespace
            )
            available_replicas = deployment.status.available_replicas
            replicas = deployment.status.replicas
            if replicas == 0:
                target_replicas = None
            else:
                target_replicas = replicas
            if available_replicas == target_replicas:
                LOGGER.info(
                    f"scale deployment  {deployment_name} to  {replicas} replicas succssfully"
                )
                return

            time.sleep(2)
        raise Exception(
            f"Failed to scale deployment  {deployment_name} to  {replicas} replicas"
        )

    #############################################
    # Network Management
    #############################################

    def create_network_policy(self, policy_body, namespace):
        """Create a network policy
        Args:
            api_client(kubernetes.client.ApiClient)
            policy_body(dict): policy definition
            namespace(str)
        Raises:
            kubernetes.client.rest.ApiException
        Return:
            V1NetworkPolicy(kubernetes.client.models.V1NetworkPolicy)

        """
        try:
            api_response = self.networkingV1Api.create_namespaced_network_policy(
                namespace=namespace, body=policy_body
            )
            LOGGER.debug(f"create network policy api_response = {api_response}")
        except ApiException as error_message:
            LOGGER.debug(f"Error when create_namespaced_network_policy: {error_message}\n")
            assert "400" in str(error_message), "FAIL: Find return code 400"
            assert "Reason: Bad Request" in str(
                error_message
            ), "FAIL: find Bad Request error found"
        return self.get_network_policy(
            policy_name=policy_body["metadata"]["name"], namespace=namespace
        )

    def delete_network_policy(
        self, policy_name: str, namespace: str, wait: int = 0
    ) -> CommonResult[bool, str]:
        """
        Delete a network policy from a specified namespace.

        Args:
            policy_name (str): Name of the network policy to delete
            namespace (str): Namespace where the network policy is located
            wait (int): Time in seconds to wait for deletion confirmation. No wait if 0. Defaults to 0

        Returns:
            CommonResult: Operation result containing:
                - bool: True if deletion was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            success, err_msg = client.delete_network_policy(
                policy_name="restrict-traffic",
                namespace="default",
                wait=30
            )
            if success:
                print("Network policy deleted successfully")
            else:
                print(f"Failed to delete network policy: {err_msg}")
        """
        try:
            LOGGER.info(
                f"Deleting Network Policy: {policy_name} from namespace: {namespace}"
            )
            self.networkingV1Api.delete_namespaced_network_policy(policy_name, namespace)

            if wait == 0:
                return CommonResult(True)

            # Wait for policy to be deleted
            bailout_time = time.time() + wait
            while time.time() < bailout_time:
                policy_result = self.get_network_policy(
                    policy_name=policy_name, namespace=namespace
                )
                if policy_result.result is None:
                    LOGGER.info(
                        f"Network Policy: {policy_name} has been deleted successfully"
                    )
                    return CommonResult(True)
                time.sleep(3)

            error_msg = f"Timeout waiting for network policy {policy_name} deletion after {wait} seconds"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

        except ApiException as e:
            if e.status == 404:
                LOGGER.warning(
                    f"Network Policy {policy_name} not found in namespace {namespace}, skipping deletion"
                )
                return CommonResult(True)

            error_msg = f"Kubernetes API error when deleting network policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

        except Exception as e:
            error_msg = f"Unexpected error when deleting network policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def get_network_policy(
        self, policy_name: str, namespace: str
    ) -> CommonResult[Optional[client.V1NetworkPolicy], str]:
        """
        Get a network policy from a specified namespace.

        Args:
            policy_name (str): Name of the network policy to retrieve
            namespace (str): Namespace where the network policy is located

        Returns:
            CommonResult: Operation result containing:
                - Optional[V1NetworkPolicy]: The network policy object if found, None if not found
                - str: Error message if an exception occurred, empty string if successful

        Example:
            policy, err_msg = client.get_network_policy(
                policy_name="restrict-traffic",
                namespace="default"
            )
            if err_msg:
                print(f"Failed to get network policy: {err_msg}")
            elif policy:
                print(f"Found network policy: {policy.metadata.name}")
            else:
                print("Network policy not found")
        """
        try:
            network_policy = self.networkingV1Api.read_namespaced_network_policy(
                name=policy_name, namespace=namespace
            )
            LOGGER.debug(
                f"Network policy '{policy_name}' exists in namespace '{namespace}'"
            )
            return CommonResult(network_policy)

        except ApiException as e:
            if e.status == 404:
                LOGGER.warning(
                    f"Network Policy '{policy_name}' not found in namespace '{namespace}'"
                )
                return CommonResult(None)

            error_msg = f"Kubernetes API error when getting network policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

        except Exception as e:
            error_msg = f"Unexpected error when getting network policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def verify_pods_unreachable(
        self,
        server_pod_name: str,
        client_pod_name: str,
        server_node_ip: str,
        server_namespace: str = "default",
        client_namespace: str = "default",
    ) -> CommonResult[bool, str]:
        """
        Verify that pods cannot communicate with each other using netcat.

        Args:
            server_pod_name (str): Name of the pod running the netcat server
            client_pod_name (str): Name of the pod running the netcat client
            server_node_ip (str): IP address of the node running the server pod
            server_namespace (str): Namespace of the server pod. Defaults to "default"
            client_namespace (str): Namespace of the client pod. Defaults to "default"

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if pods are unreachable, False if they can communicate
                - str: Error message if an exception occurred, empty string if successful

        Example:
            unreachable, err_msg = client.verify_pods_unreachable(
                server_pod_name="server-pod",
                client_pod_name="client-pod",
                server_node_ip="10.0.0.1",
                server_namespace="test",
                client_namespace="test"
            )
            if unreachable:
                print("Pods are unreachable as expected")
            else:
                print(f"Pods can communicate or error occurred: {err_msg}")
        """
        server_process = None
        try:
            # Kill any existing nc process
            kill_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                "pkill -9 nc",
            ]
            subprocess.run(kill_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

            # Start nc server
            server_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                "nc -kl 4444",
            ]
            server_process = subprocess.Popen(
                server_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
            )
        except subprocess.CalledProcessError as e:
            error_msg = f"Failed to start netcat server: {str(e.returncode)}"
            LOGGER.error(error_msg)
            if server_process:
                server_process.terminate()
            return CommonResult(False, error_msg)

        try:
            # Test connectivity from client pod
            client_cmd = [
                "kubectl",
                "exec",
                "-it",
                client_pod_name,
                "-n",
                client_namespace,
                "--",
                "sh",
                "-c",
                f"echo hello | nc -N {server_node_ip} 4444",
            ]
            subprocess.run(
                client_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True
            )
            # If we reach here, pods can communicate
            return CommonResult(False, "Pods can communicate")
        except subprocess.CalledProcessError:
            # Expected timeout indicates pods cannot communicate
            LOGGER.info("Expected timeout - pods are unreachable")
            return CommonResult(True, "")
        finally:
            # Clean up processes
            if server_process:
                server_process.terminate()

            kill_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                "pkill -9 nc",
            ]
            subprocess.run(
                kill_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, text=True
            )

    def verify_pods_connected(
        self,
        server_pod_name: str,
        client_pod_name: str,
        server_node_ip: str,
        server_namespace: str = "default",
        client_namespace: str = "default",
        target_port: str = "4444",
    ) -> CommonResult[bool, str]:
        """
        Verify that pods can communicate with each other using netcat.

        Args:
            server_pod_name (str): Name of the server pod
            client_pod_name (str): Name of the client pod
            server_node_ip (str): IP address of the node running the server pod
            server_namespace (str): Namespace of the server pod. Defaults to "default"
            client_namespace (str): Namespace of the client pod. Defaults to "default"
            target_port (str): Port number to use for connection test. Defaults to "4444"

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if pods can communicate successfully, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            connected, err_msg = client.verify_pods_connected(
                server_pod_name="server-pod",
                client_pod_name="client-pod",
                server_node_ip="10.0.0.1",
                server_namespace="test",
                client_namespace="test",
                target_port="8080"
            )
            if connected:
                print("Pods can communicate successfully")
            else:
                print(f"Pod communication failed: {err_msg}")
        """
        server_process = None
        try:
            # Kill any existing nc process
            kill_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                "pkill -9 nc",
            ]
            subprocess.run(kill_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

            # Start nc server
            server_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                f"nc -kl {target_port}",
            ]
            LOGGER.info(server_cmd)
            server_process = subprocess.Popen(
                server_cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True
            )
            time.sleep(5)

            # Send message to nc server
            client_cmd = [
                "kubectl",
                "exec",
                "-it",
                client_pod_name,
                "-n",
                client_namespace,
                "--",
                "sh",
                "-c",
                f"echo hello | nc -N {server_node_ip} {target_port}",
            ]
            LOGGER.info(client_cmd)
            subprocess.run(
                client_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True
            )

            # Get server output
            server_process.terminate()
            server_out, _ = server_process.communicate()

            if "hello" not in server_out.strip():
                return CommonResult(
                    False, "Expected message 'hello' not found in server output"
                )

            return CommonResult(True)

        except subprocess.CalledProcessError as e:
            error_msg = f"Command execution failed with return code: {e.returncode}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error during pod communication test: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        finally:
            # Clean up processes
            if server_process and not server_process.poll():
                server_process.terminate()

            # Ensure nc process is killed
            kill_cmd = [
                "kubectl",
                "exec",
                "-it",
                server_pod_name,
                "-n",
                server_namespace,
                "--",
                "sh",
                "-c",
                "pkill -9 nc",
            ]
            subprocess.run(kill_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    def verify_filtered_port_info(self, nmap_output: str) -> CommonResult[bool, str]:
        """
        Verify if the filtered port information from nmap scan matches expected values.

        Args:
            nmap_output (str): Raw output from nmap port scan containing port, state and service information

        Returns:
            CommonResult: Operation result containing:
                - bool: True if all ports match expected state and service, False otherwise
                - str: Error message if verification failed or an exception occurred

        Example:
            success, err_msg = client.verify_filtered_port_info(nmap_scan_output)
            if success:
                print("All ports match expected configuration")
            else:
                print(f"Port verification failed: {err_msg}")
        """
        try:
            port_lines = re.findall(r"(\d+/tcp\s+\w+\s+\w+)", nmap_output)
            for line in port_lines:
                parts = line.split()
                port = parts[0]
                state = parts[1]
                service = parts[2]

                if port in list(self.expected_port_info.keys()):
                    expected_state = self.expected_port_info[port]["state"]
                    expected_service = self.expected_port_info[port]["service"]

                    if state == expected_state and service == expected_service:
                        LOGGER.info(
                            f"Port: {port}, State: {state}, Service: {service}: As expected"
                        )
                    else:
                        error_msg = f"Port: {port}, State: {state}, Service: {service}: Not as expected"
                        LOGGER.info(error_msg)
                        return CommonResult(False, error_msg)
                else:
                    error_msg = f"Port: {port} - Not found in target port list"
                    LOGGER.info(error_msg)
                    return CommonResult(False, error_msg)

            return CommonResult(True)

        except Exception as e:
            error_msg = f"Unexpected error during port verification: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def exec_port_forward_command(
        self,
        pod_name: str,
        namespace: str = None,
        local_port: int = 8001,
        pod_port: int = 8001,
    ) -> CommonResult[bool, str]:
        """
        Forward a local port to a port on a pod in the cluster.

        Args:
            pod_name (str): Name of the pod to forward ports to
            namespace (str): Namespace where the pod is located. Defaults to self.default_namespace
            local_port (int): Local port to forward from. Defaults to 8001
            pod_port (int): Pod port to forward to. Defaults to 8001

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if port forwarding was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            success, err_msg = client.exec_port_forward_command(
                pod_name="my-pod"
            )
            if success:
                print("Port forwarding established successfully")
            else:
                print(f"Failed to establish port forwarding: {err_msg}")
        """
        try:
            namespace = namespace or self.default_namespace
            cmd_str = (
                f"kubectl port-forward pod/{pod_name} "
                f"-n {namespace} {local_port}:{pod_port}"
            )
            output, _ = self.device.execute(cmd_str)
            LOGGER.info(output)
            time.sleep(10)  # Wait for port forwarding to establish
            return CommonResult(True)

        except Exception as e:
            error_msg = f"Error establishing port forward to pod {pod_name}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    #############################################
    # Configmap Management
    #############################################

    def apply_configmap(self, config_yaml_path: str) -> CommonResult[bool, str]:
        """
        Apply a ConfigMap from a YAML file. If the ConfigMap already exists, it updates it;
        otherwise, it creates a new ConfigMap.

        Args:
            config_yaml_path (str): Path to the YAML file containing the ConfigMap definition

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if ConfigMap was successfully created/updated, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            success, err_msg = client.apply_configmap("path/to/configmap.yaml")
            if success:
                print("ConfigMap applied successfully")
            else:
                print(f"Failed to apply ConfigMap: {err_msg}")
        """
        try:
            # Read and parse the YAML file
            with open(config_yaml_path, "r") as f:
                config_data = yaml.safe_load(f)

            # Extract namespace and name from config
            namespace = config_data["metadata"].get("namespace", "default")
            name = config_data["metadata"]["name"]

            try:
                # Try to update existing ConfigMap
                existing_configmap = self.coreV1Api.read_namespaced_config_map(
                    name, namespace
                )
                existing_configmap.data = config_data["data"]
                self.coreV1Api.replace_namespaced_config_map(
                    name, namespace, existing_configmap
                )
                LOGGER.info(f"ConfigMap '{name}' updated successfully")
                return CommonResult(True)

            except ApiException as e:
                if e.status == 404:
                    # Create new ConfigMap if it doesn't exist
                    self.coreV1Api.create_namespaced_config_map(namespace, config_data)
                    LOGGER.info(f"ConfigMap '{name}' created successfully")
                    return CommonResult(True)

                error_msg = f"Kubernetes API error when applying ConfigMap: {e}"
                LOGGER.error(error_msg)
                return CommonResult(False, error_msg)

        except yaml.YAMLError as e:
            error_msg = f"Error parsing YAML file {config_yaml_path}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when applying ConfigMap: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def get_configmap(
        self, namespace: str, configmap_name: str
    ) -> CommonResult[Optional[client.V1ConfigMap], str]:
        """
        Retrieve a ConfigMap from a specified namespace.

        Args:
            namespace (str): The namespace where the ConfigMap is located
            configmap_name (str): The name of the ConfigMap to retrieve

        Returns:
            CommonResult[Optional[V1ConfigMap], str]: Operation result containing:
                - Optional[V1ConfigMap]: The ConfigMap object if found, None if not found
                - str: Error message if an exception occurred, empty string if successful

        Example:
            configmap, err_msg = client.get_configmap(
                namespace="default",
                configmap_name="my-config"
            )
            if err_msg:
                print(f"Failed to get ConfigMap: {err_msg}")
            else:
                print(f"Found ConfigMap: {configmap.metadata.name}")
        """
        try:
            configmap = self.coreV1Api.read_namespaced_config_map(
                name=configmap_name, namespace=namespace
            )
            return CommonResult(configmap)

        except ApiException as e:
            if e.status == 404:
                error_msg = (
                    f"ConfigMap '{configmap_name}' not found in namespace '{namespace}'"
                )
                LOGGER.warning(error_msg)
                return CommonResult(None, error_msg)

            error_msg = f"Kubernetes API error when getting ConfigMap: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

        except Exception as e:
            error_msg = f"Unexpected error when getting ConfigMap: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def backup_configmap(
        self, namespace: str, configmap_name: str, backup_path: str
    ) -> CommonResult[bool, str]:
        """
        Backup a ConfigMap to a YAML file.

        Args:
            namespace (str): The namespace where the ConfigMap is located
            configmap_name (str): The name of the ConfigMap to backup
            backup_path (str): The path where the YAML backup file will be saved

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if backup was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            backup_success, err_msg = client.backup_configmap(
                namespace="default",
                configmap_name="my-config",
                backup_path="/path/to/backup.yaml"
            )
            if backup_success:
                print("ConfigMap backed up successfully")
            else:
                print(f"Failed to backup ConfigMap: {err_msg}")
        """
        try:
            # Read the ConfigMap
            configmap = self.coreV1Api.read_namespaced_config_map(
                name=configmap_name, namespace=namespace
            )

            # Convert to dictionary
            configmap_dict = configmap.to_dict()

            # Write to YAML file
            with open(backup_path, "w") as yaml_file:
                yaml.dump(configmap_dict, yaml_file, default_flow_style=False)

            LOGGER.info(f"ConfigMap '{configmap_name}' has been backed up to {backup_path}")
            return CommonResult(True)

        except ApiException as e:
            error_msg = (
                f"Kubernetes API error when backing up ConfigMap '{configmap_name}': {e}"
            )
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except IOError as e:
            error_msg = f"IO error when writing backup file '{backup_path}': {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = (
                f"Unexpected error when backing up ConfigMap '{configmap_name}': {e}"
            )
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def delete_configmap(
        self, configmap_name: str, namespace: str = "default"
    ) -> CommonResult[bool, str]:
        """
        Delete a ConfigMap by name and namespace, and verify its deletion.

        Args:
            configmap_name (str): The name of the ConfigMap to delete
            namespace (str): The namespace where the ConfigMap is located. Defaults to "default"

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if deletion was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            delete_success, err_msg = client.delete_configmap(
                configmap_name="my-config",
                namespace="default"
            )
            if delete_success:
                print("ConfigMap deleted successfully")
            else:
                print(f"Failed to delete ConfigMap: {err_msg}")
        """
        try:
            # Attempt to delete the ConfigMap
            self.coreV1Api.delete_namespaced_config_map(configmap_name, namespace)
            LOGGER.info(f"ConfigMap '{configmap_name}' deletion initiated.")

            # Verify deletion
            try:
                self.coreV1Api.read_namespaced_config_map(configmap_name, namespace)
                error_msg = (
                    f"ConfigMap '{configmap_name}' still exists after deletion attempt"
                )
                LOGGER.error(error_msg)
                return CommonResult(False, error_msg)
            except ApiException as e:
                if e.status == 404:
                    LOGGER.info(f"ConfigMap '{configmap_name}' successfully deleted.")
                    return CommonResult(True)
                error_msg = f"Error verifying deletion of ConfigMap '{configmap_name}': {e}"
                LOGGER.error(error_msg)
                return CommonResult(False, error_msg)

        except ApiException as e:
            if e.status == 404:
                LOGGER.info(
                    f"ConfigMap '{configmap_name}' not found in namespace '{namespace}'."
                )
                return CommonResult(True)

            error_msg = f"Error deleting ConfigMap '{configmap_name}': {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error deleting ConfigMap '{configmap_name}': {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    #############################################
    # Secret Management
    #############################################

    def list_secrets(self, namespace: str = "default") -> CommonResult[List[str], str]:
        """
        List all secrets in the specified namespace.

        Args:
            namespace (str): The namespace to list secrets from. Defaults to "default"

        Returns:
            CommonResult: Operation result containing:
                - List[str]: List of secret names in the namespace
                - str: Error message if an exception occurred, empty string if successful

        Example:
            secret_names, err_msg = client.list_secrets("my-namespace")
            if err_msg:
                print(f"Failed to list secrets: {err_msg}")
            else:
                print(f"Found secrets: {secret_names}")
        """
        try:
            secrets = self.coreV1Api.list_namespaced_secret(namespace)
            secret_names = [secret.metadata.name for secret in secrets.items]
            LOGGER.info("Found secrets in namespace %s: %s", namespace, secret_names)
            return CommonResult(secret_names)

        except ApiException as e:
            error_msg = f"Kubernetes API error when listing secrets: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when listing secrets: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    def create_secret(
        self, secret_name: str, secret_data: Dict[str, bytes], namespace: str = "default"
    ) -> CommonResult[client.V1Secret, str]:
        """
        Create a Kubernetes secret in the specified namespace.
        If the secret already exists, returns the existing secret.

        Args:
            secret_name (str): Name of the secret to create
            secret_data (Dict[str, bytes]): Dictionary containing the secret data
                Keys are strings, values must be base64 encoded bytes
            namespace (str): Namespace where the secret will be created. Defaults to "default"

        Returns:
            CommonResult[V1Secret, str]: Operation result containing:
                - V1Secret: The created or existing secret object
                - str: Error message if an exception occurred, empty string if successful

        Example:
            # Create a secret with some base64 encoded data
            secret_data = {
                "username": base64.b64encode(b"admin"),
                "password": base64.b64encode(b"secret123")
            }
            secret, err_msg = client.create_secret("my-secret", secret_data, "default")
            if secret:
                print(f"Secret created: {secret.metadata.name}")
            else:
                print(f"Failed to create secret: {err_msg}")
        """
        try:
            # Check if the secret already exists
            existing_secret = self.coreV1Api.read_namespaced_secret(
                name=secret_name, namespace=namespace
            )
            LOGGER.info(
                "Secret '%s' already exists in namespace '%s'", secret_name, namespace
            )
            return CommonResult(existing_secret)

        except ApiException as e:
            if e.status != 404:
                error_msg = f"Error checking for existing secret: {e}"
                LOGGER.error(error_msg)
                return CommonResult(None, error_msg)

            try:
                # Create new secret
                metadata = client.V1ObjectMeta(name=secret_name, namespace=namespace)
                secret = client.V1Secret(metadata=metadata, data=secret_data)
                created_secret = self.coreV1Api.create_namespaced_secret(
                    namespace=namespace, body=secret
                )
                LOGGER.info(
                    "Successfully created secret '%s' in namespace '%s'",
                    secret_name,
                    namespace,
                )
                return CommonResult(created_secret)

            except ApiException as e:
                error_msg = f"Failed to create secret: {e}"
                LOGGER.error(error_msg)
                return CommonResult(None, error_msg)

        except Exception as e:
            error_msg = f"Unexpected error creating secret: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def get_secret_by_name(
        self, name: str, namespace: str = "default"
    ) -> CommonResult[client.V1Secret, str]:
        """
        Get secret in namespace by secret name.

        Args:
            name (str): Name of the secret to retrieve
            namespace (str): Namespace where the secret exists. Defaults to "default"

        Returns:
            CommonResult[V1Secret, str]: Operation result containing:
                - V1Secret: The secret object if found
                - str: Error message if an exception occurred, empty string if successful


        Example:
            secret, err_msg = client.get_secret_by_name("my-secret", "default")
            if secret:
                print(f"Secret data: {secret.data}")
            else:
                print(f"Failed to get secret: {err_msg}")
        """
        try:
            secret_info = self.coreV1Api.read_namespaced_secret(name, namespace)
            LOGGER.info(
                "Retrieved secret '%s' from namespace '%s'",
                name,
                namespace,
            )
            return CommonResult(secret_info)
        except ApiException as e:
            error_msg = f"Exception when getting secret {name}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when getting secret {name}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def delete_secret_by_name(
        self, name: str, namespace: str = "default", wait: int = 0
    ) -> CommonResult[bool, str]:
        """
        Delete secret in namespace by secret name.

        Args:
            name (str): Name of the secret to delete
            namespace (str): Namespace where the secret exists. Defaults to "default"
            wait (int): Time in seconds to wait for deletion confirmation. No wait if 0. Defaults to 0

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if deletion was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful


        Example:
            deleted, err_msg = client.delete_secret_by_name("my-secret", "default")
            if deleted:
                print("Secret deleted successfully")
            else:
                print(f"Failed to delete secret: {err_msg}")
        """
        try:
            LOGGER.info("Delete secret %s in namespace %s", name, namespace)
            self.coreV1Api.delete_namespaced_secret(name, namespace)

            if wait == 0:
                return CommonResult(True)

            bailout_time = time.time() + wait
            while time.time() < bailout_time:
                try:
                    self.coreV1Api.read_namespaced_secret(name, namespace)
                    time.sleep(3)
                except ApiException as e:
                    if e.status == 404:
                        LOGGER.info("Secret %s has been deleted", name)
                        return CommonResult(True)
                    error_msg = f"API error while waiting for secret deletion: {e}"
                    return CommonResult(False, error_msg)

            error_msg = f"Timeout waiting for secret {name} deletion after {wait} seconds"
            return CommonResult(False, error_msg)

        except ApiException as e:
            error_msg = f"Exception when deleting secret {name}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when deleting secret {name}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def list_service_accounts(
        self, name_pattern: Optional[str] = None, **kwargs
    ) -> CommonResult[List[client.V1ServiceAccount], str]:
        """
        List all service accounts across all namespaces, optionally filtered by a name pattern.

        Args:
            name_pattern (Optional[str]): A regex pattern to filter service account names. Defaults to None
            **kwargs: Additional arguments to pass to the list_service_account_for_all_namespaces method

        Returns:
            CommonResult[List[V1ServiceAccount], str]: Operation result containing:
                - List[V1ServiceAccount]: List of service accounts matching the criteria
                - str: Error message if an exception occurred, empty string if successful

        Example:
            service_accounts, err_msg = client.list_service_accounts(name_pattern="^system-.*$")
            if service_accounts:
                print(f"Found {len(service_accounts)} matching service accounts")
            else:
                print(f"Failed to list service accounts: {err_msg}")
        """
        try:
            service_account_list = self.coreV1Api.list_service_account_for_all_namespaces(
                **kwargs
            )

            if name_pattern:
                pattern = re.compile(name_pattern)
                filtered_accounts = [
                    account
                    for account in service_account_list.items
                    if pattern.match(account.metadata.name)
                ]
                return CommonResult(filtered_accounts)

            return CommonResult(service_account_list.items)

        except ApiException as e:
            error_msg = f"Kubernetes API error when listing service accounts: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when listing service accounts: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    #############################################
    # Service Management
    #############################################

    def get_service_yaml(
        self, namespace: str, service_name: str
    ) -> CommonResult[Optional[Dict[str, Any]], str]:
        """
        Get the YAML representation of a Kubernetes service.

        Args:
            namespace (str): The namespace where the service is located
            service_name (str): The name of the service to retrieve

        Returns:
            CommonResult[Optional[Dict[str, Any]], str]: Operation result containing:
                - Optional[Dict[str, Any]]: The service configuration as a dictionary if found, None if not found
                - str: Error message if an exception occurred, empty string if successful

        Example:
            service_yaml, err_msg = client.get_service_yaml(
                namespace="default",
                service_name="my-service"
            )
            if service_yaml:
                print(f"Service YAML: {service_yaml}")
            else:
                print(f"Failed to get service YAML: {err_msg}")
        """
        try:
            service = self.coreV1Api.read_namespaced_service(
                name=service_name, namespace=namespace
            )
            # Convert the service object to dictionary format
            service_dict = client.ApiClient().sanitize_for_serialization(service)
            return CommonResult(service_dict)

        except client.exceptions.ApiException as e:
            error_msg = f"Kubernetes API error when getting service YAML: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when getting service YAML: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def wait_knative_service_ready(
        self, service_name: str, namespace: str = "default", timeout_seconds: int = 300
    ) -> CommonResult[bool, str]:
        """
        Wait for a Knative service to become ready.

        Args:
            service_name (str): Name of the Knative service to wait for
            namespace (str): Namespace where the service is deployed. Defaults to "default"
            timeout_seconds (int): Maximum time to wait in seconds. Defaults to 300

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if service became ready, False otherwise
                - str: Error message if service failed to become ready or if error occurred

        Example:
            ready, err_msg = client.wait_knative_service_ready(
                service_name="my-service",
                namespace="default",
                timeout_seconds=300
            )
            if ready:
                print("Service is ready")
            else:
                print(f"Service failed to become ready: {err_msg}")
        """
        group = "serving.knative.dev"
        version = "v1"
        plural = "services"

        service_found = False
        start_time = time.time()

        while time.time() - start_time < timeout_seconds:
            try:
                service_list = self.customObjectApi.list_namespaced_custom_object(
                    group, version, namespace, plural
                )

                for service in service_list["items"]:
                    if service["metadata"]["name"] != service_name:
                        continue

                    service_found = True
                    for condition in service["status"]["conditions"]:
                        if condition["type"] != "Ready":
                            continue

                        if condition["status"] == "True":
                            return CommonResult(True, "")

            except ApiException as api_err:
                error_msg = f"Kubernetes API error: {api_err.reason}"
                LOGGER.error(error_msg)
                if api_err.status == 401:
                    # Bearer token is expired
                    self._initialize_client()
            except Exception as e:
                error_msg = f"Unexpected error checking service status: {e}"
                LOGGER.warning(error_msg)

            time.sleep(5)

        if service_found:
            error_msg = f"Service '{service_name}' did not become ready within {timeout_seconds} seconds"
        else:
            error_msg = f"Service '{service_name}' not found in namespace '{namespace}' within {timeout_seconds} seconds"

        LOGGER.error(error_msg)
        return CommonResult(False, error_msg)

    def get_knative_service(
        self, service_name: str, namespace: str = "default"
    ) -> CommonResult[Optional[Dict[str, Any]], str]:
        """
        Get a Knative service in the specified namespace.

        Args:
            service_name (str): The name of the Knative service to find
            namespace (str): The namespace where the service is located. Defaults to "default"

        Returns:
            CommonResult[Optional[Dict[str, Any]], str]: Operation result containing:
                - Optional[Dict[str, Any]]: The Knative service object if found, None otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            service, err_msg = client.get_knative_service(
                service_name="my-service",
                namespace="default"
            )
            if err_msg:
                print(f"Failed to get service: {err_msg}")
            else:
                print(f"Found service: {service['metadata']['name']}")
        """
        group = "serving.knative.dev"
        version = "v1"
        plural = "services"

        try:
            ksvc_list = self.customObjectApi.list_namespaced_custom_object(
                group, version, namespace, plural
            )

            for ksvc in ksvc_list["items"]:
                if ksvc["metadata"]["name"] == service_name:
                    return CommonResult(ksvc, "")

            error_msg = (
                f"Knative service '{service_name}' not found in namespace '{namespace}'"
            )
            LOGGER.warning(error_msg)
            return CommonResult(None, error_msg)

        except client.exceptions.ApiException as e:
            error_msg = f"Kubernetes API error when getting Knative service: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when getting Knative service: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def get_ksvc_url(
        self, service_name_prefix: str, namespace: str
    ) -> CommonResult[Optional[str], str]:
        """
        Get the external URL of a Knative service.

        Args:
            service_name_prefix (str): Name prefix of the Knative service to find
            namespace (str): Namespace where the service is deployed

        Returns:
            CommonResult[Optional[str], str]: Operation result containing:
                - Optional[str]: External service URL if found and ready, None otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            url, err_msg = client.get_ksvc_url(
                service_name_prefix="my-service",
                namespace="default"
            )
            if url:
                print(f"Service URL: {url}")
            else:
                print(f"Failed to get service URL: {err_msg}")
        """
        try:
            # Get all Knative services in namespace
            ksvc_list = self.customObjectApi.list_namespaced_custom_object(
                group="serving.knative.dev",
                version="v1",
                namespace=namespace,
                plural="services",
            )

            LOGGER.debug(f"Retrieved Knative services list: {ksvc_list}")

            # Find matching service by name prefix
            service = None
            for item in ksvc_list.get("items", []):
                service_name = item.get("metadata", {}).get("name", "")
                if service_name.startswith(service_name_prefix):
                    service = item
                    break

            if not service:
                error_msg = f"No Knative service found with prefix {service_name_prefix} in namespace {namespace}"
                LOGGER.warning(error_msg)
                return CommonResult(None, error_msg)

            status = service.get("status", {})

            # Check if service is ready
            conditions = status.get("conditions", [])
            is_ready = any(
                cond.get("type") == "Ready" and cond.get("status") == "True"
                for cond in conditions
            )

            if not is_ready:
                error_condition = next(
                    (cond for cond in conditions if cond.get("type") == "Ready"), {}
                )
                error_msg = error_condition.get("message", "No error message available")
                LOGGER.warning(f"Service not ready: {error_msg}")
                return CommonResult(None, f"Service not ready: {error_msg}")

            # Get external URL
            url = status.get("url")
            if not url:
                error_msg = "Service URL not found in status"
                LOGGER.warning(error_msg)
                return CommonResult(None, error_msg)

            return CommonResult(url)

        except Exception as e:
            error_msg = f"Error getting Knative service URL: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def list_services(
        self, namespace: str = "default", name_pattern: Optional[str] = None, **kwargs
    ) -> CommonResult[List[client.V1Service], str]:
        """
        List services in a namespace, optionally filtered by a regex pattern.

        Args:
            namespace (str): The namespace to list services from. Defaults to "default"
            name_pattern (Optional[str]): A regex pattern to filter service names. Defaults to None
            **kwargs: Additional arguments to pass to the list_namespaced_service method

        Returns:
            CommonResult[List[V1Service], str]: Operation result containing:
                - result (List[V1Service]): List of services matching the criteria
                - error_msg (str): Error message if an exception occurred, empty string if successful

        Example:
            services, err_msg = client.list_services(
                namespace="my-namespace",
                name_pattern="^web-.*$"
            )
            if err_msg:
                print(f"Failed to list services: {err_msg}")
            else:
                print(f"Found {len(services)} matching services")
        """
        try:
            service_list = self.coreV1Api.list_namespaced_service(namespace, **kwargs)

            if name_pattern:
                prog = re.compile(name_pattern)
                filtered_services = [
                    service
                    for service in service_list.items
                    if prog.match(service.metadata.name)
                ]
                return CommonResult(filtered_services)

            return CommonResult(service_list.items)

        except ApiException as e:
            error_msg = f"Kubernetes API error when listing services: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when listing services: {e}"
            LOGGER.error(error_msg)
            return CommonResult([], error_msg)

    #############################################
    # Custom Resource Management
    #############################################

    def patch_custom_resource(
        self, resource_type: str, resource_name: str, namespace: str, json_patch: str
    ) -> CommonResult[bool, str]:
        """
        Patch a custom resource using kubectl patch command.

        Args:
            resource_type (str): The type of custom resource to patch (e.g., 'deployment', 'configmap')
            resource_name (str): The name of the custom resource instance to patch
            namespace (str): The namespace where the custom resource is located
            json_patch (str): The JSON patch string to apply to the resource

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if patch was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            json_patch = '[{"op": "replace", "path": "/spec/replicas", "value": 3}]'
            patch_success, err_msg = client.patch_custom_resource(
                resource_type="deployment",
                resource_name="my-deployment",
                namespace="default",
                json_patch=json_patch
            )
            if patch_success:
                print("Resource patched successfully")
            else:
                print(f"Failed to patch resource: {error_msg}")
        """
        try:
            k8s_cmd = f'kubectl patch {resource_type} {resource_name} -n {namespace} --type json -p "{json_patch}"'
            LOGGER.info(f"Executing kubectl command: {k8s_cmd}")

            process = subprocess.run(
                shlex.split(k8s_cmd), capture_output=True, text=True, check=True
            )

            LOGGER.debug(f"Patch command output: {process.stdout}")
            return CommonResult(True)

        except subprocess.CalledProcessError as e:
            error_msg = f"Failed to patch resource: stdout={e.stdout}, stderr={e.stderr}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error while patching resource: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def verify_target_exist_in_resource(
        self,
        group: str,
        version: str,
        namespace: str,
        resource_plural: str,
        resource_name: str,
        target_str: str,
    ) -> CommonResult[bool, str]:
        """
        Verify if a target string exists in a custom resource.

        Args:
            group (str): The API group of the custom resource
            version (str): The API version of the custom resource
            namespace (str): The namespace of the custom resource
            resource_plural (str): The plural name of the custom resource as defined in the CRD spec
            resource_name (str): The name of the specific custom resource instance
            target_str (str): The target string to verify in the resource

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if the target string exists, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            target_exists, err_msg = client.verify_target_exist_in_resource(
                group="mygroup.example.com",
                version="v1",
                namespace="default",
                resource_plural="mycustomresources",
                resource_name="my-instance",
                target_str="target-value"
            )
            if target_exists:
                print("Target string found in resource")
            else:
                print(f"Target string not found: {error_msg}")
        """
        try:
            resource = self.custom_object_api.get_namespaced_custom_object(
                group=group,
                version=version,
                namespace=namespace,
                plural=resource_plural,
                name=resource_name,
            )

            if target_str not in str(resource):
                LOGGER.info(f"Target string [{target_str}] not found in resource")
                return CommonResult(False, "Target string not found in resource")

            LOGGER.info(f"Target string [{target_str}] found in resource")
            return CommonResult(True)

        except client.exceptions.ApiException as e:
            if e.status == 404:
                error_msg = f"Custom resource '{resource_name}' not found"
            else:
                error_msg = f"Error occurred while finding target string: {e}"

            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error while verifying target string: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def delete_cluster_policy(self, policy_name: str) -> CommonResult[bool, str]:
        """
        Delete a cluster policy.

        Args:
            policy_name (str): Name of the cluster policy to delete

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if deletion was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            delete_success, err_msg = client.delete_cluster_policy("my-cluster-policy")
            if delete_success:
                print("Cluster policy deleted successfully")
            else:
                print(f"Failed to delete cluster policy: {error_msg}")
        """
        group = "kyverno.io"  # API group for ClusterPolicy
        version = "v1"  # API version
        plural = "clusterpolicies"  # Plural name for ClusterPolicy

        try:
            api_response = self.customObjectApi.delete_cluster_custom_object(
                group=group,
                version=version,
                plural=plural,
                name=policy_name,
                body=client.V1DeleteOptions(),
            )
            LOGGER.debug(f"Cluster policy '{policy_name}' deleted successfully")
            LOGGER.debug(f"Delete response: {api_response}")
            return CommonResult(True)

        except ApiException as e:
            error_msg = f"Exception when deleting cluster policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when deleting cluster policy: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def create_update_crd_from_yaml(
        self, crd_yaml_path: str, resource_plural: str
    ) -> CommonResult[bool, str]:
        """
        Create or update a Custom Resource Definition (CRD) object using a YAML file.

        Args:
            crd_yaml_path (str): The path to the YAML file containing the CRD definition
            resource_plural (str): The plural name of the custom resource as defined in the CRD spec

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if the CRD was successfully created or updated, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            success, err_msg = client.create_update_crd_from_yaml(
                "path/to/my_crd.yaml",
                "customresources"
            )
            if success:
                print("CRD created/updated successfully")
            else:
                print(f"Failed to create/update CRD: {err_msg}")
        """
        try:
            # Load and parse the CRD YAML file
            with open(crd_yaml_path, "r") as f:
                crd_spec = yaml.safe_load(f)

            # Extract the necessary details from the YAML file
            group, version = crd_spec["apiVersion"].strip().split("/")
            resource_name = crd_spec["metadata"]["name"]
            namespace = crd_spec["metadata"].get("namespace", "default")

            # Check if the CRD already exists
            response, _ = self.get_crd(
                api_group=group,
                api_version=version,
                namespace=namespace,
                resource_plural=resource_plural,
                resource_name=resource_name,
            )

            # If it exists then update it, otherwise create it
            if response:
                self.customObjectApi.replace_namespaced_custom_object(
                    group=group,
                    version=version,
                    namespace=namespace,
                    plural=resource_plural,
                    name=resource_name,
                    body=crd_spec,
                )
                LOGGER.debug(f"Updated CRD: {resource_name}")
            else:
                self.customObjectApi.create_namespaced_custom_object(
                    group=group,
                    version=version,
                    namespace=namespace,
                    plural=resource_plural,
                    body=crd_spec,
                )
                LOGGER.debug(f"Created CRD: {resource_name}")

            return CommonResult(True)
        except ApiException as e:
            error_msg = f"Exception when creating/updating CRD: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except yaml.YAMLError as e:
            error_msg = f"Error parsing YAML file {crd_yaml_path}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when creating/updating CRD: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def get_crd(
        self,
        api_group: str,
        api_version: str,
        namespace: str,
        resource_plural: str,
        resource_name: str,
    ) -> CommonResult[Optional[Dict[str, Any]], str]:
        """
        Retrieves a Custom Resource Definition (CRD) object from a specified namespace.

        Args:
            api_group (str): The API group of the custom resource (e.g., 'apps', 'batch')
            api_version (str): The API version of the custom resource (e.g., 'v1', 'v1beta1')
            namespace (str): The namespace from which to retrieve the custom resource
            resource_plural (str): The plural name of the custom resource as defined in the CRD spec
            resource_name (str): The name of the specific custom resource instance

        Returns:
            CommonResult[Optional[Dict[str, Any], str]]: Operation result containing:
                - Optional[Dict[str, Any]]: The custom resource object if found, None if not found
                - str: Error message if an exception occurred, empty string if successful

        Example:
            crd, err_msg = client.get_crd(
                api_group="mygroup.example.com",
                api_version="v1",
                namespace="default",
                resource_plural="mycustomresources",
                resource_name="my-instance"
            )
            if err_msg:
                print(f"Failed to get CRD: {err_msg}")
            else:
                print(f"Found CRD: {crd}")
        """
        try:
            response = self.customObjectApi.get_namespaced_custom_object(
                group=api_group,
                version=api_version,
                namespace=namespace,
                plural=resource_plural,
                name=resource_name,
            )
            return CommonResult(response)
        except ApiException as e:
            if e.status == 404:
                return CommonResult(None, "Resource not found")

            error_msg = f"Error retrieving custom resource: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

        except Exception as e:
            error_msg = f"Unexpected error retrieving custom resource: {e}"
            LOGGER.error(error_msg)
            return CommonResult(None, error_msg)

    def delete_crd_from_yaml(
        self, crd_yaml_path: str, resource_plural: str
    ) -> CommonResult[bool, str]:
        """
        Deletes a Custom Resource Definition (CRD) object using its YAML definition file.

        Args:
            crd_yaml_path (str): Path to the YAML file containing the CRD definition
            resource_plural (str): Plural name of the custom resource as defined in the CRD spec

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if deletion was successful, False otherwise
                - str: Error message if an exception occurred, empty string if successful

        Example:
            delete_success, err_msg = client.delete_crd_from_yaml("path/to/my_crd.yaml", "customresources")
            if delete_success:
                print("CRD deleted successfully")
            else:
                print(f"Failed to delete CRD: {err_msg}")
        """
        try:
            # Load and parse the CRD YAML file
            with open(crd_yaml_path, "r") as f:
                crd_spec = yaml.safe_load(f)

            # Extract CRD metadata
            group, version = crd_spec["apiVersion"].strip().split("/")
            resource_name = crd_spec["metadata"]["name"]
            namespace = crd_spec["metadata"].get("namespace", "default")

            # Delete the CRD object
            response = self.customObjectApi.delete_namespaced_custom_object(
                group=group,
                version=version,
                namespace=namespace,
                plural=resource_plural,
                name=resource_name,
            )

            LOGGER.debug(f"CRD object {resource_name} has been deleted successfully")
            LOGGER.debug(f"Delete response: {response}")
            return CommonResult(True, "")

        except ApiException as e:
            error_msg = f"Kubernetes API error when deleting CRD object: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except yaml.YAMLError as e:
            error_msg = f"Error parsing YAML file {crd_yaml_path}: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error when deleting CRD object: {e}"
            LOGGER.error(error_msg)
            return CommonResult(False, error_msg)

    def enable_argocd_auto_sync(self, app_name: str) -> CommonResult[Dict[str, Any], str]:
        """
        Enable auto-sync for an ArgoCD application.

        Args:
            app_name (str): Name of the ArgoCD application to enable auto-sync for

        Returns:
            CommonResult[Dict[str, Any], str]: Operation result containing:
                - Dict[str, Any]: Updated application configuration if successful
                - str: Error message if an exception occurred

        Example:
            result = client.enable_argocd_auto_sync("my-application")
            if result.values[0]:
                print("Auto-sync enabled successfully")
            else:
                print(f"Failed to enable auto-sync: {result.error}")
        """
        # First get the application to confirm it exists and check API version
        app_info = self._get_argocd_app_info(app_name)
        if not app_info.values[0]:  # Check values[0] instead of data
            return app_info  # Return the error from get operation

        try:
            # Get API version from the app info
            api_version = (
                app_info.values[0].get("apiVersion", "").split("/")[1]
            )  # Use values[0]
            if not api_version:
                return CommonResult(None, f"Could not determine API version for {app_name}")

            # Use merge patch format to enable automated sync while preserving other settings
            patch = {
                "spec": {
                    "syncPolicy": {
                        "automated": {},  # Empty dict to enable auto-sync
                        "syncOptions": app_info.values[0]
                        .get("spec", {})
                        .get("syncPolicy", {})
                        .get("syncOptions", []),
                    }
                }
            }

            # Make the PATCH request using the customObjectApi directly
            result = self.customObjectApi.patch_namespaced_custom_object(
                group="argoproj.io",
                version=api_version,
                namespace="argocd",
                plural="applications",
                name=app_name,
                body=patch,
            )

            return CommonResult(result)
        except ApiException as e:
            error_msg = f"Failed to enable auto sync for {app_name}: {str(e)}"
            return CommonResult(None, error_msg)

    def _get_argocd_app_info(self, app_name: str) -> CommonResult[Dict[str, Any], str]:
        """Get ArgoCD application info.

        Args:
            app_name: Name of the ArgoCD application

        Returns:
            CommonResult with the application info or error message
        """
        try:
            result = self.customObjectApi.get_namespaced_custom_object(
                group="argoproj.io",
                version="v1alpha1",  # We'll try v1alpha1 first
                namespace="argocd",
                plural="applications",
                name=app_name,
            )
            return CommonResult(result)
        except ApiException as e:
            if e.status == 404:
                # Try v1 if v1alpha1 not found
                try:
                    result = self.customObjectApi.get_namespaced_custom_object(
                        group="argoproj.io",
                        version="v1",
                        namespace="argocd",
                        plural="applications",
                        name=app_name,
                    )
                    return CommonResult(result)
                except ApiException as e2:
                    error_msg = f"Failed to get application {app_name}: {str(e2)}"
                    return CommonResult(None, error_msg)
            error_msg = f"Failed to get application {app_name}: {str(e)}"
            return CommonResult(None, error_msg)

    def disable_argocd_auto_sync(self, app_name: str) -> CommonResult[Dict[str, Any], str]:
        """
        Disable auto-sync for an ArgoCD application.

        Args:
            app_name (str): Name of the ArgoCD application to disable auto-sync for

        Returns:
            CommonResult[Dict[str, Any], str]: Operation result containing:
                - Dict[str, Any]: Updated application configuration if successful
                - str: Error message if an exception occurred

        Example:
            result = client.disable_argocd_auto_sync("my-application")
            if result.values[0]:
                print("Auto-sync disabled successfully")
            else:
                print(f"Failed to disable auto-sync: {result.error}")
        """
        # First get the application to confirm it exists and check API version
        app_info = self._get_argocd_app_info(app_name)
        if not app_info.values[0]:  # Check values[0] instead of data
            return app_info  # Return the error from get operation

        try:
            # Log current application config
            LOGGER.debug("Current application config:")
            LOGGER.debug(app_info.values[0].get("spec", {}).get("syncPolicy", {}))

            # Get API version from the app info
            api_version = (
                app_info.values[0].get("apiVersion", "").split("/")[1]
            )  # Use values[0]
            if not api_version:
                return CommonResult(None, f"Could not determine API version for {app_name}")

            # Use merge patch format to preserve syncOptions while removing automated
            patch = {
                "spec": {
                    "syncPolicy": {
                        "automated": None,  # Set to null to remove it
                        "syncOptions": app_info.values[0]
                        .get("spec", {})
                        .get("syncPolicy", {})
                        .get("syncOptions", []),
                    }
                }
            }

            LOGGER.debug("Applying patch:")
            LOGGER.debug(patch)

            # Make the PATCH request using the customObjectApi directly
            result = self.customObjectApi.patch_namespaced_custom_object(
                group="argoproj.io",
                version=api_version,
                namespace="argocd",
                plural="applications",
                name=app_name,
                body=patch,
            )

            # Log patch result
            LOGGER.debug("Patch result:")
            LOGGER.debug(result.get("spec", {}).get("syncPolicy", {}))

            return CommonResult(result)
        except ApiException as e:
            error_msg = f"Failed to disable auto sync for {app_name}: {str(e)}"
            LOGGER.error(f"Error details: {str(e)}")
            return CommonResult(None, error_msg)

    def sync_argocd_application(
        self, app_name: str, create_namespace: bool = True, prune_last: bool = True
    ) -> CommonResult[Dict[str, Any], str]:
        """
        Manually trigger a sync operation for an ArgoCD application.

        Args:
            app_name (str): Name of the ArgoCD application to sync
            create_namespace (bool, optional): Whether to create namespace if it doesn't exist. Defaults to True.
            prune_last (bool, optional): Whether to prune resources last during sync. Defaults to True.

        Returns:
            CommonResult[Dict[str, Any], str]: Operation result containing:
                - Dict[str, Any]: Sync operation result if successful
                - str: Error message if an exception occurred

        Example:
            # Basic sync with default options
            result = client.sync_argocd_application("my-application")
            if result.values[0]:
                print("Application sync initiated successfully")
            else:
                print(f"Failed to sync application: {result.error}")

            # Sync with custom options
            result = client.sync_argocd_application(
                "my-application",
                create_namespace=False,
                prune_last=False
            )
        """
        # First get the application to confirm it exists and check API version
        app_info = self._get_argocd_app_info(app_name)
        if not app_info.values[0]:
            return app_info  # Return the error from get operation

        try:
            # Get API version from the app info
            api_version = app_info.values[0].get("apiVersion", "").split("/")[1]
            if not api_version:
                return CommonResult(None, f"Could not determine API version for {app_name}")

            # Prepare sync options
            sync_options = []
            if create_namespace:
                sync_options.append("CreateNamespace=true")
            if prune_last:
                sync_options.append("PruneLast=true")

            # Prepare the patch for sync operation
            patch = {"operation": {"sync": {"syncOptions": sync_options}}}

            # Make the PATCH request using the customObjectApi
            result = self.customObjectApi.patch_namespaced_custom_object(
                group="argoproj.io",
                version=api_version,
                namespace="argocd",
                plural="applications",
                name=app_name,
                body=patch,
            )

            return CommonResult(result)
        except ApiException as e:
            error_msg = f"Failed to sync application {app_name}: {str(e)}"
            return CommonResult(None, error_msg)


    def create_job_from_yaml(self, yaml_path: str, namespace: str = "default") -> CommonResult[Dict[str, Any], str]:
        """
        Create a job from a YAML file.

        Args:
            yaml_path (str): Path to the YAML file containing the job definition
        """
        try:
            with open(yaml_path, "r") as f:
                job_yaml = yaml.safe_load(f)

            # Delete the job if it exists
            self.delete_job_if_exists(job_yaml["metadata"]["name"], namespace)

            # Create the job
            result = self.batchV1Api.create_namespaced_job(namespace=namespace, body=job_yaml)
            return CommonResult(result, "")
        except ApiException as e:
            error_msg = f"Failed to create job from {yaml_path}: {str(e)}"
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error creating job from {yaml_path}: {str(e)}"
            return CommonResult(None, error_msg)

    def delete_job_if_exists(self, job_name: str, namespace: str) -> CommonResult[bool, str]:
        """
        Delete a job if it exists in a specific namespace.
        """
        try:
            # Check if the job exists
            result = self.batchV1Api.read_namespaced_job(name=job_name, namespace=namespace)
            if result:
                _ = self.batchV1Api.delete_namespaced_job(name=job_name, namespace=namespace)
                time.sleep(10)
                return CommonResult(True, "")
            else:
                return CommonResult(False, "Job does not exist")
        except ApiException as e:
            error_msg = f"Failed to delete job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error deleting job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
    
    def verify_job_is_running(self, job_name: str, namespace: str) -> CommonResult[bool, str]:
        """
        Verify if a job is running in a specific namespace.

        Args:
            job_name (str): Name of the job to verify
            namespace (str): Namespace where the job is running

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if job is running, False otherwise
                - str: Error message if an exception occurred
        """
        try:
            # Get the job status
            job = self.batchV1Api.read_namespaced_job(name=job_name, namespace=namespace)
            if job.status.active is not None and job.status.active > 0:
                return CommonResult(True, "")
            else:
                return CommonResult(False, "Job is not running" + str(job.status))
        except ApiException as e:
            error_msg = f"Failed to verify job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error verifying job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)

    def get_job_pod_name(self, job_name: str, namespace: str) -> CommonResult[str, str]:
        """
        Get the pod name of a job in a specific namespace.

        Args:
            job_name (str): Name of the job to get the pod name for
            namespace (str): Namespace where the job is running

        Returns:
            CommonResult[str, str]: Operation result containing:
                - str: Pod name if successful
                - str: Error message if an exception occurred
        """
        try:
            # Get the job details
            timeout = 120
            start_time = time.time()
            while True:
                podlist, _ = self.list_pods(namespace, name_pattern=job_name).values
                print([pod.status.phase for pod in podlist])
                running_pods = [pod for pod in podlist if pod.status.phase == "Running"]
                if len(running_pods) > 0:
                    break
                if time.time() - start_time > timeout:
                    return CommonResult(None, "Timeout waiting for pod to start")
                time.sleep(1)
            # Get the pod name from the job's pod template
            pod_name = running_pods[0].metadata.name
            return CommonResult(pod_name, "")
        except ApiException as e:
            error_msg = f"Failed to get pod name for job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error getting pod name for job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)

    def delete_job(self, job_name: str, namespace: str) -> CommonResult[bool, str]:
        """
        Delete a job in a specific namespace.

        Args:
            job_name (str): Name of the job to delete
            namespace (str): Namespace where the job is running

        Returns:
            CommonResult[bool, str]: Operation result containing:
                - bool: True if job is deleted, False otherwise
                - str: Error message if an exception occurred
        """
        try:
            # Delete the job
            result = self.batchV1Api.delete_namespaced_job(name=job_name, namespace=namespace)
            return CommonResult(True, "")
        except ApiException as e:
            error_msg = f"Failed to delete job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
        except Exception as e:
            error_msg = f"Unexpected error deleting job {job_name} in namespace {namespace}: {str(e)}"
            return CommonResult(None, error_msg)
