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
