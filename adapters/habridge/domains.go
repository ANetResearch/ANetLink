package habridge

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ANetResearch/ANetLink/profile"
)

// The domain table: how each Home Assistant domain projects into TSIR-D.
//
// Until now this bridge understood three domains — switch, light and sensor
// — and everything else hit a `default: return` and was silently dropped.
// That is most of a real installation: climate, covers, locks, fans, media
// players, vacuums, humidifiers, valves, alarm panels and every helper
// entity vanished, and the light it did expose could only be turned on and
// off. HA is the single largest ecosystem ANetLink can reach, and reaching
// 5% of it is not reaching it.
//
// Two rules hold throughout:
//
//   - The ANetLink contract is normalised. Brightness is [0,1] here and
//     0–255 in HA; cover position is [0,1] here and 0–100 there; colour
//     temperature is Kelvin here because Kelvin is the physical quantity and
//     mireds are an encoding. A caller must never have to know which.
//   - A capability is declared only when the entity actually supports it.
//     HA publishes supported_features and supported_color_modes precisely so
//     a client does not have to guess, and declaring a brightness control on
//     a non-dimmable bulb turns a clean "no such capability" into a runtime
//     failure.

// haFeature bits, from Home Assistant's own component constants. These are
// stable public API — HA treats changing them as a breaking change.
const (
	// cover
	featCoverOpen        = 1 << 0
	featCoverClose       = 1 << 1
	featCoverSetPosition = 1 << 2
	featCoverStop        = 1 << 3
	// fan
	featFanSetSpeed = 1 << 0
	// media_player
	featMediaPause     = 1 << 0
	featMediaVolumeSet = 1 << 2
	featMediaTurnOn    = 1 << 7
	featMediaPlay      = 1 << 14
	// climate
	featClimateTargetTemp = 1 << 0
	// vacuum
	featVacuumStop  = 1 << 3
	featVacuumStart = 1 << 13
	// alarm_control_panel
	featAlarmArmHome = 1 << 0
	featAlarmArmAway = 1 << 1
	// valve
	featValveSetPosition = 1 << 2
	// water_heater
	featWaterHeaterTargetTemp = 1 << 0
)

// capSpec is one capability an entity exposes.
type capSpec struct {
	ID     string
	Access uint8
	Unit   string
	// Requires, when non-zero, is the supported_features bit the entity must
	// advertise before this capability is declared.
	Requires int
	// Read extracts the normalised value from an entity's state and
	// attributes. A nil Read means the capability is write-only.
	Read func(e haEntity) (float64, bool)
	// Write turns normalised args into an HA service call. A nil Write means
	// the capability is read-only.
	Write func(e haEntity, args map[string]any) (service string, data map[string]any, err error)
}

// onOffWrite is the shape most domains share: {on: bool} becomes
// turn_on/turn_off with no payload.
func onOffWrite(e haEntity, args map[string]any) (string, map[string]any, error) {
	on, ok := args["on"].(bool)
	if !ok {
		return "", nil, fmt.Errorf("expected {on: true|false}")
	}
	if on {
		return "turn_on", nil, nil
	}
	return "turn_off", nil, nil
}

// onOffRead maps HA's textual state onto 1/0.
func onOffRead(e haEntity) (float64, bool) {
	switch strings.ToLower(e.State) {
	case "on", "open", "unlocked", "playing", "cleaning", "home", "detected", "active":
		return 1, true
	case "off", "closed", "locked", "idle", "paused", "standby", "docked", "away", "clear":
		return 0, true
	}
	return 0, false
}

// attrFloat reads a numeric attribute, tolerating the string/number
// ambiguity HA's JSON carries.
func attrFloat(e haEntity, key string) (float64, bool) {
	v, ok := e.Attributes[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	return 0, false
}

// stateFloat parses the entity's own state as a number.
func stateFloat(e haEntity) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(e.State), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// argFloat01 reads a normalised [0,1] argument.
func argFloat01(args map[string]any, key string) (float64, bool, error) {
	v, ok := args[key]
	if !ok {
		return 0, false, nil
	}
	f, ok := toFloat(v)
	if !ok {
		return 0, false, fmt.Errorf("%s must be a number", key)
	}
	if f < 0 || f > 1 || math.IsNaN(f) {
		return 0, false, fmt.Errorf("%s %v out of range [0,1]", key, f)
	}
	return f, true, nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

// features reads supported_features, which HA reports as a bitmask.
func features(e haEntity) int {
	f, _ := attrFloat(e, "supported_features")
	return int(f)
}

// supportsColorTemp reports whether a light can actually be tuned. HA lists
// its colour modes rather than exposing a feature bit for this.
func supportsColorTemp(e haEntity) bool {
	modes, ok := e.Attributes["supported_color_modes"].([]any)
	if !ok {
		return false
	}
	for _, m := range modes {
		if s, ok := m.(string); ok && strings.EqualFold(s, "color_temp") {
			return true
		}
	}
	return false
}

func supportsBrightness(e haEntity) bool {
	modes, ok := e.Attributes["supported_color_modes"].([]any)
	if !ok {
		// Older integrations omit the list; a reported brightness is proof
		// enough that the control exists.
		_, has := e.Attributes["brightness"]
		return has
	}
	for _, m := range modes {
		s, ok := m.(string)
		if !ok {
			continue
		}
		// Every mode except onoff implies a brightness channel.
		if !strings.EqualFold(s, "onoff") && !strings.EqualFold(s, "unknown") {
			return true
		}
	}
	return false
}

// domainCaps returns the capabilities an entity in this domain exposes,
// filtered by what the entity itself says it supports.
func domainCaps(domain string, e haEntity) []capSpec {
	var out []capSpec
	for _, c := range domainTable[domain] {
		if c.Requires != 0 && features(e)&c.Requires == 0 {
			continue
		}
		out = append(out, c)
	}
	switch domain {
	case "light":
		if supportsBrightness(e) {
			out = append(out, capSpec{
				ID: "light.brightness", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
				Read: func(e haEntity) (float64, bool) {
					b, ok := attrFloat(e, "brightness")
					return b / 255, ok // HA carries 0-255; the contract is [0,1]
				},
				Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
					v, ok, err := argFloat01(args, "brightness")
					if err != nil {
						return "", nil, err
					}
					if !ok {
						return "", nil, fmt.Errorf("expected {brightness: 0..1}")
					}
					// 0 brightness is off, not a dim glow — HA rejects
					// brightness 0 on turn_on.
					if v == 0 {
						return "turn_off", nil, nil
					}
					return "turn_on", map[string]any{"brightness": int(math.Round(v * 255))}, nil
				},
			})
		}
		if supportsColorTemp(e) {
			out = append(out, capSpec{
				ID: "light.color_temp", Access: profile.AccessSet | profile.AccessGet, Unit: "K",
				// Kelvin, not mireds: mireds are an encoding of the same
				// physical quantity, and the contract carries the quantity.
				Read: func(e haEntity) (float64, bool) { return attrFloat(e, "color_temp_kelvin") },
				Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
					v, ok := toFloat(args["kelvin"])
					if !ok {
						return "", nil, fmt.Errorf("expected {kelvin: 2000..6500}")
					}
					if v < 1000 || v > 10000 {
						return "", nil, fmt.Errorf("kelvin %v is outside any real lamp's range", v)
					}
					return "turn_on", map[string]any{"color_temp_kelvin": int(v)}, nil
				},
			})
		}
	}
	return out
}

// domainTable is the static part: capabilities that do not depend on
// per-entity feature discovery.
var domainTable = map[string][]capSpec{
	"switch": {{ID: "switch.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: onOffRead, Write: onOffWrite}},
	"input_boolean": {{ID: "switch.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: onOffRead, Write: onOffWrite}},
	"siren": {{ID: "siren.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: onOffRead, Write: onOffWrite}},
	"light": {{ID: "light.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: onOffRead, Write: onOffWrite}},

	"lock": {{
		ID: "lock.locked", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: func(e haEntity) (float64, bool) {
			switch strings.ToLower(e.State) {
			case "locked":
				return 1, true
			case "unlocked", "open":
				return 0, true
			}
			// jammed / locking / unlocking are real states and none of them
			// is "locked" — reporting them as a boolean would be a lie.
			return 0, false
		},
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			locked, ok := args["locked"].(bool)
			if !ok {
				return "", nil, fmt.Errorf("expected {locked: true|false}")
			}
			if locked {
				return "lock", nil, nil
			}
			return "unlock", nil, nil
		},
	}},

	"cover": {
		{
			ID: "cover.position", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featCoverSetPosition,
			Read: func(e haEntity) (float64, bool) {
				p, ok := attrFloat(e, "current_position")
				return p / 100, ok
			},
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				v, ok, err := argFloat01(args, "position")
				if err != nil {
					return "", nil, err
				}
				if !ok {
					return "", nil, fmt.Errorf("expected {position: 0..1}")
				}
				return "set_cover_position", map[string]any{"position": int(math.Round(v * 100))}, nil
			},
		},
		{
			ID: "cover.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featCoverOpen,
			Read:     onOffRead,
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				open, ok := args["on"].(bool)
				if !ok {
					return "", nil, fmt.Errorf("expected {on: true|false}")
				}
				if open {
					return "open_cover", nil, nil
				}
				return "close_cover", nil, nil
			},
		},
		{
			ID: "cover.stop", Access: profile.AccessSet, Requires: featCoverStop,
			Write: func(haEntity, map[string]any) (string, map[string]any, error) {
				return "stop_cover", nil, nil
			},
		},
	},

	"valve": {{
		ID: "valve.position", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Requires: featValveSetPosition,
		Read: func(e haEntity) (float64, bool) {
			p, ok := attrFloat(e, "current_valve_position")
			return p / 100, ok
		},
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			v, ok, err := argFloat01(args, "position")
			if err != nil {
				return "", nil, err
			}
			if !ok {
				return "", nil, fmt.Errorf("expected {position: 0..1}")
			}
			return "set_valve_position", map[string]any{"position": int(math.Round(v * 100))}, nil
		},
	}},

	"climate": {
		{
			ID: "climate.setpoint", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Unit: "°C", Requires: featClimateTargetTemp,
			Read: func(e haEntity) (float64, bool) { return attrFloat(e, "temperature") },
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				v, ok := toFloat(args["temperature"])
				if !ok {
					return "", nil, fmt.Errorf("expected {temperature: °C}")
				}
				// Clamp to what the entity itself says it accepts; sending a
				// setpoint outside its range makes HA reject the whole call.
				if lo, ok := attrFloat(e, "min_temp"); ok && v < lo {
					return "", nil, fmt.Errorf("temperature %g below the entity minimum %g", v, lo)
				}
				if hi, ok := attrFloat(e, "max_temp"); ok && v > hi {
					return "", nil, fmt.Errorf("temperature %g above the entity maximum %g", v, hi)
				}
				return "set_temperature", map[string]any{"temperature": v}, nil
			},
		},
		{
			// The measured temperature, which is not the setpoint. Conflating
			// the two is the most common climate integration bug there is.
			ID: "climate.temperature", Access: profile.AccessGet | profile.AccessState, Unit: "°C",
			Read: func(e haEntity) (float64, bool) { return attrFloat(e, "current_temperature") },
		},
		{
			ID: "climate.humidity", Access: profile.AccessGet | profile.AccessState, Unit: "%",
			Read: func(e haEntity) (float64, bool) { return attrFloat(e, "current_humidity") },
		},
	},

	"water_heater": {{
		ID: "water_heater.setpoint", Access: profile.AccessSet | profile.AccessGet,
		Unit: "°C", Requires: featWaterHeaterTargetTemp,
		Read: func(e haEntity) (float64, bool) { return attrFloat(e, "temperature") },
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			v, ok := toFloat(args["temperature"])
			if !ok {
				return "", nil, fmt.Errorf("expected {temperature: °C}")
			}
			return "set_temperature", map[string]any{"temperature": v}, nil
		},
	}},

	"fan": {
		{ID: "fan.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Read: onOffRead, Write: onOffWrite},
		{
			ID: "fan.speed", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featFanSetSpeed,
			Read: func(e haEntity) (float64, bool) {
				p, ok := attrFloat(e, "percentage")
				return p / 100, ok
			},
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				v, ok, err := argFloat01(args, "speed")
				if err != nil {
					return "", nil, err
				}
				if !ok {
					return "", nil, fmt.Errorf("expected {speed: 0..1}")
				}
				return "set_percentage", map[string]any{"percentage": int(math.Round(v * 100))}, nil
			},
		},
	},

	"humidifier": {
		{ID: "humidifier.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Read: onOffRead, Write: onOffWrite},
		{
			ID: "humidifier.setpoint", Access: profile.AccessSet | profile.AccessGet, Unit: "%",
			Read: func(e haEntity) (float64, bool) { return attrFloat(e, "humidity") },
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				v, ok, err := argFloat01(args, "humidity")
				if err != nil {
					return "", nil, err
				}
				if !ok {
					return "", nil, fmt.Errorf("expected {humidity: 0..1}")
				}
				return "set_humidity", map[string]any{"humidity": int(math.Round(v * 100))}, nil
			},
		},
	},

	"media_player": {
		{ID: "media.onoff", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featMediaTurnOn, Read: onOffRead, Write: onOffWrite},
		{
			ID: "media.volume", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featMediaVolumeSet,
			Read:     func(e haEntity) (float64, bool) { return attrFloat(e, "volume_level") },
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				v, ok, err := argFloat01(args, "volume")
				if err != nil {
					return "", nil, err
				}
				if !ok {
					return "", nil, fmt.Errorf("expected {volume: 0..1}")
				}
				return "volume_set", map[string]any{"volume_level": v}, nil
			},
		},
		{
			ID: "media.playing", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
			Requires: featMediaPlay | featMediaPause,
			Read: func(e haEntity) (float64, bool) {
				switch strings.ToLower(e.State) {
				case "playing":
					return 1, true
				case "paused", "idle", "standby", "off":
					return 0, true
				}
				return 0, false
			},
			Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
				play, ok := args["playing"].(bool)
				if !ok {
					return "", nil, fmt.Errorf("expected {playing: true|false}")
				}
				if play {
					return "media_play", nil, nil
				}
				return "media_pause", nil, nil
			},
		},
	},

	"vacuum": {{
		ID: "vacuum.cleaning", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Requires: featVacuumStart | featVacuumStop,
		Read:     onOffRead,
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			on, ok := args["on"].(bool)
			if !ok {
				return "", nil, fmt.Errorf("expected {on: true|false}")
			}
			if on {
				return "start", nil, nil
			}
			// Returning to base is what a person means by "stop cleaning";
			// plain stop leaves the machine stranded mid-floor.
			return "return_to_base", nil, nil
		},
	}},

	"alarm_control_panel": {{
		ID: "alarm.armed", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Requires: featAlarmArmHome | featAlarmArmAway,
		Read: func(e haEntity) (float64, bool) {
			switch strings.ToLower(e.State) {
			case "disarmed":
				return 0, true
			case "armed_home", "armed_away", "armed_night", "armed_vacation":
				return 1, true
			case "triggered":
				return 2, true
			}
			// arming / pending are transitions, not states to report as fact.
			return 0, false
		},
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			mode, _ := args["mode"].(string)
			switch strings.ToLower(strings.TrimSpace(mode)) {
			case "off", "disarm", "disarmed":
				return "alarm_disarm", nil, nil
			case "home":
				return "alarm_arm_home", nil, nil
			case "away", "on":
				return "alarm_arm_away", nil, nil
			case "night":
				return "alarm_arm_night", nil, nil
			}
			return "", nil, fmt.Errorf(`mode must be one of off|home|away|night, got %q`, mode)
		},
	}},

	"number": {{
		ID: "number.value", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: stateFloat,
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			v, ok := toFloat(args["value"])
			if !ok {
				return "", nil, fmt.Errorf("expected {value: number}")
			}
			return "set_value", map[string]any{"value": v}, nil
		},
	}},
	"input_number": {{
		ID: "number.value", Access: profile.AccessSet | profile.AccessGet | profile.AccessState,
		Read: stateFloat,
		Write: func(e haEntity, args map[string]any) (string, map[string]any, error) {
			v, ok := toFloat(args["value"])
			if !ok {
				return "", nil, fmt.Errorf("expected {value: number}")
			}
			return "set_value", map[string]any{"value": v}, nil
		},
	}},

	"button": {{
		ID: "button.press", Access: profile.AccessSet,
		Write: func(haEntity, map[string]any) (string, map[string]any, error) {
			return "press", nil, nil
		},
	}},
	"scene": {{
		ID: "scene.activate", Access: profile.AccessSet,
		Write: func(haEntity, map[string]any) (string, map[string]any, error) {
			return "turn_on", nil, nil
		},
	}},
	"script": {{
		ID: "script.run", Access: profile.AccessSet,
		Write: func(haEntity, map[string]any) (string, map[string]any, error) {
			return "turn_on", nil, nil
		},
	}},
}

// sensorCap builds the capability for a sensor or binary_sensor, whose
// identity comes from its device_class rather than its domain.
func sensorCap(domain string, e haEntity) (capSpec, bool) {
	class, _ := e.Attributes["device_class"].(string)
	switch domain {
	case "sensor":
		if class == "" {
			class = "value"
		}
		unit, _ := e.Attributes["unit_of_measurement"].(string)
		return capSpec{
			ID: "sensor." + class, Access: profile.AccessGet | profile.AccessState, Unit: unit,
			Read: stateFloat,
		}, true
	case "binary_sensor":
		if class == "" {
			class = "state"
		}
		return capSpec{
			ID: "binary_sensor." + class, Access: profile.AccessGet | profile.AccessState,
			Read: onOffRead,
		}, true
	}
	return capSpec{}, false
}

// EntityCaps is the whole projection for one entity: every capability it
// exposes, in a stable order so the profile CID does not churn.
func EntityCaps(entityID string, e haEntity) []capSpec {
	domain := strings.SplitN(entityID, ".", 2)[0]
	if c, ok := sensorCap(domain, e); ok {
		return []capSpec{c}
	}
	return domainCaps(domain, e)
}

// SupportedDomains lists what this bridge understands, for the docs and for
// operators wondering why an entity did not appear.
func SupportedDomains() []string {
	out := []string{"sensor", "binary_sensor"}
	for d := range domainTable {
		out = append(out, d)
	}
	return out
}
