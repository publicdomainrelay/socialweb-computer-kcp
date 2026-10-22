package models

type ExecutionStatus string

const (
	ExecutionSubmitted            ExecutionStatus = "submitted"
	ExecutionInProgress           ExecutionStatus = "in_progress"
	ExecutionComplete             ExecutionStatus = "complete"
	ExecutionUnknown              ExecutionStatus = "unknown"
	ExecutionInputValidationError ExecutionStatus = "input_validation_error"
)

type ExitStatus string

const (
	ExitSuccess ExitStatus = "success"
	ExitFailure ExitStatus = "failure"
)
