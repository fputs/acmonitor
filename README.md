# acmonitor

Single-page monitor + control panel for the local BurkeyCraft AzerothCore
(Playerbots) stack. Go binary, no runtime deps, HTML/JS embedded in the binary.

## What it shows
- **Services** — `ac-authserver`, `ac-llm-bridge`, `ac-worldserver`: active state,
  uptime, memory, live CPU%, PID (from `systemctl --user show`).
- **Database** — `ac-database` Docker container state + health, plus a live SQL
  ping latency.
- **Host** — load average, core count, memory usage.
- **Game** — online total, real players vs bots, account/character counts
  (queried from `acore_characters` / `acore_auth`).
- **SOAP** — worldserver `server info` output (once enabled, see below).

## Control
Buttons issue:
- **all** → `server.sh {start|stop|restart}` (DB + all services, correct ordering).
- **per-service** → `systemctl --user {start|stop|restart} <unit>`.

## Layout
- `main.go` — backend + collectors + `/api/status` and `/api/control`.
- `web/index.html` — the single page (embedded via `go:embed`).
- `config.example.json` — template; copy to `config.json` and fill in creds.
- `acmonitor.service` — systemd `--user` unit.

## Configure
```
cp config.example.json config.json
```
Edit `config.json`:
- `bind` — listen address. Use `127.0.0.1:8080` for localhost-only, or a
  tailnet/LAN IP to reach it from other devices (**no auth — see Notes**).
- `db` — MySQL host/port/user/pass for the AzerothCore DB.
- `soap` — worldserver SOAP URL plus a GM-level-3 account and password.

`config.json` is gitignored because it holds live credentials.

## Build & run
```
go build -o acmonitor .
./acmonitor            # reads ./config.json (or $ACMONITOR_CONFIG)
```

## Install as a service
```
cp acmonitor.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now acmonitor
```
To keep it (and the game stack) running after logout / across reboots, enable
lingering for the user:
```
loginctl enable-linger <user>   # needs sudo/polkit
```

## Enabling the SOAP panel
The SOAP card reads "not configured" until both `soap.user` and `soap.pass`
are set; everything else works without it. SOAP must also be enabled in
`worldserver.conf` (`SOAP.Enabled = 1`), which **takes effect only after a
worldserver restart** — do this from the panel's "Restart world server" button
when convenient (it disconnects online bots/players).

## Notes
- CPU% is a delta between polls, so the first reading after a restart shows 0.
- No authentication by design (tailnet-local). Anyone on the tailnet can hit the
  control buttons.
