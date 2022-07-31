# stallarr
Cleans out stalled torrents. 
This connects to deluge and looks for torrents that are:
 - haven't ever connected or shared
 - No ETA
 - Never Completed
 - STALL_DURATION + time added to deluge

If all the above is true, then the torrent is stalled and will be blocklisted in sonarr or radarr.

docker pull scripterkiddd/stallarr:latest

Settings (.env or env variables)
```text
DELUGE_HOST=localhost
DELUGE_PORT=58846
DELUGE_USERNAME=nobody
DELUGE_PASSWORD=deluge

SONARR_ENABLED=true    # false skips sonarr processing
SONARR_API_KEY=api_key
SONARR_URL=http://localhost:8989

RADARR_ENABLED=true    # false skips radarr processing
RADARR_API_KEY=api_key
RADARR_URL=http://localhost:7878

ONLY_LABELS=tv,movies  # comma seperated list of labels to filter stalled torrents by
PRETEND=true           # for testing 
DEBUG=true             # debug logging
REFRESH_DURATION=10m   # how often to check for new stalled torrents
STALL_DURATION=1h      # how long after the torrent was added to wait before killing it
RUN_ON_STARTUP=true    # run on startup or wait REFRESH_DURATION before running
```

Deluge username and password can be obtained from config/auth 