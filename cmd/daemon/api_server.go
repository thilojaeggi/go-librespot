package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	librespot "go-librespot"
	"net"
	"net/http"
	"strings"
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

func NewApiResponseStatusTrack(media *librespot.Media, prodInfo *ProductInfo, position int64) *ApiResponseStatusTrack {
	if media.IsTrack() {
		track := media.Track()
		var artists []string
		for _, a := range track.Artist {
			artists = append(artists, *a.Name)
		}

		dateString := track.Album.Date.String() // assuming this is a valid date string format
		parts := strings.Fields(dateString)     // Split by spaces
		if len(parts) < 3 {
			log.Fatalf("Invalid date format")
		}

		// Remove the prefixes (e.g., "year:", "month:", "day:")
		year := strings.Split(parts[0], ":")[1]
		month := strings.Split(parts[1], ":")[1]
		day := strings.Split(parts[2], ":")[1]

		// Create a valid date string in the format "YYYY-MM-DD"
		formattedDate := fmt.Sprintf("%s-%02s-%02s", year, month, day)

		// Now parse the formatted date string
		parsedDate, err := time.Parse("2006-01-02", formattedDate)
		if err != nil {
			log.Fatalf("Error parsing date: %v", err)
		}

		return &ApiResponseStatusTrack{
			Uri:         librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.Gid).Uri(),
			Name:        *track.Name,
			ArtistNames: artists,
			AlbumName:   *track.Album.Name,
			TrackCoverUrl: func() []string {
				var urls []string
				for _, cover := range track.Album.Cover {
					urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(cover.FileId)))
				}
				return urls
			}(),
			AlbumCoverUrl: func() []string {
				var urls []string
				if track.Album.CoverGroup != nil {
					for _, image := range track.Album.CoverGroup.Image {
						urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(image.FileId)))
					}
				}
				return urls
			}(),
			Position:    position,
			Duration:    int(*track.Duration),
			ReleaseDate: parsedDate,
			TrackNumber: int(*track.Number),
			DiscNumber:  int(*track.DiscNumber),
			HasLyrics:   *track.HasLyrics,
		}
	} else {
		episode := media.Episode()

		var albumCoverId string
		if len(episode.CoverImage.Image) > 0 {
			albumCoverId = hex.EncodeToString(episode.CoverImage.Image[len(episode.CoverImage.Image)-1].FileId)
		}

		return &ApiResponseStatusTrack{
			Uri:           librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeEpisode, episode.Gid).Uri(),
			Name:          *episode.Name,
			ArtistNames:   []string{*episode.Show.Name},
			AlbumName:     *episode.Show.Name,
			AlbumCoverUrl: []string{prodInfo.ImageUrl(albumCoverId)},
			Position:      position,
			Duration:      int(*episode.Duration),
			ReleaseDate:   time.Now(),
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
		artists = append(artists, *a.Name)
	}

	dateString := album.Date.String()   // assuming this is a valid date string format
	parts := strings.Fields(dateString) // Split by spaces
	if len(parts) < 3 {
		log.Fatalf("Invalid date format")
	}

	// Remove the prefixes (e.g., "year:", "month:", "day:")
	year := strings.Split(parts[0], ":")[1]
	month := strings.Split(parts[1], ":")[1]
	day := strings.Split(parts[2], ":")[1]

	// Create a valid date string in the format "YYYY-MM-DD"
	formattedDate := fmt.Sprintf("%s-%02s-%02s", year, month, day)

	// Now parse the formatted date string
	parsedDate, err := time.Parse("2006-01-02", formattedDate)

	if err != nil {
		log.Fatalf("Error parsing date: %v", err)
	}

	var tracks []ApiResponseStatusTrack

	// Get all tracks in the album from all discs
	for _, disc := range album.GetDisc() {
		for _, track := range disc.GetTrack() {
			// Skip placeholder tracks
			if track.GetName() == "" || track.GetDuration() == 0 {
				continue
			}
			artists := []string{}
			for _, artist := range track.GetArtist() {
				artists = append(artists, artist.GetName())
			}

			tracks = append(tracks, ApiResponseStatusTrack{
				Uri:         librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeTrack, track.GetGid()).Uri(),
				Name:        track.GetName(),
				ArtistNames: artists,
				AlbumName:   album.GetName(),
				TrackCoverUrl: func() []string {
					var urls []string
					for _, cover := range track.GetAlbum().GetCover() {
						urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(cover.GetFileId())))
					}
					return urls
				}(),
				AlbumCoverUrl: func() []string {
					var urls []string
					if track.GetAlbum().GetCoverGroup() != nil {
						for _, image := range track.GetAlbum().GetCoverGroup().GetImage() {
							urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(image.GetFileId())))
						}
					}
					return urls
				}(),
				Position:    int64(track.GetNumber()),
				Duration:    int(track.GetDuration()),
				ReleaseDate: parsedDate,
				TrackNumber: int(track.GetNumber()),
				DiscNumber:  int(disc.GetNumber()),
			})
		}
	}
	log.Info(tracks)

	return &ApiResponseStatusAlbum{
		Uri:         librespot.SpotifyIdFromGid(librespot.SpotifyIdTypeAlbum, album.Gid).Uri(),
		Name:        *album.Name,
		ArtistNames: artists,
		AlbumCoverUrl: func() []string {
			var urls []string
			if album.CoverGroup != nil {
				for _, image := range album.CoverGroup.Image {
					urls = append(urls, prodInfo.ImageUrl(hex.EncodeToString(image.FileId)))
				}
			}
			return urls
		}(),
		ReleaseDate: parsedDate,
		Tracks:      tracks,
		TrackCount:  len(tracks),
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

func NewApiServer(address string, port int, allowOrigin string) (_ *ApiServer, err error) {
	s := &ApiServer{allowOrigin: allowOrigin}
	s.requests = make(chan ApiRequest)

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
		log.WithError(resp.err).Error("failed handling status request")
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

		// add the client to the list
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

				// remove the client from the list
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
			// purposely do not propagate this to the caller
			log.WithError(err).Error("failed communicating with websocket client")
		}
	}
}

func (s *ApiServer) Receive() <-chan ApiRequest {
	return s.requests
}

func (s *ApiServer) Close() {
	s.close = true

	// close all websocket clients
	s.clientsLock.RLock()
	for _, client := range s.clients {
		_ = client.Close(websocket.StatusGoingAway, "")
	}
	s.clientsLock.RUnlock()

	// close the listener
	_ = s.listener.Close()
}
