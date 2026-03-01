# tyk-sre-python Helm Chart

This chart deploys the Python SRE service into Kubernetes/Minikube.

For NetworkPolicy endpoints to work correctly in Minikube, start it with a CNI that enforces policies:

```bash
minikube start --cni=calico
```

## What gets deployed

- `Deployment` for the HTTP API server on port `8080`
- `Service` to expose the pod in-cluster
- `ServiceAccount` used by the pod
- `ClusterRole` and `ClusterRoleBinding` with permissions required by API endpoints

## Endpoint coverage and permissions

The application exposes these endpoints:

- `GET /healthz`
- `GET /status/deployments`
- `GET /status/k8s-api`
- `GET /network-policies`
- `POST /network-policies/create`
- `POST /network-policies/remove`

RBAC in this chart is aligned to these endpoint operations:

- `apps/deployments` (`get`, `list`, `watch`) for `/status/deployments`
- `networking.k8s.io/networkpolicies` (`get`, `list`, `watch`, `create`, `delete`) for network policy endpoints
- non-resource URL `/version` (`get`) for `/status/k8s-api`

## Install

```bash
helm upgrade --install tyk-sre-python ./helm/tyk-sre-python \
  --namespace tyk-sre \
  --create-namespace
```

## Important values

- `image`

## Health probes

Readiness and liveness probes are configured directly in the chart and use `GET /healthz`.
