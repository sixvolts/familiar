package sidecar

// Capabilities describes the GPU sidecar's hardware and loaded models.
type Capabilities struct {
	DeviceName             string     `json:"device_name"`
	VRAMTotal              uint64     `json:"vram_total"`
	VRAMAvailable          uint64     `json:"vram_available"`
	MemoryBandwidth        uint64     `json:"memory_bandwidth"`
	ComputeArch            string     `json:"compute_arch"`
	LoadedSlots            []SlotInfo `json:"loaded_slots"`
	SupportsFlashAttention bool       `json:"supports_flash_attention"`
	Backend                string     `json:"backend"` // "rocm", "cuda", "metal", "cpu"
}

// SlotInfo describes a loaded model slot.
type SlotInfo struct {
	Name         string    `json:"name"`
	ModelID      string    `json:"model_id"`
	VRAMBytes    uint64    `json:"vram_bytes"`
	MaxContext   uint32    `json:"max_context"`
	Quantization string    `json:"quantization"`
	Status       SlotState `json:"status"`
	Backend      string    `json:"backend"`
}

// SlotState represents the lifecycle state of a model slot.
type SlotState int

const (
	SlotUnknown   SlotState = 0
	SlotLoading   SlotState = 1
	SlotReady     SlotState = 2
	SlotError     SlotState = 3
	SlotUnloading SlotState = 4
)

// HealthStatus reports GPU health and per-slot metrics.
type HealthStatus struct {
	GPUTempCelsius  float32               `json:"gpu_temp_celsius"`
	GPUUtilization  float32               `json:"gpu_utilization"`
	VRAMUtilization float32               `json:"vram_utilization"`
	UptimeSeconds   uint64                `json:"uptime_seconds"`
	Slots           map[string]SlotHealth `json:"slots"`
}

// SlotHealth reports per-slot operational metrics.
type SlotHealth struct {
	Status         SlotState `json:"status"`
	RequestsTotal  uint64    `json:"requests_total"`
	AvgInferenceMs float32   `json:"avg_inference_ms"`
	TokensPerSec   float32   `json:"tokens_per_sec"`
}

// ConversationContext provides the classifier with recent turns for
// follow-up disambiguation. Kept minimal to fit sidecar context window.
type ConversationContext struct {
	PreviousTurns []ContextTurn
}

// ContextTurn is a single turn in the conversation context.
type ContextTurn struct {
	Role    string // "user" or "assistant"
	Content string // truncated to ~200 chars for assistant, ~500 for user
}

// EmbedResult is the response from a sidecar embedding request.
type EmbedResult struct {
	Embedding   []float32 `json:"embedding"`
	Dimensions  uint32    `json:"dimensions"`
	InferenceMs float32   `json:"inference_ms"`
}
