# Scale test infra

## Bring up

0. Login to GCP
```shell
gcloud auth login
gcloud auth application-default login
```

1. Initialize terraform
```shell
terraform init
```

2. Create the cluster (this will take ~50 minutes)
```shell
terraform apply
```

3. Fetch the kubeconfig for the cluster
```shell
gcloud container clusters get-credentials scale-test-cluster --region us-east5 --project proj-dgxc-runai-np-test-mega
```

4. Install the monitoring stack
```shell
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install prometheus prometheus-community/kube-prometheus-stack -n prometheus --create-namespace
```

## Teardown

0. Login to GCP
```shell
gcloud auth login
gcloud auth application-default login
```

1. Initialize terraform
```shell
terraform init
```

2. Delete the cluster (this will take ~50 minutes)
```shell
terraform destroy
```
