/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/keenetic"
)

var (
	cfg           *rest.Config
	k8sClient     client.Client
	testEnv       *envtest.Environment
	ctx           context.Context
	cancel        context.CancelFunc
	routerWebApps *fakeWebApps
)

func TestControllers(t *testing.T) {
	// Gomega's bare Eventually() default (1s/10ms) is too tight for a full
	// watch -> reconcile -> object-visible round trip under a loaded CI
	// runner — observed as a real flake (Timed out after 1.000s) on
	// GitHub Actions even though the reconcile itself was correct.
	SetDefaultEventuallyTimeout(5 * time.Second)
	SetDefaultEventuallyPollingInterval(100 * time.Millisecond)

	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(keeneticv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	Expect(err).NotTo(HaveOccurred())

	Expect((&KeeneticHostRecordReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Keenetic: newFakeKeenetic(),
		MaxHosts: 64,
	}).SetupWithManager(mgr)).To(Succeed())

	// Держим ссылку в пакетной переменной: спеки проверяют не только объекты в
	// API, но и то, что реально доехало до «роутера» — без этого не отличить
	// «запись не тронули» от «запись снесли и завели заново».
	routerWebApps = newFakeWebApps()
	Expect((&KeeneticWebAppReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Keenetic: routerWebApps,
	}).SetupWithManager(mgr)).To(Succeed())

	Expect((&IngressReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		// Публикация включена: без DefaultUpstreamAddress вся ветка web app
		// молча выключена, и специи по ней проходили бы, ничего не проверяя.
		DefaultUpstreamAddress: "192.168.99.44",
		DefaultUpstreamPort:    80,
		DefaultUpstreamScheme:  "http",
		DefaultSecurityLevel:   "public",
		PublishByDefault:       true,
	}).SetupWithManager(mgr)).To(Succeed())

	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// fakeKeenetic — in-memory stand-in for the SSH client, keyed by hostname.
type fakeKeenetic struct {
	hosts map[string]string
}

func newFakeKeenetic() *fakeKeenetic {
	return &fakeKeenetic{hosts: map[string]string{}}
}

func (f *fakeKeenetic) EnsureHost(_ context.Context, host, ip string) error {
	f.hosts[host] = ip
	return nil
}

func (f *fakeKeenetic) DeleteHost(_ context.Context, host, _ string) error {
	delete(f.hosts, host)
	return nil
}

func (f *fakeKeenetic) HasHost(_ context.Context, host, ip string) (bool, error) {
	return f.hosts[host] == ip, nil
}

func (f *fakeKeenetic) CountHosts(_ context.Context) (int, error) {
	return len(f.hosts), nil
}

// fakeWebApps — in-memory stand-in for the router's `ip http proxy` table,
// keyed by entry name (which is how the router keys it too).
type fakeWebApps struct {
	proxies map[string]keenetic.Proxy
}

func newFakeWebApps() *fakeWebApps {
	return &fakeWebApps{proxies: map[string]keenetic.Proxy{}}
}

func (f *fakeWebApps) EnsureProxy(_ context.Context, p keenetic.Proxy) error {
	f.proxies[p.Name] = p
	return nil
}

func (f *fakeWebApps) DeleteProxy(_ context.Context, name string) error {
	delete(f.proxies, name)
	return nil
}

func (f *fakeWebApps) CountProxies(_ context.Context) (int, error) {
	return len(f.proxies), nil
}
