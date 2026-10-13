package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MainBranch is the branch every project starts with. It has no parent.
const MainBranch = "main"

// SourceType says what a branch's data is copied from (spec 4.3).
type SourceType string

const (
	// SourceParentLatest is the parent's running database, copied in backup mode.
	SourceParentLatest SourceType = "ParentLatest"
	// SourceRestorePoint is a restore point.
	SourceRestorePoint SourceType = "RestorePoint"
)

// BranchSource is where a branch's data, or the data of a rollback or reset, comes from.
//
// +kubebuilder:validation:XValidation:rule="self.type != 'RestorePoint' || has(self.restorePointId)",message="a RestorePoint source needs restorePointId"
type BranchSource struct {
	// +optional
	// +kubebuilder:default=ParentLatest
	// +kubebuilder:validation:Enum=ParentLatest;RestorePoint
	Type SourceType `json:"type,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^rp-[0-9]{8}T[0-9]{6}Z-[a-z0-9]{4}$`
	RestorePointID string `json:"restorePointId,omitempty"`
}

// DesiredState is the PostgreSQL process state a branch should be in (spec 4.3).
type DesiredState string

const (
	StateRunning DesiredState = "Running"
	StateStopped DesiredState = "Stopped"
)

// BranchSpec is the branch the API server asks for (spec 4.3).
//
// +kubebuilder:validation:XValidation:rule="has(self.parent) == has(oldSelf.parent) && (!has(self.parent) || self.parent == oldSelf.parent)",message="parent is immutable"
// +kubebuilder:validation:XValidation:rule="self.source == oldSelf.source",message="source is immutable"
type BranchSpec struct {
	// Parent is the branch this one was copied from. main has none.
	// +optional
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Parent string `json:"parent,omitempty"`

	// +optional
	// +kubebuilder:default={type: ParentLatest}
	Source BranchSource `json:"source,omitzero"`

	// Protected marks main and other branches that need approval to change (spec 1). The API
	// server enforces it.
	// +optional
	Protected bool `json:"protected,omitempty"`

	// +optional
	// +kubebuilder:default=Running
	// +kubebuilder:validation:Enum=Running;Stopped
	State DesiredState `json:"state,omitempty"`

	// Compute overrides the project's defaultCompute.
	// +optional
	Compute *ComputeSpec `json:"compute,omitempty"`

	// Restore asks for a rollback (source RestorePoint) or a reset (source ParentLatest). The
	// operator carries out each requestId once (spec 9.4).
	// +optional
	Restore *RestoreRequest `json:"restore,omitempty"`
}

// RestoreRequest is one rollback or reset.
type RestoreRequest struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	RequestID string `json:"requestId"`

	Source BranchSource `json:"source"`
}

// BranchPhase is where a branch is (spec 4.3).
type BranchPhase string

const (
	BranchCreating  BranchPhase = "Creating"
	BranchDataReady BranchPhase = "DataReady"
	BranchStarting  BranchPhase = "Starting"
	BranchRunning   BranchPhase = "Running"
	BranchStopping  BranchPhase = "Stopping"
	BranchStopped   BranchPhase = "Stopped"
	BranchRestoring BranchPhase = "Restoring"
	BranchFailed    BranchPhase = "Failed"
	BranchDeleting  BranchPhase = "Deleting"
)

// Branch condition types: DataReady once the branch has data, and Ready, which says whether
// the branch is in its desired state and why not.
const (
	ConditionDataReady = "DataReady"
)

// RestorePhase is the step a rollback or reset is at (spec 9.4).
type RestorePhase string

const (
	RestorePreparing  RestorePhase = "Preparing"
	RestoreStopping   RestorePhase = "Stopping"
	RestoreActivating RestorePhase = "Activating"
	RestoreStarting   RestorePhase = "Starting"
	RestoreDetaching  RestorePhase = "Detaching"
	RestoreDone       RestorePhase = "Done"
	RestoreFailed     RestorePhase = "Failed"
)

// BranchStatus is what the operator reports (spec 4.3).
type BranchStatus struct {
	// +optional
	Phase BranchPhase `json:"phase,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// DataGeneration is the data-N directory the branch runs on.
	// +optional
	DataGeneration int32 `json:"dataGeneration,omitempty"`

	// +optional
	CreatedFrom *CreatedFrom `json:"createdFrom,omitempty"`

	// +optional
	Endpoint *Endpoint `json:"endpoint,omitempty"`

	// +optional
	LastRestore *RestoreStatus `json:"lastRestore,omitempty"`

	// +optional
	Timings Timings `json:"timings,omitzero"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CreatedFrom is where a branch's data came from.
type CreatedFrom struct {
	// +optional
	Branch string `json:"branch,omitempty"`
	// +optional
	RestorePointID string `json:"restorePointId,omitempty"`
	// BackupStartLSN is the start of the backup the copy was made in. It is empty when the
	// copy came from a restore point, or when a retried request found the data made already.
	// +optional
	BackupStartLSN string `json:"backupStartLSN,omitempty"`
}

// Endpoint is where clients reach the branch's PostgreSQL.
type Endpoint struct {
	Host string `json:"host"`
	Port int32  `json:"port"`
}

// RestoreStatus is the last rollback or reset (spec 4.3). Source, the generations, Reverting and
// the times are not in the spec: the operator keeps them to resume after a restart (spec 10).
type RestoreStatus struct {
	// +optional
	RequestID string `json:"requestId,omitempty"`
	// +optional
	Phase RestorePhase `json:"phase,omitempty"`
	// BackupBranch holds the generation a rollback replaced.
	// +optional
	BackupBranch string `json:"backupBranch,omitempty"`
	// RestorePointID holds the generation a reset replaced.
	// +optional
	RestorePointID string `json:"restorePointId,omitempty"`
	// +optional
	Source BranchSource `json:"source,omitzero"`
	// FromGeneration is the generation the branch ran before.
	// +optional
	FromGeneration int32 `json:"fromGeneration,omitempty"`
	// ToGeneration is the new generation, once made.
	// +optional
	ToGeneration int32 `json:"toGeneration,omitempty"`
	// Reverting is set when the new generation did not start and the branch goes back to
	// FromGeneration.
	// +optional
	Reverting bool `json:"reverting,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// Timings are first-time marks of a branch's life.
type Timings struct {
	// +optional
	DataReadyAt *metav1.Time `json:"dataReadyAt,omitempty"`
	// +optional
	RunningAt *metav1.Time `json:"runningAt,omitempty"`
}

// Branch is one branch of a project: a PostgreSQL data directory on the project volume and a
// pod that runs it (spec 4.3).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Parent",type=string,JSONPath=`.spec.parent`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.spec.state`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 40 && self.metadata.name.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')",message="a branch name is a DNS-1123 label of at most 40 characters"
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'main' ? !has(self.spec.parent) : has(self.spec.parent)",message="main has no parent and every other branch has one"
type Branch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BranchSpec `json:"spec"`

	// +optional
	Status BranchStatus `json:"status,omitzero"`
}

// BranchList is a list of branches.
//
// +kubebuilder:object:root=true
type BranchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Branch `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Branch{}, &BranchList{})
}
