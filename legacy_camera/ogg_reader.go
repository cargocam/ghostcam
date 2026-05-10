package main

import (
	"fmt"
	"io"
)

// ReadOggOpusPackets reads an OGG bitstream from r and calls onPacket
// for each Opus audio packet extracted. The first two pages (OpusHead
// and OpusTags headers) are skipped automatically.
//
// This is a minimal OGG page parser — just enough to extract Opus
// packets from ffmpeg's `-f ogg -c:a libopus` output. It does not
// validate CRCs or handle chained streams.
func ReadOggOpusPackets(r io.Reader, onPacket func(data []byte)) error {
	headerPages := 0

	for {
		packets, err := readOggPage(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		// Skip the first two pages (OpusHead + OpusTags).
		if headerPages < 2 {
			headerPages++
			continue
		}

		for _, pkt := range packets {
			if len(pkt) > 0 {
				onPacket(pkt)
			}
		}
	}
}

// readOggPage reads one OGG page and returns the extracted packets.
func readOggPage(r io.Reader) ([][]byte, error) {
	// OGG page header is 27 bytes fixed.
	var hdr [27]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}

	if hdr[0] != 'O' || hdr[1] != 'g' || hdr[2] != 'g' || hdr[3] != 'S' {
		return nil, fmt.Errorf("ogg: bad capture pattern: %x", hdr[:4])
	}

	numSegments := int(hdr[26])

	// Read segment table (one byte per segment, value = segment length).
	segTable := make([]byte, numSegments)
	if numSegments > 0 {
		if _, err := io.ReadFull(r, segTable); err != nil {
			return nil, fmt.Errorf("ogg: reading segment table: %w", err)
		}
	}

	// Read all segment data.
	var totalSize int
	for _, s := range segTable {
		totalSize += int(s)
	}
	data := make([]byte, totalSize)
	if totalSize > 0 {
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("ogg: reading page data: %w", err)
		}
	}

	// Reassemble packets from segments. A segment < 255 bytes
	// terminates a packet. A segment of exactly 255 bytes means
	// the packet continues in the next segment.
	var packets [][]byte
	var current []byte
	offset := 0
	for _, segLen := range segTable {
		current = append(current, data[offset:offset+int(segLen)]...)
		offset += int(segLen)
		if segLen < 255 {
			packets = append(packets, current)
			current = nil
		}
	}
	// Partial packet (last segment was 255) spans to next page — discard.
	// Not expected for Opus at 20ms frame size (<255 bytes per frame).

	return packets, nil
}
