# contextmatrix-architect

> [!WARNING]
>
> This project is under heavy development. Breaking changes should be expected
> at the current stage.

Tell the architect what you need, in your own words. It interviews you, works
the plan out step by step and writes the cards an AI agent can build, and you
approve each big decision before the next step builds on it. The cards go on
[ContextMatrix](https://github.com/mhersson/contextmatrix), a task board where
an AI agent builds what each card describes, and you talk to the architect in
ContextMatrix's chat panel.

![The blueprint pane of an example blueprint at its sign-off: the scale bar with both slices done, the chat with the reviewers' verdicts and the sign-off waiting for approval, and the Sign-off sheet listing the four cards and six manual checks in the order they run](docs/images/00-blueprint-pane.png)

- [Guide](docs/guide.md): one real blueprint, step by step, with pictures.
- [Reference](docs/reference.md): every reply you can type, the modes, and the marks on the sheet.
- [Installing](docs/installing.md): what to set up before the first blueprint.
- [Design](docs/design.md): how it works inside, for developers.

The sections below are for whoever runs the architect. It runs as a third
ContextMatrix backend next to the agent and chat backends, and ContextMatrix's
chat panel is its client.

## Run

```bash
make build
mkdir -p ~/.config/contextmatrix-architect
./contextmatrix-architect config defaults > ~/.config/contextmatrix-architect/config.yaml
chmod 600 ~/.config/contextmatrix-architect/config.yaml   # it holds the HMAC key
# edit contextmatrix_url and api_key
./contextmatrix-architect serve
```

`serve` reads `contextmatrix-architect/config.yaml` in the user config
directory (`$XDG_CONFIG_HOME` or `~/.config` on Linux,
`~/Library/Application Support` on macOS) unless `--config` names another
file. On macOS write the file to that directory instead of `~/.config`, or
pass `--config`. `config validate <file>` loads a file exactly as `serve`
would and exits 1 on the first error.

Configure it in ContextMatrix as `backends.architect` with the same `api_key`.

An empty `state_dir`, the default, resolves to
`$XDG_STATE_HOME/contextmatrix-architect`, else
`~/.local/state/contextmatrix-architect`, on every OS. To use another
directory, set `state_dir` (or `CONTEXTMATRIX_ARCHITECT_STATE_DIR`) to an
absolute path the user can write; `~` is not expanded.

## Health

`GET /health` and `GET /readyz` need no signature. `/health` reports the
build the server runs:

```json
{"ok":true,"running_containers":0,"max_concurrent":0,"build":{"commit":"<40-char sha>","build_time":"2026-10-21T08:00:00Z","changes":[{"number":33,"kind":"fix","title":"name the cards readiness did not pass on the sign-off","merged_at":"2026-10-20T16:00:00Z"}]}}
```

`running_containers` and `max_concurrent` are always 0, because the
architect runs no containers. `build.version` is the tag the commit was built
on, and is left out when there is none. `changes` holds up to 20 merged pull
requests, newest first. Each carries its number, its kind (`feat`, `fix`, or
`change` for any other title), its title and its merge time in UTC. A
lowercase conventional `type(scope): ` prefix is dropped from the title; any
other `word:` prefix, a capitalized one included, stays, because the agent
titles its PRs with the card title. Dependabot PRs and PRs typed ci, docs,
chore, build, test, refactor or style are left out.

`make build` and `make install` stamp the commit and build time and record the
history from git. A bare `go build` of a fresh clone reports no commit, no
build time and `"changes":[]`. ContextMatrix shows this in its About popup.

## User service

On Linux the architect runs as a systemd user service, like the agent and chat
backends:

```bash
make build
./svc.sh install   # write + enable ~/.config/systemd/user/contextmatrix-architect.service
./svc.sh start     # also: stop / status / print / verify / uninstall
journalctl --user -u contextmatrix-architect -f
```

The unit runs `serve --config ${XDG_CONFIG_HOME:-~/.config}/contextmatrix-architect/config.yaml`
from this checkout, sandboxed like the sibling backends, with `~/.contextmatrix`
and the resolved `state_dir` writable. `install` creates that directory,
because the sandbox only lets the service write directories that exist when it
starts, and pins `XDG_STATE_HOME` in the unit so the service resolves the same
default as the script. It warns about a missing config, an explicit
`/var/lib/contextmatrix-architect` (the image's volume), a `state_dir` that is
not absolute, contains whitespace or is not writable, a directory it could not
create and a missing `rg`, and installs anyway. Rerun `./svc.sh install` after
changing `state_dir` or `XDG_STATE_HOME`.

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
their cost, the sign-off decisions and the ids emission created in
ContextMatrix, every artifact version, the questions and answers, the
sources, the event log and the run transcripts. It reads the store, which holds
records with the session's secrets already masked, so a running stage is not
disturbed. Committing the bundle to a repository is not built yet.

## Cost caps

CM sends the session model's price with every blueprint start, resolved from
its `token_costs` and model catalog. The architect prices every run's tokens
with it, so `max_run_cost_usd` and a blueprint's cost cap bind on any
endpoint, and a run is stopped within one model call of passing its cap. A
model CM cannot price arrives without a price: an uncapped blueprint still
runs and counts only the endpoint's reported cost (`max_turns` still bounds
every run), a capped one refuses to run until a priced model is chosen or the cap is
lifted with `cap off`. A per-persona override in `models`
names a model CM did not price, so its runs are unpriced the same way.
`max_run_cost_usd: 0` disables the per-run cap; `max_turns` still bounds every
run, and a blueprint's own cap still applies.

## Docker

```bash
make docker
docker run --rm -p 9095:9095 \
  -v architect-state:/var/lib/contextmatrix-architect \
  -v "$PWD/config.yaml:/etc/contextmatrix-architect/config.yaml:ro" \
  contextmatrix-architect:dev
```

`make docker` passes the commit, build time and tag as build args and records
the merge history first, and CI's image job does the same from a full-history
checkout. A plain `docker build .` reports no commit or build time on
`/health`.

The image sets `XDG_STATE_HOME=/var/lib`, so the default `state_dir` is the
volume. The state volume must be writable by uid 65532 (the distroless
`nonroot` user). A fresh named volume inherits that ownership from the image; a
bind mount must be chowned by hand. The mounted `config.yaml` must be readable by uid 65532.
`config.yaml.example` documents every key.

The image ships ripgrep for the read tools. A local run or the user service
needs `rg` on its PATH for grep (glob also takes `fd`); `serve` logs a warning
at startup when it is missing and runs anyway.

On Kubernetes, a Service named `contextmatrix-architect` makes the kubelet set
`CONTEXTMATRIX_ARCHITECT_PORT=tcp://<ip>:<port>` in pods of its namespace,
which the architect reads as its `port` and fails to start on. Set
`enableServiceLinks: false` on the architect's pod.
