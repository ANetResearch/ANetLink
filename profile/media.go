package profile

// Media capability family (TSIR-D extension, K212 D54).
//
// Every capability family before this one resolves to a number: a temperature,
// a set-point, an on/off bit. Cameras do not. A camera's primary capability is
// a *continuous stream* that ANetLink must never proxy — the agent needs a
// reference it can hand to something that speaks video, not bytes flowing
// through the narrow waist. The still-image and event families do resolve to
// concrete artifacts, and those we do carry.
//
// The three shapes and where their result lives:
//
//	stream.*  → tsir.Artifact{Path: uri}      — a reference, never the bytes
//	snapshot  → tsir.Artifact{ContentCID: …}  — real bytes, content-addressed
//	ptz.*     → metrics (position readback)   — ordinary numbers
//	event.*   → metrics (0/1 + timestamp)     — ordinary numbers
//
// This is why no ANetCore wire change was needed: tsir.EffectRecord already
// carried an Artifacts array with Path/MediaType/ContentCID/SizeBytes.
const (
	// CapStreamRTSP resolves to a live RTSP URI. GET only — a stream is
	// something you are given a handle to, not something you set.
	CapStreamRTSP = "stream.rtsp"
	// CapStreamSnapshot captures one still frame and returns it as a
	// content-addressed artifact. GET.
	CapStreamSnapshot = "stream.snapshot"

	// CapPTZMove is continuous pan/tilt/zoom: Args {pan,tilt,zoom} in
	// [-1,1] velocity space, optional {duration_ms}. SET.
	CapPTZMove = "ptz.move"
	// CapPTZStop halts any movement in progress. SET.
	CapPTZStop = "ptz.stop"
	// CapPTZPreset recalls a stored position: Args {preset}. SET|GET.
	CapPTZPreset = "ptz.preset"
	// CapPTZPosition reads the absolute pan/tilt/zoom position. GET|STATE.
	// This is what makes PTZ actuation verifiable rather than fire-and-forget.
	CapPTZPosition = "ptz.position"

	// CapEventMotion reports motion detection. STATE.
	CapEventMotion = "event.motion"
	// CapEventTamper reports camera tampering (lens covered, moved). STATE.
	CapEventTamper = "event.tamper"
	// CapEventLineCross reports a tripwire crossing — the most widely
	// deployed of the "smart" analytics events. STATE.
	CapEventLineCross = "event.linecross"
)

// Artifact media types the media family emits.
const (
	// MediaTypeRTSP marks an Artifact whose Path is an RTSP URI rather than
	// a stored blob. Consumers must dial it; there is nothing to fetch.
	MediaTypeRTSP = "application/x-rtsp-uri"
	// MediaTypeJPEG marks a still frame carried by content id.
	MediaTypeJPEG = "image/jpeg"
)

// IsStreamCapability reports whether id names a capability whose result is a
// reference to media rather than a measurement. Callers upstream of the
// runtime use this to decide whether an effect needs a media-aware consumer.
func IsStreamCapability(id string) bool {
	return id == CapStreamRTSP || id == CapStreamSnapshot
}
