Single-page monitor + control panel for my personal AzerothCore
(Playerbots) stack. Go binary, no runtime deps, HTML/JS embedded in the binary.

This is heavily customized to meet my setup and will certainly need to be modified
for yours.

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
Set `soap.user` and `soap.pass`, then enable in `worldserver.conf` and restart
