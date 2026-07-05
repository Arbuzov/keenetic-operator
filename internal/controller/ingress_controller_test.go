/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

var _ = Describe("Ingress controller", func() {
	It("creates an owned KeeneticHostRecord per host and cleans up hosts removed from the spec", func() {
		pathType := networkingv1.PathTypePrefix
		ing := &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: networkingv1.IngressSpec{
				Rules: []networkingv1.IngressRule{{
					Host: "web.example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{{
								Path:     "/",
								PathType: &pathType,
								Backend: networkingv1.IngressBackend{
									Service: &networkingv1.IngressServiceBackend{
										Name: "web",
										Port: networkingv1.ServiceBackendPort{Number: 80},
									},
								},
							}},
						},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, ing)).To(Succeed())

		ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: "192.168.99.60"}}
		Expect(k8sClient.Status().Update(ctx, ing)).To(Succeed())

		recKey := types.NamespacedName{Name: "web.example.com", Namespace: "default"}
		Eventually(func() error {
			var rec keeneticv1alpha1.KeeneticHostRecord
			return k8sClient.Get(ctx, recKey, &rec)
		}).Should(Succeed())

		var rec keeneticv1alpha1.KeeneticHostRecord
		Expect(k8sClient.Get(ctx, recKey, &rec)).To(Succeed())
		Expect(rec.Spec.Address).To(Equal("192.168.99.60"))
		Expect(metav1.IsControlledBy(&rec, ing)).To(BeTrue())

		// Drop the host from the Ingress spec: our own cleanup loop (not GC, which
		// doesn't run under envtest) should delete the now-unwanted owned record.
		ing.Spec.Rules = nil
		Expect(k8sClient.Update(ctx, ing)).To(Succeed())

		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticHostRecord
			err := k8sClient.Get(ctx, recKey, &got)
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
	})
})
