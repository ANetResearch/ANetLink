package onvif

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ONVIF namespace URIs. Kept as constants because they are also the
// discriminator the device uses to route a request to a service.
const (
	nsSOAP    = "http://www.w3.org/2003/05/soap-envelope"
	nsSchema  = "http://www.onvif.org/ver10/schema"
	nsDevice  = "http://www.onvif.org/ver10/device/wsdl"
	nsMedia   = "http://www.onvif.org/ver10/media/wsdl"
	nsPTZ     = "http://www.onvif.org/ver20/ptz/wsdl"
	nsEvents  = "http://www.onvif.org/ver10/events/wsdl"
	nsAddr    = "http://www.w3.org/2005/08/addressing"
	nsWSSE    = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"
	nsWSU     = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd"
	typDigest = nsWSSE + "#PasswordDigest"
	encBase64 = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary"
)

// soapFault is the error a device returns instead of a result. Reporting the
// device's own reason verbatim matters: "not authorized" and "no such profile"
// need different fixes, and collapsing both to "request failed" hides which.
type soapFault struct {
	Code   string
	Reason string
	Detail string
}

func (f *soapFault) Error() string {
	msg := f.Reason
	if msg == "" {
		msg = f.Code
	}
	if f.Detail != "" {
		msg += ": " + f.Detail
	}
	return "onvif fault: " + msg
}

// IsAuthFault reports whether a device rejected the credentials rather than
// the request. Callers use this to stop retrying and say so plainly.
func IsAuthFault(err error) bool {
	f, ok := err.(*soapFault)
	if !ok {
		return false
	}
	blob := strings.ToLower(f.Code + " " + f.Reason + " " + f.Detail)
	return strings.Contains(blob, "notauthorized") ||
		strings.Contains(blob, "not authorized") ||
		strings.Contains(blob, "authentication")
}

// usernameToken builds the WS-Security header ONVIF devices authenticate
// with: Digest = Base64(SHA1(nonce ‖ created ‖ password)) per WS-Security
// UsernameTokenProfile 1.0. The password never crosses the wire.
//
// clockSkew shifts Created to the device's clock. Cameras reject tokens whose
// timestamp drifts outside their window, and an unsynchronised camera is the
// normal case, not the exception — GetSystemDateAndTime is unauthenticated
// precisely so a client can learn the offset before it needs to authenticate.
func usernameToken(user, pass string, clockSkew time.Duration) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	created := time.Now().UTC().Add(clockSkew).Format("2006-01-02T15:04:05.000Z")

	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(pass))
	digest := base64.StdEncoding.EncodeToString(h.Sum(nil))

	var b strings.Builder
	b.WriteString(`<wsse:Security s:mustUnderstand="1" xmlns:wsse="` + nsWSSE + `" xmlns:wsu="` + nsWSU + `">`)
	b.WriteString(`<wsse:UsernameToken><wsse:Username>`)
	xml.EscapeText(&b, []byte(user))
	b.WriteString(`</wsse:Username><wsse:Password Type="` + typDigest + `">` + digest + `</wsse:Password>`)
	b.WriteString(`<wsse:Nonce EncodingType="` + encBase64 + `">` + base64.StdEncoding.EncodeToString(nonce) + `</wsse:Nonce>`)
	b.WriteString(`<wsu:Created>` + created + `</wsu:Created>`)
	b.WriteString(`</wsse:UsernameToken></wsse:Security>`)
	return b.String(), nil
}

// envelope wraps a body in a SOAP 1.2 envelope, optionally authenticated.
func envelope(header, body string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="` + nsSOAP + `"`)
	b.WriteString(` xmlns:tds="` + nsDevice + `" xmlns:trt="` + nsMedia + `"`)
	b.WriteString(` xmlns:tptz="` + nsPTZ + `" xmlns:tev="` + nsEvents + `"`)
	b.WriteString(` xmlns:tt="` + nsSchema + `" xmlns:wsa="` + nsAddr + `">`)
	if header != "" {
		b.WriteString(`<s:Header>` + header + `</s:Header>`)
	}
	b.WriteString(`<s:Body>` + body + `</s:Body></s:Envelope>`)
	return b.String()
}

// faultEnvelope is the minimal shape needed to detect and describe a fault.
// Local names only, so it matches regardless of the device's prefixes.
type faultEnvelope struct {
	Fault struct {
		Code struct {
			Value   string `xml:"Value"`
			Subcode struct {
				Value string `xml:"Value"`
			} `xml:"Subcode"`
		} `xml:"Code"`
		Reason struct {
			Text string `xml:"Text"`
		} `xml:"Reason"`
		Detail struct {
			Text string `xml:",chardata"`
		} `xml:"Detail"`
	} `xml:"Body>Fault"`
}

// call posts one SOAP request and unmarshals the response into out.
//
// A device that answers with HTTP 500 and a SOAP fault is answering, not
// failing at the transport level — the fault is parsed either way, so the
// caller gets the device's reason rather than "unexpected status 500".
func (c *client) call(ctx context.Context, url, action, body string, out any) error {
	header := ""
	if c.user != "" {
		tok, err := usernameToken(c.user, c.pass, c.skew())
		if err != nil {
			return err
		}
		header = tok
	}
	payload := envelope(header, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", fmt.Sprintf(`application/soap+xml; charset=utf-8; action="%s"`, action))

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSOAPResponse))
	if err != nil {
		return err
	}

	if bytes.Contains(raw, []byte("Fault")) {
		var fe faultEnvelope
		if xml.Unmarshal(raw, &fe) == nil && (fe.Fault.Reason.Text != "" || fe.Fault.Code.Value != "") {
			code := fe.Fault.Code.Subcode.Value
			if code == "" {
				code = fe.Fault.Code.Value
			}
			return &soapFault{
				Code:   code,
				Reason: strings.TrimSpace(fe.Fault.Reason.Text),
				Detail: strings.TrimSpace(fe.Fault.Detail.Text),
			}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("onvif: %s returned HTTP %d", action, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return xml.Unmarshal(raw, out)
}

// maxSOAPResponse caps how much of a device response we will read. Cameras
// are on the untrusted side of this boundary; an unbounded ReadAll against a
// hostile or broken device is a memory-exhaustion primitive.
const maxSOAPResponse = 4 << 20
