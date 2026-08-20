/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/prometheus/client_golang/prometheus/testutil"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/metrics"
)

const (
	sharedHost = "dev.example.com"
	// The namespace that actually exhibits this in the cluster: four Ingresses,
	// one host.
	sharedNS = "mcp"
)

func ingressOnSharedHost(name, lbIP string) *networkingv1.Ingress {
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sharedNS},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: sharedHost}},
		},
	}
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: lbIP}}
	return ing
}

func newIngressReconciler(t *testing.T, objs ...client.Object) (*IngressReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keeneticv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(keenetic) error = %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme(networking) error = %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &IngressReconciler{Client: c, Scheme: scheme}, c
}

func reconcileIngress(t *testing.T, r *IngressReconciler, ing *networkingv1.Ingress) {
	t.Helper()
	key := types.NamespacedName{Name: ing.Name, Namespace: ing.Namespace}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile(%s) error = %v", ing.Name, err)
	}
}

// Several Ingresses in one namespace legitimately share a host — the real
// cluster runs four of them on dev.whitediver.keenetic.link in `mcp`. They all
// map to one record, so a *controller* reference would let the first Ingress
// claim it and leave every other one failing forever with AlreadyOwnedError.
// Plain owner references admit all of them.
func TestSharedHostGivesOneRecordManyOwners(t *testing.T) {
	first := ingressOnSharedHost("basic-memory", "192.168.99.44")
	second := ingressOnSharedHost("mcpo", "192.168.99.44")

	r, c := newIngressReconciler(t, first, second)
	reconcileIngress(t, r, first)
	reconcileIngress(t, r, second)

	var rec keeneticv1alpha1.KeeneticHostRecord
	key := types.NamespacedName{Name: sharedHost, Namespace: sharedNS}
	if err := c.Get(context.Background(), key, &rec); err != nil {
		t.Fatalf("Get(record) error = %v", err)
	}

	if got := len(rec.OwnerReferences); got != 2 {
		t.Errorf("ownerReferences = %d, want 2 (one per Ingress sharing the host)", got)
	}
	if controllerutil.HasControllerReference(&rec) {
		t.Error("record carries a controller reference; that is what locked out the second Ingress")
	}
	if rec.Spec.Address != "192.168.99.44" {
		t.Errorf("spec.address = %q, want the load balancer IP", rec.Spec.Address)
	}
}

// Dropping the host from one of the sharing Ingresses must not yank the record
// out from under the others — only that Ingress's claim goes away. The record,
// and therefore the router entry, survives until the last claimant is gone.
func TestRecordSurvivesUntilTheLastOwnerReleasesIt(t *testing.T) {
	first := ingressOnSharedHost("basic-memory", "192.168.99.44")
	second := ingressOnSharedHost("mcpo", "192.168.99.44")

	r, c := newIngressReconciler(t, first, second)
	reconcileIngress(t, r, first)
	reconcileIngress(t, r, second)

	ctx := context.Background()
	key := types.NamespacedName{Name: sharedHost, Namespace: sharedNS}

	// The first Ingress moves off the shared host.
	var live networkingv1.Ingress
	if err := c.Get(ctx, types.NamespacedName{Name: "basic-memory", Namespace: sharedNS}, &live); err != nil {
		t.Fatalf("Get(ingress) error = %v", err)
	}
	live.Spec.Rules[0].Host = "moved.example.com"
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("Update(ingress) error = %v", err)
	}
	reconcileIngress(t, r, &live)

	var rec keeneticv1alpha1.KeeneticHostRecord
	if err := c.Get(ctx, key, &rec); err != nil {
		t.Fatalf("shared record was deleted while another Ingress still wanted it: %v", err)
	}
	if got := len(rec.OwnerReferences); got != 1 {
		t.Errorf("ownerReferences = %d, want 1 (only the releasing Ingress dropped out)", got)
	}

	// Now the last claimant lets go too.
	if err := c.Get(ctx, types.NamespacedName{Name: "mcpo", Namespace: sharedNS}, &live); err != nil {
		t.Fatalf("Get(ingress) error = %v", err)
	}
	live.Spec.Rules[0].Host = "elsewhere.example.com"
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("Update(ingress) error = %v", err)
	}
	reconcileIngress(t, r, &live)

	if err := c.Get(ctx, key, &rec); !apierrors.IsNotFound(err) {
		t.Errorf("record still present after the last owner released it (err = %v)", err)
	}
}

// Two Ingresses claiming one host but different addresses is a misconfiguration
// with no right answer — a name cannot resolve to two IPs. What matters is that
// the operator refuses to arbitrate: with MatchEveryOwner, each rewrite of
// spec.address wakes the other owner, which rewrites it back, and every lap
// reaches the router as `ip host` + `system configuration save` — flash writes
// on real hardware, forever.
func TestConflictingAddressesLeaveTheRecordAlone(t *testing.T) {
	first := ingressOnSharedHost("basic-memory", "192.168.99.44")
	second := ingressOnSharedHost("mcpo", "192.168.99.77")

	// A counter cannot be reset, so pin the delta.
	before := testutil.ToFloat64(metrics.HostRecordsAddressConflict)

	r, c := newIngressReconciler(t, first, second)
	reconcileIngress(t, r, first)
	reconcileIngress(t, r, second)

	var rec keeneticv1alpha1.KeeneticHostRecord
	key := types.NamespacedName{Name: sharedHost, Namespace: sharedNS}
	if err := c.Get(context.Background(), key, &rec); !apierrors.IsNotFound(err) {
		t.Errorf("record = %+v (err = %v), want none while the owners disagree", rec.Spec, err)
	}
	if got := testutil.ToFloat64(metrics.HostRecordsAddressConflict) - before; got < 1 {
		t.Errorf("address-conflict counter delta = %v, want at least 1 — the refusal is silent otherwise", got)
	}
}

// Agreement must still be reached through the shared view, not just when a
// single Ingress owns the host: once both report the same address the record
// appears normally.
func TestAgreementAfterConflictProducesTheRecord(t *testing.T) {
	first := ingressOnSharedHost("basic-memory", "192.168.99.44")
	second := ingressOnSharedHost("mcpo", "192.168.99.77")

	r, c := newIngressReconciler(t, first, second)
	reconcileIngress(t, r, first)

	ctx := context.Background()
	var live networkingv1.Ingress
	if err := c.Get(ctx, types.NamespacedName{Name: "mcpo", Namespace: sharedNS}, &live); err != nil {
		t.Fatalf("Get(ingress) error = %v", err)
	}
	live.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.168.99.44"}}
	if err := c.Status().Update(ctx, &live); err != nil {
		t.Fatalf("Status().Update(ingress) error = %v", err)
	}
	reconcileIngress(t, r, &live)

	var rec keeneticv1alpha1.KeeneticHostRecord
	if err := c.Get(ctx, types.NamespacedName{Name: sharedHost, Namespace: sharedNS}, &rec); err != nil {
		t.Fatalf("record missing after the owners agreed: %v", err)
	}
	if rec.Spec.Address != "192.168.99.44" {
		t.Errorf("spec.address = %q, want the agreed address", rec.Spec.Address)
	}
}

// Refusing to arbitrate the address must not also refuse ownership. If the
// disagreeing Ingress never attaches to an existing record, the other owner
// moving away deletes it as the last claim — taking the router entry this
// Ingress still depends on, and the delete event would not even wake it.
func TestConflictStillAttachesOwnershipToAnExistingRecord(t *testing.T) {
	first := ingressOnSharedHost("basic-memory", "192.168.99.44")

	r, c := newIngressReconciler(t, first)
	reconcileIngress(t, r, first)

	ctx := context.Background()
	key := types.NamespacedName{Name: sharedHost, Namespace: sharedNS}

	var rec keeneticv1alpha1.KeeneticHostRecord
	if err := c.Get(ctx, key, &rec); err != nil {
		t.Fatalf("Get(record) error = %v", err)
	}
	if got := len(rec.OwnerReferences); got != 1 {
		t.Fatalf("ownerReferences = %d, want 1 before the second Ingress appears", got)
	}

	// A second Ingress claims the same host but reports a different address.
	second := ingressOnSharedHost("mcpo", "192.168.99.77")
	if err := c.Create(ctx, second); err != nil {
		t.Fatalf("Create(ingress) error = %v", err)
	}
	reconcileIngress(t, r, second)

	if err := c.Get(ctx, key, &rec); err != nil {
		t.Fatalf("Get(record) error = %v", err)
	}
	if got := len(rec.OwnerReferences); got != 2 {
		t.Errorf("ownerReferences = %d, want 2 — the conflicting Ingress must still be an owner", got)
	}
	if rec.Spec.Address != "192.168.99.44" {
		t.Errorf("spec.address = %q, want the address left untouched during the conflict", rec.Spec.Address)
	}
}
