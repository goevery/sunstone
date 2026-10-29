package host

// Stage identifies one step of a workload operation.
type Stage string

const (
	// StageConnecting reports that orchestration is connecting to a VM.
	StageConnecting Stage = "connecting"
	// StagePulling reports that the desired image is being pulled.
	StagePulling Stage = "pulling"
	// StageCurrent reports that the deployed configuration is already
	// current and healthy.
	StageCurrent Stage = "current"
	// StageStarting reports that a replacement container is starting.
	StageStarting Stage = "starting"
	// StageRouting reports that traffic is being switched to the new
	// backend.
	StageRouting Stage = "routing"
	// StageDrained reports that the previous container no longer receives
	// traffic.
	StageDrained Stage = "drained"
	// StageHealthy reports that the operation reached a healthy state.
	StageHealthy Stage = "healthy"
	// StageRestarted reports that a workload was restarted.
	StageRestarted Stage = "restarted"
	// StageRemoved reports that a workload was removed from a VM.
	StageRemoved Stage = "removed"
)

// Progress is one fact about a workload operation on one VM. It carries
// only nonsecret data needed to render feedback; Stage is always set, and
// Image is set only for StagePulling. The per-VM host emits Stage (and
// Image); the orchestrating group fills in Workload and Instance.
type Progress struct {
	Stage    Stage
	Image    string
	Workload string
	Instance string
}

// Reporter receives progress synchronously during an operation. It must
// return promptly, must not return operational errors or influence the
// lifecycle, and cannot cancel the operation; cancel through the context.
type Reporter func(Progress)
