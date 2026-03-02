package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// App holds shared dependencies available to every HTTP handler.
type App struct {
	clientset kubernetes.Interface
}

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig, leave empty for in-cluster")
	listenAddr := flag.String("address", ":8080", "HTTP server listen address")

	flag.Parse()

	kConfig, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err)
	}

	clientset, err := kubernetes.NewForConfig(kConfig)
	if err != nil {
		panic(err)
	}

	version, err := getKubernetesVersion(clientset)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Connected to Kubernetes %s\n", version)

	a := &App{clientset: clientset}

	if err := a.startServer(*listenAddr); err != nil {
		panic(err)
	}
}

// getKubernetesVersion returns a string GitVersion of the Kubernetes server defined by the clientset.
//
// If it can't connect an error will be returned, which makes it useful to check connectivity.
func getKubernetesVersion(clientset kubernetes.Interface) (string, error) {
	version, err := clientset.Discovery().ServerVersion()
	if err != nil {
		return "", err
	}

	return version.GitVersion, nil
}

// startServer launches an HTTP server with defined handlers and blocks until it's terminated or fails with an error.
//
// Expects a listenAddr to bind to.
func (a *App) startServer(listenAddr string) error {
	http.HandleFunc("/healthz", healthHandler)
	http.HandleFunc("/status/deployments", a.statusDeploymentsHandler)
	http.HandleFunc("/status/k8s-api", a.statusK8sAPIHandler)
	http.HandleFunc("/network-policies", a.networkPoliciesHandler)
	http.HandleFunc("/network-policies/create", a.createNetworkPolicyHandler)
	http.HandleFunc("/network-policies/remove", a.removeNetworkPolicyHandler)

	fmt.Printf("Server listening on %s\n", listenAddr)

	return http.ListenAndServe(listenAddr, nil)
}

// writeJSON writes a JSON response with the given status code and data.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeText writes a plain text response with the given status code.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(text))
}

// healthHandler responds with the health status of the application.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeText(w, http.StatusOK, "ok")
}

// statusDeploymentsHandler reports whether all deployments in the cluster have
// as many healthy (ready) pods as requested by their Deployment spec.
func (a *App) statusDeploymentsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	deployments, err := a.clientset.AppsV1().Deployments("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		writeText(w, http.StatusInternalServerError, fmt.Sprintf("Error connecting to Kubernetes API: %s", err))
		return
	}

	type unhealthyDeployment struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Desired   int32  `json:"desired"`
		Ready     int32  `json:"ready"`
	}

	var unhealthy []unhealthyDeployment
	for _, d := range deployments.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		ready := d.Status.ReadyReplicas
		if ready < desired {
			unhealthy = append(unhealthy, unhealthyDeployment{
				Namespace: d.Namespace,
				Name:      d.Name,
				Desired:   desired,
				Ready:     ready,
			})
		}
	}

	if unhealthy == nil {
		unhealthy = []unhealthyDeployment{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_deployments":           len(deployments.Items),
		"total_unhealthy_deployments": len(unhealthy),
		"unhealthy_deployments":       unhealthy,
	})
}

// statusK8sAPIHandler reports whether the tool can successfully communicate
// with the configured Kubernetes API server.
func (a *App) statusK8sAPIHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	version, err := getKubernetesVersion(a.clientset)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "disconnected",
			"error":  err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status":             "connected",
		"kubernetes_version": version,
	})
}

// networkPoliciesHandler lists all currently applied "blocking" NetworkPolicies
// (those named with the on-demand-block- prefix or full deny-all policies).
func (a *App) networkPoliciesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	policies, err := a.clientset.NetworkingV1().NetworkPolicies("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	type blockedPolicy struct {
		Namespace     string   `json:"namespace"`
		Name          string   `json:"name"`
		PolicyTypes   []string `json:"policy_types"`
		IngressRules  int      `json:"ingress_rules"`
		EgressRules   int      `json:"egress_rules"`
		CreatedAt     *string  `json:"created_at"`
		CreatedByTool bool     `json:"created_by_tool"`
	}

	var blocked []blockedPolicy
	for _, p := range policies.Items {
		var policyTypes []string
		for _, pt := range p.Spec.PolicyTypes {
			policyTypes = append(policyTypes, string(pt))
		}

		hasIngress := containsString(policyTypes, "Ingress")
		hasEgress := containsString(policyTypes, "Egress")
		createdByTool := strings.HasPrefix(p.Name, "on-demand-block-")

		isDenyAll := hasIngress && hasEgress && len(p.Spec.Ingress) == 0 && len(p.Spec.Egress) == 0
		if !createdByTool && !isDenyAll {
			continue
		}

		var createdAt *string
		if !p.CreationTimestamp.IsZero() {
			t := p.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z07:00")
			createdAt = &t
		}

		if policyTypes == nil {
			policyTypes = []string{}
		}

		blocked = append(blocked, blockedPolicy{
			Namespace:     p.Namespace,
			Name:          p.Name,
			PolicyTypes:   policyTypes,
			IngressRules:  len(p.Spec.Ingress),
			EgressRules:   len(p.Spec.Egress),
			CreatedAt:     createdAt,
			CreatedByTool: createdByTool,
		})
	}

	if blocked == nil {
		blocked = []blockedPolicy{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"total_blocking_policies_found": len(blocked),
		"blocking_policies":             blocked,
	})
}

// createNetworkPolicyRequest is the expected JSON request body for creating network policies.
type createNetworkPolicyRequest struct {
	Namespace1   string            `json:"namespace1"`
	PodSelector1 map[string]string `json:"pod_selector1"`
	Namespace2   string            `json:"namespace2"`
	PodSelector2 map[string]string `json:"pod_selector2"`
}

// removeNetworkPolicyRequest is the expected JSON request body for removing network policies.
// Supports two forms:
//  1. By name:  {"namespace": "ns", "name": "policy-name"}
//  2. By params: {"namespace1": ..., "pod_selector1": ..., "namespace2": ..., "pod_selector2": ...}
type removeNetworkPolicyRequest struct {
	// By name
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// By creation params
	Namespace1   string            `json:"namespace1"`
	PodSelector1 map[string]string `json:"pod_selector1"`
	Namespace2   string            `json:"namespace2"`
	PodSelector2 map[string]string `json:"pod_selector2"`
}

// generatePolicyHash produces an 8-character MD5 hex prefix that uniquely
// identifies a pair of (namespace, podSelector) workloads, matching the
// hash algorithm used by the Python implementation.
func generatePolicyHash(ns1 string, sel1 map[string]string, ns2 string, sel2 map[string]string) string {
	sel1JSON, _ := marshalSortedMap(sel1)
	sel2JSON, _ := marshalSortedMap(sel2)
	input := fmt.Sprintf("%s:%s:%s:%s", ns1, sel1JSON, ns2, sel2JSON)
	sum := md5.Sum([]byte(input)) //nolint:gosec // non-cryptographic use
	return fmt.Sprintf("%x", sum)[:8]
}

// marshalSortedMap serialises a map[string]string to JSON with sorted keys,
// matching Python's json.dumps(sort_keys=True) behaviour.
func marshalSortedMap(m map[string]string) (string, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build a json.Marshaler-compatible ordered structure.
	type kv struct {
		k string
		v string
	}
	pairs := make([]kv, len(keys))
	for i, k := range keys {
		pairs[i] = kv{k, m[k]}
	}

	var sb strings.Builder
	sb.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(p.k)
		vb, _ := json.Marshal(p.v)
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return sb.String(), nil
}

// buildBlockingPolicy constructs a NetworkPolicy that allows ingress to
// targetNS/targetSel from everywhere EXCEPT blockedNS/blockedSel.
func buildBlockingPolicy(name, targetNS string, targetSel map[string]string, blockedNS string, blockedSel map[string]string) *networkingv1.NetworkPolicy {
	notInExpressions := podSelectorToNotInExpressions(blockedSel)

	ingressRule := networkingv1.NetworkPolicyIngressRule{
		From: []networkingv1.NetworkPolicyPeer{
			// Allow from any namespace that is NOT blockedNS.
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchExpressions: []metav1.LabelSelectorRequirement{
						{
							Key:      "kubernetes.io/metadata.name",
							Operator: metav1.LabelSelectorOpNotIn,
							Values:   []string{blockedNS},
						},
					},
				},
			},
			// Allow from blockedNS but only pods NOT matching blockedSel.
			{
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": blockedNS,
					},
				},
				PodSelector: &metav1.LabelSelector{
					MatchExpressions: notInExpressions,
				},
			},
		},
	}

	policyTypeIngress := networkingv1.PolicyTypeIngress
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: targetSel,
			},
			PolicyTypes: []networkingv1.PolicyType{policyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{ingressRule},
		},
	}
}

// podSelectorToNotInExpressions converts a label map into NotIn match expressions.
func podSelectorToNotInExpressions(sel map[string]string) []metav1.LabelSelectorRequirement {
	reqs := make([]metav1.LabelSelectorRequirement, 0, len(sel))
	for k, v := range sel {
		reqs = append(reqs, metav1.LabelSelectorRequirement{
			Key:      k,
			Operator: metav1.LabelSelectorOpNotIn,
			Values:   []string{v},
		})
	}
	return reqs
}

// createNetworkPolicyHandler creates a pair of NetworkPolicies that prevent
// two workloads (identified by namespace + label selector) from communicating.
func (a *App) createNetworkPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	var req createNetworkPolicyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	if req.Namespace1 == "" || req.PodSelector1 == nil || req.Namespace2 == "" || req.PodSelector2 == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "namespace1, pod_selector1, namespace2, and pod_selector2 are required",
		})
		return
	}

	hash := generatePolicyHash(req.Namespace1, req.PodSelector1, req.Namespace2, req.PodSelector2)
	policy1Name := fmt.Sprintf("on-demand-block-%s-from-%s-%s", req.Namespace1, req.Namespace2, hash)
	policy2Name := fmt.Sprintf("on-demand-block-%s-from-%s-%s", req.Namespace2, req.Namespace1, hash)

	policy1 := buildBlockingPolicy(policy1Name, req.Namespace1, req.PodSelector1, req.Namespace2, req.PodSelector2)
	policy2 := buildBlockingPolicy(policy2Name, req.Namespace2, req.PodSelector2, req.Namespace1, req.PodSelector1)

	ctx := context.Background()

	if _, err := a.clientset.NetworkingV1().NetworkPolicies(req.Namespace1).Create(ctx, policy1, metav1.CreateOptions{}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if _, err := a.clientset.NetworkingV1().NetworkPolicies(req.Namespace2).Create(ctx, policy2, metav1.CreateOptions{}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":   "policies_created",
		"policies": []string{policy1Name, policy2Name},
	})
}

// removeNetworkPolicyHandler removes a previously created blocking NetworkPolicy.
// Accepts either removal by (namespace + name) or by original creation parameters.
func (a *App) removeNetworkPolicyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty request body"})
		return
	}

	var req removeNetworkPolicyRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}

	ctx := context.Background()
	netClient := a.clientset.NetworkingV1().NetworkPolicies

	// Remove by explicit name.
	if req.Namespace != "" || req.Name != "" {
		if req.Namespace == "" || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "namespace and name are required"})
			return
		}
		if err := netClient(req.Namespace).Delete(ctx, req.Name, metav1.DeleteOptions{}); err != nil {
			handleK8sDeleteError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":    "deleted",
			"namespace": req.Namespace,
			"name":      req.Name,
		})
		return
	}

	// Remove by creation parameters.
	if req.Namespace1 == "" || req.PodSelector1 == nil || req.Namespace2 == "" || req.PodSelector2 == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Either (namespace and name) or (namespace1, pod_selector1, namespace2, pod_selector2) are required",
		})
		return
	}

	hash := generatePolicyHash(req.Namespace1, req.PodSelector1, req.Namespace2, req.PodSelector2)
	policy1Name := fmt.Sprintf("on-demand-block-%s-from-%s-%s", req.Namespace1, req.Namespace2, hash)
	policy2Name := fmt.Sprintf("on-demand-block-%s-from-%s-%s", req.Namespace2, req.Namespace1, hash)

	if err := netClient(req.Namespace1).Delete(ctx, policy1Name, metav1.DeleteOptions{}); err != nil {
		handleK8sDeleteError(w, err)
		return
	}
	if err := netClient(req.Namespace2).Delete(ctx, policy2Name, metav1.DeleteOptions{}); err != nil {
		handleK8sDeleteError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "deleted",
		"policies": []string{policy1Name, policy2Name},
	})
}

// handleK8sDeleteError translates a k8s API error into an HTTP response.
func handleK8sDeleteError(w http.ResponseWriter, err error) {
	if isNotFound(err) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "networkpolicy not found"})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// isNotFound returns true when the error is a 404 from the Kubernetes API.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// containsString returns true if slice contains the target string.
func containsString(slice []string, target string) bool {
	for _, s := range slice {
		if s == target {
			return true
		}
	}
	return false
}
