// opus.go implements per-channel Opus audio configuration: the ChannelAudio
// value type and the SDP fmtp rewriting that enforces it (backlog 21-25).
//
// MediaEngine codecs are global in Pion, so per-channel Opus parameters
// cannot be negotiated per subscriber via the MediaEngine. Instead the voice
// facade rewrites the Opus fmtp line of every audio m-section in SDP it
// produces (answers in HandleOffer, server-initiated renegotiation offers)
// according to the subscriber's channel configuration.
package webrtc

import (
	"fmt"
	"strings"
)

// defaultOpusBitrate is the bitrate used when a channel configures no
// explicit value (and the value advertised when munging with Bitrate 0).
const defaultOpusBitrate = 32000

// musicMinBitrate is the bitrate at or above which a stereo channel counts
// as a music channel (25): publishers in music channels bypass the
// talk-power gate.
const musicMinBitrate = 96000

// ChannelAudio is a channel's Opus audio configuration (migration 005). The
// zero value means "server default": no munging happens and the engine's
// registered Opus fmtp (minptime=10;useinbandfec=1) is used as-is.
type ChannelAudio struct {
	// Bitrate is the target Opus bitrate in bits/s (maxaveragebitrate). 0
	// means the default 32000.
	Bitrate int
	// FEC enables Opus in-band forward error correction (useinbandfec).
	FEC bool
	// DTX enables discontinuous transmission (usedtx).
	DTX bool
	// Stereo announces stereo support (stereo=1 in fmtp).
	Stereo bool
}

// IsZero reports whether the configuration is the server default.
func (c ChannelAudio) IsZero() bool {
	return c.Bitrate == 0 && !c.FEC && !c.DTX && !c.Stereo
}

// IsMusic reports whether the configuration is a music channel: stereo at a
// high bitrate (>= 96k). Music channels bypass the talk-power gate.
func (c ChannelAudio) IsMusic() bool {
	return c.Stereo && c.Bitrate >= musicMinBitrate
}

// fmtp builds the Opus fmtp parameter string for SDP munging. minptime=10 is
// preserved from the engine's codec registration.
//
// sprop-stereo mirrors stereo (RFC 7587): stereo=1 only tells the peer the SFU
// is willing to RECEIVE stereo, so with sprop-stereo absent (default 0) the
// peer builds a mono decoder and a music channel plays back mono no matter what
// the publisher sends. The SFU forwards packets untouched, so what it accepts
// is exactly what it emits (25).
func (c ChannelAudio) fmtp() string {
	bitrate := c.Bitrate
	if bitrate <= 0 {
		bitrate = defaultOpusBitrate
	}
	return fmt.Sprintf("minptime=10;maxaveragebitrate=%d;useinbandfec=%s;usedtx=%s;stereo=%s;sprop-stereo=%s",
		bitrate, bool01(c.FEC), bool01(c.DTX), bool01(c.Stereo), bool01(c.Stereo))
}

func bool01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// RewriteOpusFMTP rewrites the Opus fmtp parameters in every audio m-section
// of sdp according to cfg. It is a careful, conservative rewrite:
//
//   - Only a=fmtp lines whose payload type carries an a=rtpmap entry for
//     opus/48000 in the same m-section are touched.
//   - A missing fmtp line for the Opus payload type is inserted right after
//     the corresponding a=rtpmap line.
//   - Non-audio m-sections, non-Opus payload types, and all other lines are
//     passed through byte-identical (line ending style is preserved).
//
// Callers should skip the rewrite entirely when cfg.IsZero() to keep default
// SDP untouched.
func RewriteOpusFMTP(sdp string, cfg ChannelAudio) string {
	eol := "\n"
	if strings.Contains(sdp, "\r\n") {
		eol = "\r\n"
	}
	lines := strings.Split(sdp, eol)

	// Pass 1: locate Opus payload types and their rtpmap/fmtp lines per
	// audio m-section (original indexes; no mutation).
	replace := make(map[int]string)     // fmtp line index -> new content
	insertAfter := make(map[int]string) // rtpmap line index -> fmtp line to insert
	inAudio := false
	opusPTs := make(map[string]bool)
	rtpmapIdx := make(map[string]int)
	fmtpIdx := make(map[string]int)
	flush := func() {
		for pt := range opusPTs {
			newLine := "a=fmtp:" + pt + " " + cfg.fmtp()
			if idx, ok := fmtpIdx[pt]; ok {
				replace[idx] = newLine
			} else if idx, ok := rtpmapIdx[pt]; ok {
				insertAfter[idx] = newLine
			}
		}
		opusPTs = make(map[string]bool)
		rtpmapIdx = make(map[string]int)
		fmtpIdx = make(map[string]int)
	}

	for i, raw := range lines {
		line := strings.TrimSuffix(raw, "\r")
		if strings.HasPrefix(line, "m=") {
			if inAudio {
				flush()
			}
			inAudio = strings.HasPrefix(line, "m=audio")
			continue
		}
		if !inAudio {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "a=rtpmap:"); ok {
			// a=rtpmap:<pt> opus/48000[/2]
			pt, enc, _ := strings.Cut(rest, " ")
			if strings.HasPrefix(enc, "opus/48000") {
				opusPTs[pt] = true
				rtpmapIdx[pt] = i
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "a=fmtp:"); ok {
			pt, _, _ := strings.Cut(rest, " ")
			if opusPTs[pt] {
				fmtpIdx[pt] = i
			}
		}
	}
	if inAudio {
		flush()
	}

	// Pass 2: rebuild with replacements and insertions.
	if len(replace) == 0 && len(insertAfter) == 0 {
		return sdp
	}
	out := make([]string, 0, len(lines)+len(insertAfter))
	for i, raw := range lines {
		if newLine, ok := replace[i]; ok {
			out = append(out, newLine)
		} else {
			out = append(out, raw)
		}
		if newLine, ok := insertAfter[i]; ok {
			out = append(out, newLine)
		}
	}
	return strings.Join(out, eol)
}
