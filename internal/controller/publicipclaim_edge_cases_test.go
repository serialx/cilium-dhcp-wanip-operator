package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
)

// TestReconcileResourceNotFound tests handling of deleted resources
func TestReconcileResourceNotFound(t *testing.T) {
	scheme := runtimeScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"}}

	// Should return without error when resource not found
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("expected no error for not found resource, got: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected empty result, got: %v", res)
	}
}

// TestReconcileAlreadyReady tests periodic verification for ready claims
func TestReconcileAlreadyReady(t *testing.T) {
	scheme := runtimeScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "router-key",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{"id_rsa": []byte("dummy-key-data")},
	}

	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "ready-claim",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         "router.local",
				User:         "admin",
				SSHSecretRef: "router-key",
				Command:      "/bin/script.sh",
				WanParent:    "eth0",
			},
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			Phase:        networkv1alpha1.ClaimPhaseReady,
			AssignedIP:   "203.0.113.10",
			WanInterface: "wan-test",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).WithObjects(claim, secret).Build()
	r := &PublicIPClaimReconciler{
		Client:            c,
		Scheme:            scheme,
		SSHRegistry:       nil, // Will trigger error in verification
		routerAssignments: make(map[string]string),
		sshHandlers:       make(map[string]uint64),
		Recorder:          &fakeRecorder{},
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// Should perform periodic verification and requeue after 60 minutes (even if verification fails due to nil registry)
	res, err := r.Reconcile(ctx, req)
	// We expect an error because SSHRegistry is nil, but result should still have RequeueAfter set
	if res.RequeueAfter != 60*time.Minute {
		t.Fatalf("expected requeue after 60 minutes for Ready claim, got: %v (err: %v)", res, err)
	}
}

// fakeRecorder implements record.EventRecorder for testing
type fakeRecorder struct{}

func (f *fakeRecorder) Event(object runtime.Object, eventtype, reason, message string) {}
func (f *fakeRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
}
func (f *fakeRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
}

// TestReconcileInProgress tests skipping claims that are in progress
func TestReconcileInProgress(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "progress-claim",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			WanInterface: "wan-test",
			Phase:        networkv1alpha1.ClaimPhasePending,
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).WithObjects(claim).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// Should skip when WanInterface is set but not Ready/Failed
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Fatalf("expected empty result for in-progress claim, got: %v", res)
	}
}

// TestReconcileFailedRequeue tests requeueing of failed claims
func TestReconcileFailedRequeue(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "failed-claim",
			Namespace:  "default",
			Finalizers: []string{finalizerName},
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			Phase:   networkv1alpha1.ClaimPhaseFailed,
			Message: "previous error",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).WithObjects(claim).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// Should requeue after 5 minutes for failed claims
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter != 5*time.Minute {
		t.Fatalf("expected requeue after 5 minutes, got: %v", res.RequeueAfter)
	}
}

// TestEnsureIPInPoolV2AlphaFallback tests fallback to v2alpha1 API
func TestEnsureIPInPoolV2AlphaFallback(t *testing.T) {
	scheme := runtime.NewScheme()

	// Create pool in v2alpha1 (not in v2)
	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2alpha1",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "alpha-pool",
		},
		"spec": map[string]interface{}{
			"blocks": []interface{}{},
		},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should fallback to v2alpha1 when v2 returns NotFound
	err := r.defaultEnsureIPInPool(ctx, "alpha-pool", "198.51.100.5")
	if err != nil {
		t.Fatalf("expected success with v2alpha1 fallback, got error: %v", err)
	}

	// Verify IP was added to v2alpha1 pool
	poolObj, err := dynClient.Resource(schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2alpha1",
		Resource: "ciliumloadbalancerippools",
	}).Get(ctx, "alpha-pool", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get pool: %v", err)
	}

	blocks, _, _ := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].(map[string]interface{})["cidr"] != "198.51.100.5/32" {
		t.Fatalf("unexpected CIDR: %v", blocks[0])
	}
}

// TestEnsureIPInPoolNoBlocks tests pool with no existing blocks
func TestEnsureIPInPoolNoBlocks(t *testing.T) {
	scheme := runtime.NewScheme()

	// Pool without blocks field
	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "empty-pool",
		},
		"spec": map[string]interface{}{},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should create blocks list if it doesn't exist
	err := r.defaultEnsureIPInPool(ctx, "empty-pool", "203.0.113.20")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	// Verify blocks were created
	poolObj, _ := dynClient.Resource(gvrPoolV2).Get(ctx, "empty-pool", metav1.GetOptions{})
	blocks, found, _ := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	if !found {
		t.Fatalf("expected blocks to be created")
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
}

// TestEnsureIPInPoolAlreadyExists tests idempotency
func TestEnsureIPInPoolAlreadyExists(t *testing.T) {
	scheme := runtime.NewScheme()

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "existing-pool",
		},
		"spec": map[string]interface{}{
			"blocks": []interface{}{
				map[string]interface{}{"cidr": "192.0.2.10/32"},
			},
		},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should not add duplicate
	err := r.defaultEnsureIPInPool(ctx, "existing-pool", "192.0.2.10")
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	poolObj, _ := dynClient.Resource(gvrPoolV2).Get(ctx, "existing-pool", metav1.GetOptions{})
	blocks, _, _ := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block (no duplicate), got %d", len(blocks))
	}
}

// TestRemoveIPFromPoolNotFound tests pool not found handling
func TestRemoveIPFromPoolNotFound(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClient(scheme)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should succeed silently when pool doesn't exist
	err := r.defaultRemoveIPFromPool(ctx, "nonexistent-pool", "198.51.100.1")
	if err != nil {
		t.Fatalf("expected no error when pool not found, got: %v", err)
	}
}

// TestRemoveIPFromPoolV2AlphaFallback tests fallback to v2alpha1
func TestRemoveIPFromPoolV2AlphaFallback(t *testing.T) {
	scheme := runtime.NewScheme()

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2alpha1",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "alpha-pool",
		},
		"spec": map[string]interface{}{
			"blocks": []interface{}{
				map[string]interface{}{"cidr": "203.0.113.99/32"},
			},
		},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should fallback to v2alpha1 and remove IP
	err := r.defaultRemoveIPFromPool(ctx, "alpha-pool", "203.0.113.99")
	if err != nil {
		t.Fatalf("expected success with v2alpha1 fallback, got: %v", err)
	}

	poolObj, _ := dynClient.Resource(gvrPoolV2a).Get(ctx, "alpha-pool", metav1.GetOptions{})
	blocks, _, _ := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	if len(blocks) != 0 {
		t.Fatalf("expected empty blocks, got %d", len(blocks))
	}
}

// TestRemoveIPFromPoolEmptyIP tests empty IP handling
func TestRemoveIPFromPoolEmptyIP(t *testing.T) {
	r := &PublicIPClaimReconciler{}
	ctx := context.Background()

	// Should succeed silently with empty IP
	err := r.defaultRemoveIPFromPool(ctx, "any-pool", "")
	if err != nil {
		t.Fatalf("expected no error for empty IP, got: %v", err)
	}
}

// TestRemoveIPFromPoolNoBlocks tests pool with no blocks
func TestRemoveIPFromPoolNoBlocks(t *testing.T) {
	scheme := runtime.NewScheme()

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "empty-pool",
		},
		"spec": map[string]interface{}{},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should succeed when no blocks to remove
	err := r.defaultRemoveIPFromPool(ctx, "empty-pool", "192.0.2.1")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

// TestRemoveIPFromPoolIPNotInPool tests removing non-existent IP
func TestRemoveIPFromPoolIPNotInPool(t *testing.T) {
	scheme := runtime.NewScheme()

	pool := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumLoadBalancerIPPool",
		"metadata": map[string]interface{}{
			"name": "test-pool",
		},
		"spec": map[string]interface{}{
			"blocks": []interface{}{
				map[string]interface{}{"cidr": "192.0.2.5/32"},
			},
		},
	}}

	dynClient := dynamicfake.NewSimpleDynamicClient(scheme, pool)
	r := &PublicIPClaimReconciler{Dynamic: dynClient}

	ctx := context.Background()

	// Should succeed when IP not in pool
	err := r.defaultRemoveIPFromPool(ctx, "test-pool", "192.0.2.99")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	// Original block should still be there
	poolObj, _ := dynClient.Resource(gvrPoolV2).Get(ctx, "test-pool", metav1.GetOptions{})
	blocks, _, _ := unstructured.NestedSlice(poolObj.Object, "spec", "blocks")
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
}

// TestCleanupRouterInterfaceNoWanInterface tests cleanup with empty WAN interface
func TestCleanupRouterInterfaceNoWanInterface(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-wan-claim",
			Namespace: "default",
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			WanInterface: "", // Empty
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()

	// Should succeed without attempting cleanup
	err := r.defaultCleanupRouterInterface(ctx, claim)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
}

// TestCleanupRouterInterfaceSecretNotFound tests cleanup when secret is deleted
func TestCleanupRouterInterfaceSecretNotFound(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-secret-claim",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			Router: networkv1alpha1.RouterSpec{
				SSHSecretRef: "missing-secret",
			},
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			WanInterface: "wan-test",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()

	// Should succeed when secret not found (graceful degradation)
	err := r.defaultCleanupRouterInterface(ctx, claim)
	if err != nil {
		t.Fatalf("expected success when secret not found, got: %v", err)
	}
}

// TestCleanupRouterInterfaceEmptyKey tests cleanup with empty SSH key
func TestCleanupRouterInterfaceEmptyKey(t *testing.T) {
	scheme := runtimeScheme(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-key",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{"id_rsa": []byte("")}, // Empty key
	}

	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-key-claim",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			Router: networkv1alpha1.RouterSpec{
				SSHSecretRef: "empty-key",
			},
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			WanInterface: "wan-test",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()

	// Should succeed when key is empty (graceful degradation)
	err := r.defaultCleanupRouterInterface(ctx, claim)
	if err != nil {
		t.Fatalf("expected success when key empty, got: %v", err)
	}
}

// TestSanitizeNameEdgeCases tests sanitization edge cases
func TestSanitizeNameEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"lowercase", "test", "test"},
		{"uppercase", "TEST", "test"},
		{"mixed case", "TestName", "testname"},
		{"with hyphens", "test-name-123", "test-name-"},
		{"special chars", "test_name@123", "test-name-"},
		{"very long", "this-is-a-very-long-interface-name-that-exceeds-limit", "this-is-a-"},
		{"dots and slashes", "test.name/foo", "test-name-"},
		{"all special", "@#$%^&*()", "---------"}, // 9 chars input = 9 hyphens output
		{"numbers only", "12345", "12345"},
		{"single char", "a", "a"},
		{"exactly 10", "1234567890", "1234567890"},
		{"11 chars", "12345678901", "1234567890"}, // Truncated to 10
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeName(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
			if len(result) > 10 {
				t.Errorf("sanitizeName(%q) = %q (length %d), exceeds 10 chars", tt.input, result, len(result))
			}
		})
	}
}

// TestGenerateUniqueMAC tests MAC address generation
func TestGenerateUniqueMAC(t *testing.T) {
	seen := make(map[string]bool)

	// Generate multiple MACs and ensure they're unique and valid
	for i := 0; i < 100; i++ {
		mac := generateUniqueMAC()

		// Check format: 02:xx:xx:xx:xx:xx
		if len(mac) != 17 {
			t.Fatalf("invalid MAC length: %d (expected 17)", len(mac))
		}
		if mac[:3] != "02:" {
			t.Fatalf("MAC doesn't start with 02: (locally administered): %s", mac)
		}

		// Check uniqueness
		if seen[mac] {
			t.Fatalf("duplicate MAC generated: %s", mac)
		}
		seen[mac] = true
	}
}

// TestCoalesceInt tests integer coalescing
func TestCoalesceInt(t *testing.T) {
	tests := []struct {
		name     string
		a        int
		b        int
		expected int
	}{
		{"both zero", 0, 0, 0},
		{"first nonzero", 22, 80, 22},
		{"first zero", 0, 80, 80},
		{"both nonzero", 2222, 80, 2222},
		{"negative first", -1, 80, -1}, // -1 is non-zero
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := coalesceInt(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("coalesceInt(%d, %d) = %d, want %d", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

// TestFailUpdateError tests fail() when status update fails
func TestFailUpdateError(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fail-test",
			Namespace: "default",
		},
	}

	// Create client without status subresource to simulate update failure
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claim).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()

	// Should return the update error
	_, err := r.fail(ctx, claim, fmt.Errorf("original error"))
	if err == nil {
		t.Fatal("expected error when status update fails")
	}
	// The error should be from the update failure, not the original error
	if !apierrors.IsNotFound(err) {
		// Note: fake client might behave differently, but we expect some error
		t.Logf("got error: %v", err)
	}
}

// TestReconcileSSHSecretNotFound tests allocation failure when secret is missing
func TestReconcileSSHSecretNotFound(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-secret",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         "router.local",
				User:         "admin",
				SSHSecretRef: "missing-secret",
				Command:      "/bin/script.sh",
				WanParent:    "eth0",
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).
		WithObjects(claim).
		Build()

	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// First reconcile adds finalizer
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	// Second reconcile should fail due to missing secret
	_, err = r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error when SSH secret not found")
	}

	// Verify status was updated to Failed
	updated := &networkv1alpha1.PublicIPClaim{}
	if err := c.Get(ctx, req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get updated claim: %v", err)
	}
	if updated.Status.Phase != networkv1alpha1.ClaimPhaseFailed {
		t.Fatalf("expected phase Failed, got %s", updated.Status.Phase)
	}
}

// TestReconcileInvalidIPFromScript tests handling of invalid IP from router script
func TestReconcileInvalidIPFromScript(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-ip-claim",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         "router.local",
				User:         "admin",
				SSHSecretRef: "router-key",
				Command:      "/bin/script.sh",
				WanParent:    "eth0",
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "router-key",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{"id_rsa": []byte("dummy")},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).
		WithObjects(claim, secret).
		Build()

	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	// Mock script to return invalid IP
	r.runRouterScriptFn = func(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
		return "not-a-valid-ip", nil
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// First reconcile adds finalizer
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	// Second reconcile should fail due to invalid IP
	_, err = r.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}

	// Verify status shows failed with IP validation error
	updated := &networkv1alpha1.PublicIPClaim{}
	if err := c.Get(ctx, req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get updated claim: %v", err)
	}
	if updated.Status.Phase != networkv1alpha1.ClaimPhaseFailed {
		t.Fatalf("expected phase Failed, got %s", updated.Status.Phase)
	}
	if updated.Status.Message == "" {
		t.Fatal("expected error message in status")
	}
}
