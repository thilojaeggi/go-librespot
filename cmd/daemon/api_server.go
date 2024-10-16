package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/cors"

	librespot "github.com/devgianlu/go-librespot"
	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
	log "github.com/sirupsen/logrus"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const timeout = 10 * time.Second

type ApiServer struct {
	allowOrigin string
	certFile    string
	keyFile     string

	close    bool
	listener net.Listener

	requests chan ApiRequest

	clients     []*websocket.Conn
	clientsLock sync.RWMutex
}

var (
	ErrNoSession        = errors.New("no session")
	ErrBadRequest       = errors.New("bad request")
	ErrForbidden        = errors.New("forbidden")
	ErrNotFound         = errors.New("not found")
	ErrMethodNotAllowed = errors.New("method not allowed")
	ErrTooManyRequests  = errors.New("the app has exceeded its rate limits")
)

type ApiRequestType string

const (
	ApiRequestTypeWebApi              ApiRequestType = "web_api"
	ApiRequestTypeStatus              ApiRequestType = "status"
	ApiRequestTypeResume              ApiRequestType = "resume"
	ApiRequestTypePause               ApiRequestType = "pause"
	ApiRequestTypePlayPause           ApiRequestType = "playpause"
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
	ApiRequestTypeToken               ApiRequestType = "token"
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

type ApiRequestDataSeek struct {
	Position int64 `json:"position"`
	Relative bool  `json:"relative"`
}

type ApiRequestDataVolume struct {
	Volume   int32 `json:"volume"`
	Relative bool  `json:"relative"`
}

type ApiRequestDataWebApi struct {
	Method string
	Path   string
	Query  url.Values
}

type ApiRequestDataPlay struct {
	Uri       string `json:"uri"`
	SkipToUri string `json:"skip_to_uri"`
	Paused    bool   `json:"paused"`
}

type ApiRequestDataNext struct {
	Uri *string `json:"uri"`
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

type ApiResponseToken struct {
	Token string `json:"token"`
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

func NewApiServer(address string, port int, allowOrigin string, certFile string, keyFile string) (_ *ApiServer, err error) {
	s := &ApiServer{allowOrigin: allowOrigin, certFile: certFile, keyFile: keyFile}
	s.requests = make(chan ApiRequest)

	s.listener, err = net.Listen("tcp", fmt.Sprintf("%s:%d", address, port))
	if err != nil {
		return nil, fmt.Errorf("failed starting api listener: %w", err)
	}

	log.Infof("api server listening on %s", s.listener.Addr())

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

	if resp.err != nil {
		switch {
		case errors.Is(resp.err, ErrNoSession):
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(resp.err, ErrForbidden):
			w.WriteHeader(http.StatusForbidden)
			return
		case errors.Is(resp.err, ErrNotFound):
			w.WriteHeader(http.StatusNotFound)
			return
		case errors.Is(resp.err, ErrMethodNotAllowed):
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		case errors.Is(resp.err, ErrTooManyRequests):
			w.WriteHeader(http.StatusTooManyRequests)
			return
		case errors.Is(resp.err, ErrBadRequest):
			w.WriteHeader(http.StatusBadRequest)
			return
		default:
			log.WithError(resp.err).Errorf("failed handling request %s", req.Type)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	switch respData := resp.data.(type) {
	case []byte:
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(respData)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(respData)
	}
}

func (s *ApiServer) serve() {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	m.Handle("/web-api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleRequest(ApiRequest{
			Type: ApiRequestTypeWebApi,
			Data: ApiRequestDataWebApi{
				Method: r.Method,
				Path:   strings.TrimPrefix(r.URL.Path, "/web-api/"),
				Query:  r.URL.Query(),
			},
		}, w)
	}))
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

		if len(data.Uri) == 0 {
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
	m.HandleFunc("/player/playpause", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypePlayPause}, w)
	})
	m.HandleFunc("/player/next", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var data ApiRequestDataNext
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeNext, Data: data}, w)
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

		var data ApiRequestDataSeek
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if !data.Relative && data.Position < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeSeek, Data: data}, w)
	})
	m.HandleFunc("/player/volume", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			s.handleRequest(ApiRequest{Type: ApiRequestTypeGetVolume}, w)
		} else if r.Method == "POST" {
			var data ApiRequestDataVolume
			if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !data.Relative && data.Volume < 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			s.handleRequest(ApiRequest{Type: ApiRequestTypeSetVolume, Data: data}, w)
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

		if len(data.Uri) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeAddToQueue, Data: data.Uri}, w)
	})
	m.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		s.handleRequest(ApiRequest{Type: ApiRequestTypeToken}, w)
	})
	m.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		opts := &websocket.AcceptOptions{}
		if len(s.allowOrigin) > 0 {
			allow := s.allowOrigin
			allow = strings.TrimPrefix(allow, "http://")
			allow = strings.TrimPrefix(allow, "https://")
			allow = strings.TrimSuffix(allow, "/")
			opts.OriginPatterns = []string{allow}
		}

		c, err := websocket.Accept(w, r, opts)
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

	c := cors.New(cors.Options{
		AllowedOrigins:      []string{s.allowOrigin},
		AllowPrivateNetwork: true,
		AllowCredentials:    true,
	})

	var err error
	if len(s.certFile) > 0 && len(s.keyFile) > 0 {
		err = http.ServeTLS(s.listener, c.Handler(m), s.certFile, s.keyFile)
	} else {
		err = http.Serve(s.listener, c.Handler(m))
	}

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
