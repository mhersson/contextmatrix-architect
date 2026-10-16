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
to three rounds. The session then reports whether the cards are ready. Ready
cards go to sign-off: reply approve to create them in ContextMatrix, with a
playbook for two or more, or reject with feedback to re-run decomposition. The
auto+emit and yolo modes approve on their own when the preflight finds nothing
that needs a human first; manual steps it lists are shown with the emitted
cards. In yolo mode the architect then starts the run: one card with
run_card, several with run_playbook. If a card's repository has no commits it
waits for a first commit and a reply of approve. While the session is open the
architect watches the emitted cards and playbook in ContextMatrix and posts
each change, ending with the pull requests and compare links.

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
every run), a capped one refuses to run until a priced model is chosen or a
new blueprint is started without a cap. A per-persona override in `models`
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

The image sets `XDG_STATE_HOME=/var/lib`, so the default `state_dir` is the
volume. The state volume must be writable by uid 65532 (the distroless
`nonroot` user). A fresh named volume inherits that ownership from the image; a
bind mount must be chowned by hand. The mounted `config.yaml` must be readable by uid 65532.
`config.yaml.example` documents every key.

The image ships ripgrep for the read tools. A local run or the user service
needs `rg` on its PATH for grep (glob also takes `fd`); `serve` logs a warning
at startup when it is missing and runs anyway.
