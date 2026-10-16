# contextmatrix-architect

Turns a stated need into a blueprint of ContextMatrix cards. Runs as a third
ContextMatrix backend next to the agent and chat backends; ContextMatrix's
chat panel is its client. See `docs/design.md`.

## Run

```bash
make build
./architect config defaults > config.yaml   # edit contextmatrix_url, api_key
./architect serve --config config.yaml
```

Configure it in ContextMatrix as `backends.architect` with the same `api_key`.

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
be chowned by hand. `config.yaml.example` documents every key.
