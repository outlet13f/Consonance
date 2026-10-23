package v1alpha1

// Condition reasons the operator writes. They are part of the API: the API server matches on
// them to tell a client what went wrong, so their values never change.

// Reasons of a Branch's DataReady=False. The first four are permanent (spec 10: phase Failed,
// the reason recorded); the rest are retried.
const (
	ReasonReflinkUnsupported  = "ReflinkUnsupported"
	ReasonInsufficientStorage = "InsufficientStorage"
	ReasonSourceNotFound      = "SourceNotFound"
	ReasonInvalid             = "Invalid"
	ReasonParentNotRunning    = "ParentNotRunning"
	ReasonCopyFailed          = "CopyFailed"
	ReasonInitMainFailed      = "InitMainFailed"
)

// Reasons of a Branch's Ready=False. When Ready is true the reason is the phase name (Running,
// Stopped).
const (
	ReasonSupervisorUnavailable = "SupervisorUnavailable"
	ReasonStartFailed           = "StartFailed"
	ReasonUnschedulable         = "Unschedulable" // a Project's AgentReady uses it too
	ReasonPodCreateFailed       = "PodCreateFailed"
)

// Reasons of a Project's conditions.
const (
	ReasonAgentUnavailable = "AgentUnavailable"
	ReasonAgentPodFailed   = "AgentPodFailed"
	ReasonMainNotRunning   = "MainNotRunning"
	ReasonInvalidNamespace = "InvalidNamespace"
	ReasonBootstrapFailed  = "BootstrapFailed"
)

// PermanentDataFailure reports a DataReady reason that retrying cannot fix (spec 10).
func PermanentDataFailure(reason string) bool {
	switch reason {
	case ReasonReflinkUnsupported, ReasonInsufficientStorage, ReasonSourceNotFound, ReasonInvalid:
		return true
	}
	return false
}
