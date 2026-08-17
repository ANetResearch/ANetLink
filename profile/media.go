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
	// CapPTZAbsolute drives to an absolute bearing: Args {pan,tilt,zoom} in
	// [-1,1] coordinate space (not velocity), optional {speed}. SET.
	//
	// This is the capability an agent actually wants. "Point at the front
	// door" is a position, and expressing it as a timed velocity nudge —
	// the only thing ptz.move can do — makes the result depend on how
	// loaded the camera happens to be.
	CapPTZAbsolute = "ptz.absolute"
	// CapPTZHome returns the head to its configured home bearing. SET.
	CapPTZHome = "ptz.home"
	// CapPTZPresetList enumerates stored presets: returns tokens and names
	// so an agent can say "go to the loading bay" without knowing it is
	// preset 7. GET.
	CapPTZPresetList = "ptz.preset.list"
	// CapPTZPresetSave stores the current bearing: Args {preset, name}. SET.
	CapPTZPresetSave = "ptz.preset.save"
	// CapPTZTour starts or stops a patrol: Args {tour, on}. SET.
	CapPTZTour = "ptz.tour"

	// CapLensFocus drives focus: Args {focus} in [-1,1] (negative near,
	// positive far), or {auto: true} to hand it back to the camera. SET.
	CapLensFocus = "lens.focus"
	// CapImagingBrightness, and the three below it, are the imaging
	// parameters ONVIF standardises. Args {value} in [0,1] normalised
	// across vendor ranges. SET|GET.
	CapImagingBrightness = "imaging.brightness"
	CapImagingContrast   = "imaging.contrast"
	CapImagingSaturation = "imaging.saturation"
	CapImagingSharpness  = "imaging.sharpness"
	// CapImagingIRCut controls the infrared cut filter — day/night mode.
	// Args {mode: "on"|"off"|"auto"}. SET|GET. This is the one imaging
	// control with a physical, observable consequence: the filter is a
	// mechanical part that moves.
	CapImagingIRCut = "imaging.ircut"

	// CapSystemReboot restarts the device. SET.
	//
	// Deliberately its own capability rather than a flag on something else:
	// rebooting a camera is disruptive and an agent should have to name it.
	CapSystemReboot = "system.reboot"
	// CapSystemInfo reports firmware, serial and uptime. GET.
	CapSystemInfo = "system.info"

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
