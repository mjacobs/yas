# Deploying yas

## Binaries (cgo-free static)

```bash
CGO_ENABLED=0 go build -o ~/.local/bin/yas        ./cmd/yas
CGO_ENABLED=0 go build -o ~/.local/bin/yas-server ./cmd/yas-server
```

## Config (`~/.config/yas/`, mode 0600)

- `server.json` — `{"addr":"127.0.0.1:8732","database_url":"postgres://USER@HOST:5432/yas","token":"…"}`.
  Omit the password from `database_url` and put it in `~/.pgpass` — pgx reads it.
- `config.json` — `{"server_url":"http://127.0.0.1:8732","token":"…"}` (the **same** token).

## systemd (`--user`): server + periodic client sync

```bash
mkdir -p ~/.config/systemd/user
cp deploy/systemd/* ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now yas-server.service   # the homelab hub
systemctl --user enable --now yas-sync.timer        # client sync every 5 min
loginctl enable-linger "$USER"                        # keep user services running without a login session
```

`yas-server.service` runs the Postgres-backed sync hub; `yas-sync.timer` fires
`yas-sync.service` (a `yas sync` oneshot) every few minutes.

## Live capture (zsh)

```bash
cp shell/yas.zsh ~/.config/yas/hook.zsh
# in ~/.zshrc:
[[ -f ~/.config/yas/hook.zsh ]] && source ~/.config/yas/hook.zsh
```

The hook records each command into the local SQLite replica via two-phase
`preexec`/`precmd` capture; the timer syncs it to the server. Redact sensitive
commands with `ignore_patterns` in `config.json`.

## Exposing the server to other machines

The server binds `127.0.0.1` by default. To let other homelab machines sync,
set `addr` to a LAN interface and run it behind TLS or Tailscale (the bearer
token is the only auth). Point each client's `server_url` at it.
