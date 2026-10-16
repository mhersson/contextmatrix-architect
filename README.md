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

## Cost caps

Cost is what the LLM endpoint reports per call. An endpoint that reports no
cost shows 0 USD on every stage, and neither `max_run_cost_usd` nor a
blueprint's cost cap binds; `max_turns` still bounds every run.

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
