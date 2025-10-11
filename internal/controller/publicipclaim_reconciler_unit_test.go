package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkv1alpha1 "serialx.net/cilium-dhcp-wanip-operator/api/v1alpha1"
)

func TestReconcileValidationFailure(t *testing.T) {
	scheme := runtimeScheme(t)
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validation-test",
			Namespace: "default",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&networkv1alpha1.PublicIPClaim{}).WithObjects(claim).Build()
	r := &PublicIPClaimReconciler{Client: c, Scheme: scheme}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// First reconcile should add the finalizer and requeue without error
	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("unexpected error on first reconcile: %v", err)
	}
	if res == (ctrl.Result{}) {
		t.Fatalf("expected non-zero result to trigger requeue")
	}

	// Second reconcile should fail validation and set status to Failed
	_, err = r.Reconcile(ctx, req)
	if err == nil {
		t.Fatalf("expected validation error on second reconcile")
	}

	updated := &networkv1alpha1.PublicIPClaim{}
	if err := c.Get(ctx, req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}

	if updated.Status.Phase != networkv1alpha1.ClaimPhaseFailed {
		t.Fatalf("expected phase Failed, got %s", updated.Status.Phase)
	}
	if updated.Status.Message == "" {
		t.Fatalf("expected failure message to be set")
	}
}

func TestReconcileSuccess(t *testing.T) {
	scheme := runtimeScheme(t)
	const assignedTestIP = "203.0.113.10"
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-claim-alpha",
			Namespace: "default",
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         "router.local",
				User:         "admin",
				SSHSecretRef: "router-key",
				Command:      "/usr/local/bin/alloc_public_ip.sh",
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

	expectedWan := "wan-" + sanitizeName(claim.Name)
	scriptCalled := false
	ensured := false

	r.runRouterScriptFn = func(ctx context.Context, claim *networkv1alpha1.PublicIPClaim, wanIf, macAddr string) (string, error) {
		scriptCalled = true
		if wanIf != expectedWan {
			t.Fatalf("unexpected WAN interface: %s", wanIf)
		}
		if macAddr == "" {
			t.Fatalf("expected MAC address to be generated")
		}
		return assignedTestIP, nil
	}
	r.ensureIPInPoolFn = func(ctx context.Context, poolName, ip string) error {
		ensured = true
		if poolName != claim.Spec.PoolName {
			t.Fatalf("unexpected pool name: %s", poolName)
		}
		if ip != assignedTestIP {
			t.Fatalf("unexpected ip: %s", ip)
		}
		return nil
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	// First reconcile adds finalizer
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}

	// Second reconcile performs allocation
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}

	if !scriptCalled {
		t.Fatalf("expected router script to be invoked")
	}
	if !ensured {
		t.Fatalf("expected ensureIPInPool to be invoked")
	}

	updated := &networkv1alpha1.PublicIPClaim{}
	if err := c.Get(ctx, req.NamespacedName, updated); err != nil {
		t.Fatalf("failed to get claim: %v", err)
	}

	if updated.Status.AssignedIP != assignedTestIP {
		t.Fatalf("unexpected assigned IP: %s", updated.Status.AssignedIP)
	}
	if updated.Status.Phase != networkv1alpha1.ClaimPhaseReady {
		t.Fatalf("expected phase Ready, got %s", updated.Status.Phase)
	}
	if updated.Status.WanInterface != expectedWan {
		t.Fatalf("unexpected WAN interface persisted: %s", updated.Status.WanInterface)
	}
	if updated.Status.MacAddress == "" {
		t.Fatalf("expected MAC address to be persisted")
	}
	if !updated.Status.ConfigurationVerified {
		t.Fatalf("expected configuration to be verified")
	}
	if updated.Status.LastVerified == nil {
		t.Fatalf("expected LastVerified timestamp to be set")
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, "Ready")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready condition true, got %#v", cond)
	}
}

func TestReconcileDeletion(t *testing.T) {
	scheme := runtimeScheme(t)
	deletionTime := metav1.NewTime(time.Now())
	claim := &networkv1alpha1.PublicIPClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "delete-claim",
			Namespace:         "default",
			Finalizers:        []string{finalizerName},
			DeletionTimestamp: &deletionTime,
		},
		Spec: networkv1alpha1.PublicIPClaimSpec{
			PoolName: "public-pool",
			Router: networkv1alpha1.RouterSpec{
				Host:         "router.local",
				User:         "admin",
				SSHSecretRef: "router-key",
				Command:      "/usr/local/bin/alloc_public_ip.sh",
				WanParent:    "eth0",
			},
		},
		Status: networkv1alpha1.PublicIPClaimStatus{
			AssignedIP:   "198.51.100.25",
			WanInterface: "wan-delete",
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

	cleanupCalled := false
	removed := false
	r.cleanupRouterInterfaceFn = func(ctx context.Context, claim *networkv1alpha1.PublicIPClaim) error {
		cleanupCalled = true
		if claim.Status.WanInterface != "wan-delete" {
			t.Fatalf("unexpected WAN interface during cleanup: %s", claim.Status.WanInterface)
		}
		return nil
	}
	r.removeIPFromPoolFn = func(ctx context.Context, poolName, ip string) error {
		removed = true
		if ip != "198.51.100.25" {
			t.Fatalf("unexpected IP removal: %s", ip)
		}
		return nil
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}}

	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	if !cleanupCalled {
		t.Fatalf("expected cleanup to be called")
	}
	if !removed {
		t.Fatalf("expected IP removal to be called")
	}

	updated := &networkv1alpha1.PublicIPClaim{}
	if err := c.Get(ctx, req.NamespacedName, updated); err != nil {
		if apierrors.IsNotFound(err) {
			return // claim fully deleted after finalizer removal
		}
		t.Fatalf("failed to get claim: %v", err)
	}
	if containsString(updated.Finalizers, finalizerName) {
		t.Fatalf("expected finalizer to be removed")
	}
}

func runtimeScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := networkv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add core scheme: %v", err)
	}
	return scheme
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
