/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"flag"
	"os"
	"strconv"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	keeneticv1alpha1 "github.com/Arbuzov/keenetic-operator/api/v1alpha1"
	"github.com/Arbuzov/keenetic-operator/internal/controller"
	"github.com/Arbuzov/keenetic-operator/internal/keenetic"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme)) // включает networking/v1 (Ingress)
	utilruntime.Must(keeneticv1alpha1.AddToScheme(scheme))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	var metricsAddr, probeAddr string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         true, // одна активная реплика → конфиг роутера правит один
		LeaderElectionID:       "keenetic-operator.whitediver.com",
	})
	if err != nil {
		setupLog.Error(err, "не удалось создать manager")
		os.Exit(1)
	}

	// SSH-клиент Keenetic — креды из Secret через env
	kc := &keenetic.Client{
		Host:               env("KEENETIC_HOST", "192.168.99.1:22"),
		User:               os.Getenv("KEENETIC_USER"),
		Password:           os.Getenv("KEENETIC_PASSWORD"),
		HostKeyFingerprint: os.Getenv("KEENETIC_HOST_KEY"),
	}
	if kc.HostKeyFingerprint == "" {
		setupLog.Info("KEENETIC_HOST_KEY не задан — проверка SSH host key роутера отключена (ок для LAN, для прода задайте фингерпринт)")
	}

	maxHostsEnv := env("KEENETIC_MAX_HOSTS", "64")
	maxHosts, err := strconv.Atoi(maxHostsEnv)
	if err != nil {
		setupLog.Error(err, "некорректный KEENETIC_MAX_HOSTS", "value", maxHostsEnv)
		os.Exit(1)
	}

	if err := (&controller.KeeneticHostRecordReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Keenetic: kc,
		MaxHosts: maxHosts,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "не удалось создать контроллер", "controller", "KeeneticHostRecord")
		os.Exit(1)
	}

	if err := (&controller.IngressReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		DefaultAddress: os.Getenv("DEFAULT_INGRESS_IP"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "не удалось создать контроллер", "controller", "Ingress")
		os.Exit(1)
	}

	_ = mgr.AddHealthzCheck("healthz", healthz.Ping)
	_ = mgr.AddReadyzCheck("readyz", healthz.Ping)

	setupLog.Info("запускаем manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager упал")
		os.Exit(1)
	}
}
