package go_librespot

import (
	"errors"
	metadatapb "go-librespot/proto/spotify/metadata"
)

var (
	ErrMediaRestricted    = errors.New("media is restricted")
	ErrNoSupportedFormats = errors.New("no supported formats")
)

type Media struct {
	track   *metadatapb.Track
	album   *metadatapb.Album
	episode *metadatapb.Episode
}

func NewMediaFromTrack(track *metadatapb.Track) *Media {
	if track == nil {
		panic("nil track")
	}

	return &Media{track: track, episode: nil}
}

func NewMediaFromEpisode(episode *metadatapb.Episode) *Media {
	if episode == nil {
		panic("nil episode")
	}

	return &Media{track: nil, episode: episode}
}

func NewMediaFromAlbum(album *metadatapb.Album) *Media {
	if album == nil {
		panic("nil album")
	}

	return &Media{track: nil, album: album}
}

func (te Media) IsTrack() bool {
	return te.track != nil
}

func (te Media) IsAlbum() bool {
	return te.album != nil
}

func (te Media) IsEpisode() bool {
	return te.episode != nil
}

func (te Media) Track() *metadatapb.Track {
	if te.track == nil {
		panic("not a track")
	}

	return te.track
}

func (te Media) Album() *metadatapb.Album {
	if te.album == nil {
		panic("not an album")
	}

	return te.album
}

func (te Media) Episode() *metadatapb.Episode {
	if te.episode == nil {
		panic("not an episode")
	}

	return te.episode
}

func (te Media) Name() string {
	if te.track != nil {
		return *te.track.Name
	} else {
		return *te.episode.Name
	}
}

func (te Media) Duration() int32 {
	if te.track != nil {
		return *te.track.Duration
	} else {
		return *te.episode.Duration
	}
}

func (te Media) Restriction() []*metadatapb.Restriction {
	if te.track != nil {
		return te.track.Restriction
	} else {
		return te.episode.Restriction
	}
}
