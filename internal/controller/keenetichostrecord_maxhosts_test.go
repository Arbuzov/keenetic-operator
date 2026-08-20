/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

// TestMaxHostsGuard exercises the 64-entry cap without spinning up envtest:
// a plain fake client is enough since the guard only touches the fake router state.
func TestMaxHostsGuard(t *testing.T) {
	r, rec := newRecordReconciler(t, testMaxHosts)

	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: rec.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var got keeneticv1alpha1.KeeneticHostRecord
	if err := r.Get(ctx, types.NamespacedName{Name: rec.Name}, &got); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status.Applied {
		t.Errorf("Status.Applied = true, want false when the router is at its host limit")
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "LimitReached" {
		t.Errorf("Ready condition = %+v, want False/LimitReached", cond)
	}
}
