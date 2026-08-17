package onvif

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ANetResearch/ANetCore/effect"
	"github.com/ANetResearch/ANetLink/internal/digest"
	"github.com/ANetResearch/ANetLink/internal/rtsp"
	"github.com/ANetResearch/ANetLink/profile"
	"github.com/ANetResearch/ANetLink/sdk"
)

// maxSnapshot caps a still frame. A 4K JPEG runs ~2 MB; 16 MB leaves room
// without letting a broken camera pull the daemon over.
const maxSnapshot = 16 << 20

// resolveMedia picks the media profile and resolves its stream and snapshot
// URIs. A camera with no media profiles is not a camera we can use, and that
// is an error rather than a device with zero capabilities.
func (a *Adapter) resolveMedia(ctx context.Context, d *device) error {
	var profs profilesResponse
	if err := d.cli.call(ctx, d.mediaX, nsMedia+"/GetProfiles", `<trt:GetProfiles/>`, &profs); err != nil {
		return fmt.Errorf("GetProfiles: %w", err)
	}
	if len(profs.Body.Profiles) == 0 {
		return fmt.Errorf("camera advertises no media profiles")
	}

	d.profTok = profs.Body.Profiles[0].Token
	if want := d.cfg.ProfileToken; want != "" {
		found := false
		for _, p := range profs.Body.Profiles {
			if p.Token == want {
				d.profTok, found = p.Token, true
				break
			}
		}
		if !found {
			// Falling back to the first profile would silently stream the
			// wrong resolution from the wrong lens on a multi-sensor camera.
			return fmt.Errorf("configured profile token %q not offered by camera", want)
		}
	}

	var su streamURIResponse
	body := `<trt:GetStreamUri><trt:StreamSetup>` +
		`<tt:Stream>RTP-Unicast</tt:Stream>` +
		`<tt:Transport><tt:Protocol>RTSP</tt:Protocol></tt:Transport>` +
		`</trt:StreamSetup><trt:ProfileToken>` + d.profTok + `</trt:ProfileToken></trt:GetStreamUri>`
	if err := d.cli.call(ctx, d.mediaX, nsMedia+"/GetStreamUri", body, &su); err == nil {
		d.streamU = strings.TrimSpace(su.Body.MediaURI.URI)
	}

	// Snapshot is optional in Profile S; its absence is not an error.
	var sn snapshotURIResponse
	snBody := `<trt:GetSnapshotUri><trt:ProfileToken>` + d.profTok + `</trt:ProfileToken></trt:GetSnapshotUri>`
	if err := d.cli.call(ctx, d.mediaX, nsMedia+"/GetSnapshotUri", snBody, &sn); err == nil {
		d.snapshot = strings.TrimSpace(sn.Body.MediaURI.URI)
	}

	if d.streamU == "" && d.snapshot == "" {
		return fmt.Errorf("camera offered neither a stream nor a snapshot uri")
	}
	return nil
}

// invokeStream returns the live stream reference.
//
// The two paths differ in what they can honestly claim. Without
// VerifyStream, all we have is the camera's assertion that a URI exists —
// UNVERIFIED, with the reference attached so the caller can still use it.
// With VerifyStream, an RTSP DESCRIBE actually reached the media plane and
// got a session description back, which is verification.
func (a *Adapter) invokeStream(ctx context.Context, d *device) (effect.Effect, error) {
	if d.streamU == "" {
		return sdk.NewEffect().Unavailable("camera has no stream uri").Build(), nil
	}
	b := sdk.NewEffect().
		StreamURI(d.streamU).
		Requested(profile.CapStreamRTSP).
		Protocol("onvif").NativeAck(true).
		Trust(uint8(profile.V1), uint8(profile.A1))

	if !a.cfg.VerifyStream {
		// Deliberately not Verified(): the URI is a claim, not a measurement.
		return b.Observed("uri advertised, stream not probed").Build(), nil
	}

	sdp, err := rtsp.Describe(ctx, a.dialer(), d.streamU, d.cfg.User, d.cfg.Pass)
	if err != nil {
		return sdk.NewEffect().
			StreamURI(d.streamU).
			Requested(profile.CapStreamRTSP).Protocol("onvif").
			Unavailable("stream probe failed: " + err.Error()).Build(), nil
	}
	codec := rtsp.VideoCodec(sdp)
	if codec == "" {
		codec = "unknown"
	}
	return b.Metric("stream_live", 1).
		Observed("RTSP DESCRIBE ok, video codec "+codec).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}

// invokeSnapshot fetches one still frame. Unlike a stream URI, the bytes
// themselves are the evidence: they are content-addressed, so the same frame
// reported twice carries the same id and a truncated transfer cannot pass.
func (a *Adapter) invokeSnapshot(ctx context.Context, d *device) (effect.Effect, error) {
	if d.snapshot == "" {
		return sdk.NewEffect().Unavailable("camera has no snapshot uri").Build(), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.snapshot, nil)
	if err != nil {
		return sdk.NewEffect().Fail(err.Error()).Build(), nil
	}
	cli := &http.Client{
		Timeout:   a.cfg.Timeout,
		Transport: digest.NewTransport(http.DefaultTransport, d.cfg.User, d.cfg.Pass),
	}
	resp, err := cli.Do(req)
	if err != nil {
		return sdk.NewEffect().Unavailable("snapshot fetch: " + err.Error()).Build(), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return sdk.NewEffect().Fail("snapshot rejected: camera did not accept the credentials").Build(), nil
	}
	if resp.StatusCode != http.StatusOK {
		return sdk.NewEffect().Fail(fmt.Sprintf("snapshot returned HTTP %d", resp.StatusCode)).Build(), nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshot))
	if err != nil {
		return sdk.NewEffect().Unavailable("snapshot read: " + err.Error()).Build(), nil
	}
	if len(data) == 0 {
		return sdk.NewEffect().Fail("camera returned an empty frame").Build(), nil
	}
	mt := resp.Header.Get("Content-Type")
	if mt == "" {
		mt = profile.MediaTypeJPEG
	}

	return sdk.NewEffect().
		Snapshot(data, mt).
		Metric("snapshot_bytes", float64(len(data))).
		Requested(profile.CapStreamSnapshot).
		Protocol("onvif").NativeAck(true).
		Observed(fmt.Sprintf("%d bytes of %s", len(data), mt)).
		Trust(uint8(profile.V2), uint8(profile.A1)).
		Verified().Build(), nil
}
