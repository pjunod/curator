package ports

import "context"

// RunnerDispositionRequest binds destruction at the owner, independently of a
// numeric queue handle or Curator's observation of a terminal.
type RunnerDispositionRequest struct {
	RequestID        string `json:"requestId"`
	Action           string `json:"action"`
	Transfer         string `json:"transfer"`
	RunnerInstanceID string `json:"runnerInstanceId"`
	Generation       string `json:"generation"`
	SourceArtifactID string `json:"sourceArtifactId,omitempty"`
	SourceGeneration string `json:"sourceGeneration,omitempty"`
}
type RunnerDisposition struct {
	RequestID        string `json:"requestId"`
	JobID            string `json:"jobId"`
	Transfer         string `json:"transfer"`
	RunnerInstanceID string `json:"runnerInstanceId"`
	Generation       string `json:"generation"`
	State            string `json:"state"`
	Reason           string `json:"reason,omitempty"`
	OperationID      string `json:"operationId,omitempty"`
}
type RunnerDispositionProtocol interface {
	DisposeJob(context.Context, string, RunnerDispositionRequest) (RunnerDisposition, error)
}
