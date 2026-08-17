package zigbee

import (
	"encoding/binary"
	"errors"
)

// ZCL cluster ids we map (Zigbee Cluster Library spec).
const (
	ClusterOnOff             uint16 = 0x0006
	ClusterLevelControl      uint16 = 0x0008
	ClusterTemperatureMeas   uint16 = 0x0402
	ClusterHumidityMeas      uint16 = 0x0405
	ClusterOccupancySensing  uint16 = 0x0406
	ClusterElectricalMeasure uint16 = 0x0B04
)

// ZCL attribute ids (per cluster, the ones we read).
const (
	AttrOnOff         uint16 = 0x0000
	AttrCurrentLevel  uint16 = 0x0000
	AttrMeasuredValue uint16 = 0x0000
	AttrOccupancy     uint16 = 0x0000
)

// ZCL data types (spec §2.6.2).
const (
	TypeBool   byte = 0x10
	TypeUint8  byte = 0x20
	TypeUint16 byte = 0x21
	TypeInt16  byte = 0x29
	TypeEnum8  byte = 0x30
	TypeMap8   byte = 0x18
)

// ZCL frame control bits.
const (
	frameTypeGlobal  byte = 0x00
	frameTypeCluster byte = 0x01
)

// Global commands.
const (
	CmdReadAttributes         byte = 0x00
	CmdReadAttributesResponse byte = 0x01
)

// On/Off cluster commands.
const (
	CmdOff    byte = 0x00
	CmdOn     byte = 0x01
	CmdToggle byte = 0x02
)

var ErrZCLMalformed = errors.New("zigbee: malformed ZCL frame")

// BuildReadAttributes builds a ZCL Read Attributes frame for one attribute.
func BuildReadAttributes(seq byte, attr uint16) []byte {
	b := []byte{frameTypeGlobal, seq, CmdReadAttributes, 0, 0}
	binary.LittleEndian.PutUint16(b[3:], attr)
	return b
}

// BuildOnOffCommand builds an On/Off cluster command frame.
func BuildOnOffCommand(seq byte, on bool) []byte {
	cmd := CmdOff
	if on {
		cmd = CmdOn
	}
	return []byte{frameTypeCluster, seq, cmd}
}

// AttributeValue is one decoded attribute from a Read Attributes Response.
type AttributeValue struct {
	ID     uint16
	Status byte
	Type   byte
	Value  float64
}

// ParseReadAttributesResponse decodes a ZCL Read Attributes Response.
func ParseReadAttributesResponse(b []byte) ([]AttributeValue, error) {
	if len(b) < 3 {
		return nil, ErrZCLMalformed
	}
	// frame control, seq, command
	if b[2] != CmdReadAttributesResponse {
		return nil, ErrZCLMalformed
	}
	i := 3
	var out []AttributeValue
	for i+3 <= len(b) {
		av := AttributeValue{ID: binary.LittleEndian.Uint16(b[i:]), Status: b[i+2]}
		i += 3
		if av.Status != 0x00 { // failure: no value follows
			out = append(out, av)
			continue
		}
		if i >= len(b) {
			return nil, ErrZCLMalformed
		}
		av.Type = b[i]
		i++
		v, n, err := decodeValue(av.Type, b[i:])
		if err != nil {
			return nil, err
		}
		av.Value = v
		i += n
		out = append(out, av)
	}
	if len(out) == 0 {
		return nil, ErrZCLMalformed
	}
	return out, nil
}

func decodeValue(typ byte, b []byte) (float64, int, error) {
	switch typ {
	case TypeBool, TypeUint8, TypeEnum8, TypeMap8:
		if len(b) < 1 {
			return 0, 0, ErrZCLMalformed
		}
		return float64(b[0]), 1, nil
	case TypeUint16:
		if len(b) < 2 {
			return 0, 0, ErrZCLMalformed
		}
		return float64(binary.LittleEndian.Uint16(b)), 2, nil
	case TypeInt16:
		if len(b) < 2 {
			return 0, 0, ErrZCLMalformed
		}
		return float64(int16(binary.LittleEndian.Uint16(b))), 2, nil
	}
	return 0, 0, ErrZCLMalformed
}

// ClusterCapability maps a ZCL cluster to an ANetLink capability id and the
// scale that turns raw attribute values into physical units (R02 §4:
// Zigbee Cluster -> Capability, Attribute -> Property).
type ClusterCapability struct {
	Cluster    uint16
	Attribute  uint16
	Capability string
	Scale      float64
	Unit       string
	Writable   bool
}

// StandardClusters is the built-in mapping table — the Zigbee equivalent of
// what zigbee-herdsman-converters encodes, for the clusters that cover most
// lights, plugs and sensors.
var StandardClusters = []ClusterCapability{
	{ClusterOnOff, AttrOnOff, "switch.onoff", 1, "", true},
	{ClusterLevelControl, AttrCurrentLevel, "light.brightness", 1.0 / 254.0, "", true},
	{ClusterTemperatureMeas, AttrMeasuredValue, "sensor.temperature", 0.01, "°C", false},
	{ClusterHumidityMeas, AttrMeasuredValue, "sensor.humidity", 0.01, "%", false},
	{ClusterOccupancySensing, AttrOccupancy, "sensor.occupancy", 1, "", false},
}

// CapabilityForCluster returns the mapping for a cluster id.
func CapabilityForCluster(cluster uint16) (ClusterCapability, bool) {
	for _, c := range StandardClusters {
		if c.Cluster == cluster {
			return c, true
		}
	}
	return ClusterCapability{}, false
}
