import hashlib
import socketserver
import json
from kubernetes import client
from kubernetes.client.rest import ApiException
from http.server import BaseHTTPRequestHandler


class AppHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        """Catch all incoming GET requests"""
        if self.path == "/healthz":
            self.healthz()
        elif self.path == "/status/deployments":
            self.status_deployments()
        elif self.path == "/status/k8s-api":
            self.status_k8s_api()
        elif self.path == "/network-policies":
            self.list_network_policies()
        else:
            self.send_error(404)

    def do_POST(self):
        """Handle all incoming POST requests"""
        if self.path == "/network-policies/create":
            self.create_network_policy()
        elif self.path == "/network-policies/remove":
            self.remove_network_policy()
        else:
            self.send_error(404)

    def status_deployments(self):
        """Check if all deployments have their desired number of healthy pods"""
        try:
            apps_v1_api = client.AppsV1Api()
            deployments = apps_v1_api.list_deployment_for_all_namespaces(watch=False)

            total_deployments = len(deployments.items)
            unhealthy_deployments = []
            for deployment in deployments.items:
                desired_replicas = deployment.spec.replicas or 1
                ready_replicas = deployment.status.ready_replicas or 0

                if ready_replicas < desired_replicas:
                    unhealthy_deployments.append(
                        {
                            "namespace": deployment.metadata.namespace,
                            "name": deployment.metadata.name,
                            "desired": desired_replicas,
                            "ready": ready_replicas,
                        }
                    )

            response_data = {
                "total_deployments": total_deployments,
                "total_unhealthy_deployments": len(unhealthy_deployments),
                "unhealthy_deployments": unhealthy_deployments,
            }
            self.respond(200, json.dumps(response_data))
        except Exception as e:
            self.respond(500, "Error connecting to Kubernetes API: {}".format(str(e)))

    def status_k8s_api(self):
        """Check if we can successfully communicate with the Kubernetes API server"""
        try:
            k8s_api_client = client.ApiClient()
            version = client.VersionApi(k8s_api_client).get_code()

            response_data = {
                "status": "connected",
                "kubernetes_version": version.git_version,
            }
            self.respond(200, json.dumps(response_data))
        except Exception as e:
            response_data = {"status": "disconnected", "error": str(e)}
            self.respond(503, json.dumps(response_data))

    def _generate_policy_hash(
        self, namespace1, pod_selector1, namespace2, pod_selector2
    ):
        """Generate a hash from the combined input parameters"""
        input_string = f"{namespace1}:{json.dumps(pod_selector1, sort_keys=True)}:{namespace2}:{json.dumps(pod_selector2, sort_keys=True)}"
        return hashlib.md5(input_string.encode()).hexdigest()[:8]

    def create_network_policy(self):
        """Create a NetworkPolicy to prevent traffic between two workloads(K8s Namespace + labelSelector)"""
        try:
            content_length = int(self.headers["Content-Length"])
            body = json.loads(self.rfile.read(content_length))

            namespace1 = body.get("namespace1")
            pod_selector1 = body.get("pod_selector1")
            namespace2 = body.get("namespace2")
            pod_selector2 = body.get("pod_selector2")

            if not all([namespace1, pod_selector1, namespace2, pod_selector2]):
                return self.respond(
                    400,
                    json.dumps(
                        {
                            "error": "namespace1, pod_selector1, namespace2, and pod_selector2 are required"
                        }
                    ),
                )
            policy_hash = self._generate_policy_hash(
                namespace1, pod_selector1, namespace2, pod_selector2
            )
            network_api = client.NetworkingV1Api()

            # Block ingress to namespace1:pod_selector1 from namespace2:pod_selector2
            policy1 = client.V1NetworkPolicy(
                metadata=client.V1ObjectMeta(
                    name=f"on-demand-block-{namespace1}-from-{namespace2}-{policy_hash}"
                ),
                spec=client.V1NetworkPolicySpec(
                    pod_selector=client.V1LabelSelector(match_labels=pod_selector1),
                    policy_types=["Ingress"],
                    ingress=[
                        # Allow from all namespaces EXCEPT namespace2
                        client.V1NetworkPolicyIngressRule(
                            _from=[
                                client.V1NetworkPolicyPeer(
                                    namespace_selector=client.V1LabelSelector(
                                        match_expressions=[
                                            client.V1LabelSelectorRequirement(
                                                key="kubernetes.io/metadata.name",
                                                operator="NotIn",
                                                values=[namespace2],
                                            )
                                        ]
                                    )
                                ),
                                #  Allow from namespace2  but NOT matching pod_selector2
                                client.V1NetworkPolicyPeer(
                                    namespace_selector=client.V1LabelSelector(
                                        match_labels={
                                            "kubernetes.io/metadata.name": namespace2
                                        }
                                    ),
                                    pod_selector=client.V1LabelSelector(
                                        match_expressions=[
                                            client.V1LabelSelectorRequirement(
                                                key=k,
                                                operator="NotIn",
                                                values=[v],
                                            )
                                            for k, v in pod_selector2.items()
                                        ]
                                    ),
                                ),
                            ]
                        )
                    ],
                ),
            )
            network_api.create_namespaced_network_policy(namespace1, policy1)

            # Block ingress to namespace2:pod_selector2 from namespace1:pod_selector1
            policy2 = client.V1NetworkPolicy(
                metadata=client.V1ObjectMeta(
                    name=f"on-demand-block-{namespace2}-from-{namespace1}-{policy_hash}"
                ),
                spec=client.V1NetworkPolicySpec(
                    pod_selector=client.V1LabelSelector(match_labels=pod_selector2),
                    policy_types=["Ingress"],
                    ingress=[
                        # Allow from all namespaces EXCEPT namespace1
                        client.V1NetworkPolicyIngressRule(
                            _from=[
                                client.V1NetworkPolicyPeer(
                                    namespace_selector=client.V1LabelSelector(
                                        match_expressions=[
                                            client.V1LabelSelectorRequirement(
                                                key="kubernetes.io/metadata.name",
                                                operator="NotIn",
                                                values=[namespace1],
                                            )
                                        ]
                                    )
                                ),
                                #  Allow from namespace1  but NOT matching pod_selector1
                                client.V1NetworkPolicyPeer(
                                    namespace_selector=client.V1LabelSelector(
                                        match_labels={
                                            "kubernetes.io/metadata.name": namespace1
                                        }
                                    ),
                                    pod_selector=client.V1LabelSelector(
                                        match_expressions=[
                                            client.V1LabelSelectorRequirement(
                                                key=k,
                                                operator="NotIn",
                                                values=[v],
                                            )
                                            for k, v in pod_selector1.items()
                                        ]
                                    ),
                                ),
                            ]
                        )
                    ],
                ),
            )
            network_api.create_namespaced_network_policy(namespace2, policy2)

            self.respond(
                201,
                json.dumps(
                    {
                        "status": "policies_created",
                        "policies": [
                            f"on-demand-block-{namespace1}-from-{namespace2}-{policy_hash}",
                            f"on-demand-block-{namespace2}-from-{namespace1}-{policy_hash}",
                        ],
                    }
                ),
            )
        except Exception as e:
            self.respond(500, json.dumps({"error": str(e)}))

    def remove_network_policy(self):
        """Remove a previously created or detected blocking NetworkPolicy

        Accepts either:
        1. Policy name and namespace: {"namespace": "ns1", "name": "policy-name"}
        2. Creation parameters: {"namespace1": "ns1", "pod_selector1": {...}, "namespace2": "ns2", "pod_selector2": {...}}
        """
        try:
            content_length = int(self.headers.get("Content-Length", 0))
            if content_length == 0:
                return self.respond(400, json.dumps({"error": "empty request body"}))

            body = json.loads(self.rfile.read(content_length))
            network_api = client.NetworkingV1Api()

            # Check if removal by name and namespace
            if "namespace" in body and "name" in body:
                namespace = body.get("namespace")
                name = body.get("name")
                if not namespace or not name:
                    return self.respond(
                        400, json.dumps({"error": "namespace and name are required"})
                    )
                network_api.delete_namespaced_network_policy(
                    name=name, namespace=namespace
                )
                self.respond(
                    200,
                    json.dumps(
                        {"status": "deleted", "namespace": namespace, "name": name}
                    ),
                )

            # Check if removal by creation parameters
            elif all(
                k in body
                for k in ["namespace1", "pod_selector1", "namespace2", "pod_selector2"]
            ):
                namespace1 = body.get("namespace1")
                pod_selector1 = body.get("pod_selector1")
                namespace2 = body.get("namespace2")
                pod_selector2 = body.get("pod_selector2")

                policy_hash = self._generate_policy_hash(
                    namespace1, pod_selector1, namespace2, pod_selector2
                )
                policy1_name = (
                    f"on-demand-block-{namespace1}-from-{namespace2}-{policy_hash}"
                )
                policy2_name = (
                    f"on-demand-block-{namespace2}-from-{namespace1}-{policy_hash}"
                )

                network_api.delete_namespaced_network_policy(
                    name=policy1_name, namespace=namespace1
                )

                network_api.delete_namespaced_network_policy(
                    name=policy2_name, namespace=namespace2
                )
                self.respond(
                    200,
                    json.dumps(
                        {"status": "deleted", "policies": [policy1_name, policy2_name]}
                    ),
                )
            else:
                return self.respond(
                    400,
                    json.dumps(
                        {
                            "error": "Either (namespace and name) or (namespace1, pod_selector1, namespace2, pod_selector2) are required"
                        }
                    ),
                )

        except ApiException as e:
            if e.status == 404:
                self.respond(404, json.dumps({"error": "networkpolicy not found"}))
            else:
                self.respond(
                    e.status if hasattr(e, "status") else 500,
                    json.dumps({"error": e.reason or str(e)}),
                )
        except Exception as e:
            self.respond(500, json.dumps({"error": str(e)}))

    def list_network_policies(self):
        """List currently applied 'blocking' NetworkPolicies (created by this tool or matching block pattern)"""
        try:
            network_api = client.NetworkingV1Api()
            applied_network_policies = (
                network_api.list_network_policy_for_all_namespaces(watch=False)
            )

            blocked = []
            for p in applied_network_policies.items:
                name = p.metadata.name
                ns = p.metadata.namespace
                spec = p.spec or client.V1NetworkPolicySpec()
                policy_types = spec.policy_types or []
                ingress = spec.ingress or []
                egress = spec.egress or []

                is_block = name.startswith("on-demand-block-") or (
                    "Ingress" in policy_types
                    and "Egress" in policy_types
                    and len(ingress) == 0
                    and len(egress) == 0
                )

                if is_block:
                    blocked.append(
                        {
                            "namespace": ns,
                            "name": name,
                            "policy_types": policy_types,
                            "ingress_rules": len(ingress),
                            "egress_rules": len(egress),
                            "created_at": (
                                p.metadata.creation_timestamp.isoformat()
                                if p.metadata.creation_timestamp
                                else None
                            ),
                            "created_by_tool": name.startswith("on-demand-block-"),
                        }
                    )

            response_data = {
                "total_blocking_policies_found": len(blocked),
                "blocking_policies": blocked,
            }
            self.respond(200, json.dumps(response_data))
        except Exception as e:
            self.respond(500, json.dumps({"error": str(e)}))

    def healthz(self):
        """Responds with the health status of the application"""
        self.respond(200, "ok")

    def respond(self, status: int, content: str):
        """Writes content and status code to the response socket"""
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()

        self.wfile.write(bytes(content, "UTF-8"))


def get_kubernetes_version(api_client: client.ApiClient) -> str:
    """
    Returns a string GitVersion of the Kubernetes server defined by the api_client.

    If it can't connect an underlying exception will be thrown.
    """
    version = client.VersionApi(api_client).get_code()
    return version.git_version


def start_server(address):
    """
    Launches an HTTP server with handlers defined by AppHandler class and blocks until it's terminated.

    Expects an address in the format of `host:port` to bind to.

    Throws an underlying exception in case of error.
    """
    try:
        host, port = address.split(":")
    except ValueError:
        print("invalid server address format")
        return

    with socketserver.TCPServer((host, int(port)), AppHandler) as httpd:
        print("Server listening on {}".format(address))
        httpd.serve_forever()
