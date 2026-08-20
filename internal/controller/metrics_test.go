/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/metrics"
)

// testMaxHosts mirrors the router's real `ip host` ceiling, so a call passing it
// as routerHosts reads as "the router is full".
const testMaxHosts = 64

// newRecordReconciler wires a reconciler over a fake API client and a fake
// router — same shape as TestMaxHostsGuard, no envtest needed.
func newRecordReconciler(t *testing.T, routerHosts int) (*KeeneticHostRecordReconciler, *keeneticv1alpha1.KeeneticHostRecord) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := keeneticv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	rec := &keeneticv1alpha1.KeeneticHostRecord{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "new-host.example.com",
			Finalizers: []string{hostRecordFinalizer},
		},
		Spec: keeneticv1alpha1.KeeneticHostRecordSpec{
			Hostname: "new-host.example.com",
			Address:  "192.168.99.99",
		},
	}

	router := newFakeKeenetic()
	for i := range routerHosts {
		router.hosts[fmt.Sprintf("existing-%d.example.com", i)] = "10.0.0.1"
	}

	return &KeeneticHostRecordReconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(rec).WithStatusSubresource(rec).Build(),
		Scheme:   scheme,
		Keenetic: router,
		MaxHosts: testMaxHosts,
	}, rec
}

// The gauge publishes what CountHosts observed, with no arithmetic layered on
// top, so nothing can accumulate into a value the router never reported. The
// record applied by this very reconcile is therefore not counted yet — it shows
// up on the next pass.
func TestRouterHostsGaugePublishesTheObservedCount(t *testing.T) {
	metrics.RouterHosts.Set(0)
	rejectedBefore := testutil.ToFloat64(metrics.HostRecordsLimitRejected)

	r, rec := newRecordReconciler(t, 5)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: rec.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got := testutil.ToFloat64(metrics.RouterHosts); got != 5 {
		t.Errorf("keenetic_router_hosts = %v, want 5 (the count read from the router this pass)", got)
	}
	// The other half of the limit counter's contract: it must stay put with room
	// to spare, or an alert on it would fire on a perfectly healthy router.
	if got := testutil.ToFloat64(metrics.HostRecordsLimitRejected) - rejectedBefore; got != 0 {
		t.Errorf("keenetic_host_records_limit_rejected_total delta = %v, want 0 well below the cap", got)
	}
}

// The limit path returns a nil error on purpose (it requeues instead), so it is
// invisible in controller_runtime_reconcile_errors_total. This counter is the
// only signal that records are being silently dropped on the floor.
func TestLimitRejectedCounterFiresOnFullRouter(t *testing.T) {
	metrics.RouterHosts.Set(0)

	r, rec := newRecordReconciler(t, testMaxHosts)

	// A counter cannot be reset, so pin the delta rather than the absolute value.
	before := testutil.ToFloat64(metrics.HostRecordsLimitRejected)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: rec.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if got := testutil.ToFloat64(metrics.HostRecordsLimitRejected) - before; got != 1 {
		t.Errorf("keenetic_host_records_limit_rejected_total delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.RouterHosts); got != 64 {
		t.Errorf("keenetic_router_hosts = %v, want 64 (nothing was added)", got)
	}
}
