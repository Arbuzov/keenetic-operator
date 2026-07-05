/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package v1alpha1 contains API Schema definitions for the keenetic v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=keenetic.whitediver.com
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion — группа/версия, под которой регистрируются типы этого пакета.
	SchemeGroupVersion = schema.GroupVersion{Group: "keenetic.whitediver.com", Version: "v1alpha1"}

	// SchemeBuilder собирает funcs, добавляющие типы в scheme.
	SchemeBuilder = &runtime.SchemeBuilder{}

	// AddToScheme добавляет типы этой группы/версии в переданную scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
