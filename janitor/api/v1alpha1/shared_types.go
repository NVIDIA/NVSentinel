package v1alpha1

// Condition represents the state of a janitor operation.
type Condition string

const (
	// ConditionComplete indicates the operation has completed successfully.
	ConditionComplete Condition = "Complete"
	// ConditionFailed indicates the operation has failed.
	ConditionFailed Condition = "Failed"
	// ConditionReported indicates the operation has been reported to an external system.
	ConditionReported Condition = "Reported"
)

// Reason provides more detail about why a condition occurred.
type Reason string

const (
	// ReasonCreatedResource indicates a Kubernetes resource was created.
	ReasonCreatedResource Reason = "CreatedResource"
	// ReasonNodeMarkedFaulty indicates a node has been marked as faulty.
	ReasonNodeMarkedFaulty Reason = "NodeMarkedFaulty"
	// ReasonNodeReportedFaulty indicates a node has been reported as faulty to an external system.
	ReasonNodeReportedFaulty Reason = "NodeReportedFaulty"
	// ReasonNodeReportingFailed indicates reporting a node fault to an external system failed.
	ReasonNodeReportingFailed Reason = "NodeReportingFailed"
	// ReasonNotFound indicates a resource was not found.
	ReasonNotFound Reason = "NotFound"
	// ReasonNodeNotFound indicates a node was not found.
	ReasonNodeNotFound Reason = "NodeNotFound"
	// ReasonPending indicates an operation is pending.
	ReasonPending Reason = "Pending"
	// ReasonSupportCaseOpened indicates a support case was opened.
	ReasonSupportCaseOpened Reason = "SupportCaseOpened"
	// ReasonTimeoutExceeded indicates an operation timed out.
	ReasonTimeoutExceeded Reason = "TimeoutExceeded"
	// ReasonUpdateFailed indicates updating a resource failed.
	ReasonUpdateFailed Reason = "UpdateFailed"
	// ReasonWaitingForConfirmation indicates an operation is waiting for confirmation.
	ReasonWaitingForConfirmation Reason = "WaitingForConfirmation"
)

// NodeRepairAction specifies the type of repair action to take on a node.
type NodeRepairAction string

const (
	// NodeRepairActionRebootNode indicates the node should be rebooted.
	NodeRepairActionRebootNode = NodeRepairAction("RebootNode")
	// NodeRepairActionTerminateNode indicates the node should be terminated.
	NodeRepairActionTerminateNode = NodeRepairAction("TerminateNode")
)

// NodeRepairStatus represents the current status of a node repair operation.
type NodeRepairStatus string

const (
	// NodeRepairStatusUnknown indicates the repair status is unknown.
	NodeRepairStatusUnknown = NodeRepairStatus("Unknown")
	// NodeRepairStatusInProgress indicates the repair is currently in progress.
	NodeRepairStatusInProgress = NodeRepairStatus("InProgress")
	// NodeRepairStatusSuccess indicates the repair completed successfully.
	NodeRepairStatusSuccess = NodeRepairStatus("Success")
	// NodeRepairStatusFailed indicates the repair failed.
	NodeRepairStatusFailed = NodeRepairStatus("Failed")
)
