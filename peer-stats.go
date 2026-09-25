package torrent

import "time"

type PeerStats struct {
	ConnStats

	DownloadRate        float64
	LastWriteUploadRate float64
	// How many pieces the peer has.
	RemotePieceCount int
	// The peer is choking us, and since when.
	PeerChoking      bool
	PeerChokingSince time.Time
}
