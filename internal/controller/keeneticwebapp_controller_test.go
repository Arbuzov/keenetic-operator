/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
)

func webApp(namespace, objName, entryName, domain string) *keeneticv1alpha1.KeeneticWebApp {
	return &keeneticv1alpha1.KeeneticWebApp{
		ObjectMeta: metav1.ObjectMeta{Name: objName, Namespace: namespace},
		Spec: keeneticv1alpha1.KeeneticWebAppSpec{
			Name:            entryName,
			Domain:          domain,
			UpstreamAddress: "192.168.99.44",
			UpstreamPort:    80,
			UpstreamScheme:  "http",
			SecurityLevel:   "public",
		},
	}
}

func mustCreateNamespace(name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil {
		Expect(err).To(MatchError(ContainSubstring("already exists")))
	}
}

var _ = Describe("KeeneticWebApp controller", func() {
	It("removes the previous router entry when the entry name changes", func() {
		app := webApp("default", "rename.example.link", "renameold", "rename.example.link")
		Expect(k8sClient.Create(ctx, app)).To(Succeed())

		Eventually(func() bool {
			_, ok := routerWebApps.proxies["renameold"]
			return ok
		}).Should(BeTrue())

		// The router keys an entry on its name and cannot tell us which of its
		// entries used to be ours, so a rename that only writes the new name
		// strands the old one on the router forever.
		Eventually(func() error {
			var got keeneticv1alpha1.KeeneticWebApp
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(app), &got); err != nil {
				return err
			}
			got.Spec.Name = "renamenew"
			return k8sClient.Update(ctx, &got)
		}).Should(Succeed())

		Eventually(func() bool {
			_, ok := routerWebApps.proxies["renamenew"]
			return ok
		}).Should(BeTrue())
		Eventually(func() bool {
			_, ok := routerWebApps.proxies["renameold"]
			return ok
		}).Should(BeFalse())
	})

	It("keeps the router entry when another publication still claims the same name", func() {
		// One host served from several namespaces is the normal case here, not
		// an edge one: every namespace gets its own object with the same entry
		// name. Deleting the first must not unpublish the rest.
		mustCreateNamespace("shared-a")
		mustCreateNamespace("shared-b")

		a := webApp("shared-a", "shared.example.link", "sharedentry", "shared.example.link")
		b := webApp("shared-b", "shared.example.link", "sharedentry", "shared.example.link")
		Expect(k8sClient.Create(ctx, a)).To(Succeed())
		Expect(k8sClient.Create(ctx, b)).To(Succeed())

		Eventually(func() bool {
			_, ok := routerWebApps.proxies["sharedentry"]
			return ok
		}).Should(BeTrue())

		Expect(k8sClient.Delete(ctx, a)).To(Succeed())
		Eventually(func() bool {
			var got keeneticv1alpha1.KeeneticWebApp
			return k8sClient.Get(ctx, client.ObjectKeyFromObject(a), &got) != nil
		}).Should(BeTrue())

		// b still wants it, so the entry stays.
		Consistently(func() bool {
			_, ok := routerWebApps.proxies["sharedentry"]
			return ok
		}).Should(BeTrue())
	})

	It("refuses to publish when the entry name is claimed for a different domain", func() {
		// Two different FQDNs whose first labels collide would otherwise
		// overwrite each other's entry on every reconcile — and every rewrite
		// is a `system configuration save`, i.e. a flash write.
		first := webApp("default", "clash-first.example.link", "clashentry", "clash.a.example.link")
		second := webApp("default", "clash-second.example.link", "clashentry", "clash.b.example.link")
		Expect(k8sClient.Create(ctx, first)).To(Succeed())

		Eventually(func() bool {
			_, ok := routerWebApps.proxies["clashentry"]
			return ok
		}).Should(BeTrue())

		Expect(k8sClient.Create(ctx, second)).To(Succeed())

		Eventually(func() string {
			var got keeneticv1alpha1.KeeneticWebApp
			if err := k8sClient.Get(ctx, types.NamespacedName{
				Name: second.Name, Namespace: second.Namespace,
			}, &got); err != nil {
				return ""
			}
			cond := apimeta.FindStatusCondition(got.Status.Conditions, "Ready")
			if cond == nil {
				return ""
			}
			return cond.Reason
		}).Should(Equal("NameConflict"))

		// The entry on the router keeps belonging to whoever had it first. The
		// router stores the zone, not the FQDN — it builds <name>.<zone> itself.
		Expect(routerWebApps.proxies["clashentry"].Zone).To(Equal("a.example.link"))
	})
})
