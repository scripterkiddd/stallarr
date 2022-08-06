package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kong"
	delugeclient "github.com/gdm85/go-libdeluge"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/errgroup"

	"golift.io/starr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

func main() {
	s, cleanup := New()
	defer func() {
		if err := cleanup(); err != nil {
			log.Printf("Error in cleanup %s \n", err.Error())
		}
	}()
	s.log.Info().Msg("Init")

	done := s.Run()
	<-done
}

type Service struct {
	log    zerolog.Logger
	deluge delugeclient.Client
	sonarr *sonarr.Sonarr
	radarr *radarr.Radarr
	config Config
	ctx    context.Context
}

type Config struct {
	DelugeHost     string `env:"DELUGE_HOST" default:"localhost"`
	DelugePort     uint   `env:"DELUGE_PORT" default:"58846"`
	DelugeUsername string `env:"DELUGE_USERNAME" default:"nobody"`
	DelugePassword string `env:"DELUGE_PASSWORD" default:"deluge"`

	EnableSonarr         bool   `env:"SONARR_ENABLED"`
	SonarrAPIKey         string `env:"SONARR_API_KEY"`
	SonarrURL            string `env:"SONARR_URL" default:"http://localhost:8989"`
	SonarrSearchOnDelete bool   `env:"SONARR_SEARCH_ON_DELETE" default:"true"`

	EnableRadarr         bool   `env:"RADARR_ENABLED"`
	RadarrAPIKey         string `env:"RADARR_API_KEY" default:"deluge"`
	RadarrURL            string `env:"RADARR_URL"  default:"http://localhost:7878"`
	RadarrSearchOnDelete bool   `env:"RADARR_SEARCH_ON_DELETE" default:"true"`

	OnlyLabels      []string      `env:"ONLY_LABELS" sep:","`
	RefreshDuration time.Duration `env:"REFRESH_DURATION" default:"10m" type:"time.Duration"`
	StallDuration   time.Duration `env:"STALL_DURATION" default:"1h" type:"time.Duration"`

	Pretend      bool `env:"PRETEND" default:"false"`
	RunOnStartup bool `env:"RUN_ON_STARTUP" default:"true"`
	Debug        bool `env:"DEBUG" default:"false"`
}

type Torrents map[string]*delugeclient.TorrentStatus

const DelugeTimeout = 2

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

	s := &Service{
		log: zerolog.New(os.Stdout).With().
			Timestamp().
			Logger(),
		config: config,
		ctx:    context.Background(),
	}
	if err := s.Setup(); err != nil {
		s.log.Fatal().Err(err).Msg("setup failed")
	}
	return s, func() error {
		s.ctx.Done()
		return s.deluge.Close()
	}
}

func (s *Service) Setup() error {
	wg := &errgroup.Group{}
	wg.Go(func() error {
		timeout := time.Minute * DelugeTimeout
		deluge := delugeclient.NewV2(delugeclient.Settings{
			Hostname:         s.config.DelugeHost,
			Port:             s.config.DelugePort,
			Login:            s.config.DelugeUsername,
			ReadWriteTimeout: timeout,
			Password:         s.config.DelugePassword,
		})
		s.deluge = deluge.Client
		return deluge.Connect()
	})
	wg.Go(func() error {
		if !s.config.EnableSonarr {
			return nil
		}
		sonarrConfig := starr.New(s.config.SonarrAPIKey, s.config.SonarrURL, 0)
		s.sonarr = sonarr.New(sonarrConfig)
		s.log.Debug().Msgf("Testing sonarr connection")
		_, err := s.sonarr.GetQueue(1, 1)
		return err
	})
	wg.Go(func() error {
		if !s.config.EnableRadarr {
			return nil
		}
		radarrConfig := starr.New(s.config.RadarrAPIKey, s.config.RadarrURL, 0)
		s.radarr = radarr.New(radarrConfig)
		s.log.Debug().Msgf("Testing radarr connection")
		_, err := s.sonarr.GetQueue(1, 1)
		return err
	})
	return wg.Wait()
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
		s.log.Debug().Msgf("Failed to connect to deluge %s", err.Error())
		return
	}
	allStalled, err := s.GetStalledTorrents()
	if err != nil {
		s.log.Debug().Msgf("Failed to gather torrents %s", err.Error())
		s.log.Fatal().Err(err)
		return
	}
	for _, v := range allStalled {
		s.log.Debug().Msgf("Target stalled torrent %s", v.Name)
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

func (s *Service) SonarrDelete(stalled Torrents) (Torrents, error) {
	if !s.config.EnableSonarr {
		return stalled, nil
	}
	s.log.Debug().Msgf("Starting Sonarr processing")
	q, err := s.sonarr.GetQueue(0, 0)
	if err != nil {
		return nil, err
	}
	episodesToSearch := []int64{}
	for _, record := range q.Records {
		for k, v := range stalled {
			if IsLikeTitle(record.Title, v.Name) {
				episodesToSearch = append(episodesToSearch, record.EpisodeID)
				s.log.Info().Msgf("%d", record.EpisodeID)
				deleteErr := s.DoSonarrDelete(record, v)
				if deleteErr == nil {
					delete(stalled, k) // on success remove it from the kill list
				}
			}
		}
	}
	if s.config.SonarrSearchOnDelete && len(episodesToSearch) > 0 {
		if searchErr := s.AttemptSonarrSearch(episodesToSearch); searchErr != nil {
			s.log.Err(searchErr).Msgf("failed to search sonarr")
		}
	}
	s.log.Debug().Msgf("Done Sonarr processing")

	return stalled, nil
}

func IsLikeTitle(recordName, torrentName string) bool {
	return recordName == torrentName || strings.HasPrefix(torrentName, recordName)
}

func (s *Service) AttemptSonarrSearch(episodes []int64) error {
	if s.config.Pretend {
		s.log.Info().Msgf("PRETEND: search command sent")
		return nil
	}
	resp, err := s.sonarr.SendCommandContext(s.ctx, &sonarr.CommandRequest{
		Name:       sonarr.EpisodeSearch,
		EpisodeIDs: episodes,
	})
	s.log.Info().Msgf("started search for epID %+v and got %s", episodes, resp.Status)
	return err
}

func (s *Service) DoSonarrDelete(record *sonarr.QueueRecord, torrent *delugeclient.TorrentStatus) error {
	if s.config.Pretend {
		s.log.Info().Msgf("PRETEND: Will delete %s from sonarr", torrent.Name)
		return nil
	}
	if err := s.sonarr.DeleteQueueRecord(
		s.ctx,
		record,
		&sonarr.DeleteQueueRecordParam{Blacklist: true}); err != nil {
		s.log.Err(err).Msgf("failed to remove %s from sonarr", torrent.Name)
		return err
	}
	s.log.Debug().Msgf("Successfully blocked %s in sonarr", torrent.Name)
	return nil
}

func (s *Service) RadarrDelete(stalled Torrents) (Torrents, error) {
	if !s.config.EnableRadarr {
		return stalled, nil
	}
	s.log.Debug().Msgf("Starting Radarr processing")
	q, err := s.radarr.GetQueue(0, 0)
	if err != nil {
		return nil, err
	}
	idsToSearch := []int64{}
	for _, record := range q.Records {
		for k, v := range stalled {
			if IsLikeTitle(record.Title, v.Name) {
				idsToSearch = append(idsToSearch, record.MovieID)
				deleteErr := s.DoRadarrDelete(record, v)
				if deleteErr == nil {
					delete(stalled, k) // on success remove it from the kill list
				}
			}
		}
	}
	if s.config.RadarrSearchOnDelete && len(idsToSearch) > 0 {
		if searchErr := s.AttemptRadarrSearch(idsToSearch); searchErr != nil {
			s.log.Err(searchErr).Msgf("failed to search radarr")
		}
	}
	s.log.Debug().Msgf("Done Radarr processing")
	return stalled, nil
}

func (s *Service) AttemptRadarrSearch(ids []int64) error {
	if s.config.Pretend {
		s.log.Info().Msgf("PRETEND: search command sent")
		return nil
	}
	resp, err := s.radarr.SendCommandContext(s.ctx, &radarr.CommandRequest{
		Name:     radarr.MoviesSearch,
		MovieIDs: ids,
	})
	s.log.Info().Msgf("started search for epID %+v and got %s", ids, resp.Status)
	return err
}

func (s *Service) DoRadarrDelete(record *radarr.QueueRecord, torrent *delugeclient.TorrentStatus) error {
	if s.config.Pretend {
		s.log.Info().Msgf("PRETEND: Will delete %s from radarr", torrent.Name)
		return nil
	}
	if err := s.radarr.DeleteQueueRecord(s.ctx, record, &radarr.DeleteQueueRecordParam{
		Blocklist:        true,
		RemoveFromClient: true,
	}); err != nil {
		s.log.Err(err).Msgf("failed to remove %s from radarr", torrent.Name)
		return err
	}
	s.log.Debug().Msgf("Successfully blocked %s in radarr", torrent.Name)

	return nil
}

func (s *Service) FilterTorrentsForStalled(downloading Torrents) Torrents {
	now := time.Now()
	res := map[string]*delugeclient.TorrentStatus{}
	for k, v := range downloading {
		cutoff := time.Unix(int64(v.TimeAdded), 0).Add(s.config.StallDuration)
		if v.ETA == 0 && v.TotalDone == 0 && v.CompletedTime == 0 && now.After(cutoff) {
			res[k] = v
			s.log.Debug().Msgf("Found stalled torrent %s", v.Name)
		}
	}

	return res
}

func (s *Service) FilterForLabel(downloading Torrents) (Torrents, error) {
	if len(s.config.OnlyLabels) == 0 {
		return downloading, nil
	}
	res := map[string]*delugeclient.TorrentStatus{}
	label, err := s.deluge.LabelPlugin()
	if err != nil {
		return nil, err
	}
	for k, v := range downloading {
		torLabel, labelErr := label.GetTorrentLabel(k)
		if labelErr != nil {
			return nil, labelErr
		}
		if Contains(torLabel, s.config.OnlyLabels) {
			res[k] = v
		}
	}

	return res, nil
}

func (s *Service) GetStalledTorrents() (Torrents, error) {
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
