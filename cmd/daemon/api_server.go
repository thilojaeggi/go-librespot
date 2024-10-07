package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	librespot "go-librespot"
	metadatapb "go-librespot/proto/spotify/metadata"
	"net"
	"net/http"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const timeout = 10 * time.Second

type ApiServer struct {
	allowOrigin string

	close    bool
	listener net.Listener

	requests chan ApiRequest

	clients     []*websocket.Conn
	clientsLock sync.RWMutex
}

var ErrNoSession = errors.New("no session")

type ApiRequestType string

const (
	ApiRequestTypeStatus              ApiRequestType = "status"
	ApiRequestTypeResume              ApiRequestType = "resume"
	ApiRequestTypePause               ApiRequestType = "pause"
	ApiRequestTypeSeek                ApiRequestType = "seek"
	ApiRequestTypePrev                ApiRequestType = "prev"
	ApiRequestTypeNext                ApiRequestType = "next"
	ApiRequestTypePlay                ApiRequestType = "play"
	ApiRequestTypeGetVolume           ApiRequestType = "get_volume"
	ApiRequestTypeSetVolume           ApiRequestType = "set_volume"
	ApiRequestTypeSetRepeatingContext ApiRequestType = "repeating_context"
	ApiRequestTypeSetRepeatingTrack   ApiRequestType = "repeating_track"
	ApiRequestTypeSetShufflingContext ApiRequestType = "shuffling_context"
	ApiRequestTypeAddToQueue          ApiRequestType = "add_to_queue"
)

type ApiEventType string

const (
	ApiEventTypePlaying        ApiEventType = "playing"
	ApiEventTypeNotPlaying     ApiEventType = "not_playing"
	ApiEventTypeWillPlay       ApiEventType = "will_play"
	ApiEventTypePaused         ApiEventType = "paused"
	ApiEventTypeActive         ApiEventType = "active"
	ApiEventTypeInactive       ApiEventType = "inactive"
	ApiEventTypeMetadata       ApiEventType = "metadata"
	ApiEventTypeAlbumMetadata  ApiEventType = "album_metadata"
	ApiEventTypeVolume         ApiEventType = "volume"
	ApiEventTypeSeek           ApiEventType = "seek"
	ApiEventTypeStopped        ApiEventType = "stopped"
	ApiEventTypeRepeatTrack    ApiEventType = "repeat_track"
	ApiEventTypeRepeatContext  ApiEventType = "repeat_context"
	ApiEventTypeShuffleContext ApiEventType = "shuffle_context"
)

type ApiRequest struct {
	Type ApiRequestType
	Data any

	resp chan apiResponse
}

func (r *ApiRequest) Reply(data any, err error) {
	r.resp <- apiResponse{data, err}
}

type ApiRequestDataPlay struct {
	Uri       string `json:"uri"`
	SkipToUri string `json:"skip_to_uri"`
	Paused    bool   `json:"paused"`
}

type apiResponse struct {
	data any
	err  error
}

type ApiResponseStatusTrack struct {
	Uri           string    `json:"uri"`
	Name          string    `json:"name"`
	ArtistNames   []string  `json:"artist_names"`
	AlbumName     string    `json:"album_name"`
	TrackCoverUrl []string  `json:"track_cover_url"`
	AlbumCoverUrl []string  `json:"album_cover_url"`
	Position      int64     `json:"position"`
	Duration      int       `json:"duration"`
	ReleaseDate   time.Time `json:"release_date"`
	TrackNumber   int       `json:"track_number"`
	DiscNumber    int       `json:"disc_number"`
	HasLyrics     bool      `json:"has_lyrics"`
}

type ApiResponseStatusAlbum struct {
	Uri           string                   `json:"uri"`
	Name          string                   `json:"name"`
	ArtistNames   []string                 `json:"artist_names"`
	AlbumCoverUrl []string                 `json:"album_cover_url"`
	ReleaseDate   time.Time                `json:"release_date"`
	Tracks        []ApiResponseStatusTrack `json:"tracks"`
	TrackCount    int                      `json:"track_count"`
}

func parseDateString(dateString string) (time.Time, error) {
	var year, month, day int
	n, err := fmt.Sscanf(dateString, "year:%d month:%d day:%d", &year, &month, &day)
	if err != nil || n != 3 {
		return time.Time{}, fmt.Errorf("failed to parse date: %v", err)
	}
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), nil
}

func derefString(s *string) string {
	if s != nil {
		return *s
	}
	return ""
}

func derefUint32(u *uint32) uint32 {
	if u != nil {
		return *u
	}
	return 0
}

func derefInt32(i *int32) int32 {
	if i != nil {
		return *i
	}
	return 0
}

func derefInt64(i *int64) int64 {
	if i != nil {
		return *i
	}
	return 0
}

func derefUint64(u *uint64) uint64 {
	if u != nil {
		return *u
	}
	return 0
}

func extractCoverUrls(covers []*metadatapb.Image, prodInfo *ProductInfo) []string {
	var urls []string
	for _, cover := range covers {
		if cover != nil && cover.FileId != nil {
			urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(cover.FileId)))
		}
	}
	return urls
}

func extractCoverGroupUrls(group *metadatapb.ImageGroup, prodInfo *ProductInfo) []string {
	var urls []string
	if group != nil {
		for _, image := range group.Image {
			if image.FileId != nil {
				urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(image.FileId)))
			}
		}
	}
	return urls
}
func NewApiResponseStatusTrack(media *librespot.Media, prodInfo *ProductInfo, position int64) *ApiResponseStatusTrack {
	if media.IsTrack() {
		track := media.Track()
		var artists []string
		for _, a := range track.Artist {
			if a.Name != nil {
				artists = append(artists, *a.Name)
			}
		}

		var parsedDate time.Time
		dateString := track.Album.Date.String()
		if dateString != "" {
			date, err := parseDateString(dateString)
			if err != nil {
				log.WithError(err).Warn("Error parsing date")
			} else {
				parsedDate = date
			}
		}

		return &ApiResponseStatusTrack{
			Uri:           librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.Gid).Uri(),
			Name:          derefString(track.Name),
			ArtistNames:   artists,
			AlbumName:     derefString(track.Album.Name),
			TrackCoverUrl: extractCoverUrls(track.Album.Cover, prodInfo),
			AlbumCoverUrl: extractCoverGroupUrls(track.Album.CoverGroup, prodInfo),
			Position:      position,
			Duration:      int(derefInt32(track.Duration)), // Use derefInt32
			ReleaseDate:   parsedDate,
			TrackNumber:   int(derefInt32(track.Number)),     // Use derefInt32
			DiscNumber:    int(derefInt32(track.DiscNumber)), // Use derefInt32
			HasLyrics:     track.GetHasLyrics(),
		}
	} else {
		episode := media.Episode()

		var parsedDate time.Time
		dateString := episode.PublishTime.String()
		if dateString != "" {
			date, err := parseDateString(dateString)
			if err != nil {
				log.WithError(err).Warn("Error parsing date")
			} else {
				parsedDate = date
			}
		}

		var albumCoverId string
		if len(episode.CoverImage.Image) > 0 {
			albumCoverId = hex.EncodeToString(episode.CoverImage.Image[len(episode.CoverImage.Image)-1].FileId)
		}

		return &ApiResponseStatusTrack{
			Uri:           librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeEpisode, episode.Gid).Uri(),
			Name:          derefString(episode.Name),
			ArtistNames:   []string{derefString(episode.Show.Name)},
			AlbumName:     derefString(episode.Show.Name),
			AlbumCoverUrl: []string{prodInfo.ImageUrl(albumCoverId)},
			Position:      position,
			Duration:      int(derefInt32(episode.Duration)), // Use derefInt32
			ReleaseDate:   parsedDate,
			TrackNumber:   0,
			DiscNumber:    0,
			HasLyrics:     false,
		}
	}
}
func NewApiResponseStatusAlbum(media *librespot.Media, prodInfo *ProductInfo) *ApiResponseStatusAlbum {
	album := media.Album()

	var artists []string
	for _, a := range album.Artist {
		artists = append(artists, derefString(a.Name))
	}

	var parsedDate time.Time
	dateString := album.Date.String()
	if dateString != "" {
		date, err := parseDateString(dateString)
		if err != nil {
			log.WithError(err).Warn("Error parsing date")
		} else {
			parsedDate = date
		}
	}

	var tracks []ApiResponseStatusTrack
	for _, disc := range album.GetDisc() {
		for _, track := range disc.GetTrack() {
			if track.GetName() == "" || track.GetDuration() == 0 {
				continue
			}
			var trackArtists []string
			for _, artist := range track.GetArtist() {
				trackArtists = append(trackArtists, derefString(artist.Name))
			}

			tracks = append(tracks, ApiResponseStatusTrack{
				Uri:           librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.GetGid()).Uri(),
				Name:          derefString(track.Name),
				ArtistNames:   trackArtists,
				AlbumName:     derefString(album.Name),
				TrackCoverUrl: extractCoverUrls(track.GetAlbum().GetCover(), prodInfo),
				AlbumCoverUrl: extractCoverGroupUrls(track.GetAlbum().GetCoverGroup(), prodInfo),
				Position:      int64(derefInt32(track.Number)), // Use derefInt32
				Duration:      int(derefInt32(track.Duration)), // Use derefInt32
				ReleaseDate:   parsedDate,
				TrackNumber:   int(derefInt32(track.Number)), // Use derefInt32
				DiscNumber:    int(derefInt32(disc.Number)),  // Use derefInt32
				HasLyrics:     track.GetHasLyrics(),
			})
		}
	}

	return &ApiResponseStatusAlbum{
		Uri:           librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeAlbum, album.Gid).Uri(),
		Name:          derefString(album.Name),
		ArtistNames:   artists,
		AlbumCoverUrl: extractCoverGroupUrls(album.CoverGroup, prodInfo),
		ReleaseDate:   parsedDate,
		Tracks:        tracks,
		TrackCount:    len(tracks),
	}
}

type ApiResponseStatus struct {
	Username       string                  `json:"username"`
	DeviceId       string                  `json:"device_id"`
	DeviceType     string                  `json:"device_type"`
	DeviceName     string                  `json:"device_name"`
	PlayOrigin     string                  `json:"play_origin"`
	Stopped        bool                    `json:"stopped"`
	Paused         bool                    `json:"paused"`
	Buffering      bool                    `json:"buffering"`
	Volume         uint32                  `json:"volume"`
	VolumeSteps    uint32                  `json:"volume_steps"`
	RepeatContext  bool                    `json:"repeat_context"`
	RepeatTrack    bool                    `json:"repeat_track"`
	ShuffleContext bool                    `json:"shuffle_context"`
	Track          *ApiResponseStatusTrack `json:"track"`
}

type ApiResponseVolume struct {
	Value uint32 `json:"value"`
	Max   uint32 `json:"max"`
}

type ApiEvent struct {
	Type ApiEventType `json:"type"`
	Data any          `json:"data"`
}

type ApiEventDataMetadata ApiResponseStatusTrack

type ApiEventDataAlbumMetadata ApiResponseStatusAlbum

type ApiEventDataVolume ApiResponseVolume

type ApiEventDataPlaying struct {
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataWillPlay struct {
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataNotPlaying struct {
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataPaused struct {
	Uri        string `json:"uri"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataStopped struct {
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataSeek struct {
	Uri        string `json:"uri"`
	Position   int    `json:"position"`
	Duration   int    `json:"duration"`
	PlayOrigin string `json:"play_origin"`
}

type ApiEventDataRepeatTrack struct {
	Value bool `json:"value"`
}

type ApiEventDataRepeatContext struct {
	Value bool `json:"value"`
}

type ApiEventDataShuffleContext struct {
	Value bool `json:"value"`
}

func NewApiServer(address string, port int, allowOrigin string) (*ApiServer, error) {
	s := &ApiServer{allowOrigin: allowOrigin}
	s.requests = make(chan ApiRequest)

	var err error
	s.listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, fmt.Errorf("failed starting api listener: %w", err)
	}

	go s.serve()
	return s, nil
}

func NewStubApiServer() (*ApiServer, error) {
	s := &ApiServer{}
	s.requests = make(chan ApiRequest)
	return s, nil
}

func (s *ApiServer) handleRequest(req ApiRequest, w http.ResponseWriter) {
	req.resp = make(chan apiResponse, 1)
	s.requests <- req
	resp := <-req.resp
	if errors.Is(resp.err, ErrNoSession) {
		w.WriteHeader(http.StatusNoContent)
		return
	} else if resp.err != nil {
		log.WithError(resp.err).Error("failed handling request")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp.data)
}

func (s *ApiServer) allowOriginMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if len(s.allowOrigin) > 0 {
			w.Header().Set("Access-Control-Allow-Origin", s.allowOrigin)
		}

		next.ServeHTTP(w, req)
	})
}

func (s *ApiServer) serve() {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	m.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeStatus}, w)
	})
	m.HandleFunc("/player/play", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataPlay
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePlay, Data: data}, w)
	})
	m.HandleFunc("/player/resume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeResume}, w)
	})
	m.HandleFunc("/player/pause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePause}, w)
	})
	m.HandleFunc("/player/next", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeNext}, w)
	})
	m.HandleFunc("/player/prev", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePrev}, w)
	})
	m.HandleFunc("/player/seek", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Position int64 `json:"position"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSeek, Data: data.Position}, w)
	})
	m.HandleFunc("/player/volume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			s.handleRequest(ApiRequest{Type: ApiRequestTypeGetVolume}, w)
		} else if r.Method == "POST" {
			var data struct {
				Volume uint32 `json:"volume"`
			}
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			s.handleRequest(ApiRequest{Type: ApiRequestTypeSetVolume, Data: data.Volume}, w)
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	m.HandleFunc("/player/repeat_context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Repeat bool `json:"repeat_context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetRepeatingContext, Data: data.Repeat}, w)
	})
	m.HandleFunc("/player/repeat_track", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Repeat bool `json:"repeat_track"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetRepeatingTrack, Data: data.Repeat}, w)
	})
	m.HandleFunc("/player/shuffle_context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Shuffle bool `json:"shuffle_context"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSetShufflingContext, Data: data.Shuffle}, w)
	})
	m.HandleFunc("/player/add_to_queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data struct {
			Uri string `json:"uri"`
		}
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeAddToQueue, Data: data.Uri}, w)
	})
	m.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.WithError(err).Error("failed accepting websocket connection")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		s.clientsLock.Lock()
		s.clients = append(s.clients, c)
		s.clientsLock.Unlock()

		log.Debugf("new websocket client")

		for {
			_, _, err := c.Read(context.Background())
			if s.close {
				return
			} else if err != nil {
				log.WithError(err).Error("websocket connection errored")

				s.clientsLock.Lock()
				for i, cc := range s.clients {
					if cc == c {
						s.clients = append(s.clients[:i], s.clients[i+1:]...)
						break
					}
				}
				s.clientsLock.Unlock()
				return
			}
		}
	})

	err := http.Serve(s.listener, s.allowOriginMiddleware(m))
	if s.close {
		return
	} else if err != nil {
		log.WithError(err).Fatal("failed serving api")
	}
}

func (s *ApiServer) Emit(ev *ApiEvent) {
	s.clientsLock.RLock()
	defer s.clientsLock.RUnlock()

	log.Tracef("emitting websocket event: %s", ev.Type)

	for _, client := range s.clients {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := wsjson.Write(ctx, client, ev)
		cancel()
		if err != nil {
			log.WithError(err).Error("failed communicating with websocket client")
		}
	}
}

func (s *ApiServer) Receive() <-chan ApiRequest {
	return s.requests
}

func (s *ApiServer) Close() {
	s.close = true

	s.clientsLock.RLock()
	for _, client := range s.clients {
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	s.clientsLock.RUnlock()

	_ = s.listener.Close()
}
