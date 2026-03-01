# Tyk SRE Tool Setup Guide

## Prerequisites

Install these tools first (if not already installed):

- `python3`, `pip3`
- `kubectl`
- `minikube`
- `docker`
- `helm` (required only for Helm deployment scenario)
- `curl`

For macOS (Homebrew), you can use:

```bash
brew install python kubectl minikube helm curl
brew install --cask docker
```

## Start Minikube (Required for both scenarios)

Use Calico CNI so NetworkPolicy rules are actually enforced:

```bash
minikube start --cni=calico
```

## Scenario 1: Run app locally (without Helm)

Run the Python service on your machine while it talks to Minikube API server:

```bash
python3 -m venv .venv
source .venv/bin/activate
pip3 install -r requirements.txt
python main.py --kubeconfig ~/.kube/config --address 127.0.0.1:8080
```

Keep this terminal running.

In another terminal:

```bash
export APP_URL=http://127.0.0.1:8080
```

## Scenario 2: Run app in Minikube end-to-end (Helm)

Deploy the tool inside the cluster:

```bash
helm upgrade --install tyk-sre-python ./helm/tyk-sre-python \
	--namespace tyk-sre \
	--create-namespace
```

Verify resources:

```bash
kubectl get all -n tyk-sre
kubectl get clusterrole,clusterrolebinding | grep tyk-sre-python
```

Expose locally for testing:

```bash
kubectl port-forward -n tyk-sre svc/tyk-sre-python 8080:8080
```

Keep this terminal running.

In another terminal:

```bash
export APP_URL=http://127.0.0.1:8080
```

> **Note (macOS Minikube networking):**
> this guide uses `kubectl port-forward` for reliable local access.
> On Linux, you can also expose via Ingress (with an ingress controller) if preferred.

---

## Common test guide for all SRE scenarios

Use the commands below after setting `APP_URL` in either scenario.

### A) Check Kubernetes API connectivity

```bash
curl -s ${APP_URL}/healthz
curl -s ${APP_URL}/status/k8s-api
```

### B) Check deployment health vs requested replicas

```bash
curl -s ${APP_URL}/status/deployments
```

### C) On-demand network isolation between workloads

Create test namespaces/workloads first:

```bash
kubectl create namespace team-a
kubectl create namespace team-b

kubectl -n team-a create deployment frontend --image=nginx --replicas=1
kubectl -n team-b create deployment backend --image=nginx --replicas=1

kubectl -n team-a expose deployment frontend --port=80 --target-port=80
kubectl -n team-b expose deployment backend --port=80 --target-port=80
```

Patch workload labels to match selectors used by the API payload:

```bash
kubectl -n team-a label deployment frontend app=frontend --overwrite
kubectl -n team-b label deployment backend app=backend --overwrite
```

Check connectivity before blocking (expected HTTP `200`):

```bash
kubectl -n team-a exec deploy/frontend -- curl http://backend.team-b.svc.cluster.local
kubectl -n team-b exec deploy/backend -- curl http://frontend.team-a.svc.cluster.local
```

Create on-demand block policy:

```bash
curl -s -X POST ${APP_URL}/network-policies/create \
	-H 'Content-Type: application/json' \
	-d '{
		"namespace1": "team-a",
		"pod_selector1": {"app": "frontend"},
		"namespace2": "team-b",
		"pod_selector2": {"app": "backend"}
	}'
```

List blocking policies:

```bash
curl -s ${APP_URL}/network-policies
```

Check connectivity after blocking (expected failure/timeout or non-200):

```bash
kubectl -n team-a exec deploy/frontend -- sh -c 'curl -m 5 http://backend.team-b.svc.cluster.local >/dev/null 2>&1 && echo reachable || echo unreachable'
kubectl -n team-b exec deploy/backend -- sh -c 'curl -m 5 http://frontend.team-a.svc.cluster.local >/dev/null 2>&1 && echo reachable || echo unreachable'
```

Remove on-demand block policy:

```bash
curl -s -X POST ${APP_URL}/network-policies/remove \
	-H 'Content-Type: application/json' \
	-d '{
		"namespace1": "team-a",
		"pod_selector1": {"app": "frontend"},
		"namespace2": "team-b",
		"pod_selector2": {"app": "backend"}
	}'
```

Cleanup test resources:

```bash
kubectl delete namespace team-a team-b
```

## Optional cleanup for Helm scenario

```bash
helm uninstall tyk-sre-python -n tyk-sre
```
