/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1alpha1

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// TestAddToSchemeRegistersMetaOptions guards a regression that only surfaces
// against a real API server: AddToScheme must wire up metav1.AddToGroupVersion
// for SchemeGroupVersion, or the apiserver rejects CreateOptions/GetOptions/...
// for our types with "... is not suitable for converting to
// keenetic.whitediver.com/v1alpha1" — envtest caught this in CI when a
// hand-rolled runtime.SchemeBuilder was used instead of
// controller-runtime/pkg/scheme.Builder.
func TestAddToSchemeRegistersMetaOptions(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if _, err := s.New(SchemeGroupVersion.WithKind("CreateOptions")); err != nil {
		t.Errorf("scheme doesn't know CreateOptions for %s: %v (metav1.AddToGroupVersion missing?)", SchemeGroupVersion, err)
	}
}
