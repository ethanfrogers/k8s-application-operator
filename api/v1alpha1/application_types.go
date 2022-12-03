/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"time"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

type Artifact struct {
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	Repository string               `json:"repository,omitempty"`
	Version    string               `json:"version,omitempty"`
	Values     runtime.RawExtension `json:"values,omitempty"`
}

type Constraint struct {
	Kind string `json:"kind"`

	DependsOn  *DependsOnConstraint  `json:"dependsOn,omitempty"`
	HelmCanary *HelmCanaryConstraint `json:"helmCanary,omitempty"`
}

type DependsOnConstraint struct {
	EnvironmentName string   `json:"environment"`
	Artifacts       []string `json:"artifacts,omitempty"`
}

type HelmCanaryConstraint struct {
	TTL    string               `json:"ttl,omitempty"`
	Values runtime.RawExtension `json:"values,omitempty"`
}

type StaticPlacement struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
}

type DynamicPlacement struct {
	Selector map[string]string `json:"selector"`
}

type Placement struct {
	StaticPlacement  *StaticPlacement  `json:"staticPlacement,omitempty"`
	DynamicPlacement *DynamicPlacement `json:"dynamicPlacement,omitempty"`
}

type Verify struct {
	Wait *time.Duration `json:"wait,omitempty"`
}

type PostDeploymentHooks struct {
	Verify Verify `json:"verify,omitempty"`
}

type Lifecycle struct {
	PostDeployment PostDeploymentHooks `json:"postDeployment,omitempty"`
	OnFailure      string              `json:"onFailure,omitempty"`
}

type Environment struct {
	Name              string       `json:"name"`
	RequiredArtifacts []string     `json:"requiredArtifacts"`
	Constraints       []Constraint `json:"constraints,omitempty"`
	Placement         *Placement   `json:"placement,omitempty"`
	Lifecycle         Lifecycle    `json:"lifecycle,omitempty"`
}

// ApplicationSpec defines the desired state of Application
type ApplicationSpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	Artifacts    []Artifact    `json:"artifacts"`
	Environments []Environment `json:"environments"`
}

// ApplicationStatus defines the observed state of Application
type ApplicationStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	Conditions              []metav1.Condition             `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
	ControllerWorkflowID    string                         `json:"controllerWorkflowID"`
	ControllerWorkflowRunID string                         `json:"controllerWorkflowRunID"`
	DeployedArtifacts       map[string]map[string]Artifact `json:"deployedArtifacts,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+genclient

// Application is the Schema for the applications API
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ApplicationSpec   `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ApplicationList contains a list of Application
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Application{}, &ApplicationList{})
}
