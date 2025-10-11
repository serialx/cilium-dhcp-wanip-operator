/*
Copyright 2025.

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
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// RouterSpec defines the router configuration for allocating public IPs
type RouterSpec struct {
	// Host is the router's IP address or hostname
	// +kubebuilder:validation:Required
	Host string `json:"host"`

	// Port is the SSH port (defaults to 22)
	// +kubebuilder:default=22
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port int `json:"port,omitempty"`

	// User is the SSH username
	// +kubebuilder:validation:Required
	User string `json:"user"`

	// SSHSecretRef is the name of the secret containing the SSH private key (key: id_rsa)
	// +kubebuilder:validation:Required
	SSHSecretRef string `json:"sshSecretRef"`

	// Command is the router script to run (prints only the public IP)
	// +kubebuilder:default="/usr/local/bin/alloc_public_ip.sh"
	// +optional
	Command string `json:"command,omitempty"`

	// WanParent is the physical WAN interface (e.g., eth9, eth0)
	// +kubebuilder:validation:Required
	WanParent string `json:"wanParent"`

	// WanInterface is the macvlan interface name (auto-generated from claim name if omitted)
	// +kubebuilder:validation:MaxLength=15
	// +kubebuilder:validation:Pattern="^[a-z0-9-]+$"
	// +optional
	WanInterface string `json:"wanInterface,omitempty"`

	// MacAddress is the MAC address for DHCP (auto-generated if omitted)
	// +kubebuilder:validation:Pattern="^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$"
	// +optional
	MacAddress string `json:"macAddress,omitempty"`
}

// ClaimPhase represents the current phase of a PublicIPClaim
type ClaimPhase string

const (
	ClaimPhasePending      ClaimPhase = "Pending"
	ClaimPhaseProvisioning ClaimPhase = "Provisioning"
	ClaimPhaseReady        ClaimPhase = "Ready"
	ClaimPhaseFailed       ClaimPhase = "Failed"
)

// Condition types for PublicIPClaim
const (
	// ConditionReady indicates the claim is ready and configuration is verified
	ConditionReady = "Ready"

	// ConditionConfigurationDrifted indicates router state doesn't match desired state
	ConditionConfigurationDrifted = "ConfigurationDrifted"

	// ConditionRecovering indicates operator is reapplying configuration
	ConditionRecovering = "Recovering"
)

// PublicIPClaimSpec defines the desired state of PublicIPClaim
type PublicIPClaimSpec struct {
	// PoolName is the name of the CiliumLoadBalancerIPPool to update
	// +kubebuilder:validation:Required
	PoolName string `json:"poolName"`

	// Router contains the router configuration
	// +kubebuilder:validation:Required
	Router RouterSpec `json:"router"`
}

// PublicIPClaimStatus defines the observed state of PublicIPClaim.
type PublicIPClaimStatus struct {
	// Phase is the current phase of the claim
	// +kubebuilder:validation:Enum=Pending;Provisioning;Ready;Failed
	// +optional
	Phase ClaimPhase `json:"phase,omitempty"`

	// Message provides additional information about the current phase
	// +optional
	Message string `json:"message,omitempty"`

	// AssignedIP is the public IP address that was allocated
	// +optional
	AssignedIP string `json:"assignedIP,omitempty"`

	// WanInterface is the actual interface name created on the router
	// +optional
	WanInterface string `json:"wanInterface,omitempty"`

	// MacAddress is the actual MAC address used for DHCP
	// +optional
	MacAddress string `json:"macAddress,omitempty"`

	// RouterUptime is the router uptime in seconds at last check (used to detect reboots)
	// +optional
	RouterUptime int64 `json:"routerUptime,omitempty"`

	// LastVerified is the timestamp when configuration was last verified on the router
	// +optional
	LastVerified *metav1.Time `json:"lastVerified,omitempty"`

	// ConfigurationVerified indicates whether current configuration has been verified on router
	// +optional
	ConfigurationVerified bool `json:"configurationVerified,omitempty"`

	// LastReconciliationReason indicates the reason for the last reconciliation
	// Possible values: router_reboot, interface_missing, dhcp_client_stopped, proxy_arp_disabled,
	// periodic_check, verified, configuration_applied
	// +optional
	LastReconciliationReason string `json:"lastReconciliationReason,omitempty"`

	// ConnectionState indicates the SSH connection state to the router
	// Possible values: connected, reconnecting, disconnected
	// +optional
	ConnectionState string `json:"connectionState,omitempty"`

	// Conditions represent the latest available observations of the claim's state
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pic;pics
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.status.assignedIP`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Connection",type=string,JSONPath=`.status.connectionState`,priority=1
// +kubebuilder:printcolumn:name="Last Verified",type=date,JSONPath=`.status.lastVerified`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PublicIPClaim is the Schema for the publicipclaims API
type PublicIPClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of PublicIPClaim
	// +required
	Spec PublicIPClaimSpec `json:"spec"`

	// status defines the observed state of PublicIPClaim
	// +optional
	Status PublicIPClaimStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// PublicIPClaimList contains a list of PublicIPClaim
type PublicIPClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PublicIPClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PublicIPClaim{}, &PublicIPClaimList{})
}
