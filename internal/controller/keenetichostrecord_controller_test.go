/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

var _ = Describe("KeeneticHostRecord controller", func() {
	It("applies the record to the router and marks it Ready", func() {
		rec := &keeneticv1alpha1.KeeneticHostRecord{
			ObjectMeta: metav1.ObjectMeta{Name: "nas.example.com", Namespace: "default"},
			Spec: keeneticv1alpha1.KeeneticHostRecordSpec{
				Hostname: "nas.example.com",
				Address:  "192.168.99.44",
			},
		}
		Expect(k8sClient.Create(ctx, rec)).To(Succeed())

		key := types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace}
		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticHostRecord
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return false
			}
			return got.Status.Applied
		}).Should(BeTrue())

		var got keeneticv1alpha1.KeeneticHostRecord
		Expect(k8sClient.Get(ctx, key, &got)).To(Succeed())
		cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Finalizers).To(ContainElement(hostRecordFinalizer))
	})

	It("removes the finalizer and the router entry on delete", func() {
		rec := &keeneticv1alpha1.KeeneticHostRecord{
			ObjectMeta: metav1.ObjectMeta{Name: "printer.example.com", Namespace: "default"},
			Spec: keeneticv1alpha1.KeeneticHostRecordSpec{
				Hostname: "printer.example.com",
				Address:  "192.168.99.55",
			},
		}
		Expect(k8sClient.Create(ctx, rec)).To(Succeed())

		key := types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace}
		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticHostRecord
			if err := k8sClient.Get(ctx, key, &got); err != nil {
				return false
			}
			return got.Status.Applied
		}).Should(BeTrue())

		Expect(k8sClient.Delete(ctx, rec)).To(Succeed())

		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticHostRecord
			err := k8sClient.Get(ctx, key, &got)
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
	})
})
