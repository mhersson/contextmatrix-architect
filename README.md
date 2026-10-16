# contextmatrix-architect

Turns a stated need into a blueprint of ContextMatrix cards. Runs as a third
ContextMatrix backend next to the agent and chat backends; ContextMatrix's
chat panel is its client. See `docs/design.md`.

## Run

```bash
make build
./architect config defaults > config.yaml   # edit contextmatrix_url, api_key, state_dir
./architect serve --config config.yaml
```

Configure it in ContextMatrix as `backends.architect` with the same `api_key`.

The default `state_dir` is `/var/lib/contextmatrix-architect`. For a local run
as a normal user, point `state_dir` (or `CONTEXTMATRIX_ARCHITECT_STATE_DIR`) at
a directory that user can write.

## Code checkouts

Code lives under `<state_dir>/workspaces`: `mirrors/` holds one bare mirror per
repository, fetched over HTTPS with credentials CM mints per session, and
`trees/<blueprint>/<project>` holds one plain tree per blueprint and project at
the default branch's commit, written without a git binary. Trees not written for
`workspace_max_age_days` are swept at startup and hourly; mirrors stay.

## Cost caps

CM sends the session model's price with every blueprint start, resolved from
its `token_costs` and model catalog. The architect prices every run's tokens
with it, so `max_run_cost_usd` and a blueprint's cost cap bind on any
endpoint, and a run is stopped within one model call of passing its cap. A
model CM cannot price arrives without a price: an uncapped blueprint still
runs and counts only the endpoint's reported cost (`max_turns` still bounds
every run), a capped one refuses to run until
a priced model is chosen or a new blueprint is started without a cap.

## Docker

```bash
make docker
docker run --rm -p 9094:9094 \
  -v architect-state:/var/lib/contextmatrix-architect \
  -v "$PWD/config.yaml:/etc/contextmatrix-architect/config.yaml:ro" \
  contextmatrix-architect:dev
```

The state volume must be writable by uid 65532 (the distroless `nonroot` user).
A fresh named volume inherits that ownership from the image; a bind mount must
be chowned by hand. The mounted `config.yaml` must be readable by uid 65532.
`config.yaml.example` documents every key.
