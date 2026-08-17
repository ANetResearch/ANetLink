// Package ble is the Bluetooth Low Energy adapter. Per K207 A3′ it assumes
// nothing about the host: the primary path talks to the kernel's HCI socket
// directly (AF_BLUETOOTH raw sockets — no BlueZ daemon, no D-Bus, no cgo),
// and BlueZ is only used when it happens to be present and healthy.
//
// R02 §4 mapping: a BLE peripheral -> Node, a GATT service -> Endpoint, a
// characteristic -> Property/Action. Honest trust: V1/A0 for advertisement-
// only sensors (broadcast, no ack), V2/A1 once a connection reads back state.
//
// Scope (v0): HCI socket open/reset, LE scan (advertising reports) and the
// device/capability projection. GATT connect+read is the next increment.
package ble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// HCI protocol constants (Linux BlueZ kernel ABI, not the daemon).
const (
	afBluetooth    = 31
	btProtoHCI     = 1
	hciChannelUser = 1
	hciChannelRaw  = 0

	hciCommandPkt = 0x01
	hciEventPkt   = 0x04

	ogfLEControl       = 0x08
	ocfLESetScanParams = 0x000B
	ocfLESetScanEnable = 0x000C

	evtLEMeta         = 0x3E
	subEvtLEAdvReport = 0x02
)

// sockaddrHCI mirrors struct sockaddr_hci from the kernel.
type sockaddrHCI struct {
	family  uint16
	dev     uint16
	channel uint16
}

// HCIDevice is a raw HCI socket bound to one controller.
type HCIDevice struct {
	fd  int
	dev int
	mu  sync.Mutex
}

// OpenHCI binds a raw HCI socket to controller index dev (hci0 = 0). It needs
// CAP_NET_RAW; the error says so plainly when it is missing.
func OpenHCI(dev int) (*HCIDevice, error) {
	fd, err := syscall.Socket(afBluetooth, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, btProtoHCI)
	if err != nil {
		return nil, fmt.Errorf("ble: open HCI socket (need CAP_NET_RAW): %w", err)
	}
	addr := sockaddrHCI{family: afBluetooth, dev: uint16(dev), channel: hciChannelRaw}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr)); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("ble: bind hci%d: %w", dev, errno)
	}
	return &HCIDevice{fd: fd, dev: dev}, nil
}

func (h *HCIDevice) Close() error { return syscall.Close(h.fd) }

// sendCommand writes an HCI command packet (OGF/OCF + parameters).
func (h *HCIDevice) sendCommand(ogf uint16, ocf uint16, params []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	opcode := (ogf << 10) | ocf
	pkt := make([]byte, 4+len(params))
	pkt[0] = hciCommandPkt
	binary.LittleEndian.PutUint16(pkt[1:], opcode)
	pkt[3] = byte(len(params))
	copy(pkt[4:], params)
	_, err := syscall.Write(h.fd, pkt)
	return err
}

// StartScan enables passive LE scanning.
func (h *HCIDevice) StartScan() error {
	// LE Set Scan Parameters: passive, 10ms interval/window, public address,
	// accept all advertisements.
	params := []byte{0x00, 0x10, 0x00, 0x10, 0x00, 0x00, 0x00}
	if err := h.sendCommand(ogfLEControl, ocfLESetScanParams, params); err != nil {
		return err
	}
	// LE Set Scan Enable: enable, no duplicate filtering.
	return h.sendCommand(ogfLEControl, ocfLESetScanEnable, []byte{0x01, 0x00})
}

// StopScan disables LE scanning.
func (h *HCIDevice) StopScan() error {
	return h.sendCommand(ogfLEControl, ocfLESetScanEnable, []byte{0x00, 0x00})
}

// Advertisement is one decoded LE advertising report.
type Advertisement struct {
	Address     string // "AA:BB:CC:DD:EE:FF"
	RSSI        int8
	LocalName   string
	ManufData   []byte
	ServiceData map[uint16][]byte
}

var errNotAdvReport = errors.New("ble: not an advertising report")

// ParseAdvertisingReport decodes an HCI LE Advertising Report event packet.
// Exported so the decoder is testable without a radio.
func ParseAdvertisingReport(pkt []byte) (*Advertisement, error) {
	// pkt: [type=0x04][evt=0x3E][plen][subevt=0x02][num reports][evt type]
	//      [addr type][addr(6)][data len][data...][rssi]
	if len(pkt) < 12 || pkt[0] != hciEventPkt || pkt[1] != evtLEMeta || pkt[3] != subEvtLEAdvReport {
		return nil, errNotAdvReport
	}
	i := 5 // skip type/evt/plen/subevt/numreports
	if len(pkt) < i+9 {
		return nil, errNotAdvReport
	}
	i++ // event type
	i++ // address type
	var mac [6]byte
	copy(mac[:], pkt[i:i+6])
	i += 6
	dataLen := int(pkt[i])
	i++
	if len(pkt) < i+dataLen+1 {
		return nil, errNotAdvReport
	}
	adv := &Advertisement{
		Address:     fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", mac[5], mac[4], mac[3], mac[2], mac[1], mac[0]),
		ServiceData: map[uint16][]byte{},
	}
	parseAD(pkt[i:i+dataLen], adv)
	adv.RSSI = int8(pkt[i+dataLen])
	return adv, nil
}

// parseAD walks the AD structures inside an advertisement payload.
func parseAD(data []byte, adv *Advertisement) {
	for i := 0; i < len(data); {
		l := int(data[i])
		if l == 0 || i+1+l > len(data) {
			return
		}
		typ := data[i+1]
		val := data[i+2 : i+1+l]
		switch typ {
		case 0x08, 0x09: // shortened / complete local name
			adv.LocalName = strings.TrimRight(string(val), "\x00")
		case 0xFF: // manufacturer specific
			adv.ManufData = append([]byte(nil), val...)
		case 0x16: // service data, 16-bit UUID
			if len(val) >= 2 {
				adv.ServiceData[binary.LittleEndian.Uint16(val)] = append([]byte(nil), val[2:]...)
			}
		}
		i += 1 + l
	}
}

// Scan reads advertising reports until ctx is done, calling fn for each.
func (h *HCIDevice) Scan(ctx context.Context, fn func(*Advertisement)) error {
	if err := h.StartScan(); err != nil {
		return err
	}
	defer h.StopScan()
	buf := make([]byte, 260)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		tv := syscall.Timeval{Sec: 0, Usec: 200000}
		_ = syscall.SetsockoptTimeval(h.fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
		n, err := syscall.Read(h.fd, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if adv, perr := ParseAdvertisingReport(buf[:n]); perr == nil {
			fn(adv)
		}
	}
}

// hciAvailable reports whether any HCI controller can be opened.
func hciAvailable() (int, error) {
	for dev := 0; dev < 4; dev++ {
		h, err := OpenHCI(dev)
		if err == nil {
			h.Close()
			return dev, nil
		}
	}
	return -1, errors.New("no usable HCI controller (no adapter, or missing CAP_NET_RAW)")
}

var _ = time.Second
