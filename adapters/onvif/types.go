package onvif

import "time"

// The response shapes below deliberately use local element names only. Go's
// encoding/xml matches any namespace for an unqualified tag, and cameras
// disagree constantly about prefixes (trt/tds/tt vs ter/tev vs none at all)
// while agreeing on local names. Pinning namespaces here would break on
// devices that are otherwise perfectly compliant.

// systemDateAndTime carries the camera clock. ONVIF specifies this call as
// unauthenticated so a client can correct for drift before it has to build a
// WS-Security timestamp.
type systemDateAndTime struct {
	Body struct {
		SystemDateAndTime struct {
			UTCDateTime struct {
				Time struct {
					Hour   int `xml:"Hour"`
					Minute int `xml:"Minute"`
					Second int `xml:"Second"`
				} `xml:"Time"`
				Date struct {
					Year  int `xml:"Year"`
					Month int `xml:"Month"`
					Day   int `xml:"Day"`
				} `xml:"Date"`
			} `xml:"UTCDateTime"`
		} `xml:"SystemDateAndTime"`
	} `xml:"Body>GetSystemDateAndTimeResponse"`
}

// utc reconstructs the camera's clock. A camera that reports a zero year is
// reporting that it has no clock, not that it is the year zero.
func (s systemDateAndTime) utc() (time.Time, bool) {
	d := s.Body.SystemDateAndTime.UTCDateTime
	if d.Date.Year == 0 {
		return time.Time{}, false
	}
	return time.Date(d.Date.Year, time.Month(d.Date.Month), d.Date.Day,
		d.Time.Hour, d.Time.Minute, d.Time.Second, 0, time.UTC), true
}

type deviceInformation struct {
	Body struct {
		Manufacturer    string `xml:"Manufacturer"`
		Model           string `xml:"Model"`
		FirmwareVersion string `xml:"FirmwareVersion"`
		SerialNumber    string `xml:"SerialNumber"`
		HardwareID      string `xml:"HardwareId"`
	} `xml:"Body>GetDeviceInformationResponse"`
}

type serviceAddr struct {
	XAddr string `xml:"XAddr"`
}

type capabilitiesResponse struct {
	Body struct {
		Capabilities struct {
			Media   serviceAddr `xml:"Media"`
			PTZ     serviceAddr `xml:"PTZ"`
			Events  serviceAddr `xml:"Events"`
			Imaging serviceAddr `xml:"Imaging"`
		} `xml:"Capabilities"`
	} `xml:"Body>GetCapabilitiesResponse"`
}

type mediaProfile struct {
	Token string `xml:"token,attr"`
	Name  string `xml:"Name"`
}

type profilesResponse struct {
	Body struct {
		Profiles []mediaProfile `xml:"Profiles"`
	} `xml:"Body>GetProfilesResponse"`
}

type streamURIResponse struct {
	Body struct {
		MediaURI struct {
			URI string `xml:"Uri"`
		} `xml:"MediaUri"`
	} `xml:"Body>GetStreamUriResponse"`
}

type snapshotURIResponse struct {
	Body struct {
		MediaURI struct {
			URI string `xml:"Uri"`
		} `xml:"MediaUri"`
	} `xml:"Body>GetSnapshotUriResponse"`
}

// vector2D and vector1D carry PTZ coordinates, which ONVIF puts in
// attributes rather than element text.
type vector2D struct {
	X float64 `xml:"x,attr"`
	Y float64 `xml:"y,attr"`
}

type vector1D struct {
	X float64 `xml:"x,attr"`
}

type ptzStatusResponse struct {
	Body struct {
		PTZStatus struct {
			Position struct {
				PanTilt vector2D `xml:"PanTilt"`
				Zoom    vector1D `xml:"Zoom"`
			} `xml:"Position"`
			MoveStatus struct {
				PanTilt string `xml:"PanTilt"`
				Zoom    string `xml:"Zoom"`
			} `xml:"MoveStatus"`
		} `xml:"PTZStatus"`
	} `xml:"Body>GetStatusResponse"`
}

type subscriptionResponse struct {
	Body struct {
		SubscriptionReference struct {
			Address string `xml:"Address"`
		} `xml:"SubscriptionReference"`
	} `xml:"Body>CreatePullPointSubscriptionResponse"`
}

// simpleItem is ONVIF's universal name/value pair inside event messages.
type simpleItem struct {
	Name  string `xml:"Name,attr"`
	Value string `xml:"Value,attr"`
}

type notificationMessage struct {
	Topic   string `xml:"Topic"`
	Message struct {
		Inner struct {
			UtcTime string       `xml:"UtcTime,attr"`
			Source  []simpleItem `xml:"Source>SimpleItem"`
			Data    []simpleItem `xml:"Data>SimpleItem"`
		} `xml:"Message"`
	} `xml:"Message"`
}

type pullMessagesResponse struct {
	Body struct {
		Messages []notificationMessage `xml:"NotificationMessage"`
	} `xml:"Body>PullMessagesResponse"`
}

// probeMatch is one WS-Discovery hit.
type probeMatch struct {
	Address string `xml:"EndpointReference>Address"`
	Types   string `xml:"Types"`
	Scopes  string `xml:"Scopes"`
	XAddrs  string `xml:"XAddrs"`
}

type probeMatches struct {
	Body struct {
		Matches []probeMatch `xml:"ProbeMatch"`
	} `xml:"Body>ProbeMatches"`
}
