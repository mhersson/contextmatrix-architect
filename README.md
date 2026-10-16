# contextmatrix-architect

Turns a stated need into a blueprint of ContextMatrix cards. Runs as a third
ContextMatrix backend next to the agent and chat backends; ContextMatrix's
chat panel is its client. See `docs/design.md`.

## What it does today

A blueprint session runs intake on the first message, the need, and stores
the next message as the design. Grounding, decomposition and readiness then
run in turn over read-only checkouts of the target projects' repositories.
Every decomposition submit is checked against the card contract inside the
run, and readiness sends cards that are not ready back to decomposition for up
to three rounds. The session ends with a report of whether the cards are
ready, with the findings when they are not; nothing is emitted into
ContextMatrix yet.

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
the default branch's commit, written without a git binary. A project's
secondary repository sits beside it at `trees/<blueprint>/<project>@<repo>`.
Trees not written for `workspace_max_age_days` are swept at startup and hourly;
mirrors stay.

## Export

Reply `export` in the chat panel to write the blueprint's record as markdown
under `<state_dir>/exports/<blueprint>/<time>`: a summary with the runs and
their cost, every artifact version, the questions and answers, the sources,
the event log and the run transcripts. It reads the store, which holds
records with the session's secrets already masked, so a running stage is not
disturbed. Committing the bundle to a repository is not built yet.

## Cost caps

CM sends the session model's price with every blueprint start, resolved from
its `token_costs` and model catalog. The architect prices every run's tokens
with it, so `max_run_cost_usd` and a blueprint's cost cap bind on any
endpoint, and a run is stopped within one model call of passing its cap. A
model CM cannot price arrives without a price: an uncapped blueprint still
runs and counts only the endpoint's reported cost (`max_turns` still bounds
every run), a capped one refuses to run until a priced model is chosen or a
new blueprint is started without a cap. A per-persona override in `models`
names a model CM did not price, so its runs are unpriced the same way.

## Docker

```bash
make docker
docker run --rm -p 9095:9095 \
  -v architect-state:/var/lib/contextmatrix-architect \
  -v "$PWD/config.yaml:/etc/contextmatrix-architect/config.yaml:ro" \
  contextmatrix-architect:dev
```

The state volume must be writable by uid 65532 (the distroless `nonroot` user).
A fresh named volume inherits that ownership from the image; a bind mount must
be chowned by hand. The mounted `config.yaml` must be readable by uid 65532.
`config.yaml.example` documents every key.

The image ships ripgrep for the read tools; a local run needs `rg` on the path
for grep and glob.
