// Command gen regenerates the OUI table from the IEEE MA-L registry.
//
//	go run ./fingerprint/gen -out fingerprint/oui_table.go
//
// The source is https://standards-oui.ieee.org/oui/oui.csv — the registry
// itself, not any redistribution of it. This matters: the Nmap Public Source
// License §3 makes anything that "reads or includes Covered Software data
// files" a derivative work under NPSL, so nmap-mac-prefixes must never be
// the input here even though it carries the same underlying IEEE data.
//
// The registry has ~40,000 organisations. Embedding all of them would add
// megabytes to every binary for the sake of vendors ANetLink will never
// meet, so the curated list below selects the manufacturers that actually
// turn up on a building network — and, more usefully, maps each to a
// canonical name, a device class and the ecosystems that can reach it.
package main

import (
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ieeeMAL = "https://standards-oui.ieee.org/oui/oui.csv"

// spec is one curated vendor: how to recognise its IEEE registration
// strings, and what we know about it that IEEE does not.
type spec struct {
	name string
	// match are lowercase substrings of the IEEE "Organization Name". A
	// company registers under many spellings — Hikvision alone has dozens —
	// so matching is by substring, with exclude to kill collisions.
	match   []string
	exclude []string
	class   string
	ecos    []string
	adapter []string
}

// The catalogue. Ordered by how likely an operator is to meet the vendor on
// a Chinese building network, because that is the network ANetLink is for.
var specs = []spec{
	// --- cameras and video ---
	{name: "Hikvision", match: []string{"hikvision"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF", "EcoISAPI"}, adapter: []string{"onvif", "hikvision"}},
	{name: "Dahua", match: []string{"dahua"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF", "EcoDahua"}, adapter: []string{"onvif", "dahua"}},
	{name: "Uniview", match: []string{"uniview", "zhejiang uniview"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "Axis Communications", match: []string{"axis communications"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "Hanwha Vision", match: []string{"hanwha"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "Bosch Security", match: []string{"bosch security"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "VIVOTEK", match: []string{"vivotek"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "Amcrest", match: []string{"amcrest"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF", "EcoDahua"}, adapter: []string{"onvif", "dahua"}},
	{name: "Reolink", match: []string{"reolink"}, class: "ClassCamera",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},
	{name: "TP-Link Tapo", match: []string{"tp-link"}, class: "ClassNetwork",
		ecos: []string{"EcoONVIF"}, adapter: []string{"onvif"}},

	// --- Chinese smart-home ecosystems ---
	// No adapter yet: these are the D58 gap, and the catalogue names it
	// rather than quietly omitting them.
	{name: "Xiaomi", match: []string{"xiaomi", "beijing xiaomi"}, class: "ClassSmartHome",
		ecos: []string{"EcoMIoT"}},
	{name: "Yeelight", match: []string{"yeelink", "yeelight"}, class: "ClassSmartHome",
		ecos: []string{"EcoMIoT"}},
	{name: "Aqara (Lumi)", match: []string{"lumi united", "aqara"}, class: "ClassSmartHome",
		ecos: []string{"EcoAqara", "EcoZigbee"}},
	{name: "Tuya", match: []string{"tuya"}, class: "ClassSmartHome",
		ecos: []string{"EcoTuya"}},
	{name: "Broadlink", match: []string{"broadlink"}, class: "ClassSmartHome"},
	{name: "Roborock", match: []string{"roborock", "beijing roborock"}, class: "ClassRobot",
		ecos: []string{"EcoMIoT"}},
	// Absent by design, not by oversight: ORVIBO, Ecovacs and ITEAD/Sonoff
	// hold no IEEE block in MA-L or MA-S. Their products ship the OUI of
	// whoever made the radio module — usually Espressif — so a MAC lookup
	// honestly reports "an ESP32 someone built a product around" and cannot
	// name the brand. Identifying those needs a service-level fingerprint,
	// not an OUI.

	// --- western smart-home ---
	{name: "Signify (Philips Hue)", match: []string{"signify", "philips lighting"},
		class: "ClassLighting", ecos: []string{"EcoZigbee", "EcoMatter"}},
	{name: "IKEA", match: []string{"ikea of sweden"}, class: "ClassSmartHome",
		ecos: []string{"EcoZigbee", "EcoMatter"}},
	{name: "Shelly", match: []string{"shelly", "allterco"}, class: "ClassSmartHome"},
	{name: "Nanoleaf", match: []string{"nanoleaf"}, class: "ClassLighting",
		ecos: []string{"EcoMatter"}},
	{name: "Belkin Wemo", match: []string{"belkin"}, class: "ClassSmartHome"},
	{name: "Lutron", match: []string{"lutron"}, class: "ClassLighting"},
	{name: "Netatmo", match: []string{"netatmo"}, class: "ClassSmartHome"},
	{name: "Somfy", match: []string{"somfy"}, class: "ClassSmartHome"},
	{name: "Sonos", match: []string{"sonos"}, class: "ClassSmartHome"},

	// --- silicon that ends up inside everything else ---
	// An Espressif OUI means "an ESP32 someone built a product around" and
	// nothing more. Saying that plainly beats guessing at a product.
	{name: "Espressif (ESP32/ESP8266)", match: []string{"espressif"}, class: "ClassSmartHome"},

	// --- building and industrial automation ---
	{name: "Siemens", match: []string{"siemens"}, class: "ClassIndustrial",
		ecos: []string{"EcoBACnet", "EcoModbus"}, adapter: []string{"bacnet", "modbus", "opcua"}},
	{name: "Schneider Electric", match: []string{"schneider electric", "apc by schneider"},
		class: "ClassIndustrial", ecos: []string{"EcoBACnet", "EcoModbus"},
		adapter: []string{"bacnet", "modbus"}},
	{name: "Honeywell", match: []string{"honeywell"}, class: "ClassIndustrial",
		ecos: []string{"EcoBACnet"}, adapter: []string{"bacnet"}},
	{name: "Johnson Controls", match: []string{"johnson controls"}, class: "ClassIndustrial",
		ecos: []string{"EcoBACnet"}, adapter: []string{"bacnet"}},
	{name: "Legrand", match: []string{"legrand"}, class: "ClassIndustrial"},
	{name: "Beckhoff", match: []string{"beckhoff"}, class: "ClassIndustrial",
		ecos: []string{"EcoModbus"}, adapter: []string{"modbus", "opcua"}},
	{name: "WAGO", match: []string{"wago"}, class: "ClassIndustrial",
		ecos: []string{"EcoModbus"}, adapter: []string{"modbus"}},
	{name: "Phoenix Contact", match: []string{"phoenix contact"}, class: "ClassIndustrial",
		ecos: []string{"EcoModbus"}, adapter: []string{"modbus"}},
	{name: "Moxa", match: []string{"moxa"}, class: "ClassIndustrial",
		ecos: []string{"EcoModbus"}, adapter: []string{"modbus"}},
	{name: "Advantech", match: []string{"advantech"}, class: "ClassIndustrial",
		ecos: []string{"EcoModbus"}, adapter: []string{"modbus"}},
	{name: "Delta Controls", match: []string{"delta controls"}, class: "ClassIndustrial",
		ecos: []string{"EcoBACnet"}, adapter: []string{"bacnet"}},

	// --- network gear, so an operator can tell infrastructure from devices ---
	{name: "Ubiquiti", match: []string{"ubiquiti"}, class: "ClassNetwork"},
	{name: "Huawei", match: []string{"huawei"}, class: "ClassNetwork"},
	{name: "H3C", match: []string{"h3c", "new h3c"}, class: "ClassNetwork"},
	{name: "Ruijie", match: []string{"ruijie"}, class: "ClassNetwork"},
}

func main() {
	out := flag.String("out", "fingerprint/oui_table.go", "output file")
	src := flag.String("src", ieeeMAL, "IEEE MA-L registry CSV")
	flag.Parse()

	rows, err := fetch(*src)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}

	// oui -> vendor index, and a per-vendor count for the report.
	assign := map[uint32]int{}
	counts := make([]int, len(specs))
	ambiguous := 0

	for _, r := range rows {
		org := strings.ToLower(r.org)
		idx := -1
		for i, s := range specs {
			if !matches(org, s) {
				continue
			}
			if idx >= 0 {
				// Two curated vendors claim the same registration. Assigning
				// it to whichever came first would be arbitrary, so drop it
				// and report — a wrong vendor is worse than no vendor.
				idx = -2
				break
			}
			idx = i
		}
		switch {
		case idx == -2:
			ambiguous++
		case idx >= 0:
			assign[r.oui] = idx
			counts[idx]++
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, `// Code generated by fingerprint/gen; DO NOT EDIT.
//
// Source: IEEE MA-L registry (%s), retrieved %s.
// The IEEE registry is the origin of this data. It is deliberately NOT
// taken from nmap-mac-prefixes: NPSL §3 would make any consumer of that
// file a derivative work under NPSL. See catalogue.go.

package fingerprint

// vendors is the curated catalogue. Index order is stable across
// regeneration because it follows the order of specs in gen/main.go.
var vendors = []Vendor{
`, ieeeMAL, time.Now().UTC().Format("2006-01-02"))

	for _, s := range specs {
		fmt.Fprintf(&b, "\t{Name: %s, Class: %s", strconv.Quote(s.name), s.class)
		if len(s.ecos) > 0 {
			fmt.Fprintf(&b, ", Ecosystems: []Ecosystem{%s}", strings.Join(s.ecos, ", "))
		}
		if len(s.adapter) > 0 {
			quoted := make([]string, len(s.adapter))
			for i, a := range s.adapter {
				quoted[i] = strconv.Quote(a)
			}
			fmt.Fprintf(&b, ", Adapters: []string{%s}", strings.Join(quoted, ", "))
		}
		fmt.Fprint(&b, "},\n")
	}
	fmt.Fprint(&b, "}\n\n// ouiIndex maps a 24-bit OUI to an index into vendors.\nvar ouiIndex = map[uint32]uint16{\n")

	ouis := make([]uint32, 0, len(assign))
	for o := range assign {
		ouis = append(ouis, o)
	}
	sort.Slice(ouis, func(i, j int) bool { return ouis[i] < ouis[j] })
	for _, o := range ouis {
		fmt.Fprintf(&b, "\t0x%06X: %d,\n", o, assign[o])
	}
	fmt.Fprint(&b, "}\n")

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		fmt.Fprintln(os.Stderr, "gen: generated source does not parse:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, formatted, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}

	fmt.Printf("wrote %s: %d OUIs across %d vendors (%d registrations dropped as ambiguous)\n",
		*out, len(assign), len(specs), ambiguous)
	for i, s := range specs {
		if counts[i] == 0 {
			// A curated vendor that matched nothing is a stale or misspelled
			// pattern, not a vendor with no hardware. Say so.
			fmt.Printf("  !! %-28s no OUI matched — check the match patterns\n", s.name)
			continue
		}
		fmt.Printf("  %-28s %4d OUI\n", s.name, counts[i])
	}
}

func matches(org string, s spec) bool {
	for _, x := range s.exclude {
		if strings.Contains(org, x) {
			return false
		}
	}
	for _, m := range s.match {
		if strings.Contains(org, m) {
			return true
		}
	}
	return false
}

type row struct {
	oui uint32
	org string
}

func fetch(src string) ([]row, error) {
	var rc io.ReadCloser
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		cli := &http.Client{Timeout: 3 * time.Minute}
		resp, err := cli.Get(src)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("IEEE registry returned HTTP %d", resp.StatusCode)
		}
		rc = resp.Body
	} else {
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		rc = f
	}
	defer rc.Close()

	cr := csv.NewReader(rc)
	cr.FieldsPerRecord = -1
	records, err := cr.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("registry has %d rows; that is not the registry", len(records))
	}

	var out []row
	for _, rec := range records[1:] {
		if len(rec) < 3 {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(rec[1]), 16, 32)
		if err != nil {
			continue
		}
		out = append(out, row{oui: uint32(v), org: strings.TrimSpace(rec[2])})
	}
	return out, nil
}
