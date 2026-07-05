/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// KeeneticHostRecordSpec — желаемая запись `ip host` на роутере.
type KeeneticHostRecordSpec struct {
	// Hostname — FQDN, который надо зарегистрировать,
	// например grafana.whitediver.keenetic.link
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`
	Hostname string `json:"hostname"`

	// Address — IPv4, в который резолвится Hostname (обычно LB-адрес ingress).
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)$`
	Address string `json:"address"`
}

// KeeneticHostRecordStatus — наблюдаемое состояние.
type KeeneticHostRecordStatus struct {
	// Applied — присутствует ли запись на роутере прямо сейчас.
	Applied bool `json:"applied,omitempty"`

	// ObservedGeneration — поколение spec, на котором последний раз сошлись.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions — стандартный k8s-паттерн (тип Ready и т.п.).
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.spec.hostname`
//+kubebuilder:printcolumn:name="Address",type=string,JSONPath=`.spec.address`
//+kubebuilder:printcolumn:name="Applied",type=boolean,JSONPath=`.status.applied`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KeeneticHostRecord — одна статическая DNS-запись на Keenetic.
type KeeneticHostRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KeeneticHostRecordSpec   `json:"spec,omitempty"`
	Status KeeneticHostRecordStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KeeneticHostRecordList — список.
type KeeneticHostRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KeeneticHostRecord `json:"items"`
}

func init() {
	// v4.15: SchemeBuilder — это runtime.SchemeBuilder, регистрируем типы функцией.
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(SchemeGroupVersion, &KeeneticHostRecord{}, &KeeneticHostRecordList{})
		return nil
	})
}
