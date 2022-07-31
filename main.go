package main

import (
	"context"
	"fmt"
	"github.com/alecthomas/kong"
	delugeclient "github.com/gdm85/go-libdeluge"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golift.io/starr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
	"os"
	"time"
)

func main() {
	s, cleanup := New()
	defer func() {
		if err := cleanup(); err != nil {
			fmt.Printf("Error in cleanup %s \n", err.Error())
		}
	}()
	s.log.Info().Msg("Init")

	//done := make(chan bool)
	done := s.Run()
	<-done
}

type Service struct {
	log    zerolog.Logger
	deluge delugeclient.Client
	sonarr *sonarr.Sonarr
	radarr *radarr.Radarr
	config Config
}

type Config struct {
	DelugeHost     string `env:"DELUGE_HOST" default:"localhost"`
	DelugePort     uint   `env:"DELUGE_PORT" default:"58846"`
	DelugeUsername string `env:"DELUGE_USERNAME" default:"nobody"`
	DelugePassword string `env:"DELUGE_PASSWORD" default:"deluge"`

	EnableSonarr bool   `env:"SONARR_ENABLED"`
	SonarrAPIKey string `env:"SONARR_API_KEY"`
	SonarrURL    string `env:"SONARR_URL" default:"http://localhost:8989"`

	EnableRadarr bool   `env:"RADARR_ENABLED"`
	RadarrAPIKey string `env:"RADARR_API_KEY" default:"deluge"`
	RadarrURL    string `env:"RADARR_URL"  default:"http://localhost:7878"`

	OnlyLabels      []string      `env:"ONLY_LABELS" sep:","`
	RefreshDuration time.Duration `env:"REFRESH_DURATION" default:"10m" type:"time.Duration"`
	StallDuration   time.Duration `env:"STALL_DURATION" default:"1h" type:"time.Duration"`

	Pretend      bool `env:"PRETEND" default:"false"`
	RunOnStartup bool `env:"RUN_ON_STARTUP" default:"true"`
	Debug        bool `env:"DEBUG" default:"false"`
}

func New() (*Service, func() error) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	if err := godotenv.Load(); err != nil {
		log.Warn().Msg("failed to load .env. reading env vars")
	}
	config := Config{}
	kong.Parse(&config)
	if config.Debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}
	deluge := delugeclient.NewV2(delugeclient.Settings{
		Hostname:         config.DelugeHost,
		Port:             config.DelugePort,
		Login:            config.DelugeUsername,
		ReadWriteTimeout: time.Minute * 2,
		Password:         config.DelugePassword,
	})

	sonarrConfig := starr.New(config.SonarrAPIKey, config.SonarrURL, 0)
	radarrConfig := starr.New(config.RadarrAPIKey, config.RadarrURL, 0)

	return &Service{
			log:    zerolog.New(os.Stdout),
			deluge: deluge.Client,
			sonarr: sonarr.New(sonarrConfig),
			radarr: radarr.New(radarrConfig),
			config: config,
		}, func() error {
			return deluge.Close()
		}
}

func (s *Service) Run() chan struct{} {
	ticker := time.NewTicker(s.config.RefreshDuration)
	quit := make(chan struct{})
	if s.config.RunOnStartup {
		s.Process()
	}
	go func() {
		for {
			select {
			case <-ticker.C:
				s.Process()
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()
	return quit
}

func (s *Service) Process() {
	s.log.Debug().Msgf("Starting process")
	if err := s.deluge.Connect(); err != nil {
		s.log.Fatal().Err(err)
	}
	allStalled, err := s.GetStalledTorrents()
	if err != nil {
		s.log.Fatal().Err(err)
	}
	for k := range allStalled {
		s.log.Debug().Msgf("Target stalled torrent %s", k)
	}
	var processErr error
	allStalled, processErr = s.SonarrDelete(allStalled)
	if processErr != nil {
		s.log.Fatal().Err(err)
	}
	_, processErr = s.RadarrDelete(allStalled)
	if processErr != nil {
		s.log.Fatal().Err(err)
	}
}

func (s *Service) SonarrDelete(stalled map[string]*delugeclient.TorrentStatus) (map[string]*delugeclient.TorrentStatus, error) {
	if !s.config.EnableSonarr {
		return stalled, nil
	}
	s.log.Debug().Msgf("Starting Sonarr processing")
	q, err := s.sonarr.GetQueue(0, 0)
	if err != nil {
		return nil, err
	}
	for _, record := range q.Records {
		for k, v := range stalled {
			if record.Title == v.Name {
				if s.config.Pretend {
					s.log.Info().Msgf("PRETEND: Will delete %s from sonarr", v.Name)
					continue
				}
				if err := s.sonarr.DeleteQueueRecord(context.Background(), record, &sonarr.DeleteQueueRecordParam{Blacklist: true}); err != nil {
					s.log.Err(err).Msgf("failed to remove %s from sonarr", v.Name)
				} else {
					s.log.Debug().Msgf("Successfully blocked %s in sonarr", k)
					delete(stalled, k) // on success remove it from the kill list
				}
			}
		}
	}
	s.log.Debug().Msgf("Done Sonarr processing")

	return stalled, nil
}

func (s *Service) RadarrDelete(stalled map[string]*delugeclient.TorrentStatus) (map[string]*delugeclient.TorrentStatus, error) {
	if !s.config.EnableRadarr {
		return stalled, nil
	}
	s.log.Debug().Msgf("Starting Radarr processing")
	q, err := s.radarr.GetQueue(0, 0)
	if err != nil {
		return nil, err
	}
	for _, record := range q.Records {
		for k, v := range stalled {
			if record.Title == v.Name {
				if s.config.Pretend {
					s.log.Info().Msgf("PRETEND: Will delete %s from radarr", v.Name)
					continue
				}
				if err := s.radarr.DeleteQueueRecord(context.Background(), record, &radarr.DeleteQueueRecordParam{
					Blocklist:        true,
					RemoveFromClient: true,
				}); err != nil {
					s.log.Err(err).Msgf("failed to remove %s from radarr", v.Name)
				} else {
					s.log.Debug().Msgf("Successfully blocked %s in radarr", k)
					delete(stalled, k) // on success remove it from the kill list
				}
			}
		}
	}
	s.log.Debug().Msgf("Done Radarr processing")

	return stalled, nil
}

func (s *Service) FilterTorrentsForStalled(downloading map[string]*delugeclient.TorrentStatus) map[string]*delugeclient.TorrentStatus {
	now := time.Now()
	res := map[string]*delugeclient.TorrentStatus{}
	for k, v := range downloading {
		cutoff := time.Unix(int64(v.TimeAdded), 0).Add(s.config.StallDuration)
		if v.ETA == 0 && v.TotalDone == 0 && v.CompletedTime == 0 && now.After(cutoff) {
			res[k] = v
			s.log.Debug().Msgf("Found stalled torrent %s", k)
		}
	}

	return res
}

func (s *Service) FilterForLabel(downloading map[string]*delugeclient.TorrentStatus) (map[string]*delugeclient.TorrentStatus, error) {
	if len(s.config.OnlyLabels) == 0 {
		return downloading, nil
	}
	res := map[string]*delugeclient.TorrentStatus{}
	label, err := s.deluge.LabelPlugin()
	if err != nil {
		return nil, err
	}
	for k, v := range downloading {
		torLabel, err := label.GetTorrentLabel(k)
		if err != nil {
			return nil, err
		}
		if Contains(torLabel, s.config.OnlyLabels) {
			res[k] = v
		}
	}

	return res, nil
}

func (s *Service) GetStalledTorrents() (map[string]*delugeclient.TorrentStatus, error) {
	session, err := s.deluge.SessionState()
	if err != nil {
		return nil, err
	}
	downloading, err := s.deluge.TorrentsStatus(delugeclient.StateDownloading, session)
	if err != nil {
		return nil, err
	}
	downloading = s.FilterTorrentsForStalled(downloading)

	return s.FilterForLabel(downloading)
}

func Contains[T comparable](item T, items []T) bool {
	for i := range items {
		if items[i] == item {
			return true
		}
	}
	return false
}
