# Installing the architect

The architect is the part of ContextMatrix that interviews you about something
you want built and turns your answers into a plan and a set of cards. This page
says what you need and how to get it running. The easy way is one installer.

## What you need

- **ContextMatrix** itself, the board where work is tracked as cards. Its
  [README](https://github.com/mhersson/contextmatrix/blob/main/README.md)
  explains what it is and how to get it.
- **The agent backend**, the helper program (a **backend**) that lets an AI
  agent build a card. The architect only plans; the agent builds what the
  plan's cards ask for. Both are in ContextMatrix's README.
- **An LLM endpoint**, the online service that runs the AI model, and a key
  to use it. You give it to ContextMatrix once, and ContextMatrix hands it to
  the architect for every blueprint.
- **A Sonnet-class model**, Anthropic's Claude Sonnet or one as capable. Smaller
  models sent empty or broken answers on large results in testing, and the
  architect does not repair them.

You talk to the architect in ContextMatrix's **chat panel**, the Chat page
where you also start chats with the AI.

## The easy way

[contextmatrix-setup](https://github.com/mhersson/contextmatrix-setup)
installs ContextMatrix, the agent backend and the architect on your own
computer (macOS or Linux), asks for what it needs one question at a time and
connects them. The architect comes with it. To install it:

```bash
go install github.com/mhersson/contextmatrix-setup@latest
contextmatrix-setup install
```

Its README lists the tools it needs first and the items to have ready, such as
your LLM endpoint key. The architect also needs `rg` (ripgrep, a search tool),
which the installer does not install: get it from your package manager, for
example `brew install ripgrep`. If you installed ContextMatrix with the setup
tool before the architect existed, `contextmatrix-setup update` adds it.

## The manual way

Do this if you run ContextMatrix yourself and want to add the architect. You
need to build the architect from this repository (see
[its README](../README.md)); the steps below connect it.

1. Print the architect's settings with their defaults, save them as its
   config file and keep that file private, because it holds a key:

   ```bash
   mkdir -p ~/.config/contextmatrix-architect
   ./contextmatrix-architect config defaults > ~/.config/contextmatrix-architect/config.yaml
   chmod 600 ~/.config/contextmatrix-architect/config.yaml
   ```

2. Set the two keys that must be filled in:

   ```yaml
   contextmatrix_url: "<address-of-your-contextmatrix>"
   api_key: "<your-shared-key>"
   ```

   The shared key is a password the architect and ContextMatrix both know. Make
   it at least 32 characters long.

3. Check the file. This reads it exactly as the architect will, and prints the
   first problem it finds:

   ```bash
   ./contextmatrix-architect config validate ~/.config/contextmatrix-architect/config.yaml
   ```

4. Tell ContextMatrix about the architect, in its config file, and restart
   ContextMatrix (it reads backends only at start). The key is the same
   shared key, and the port is the architect's, 9095 unless you changed it:

   ```yaml
   backends:
     architect:
       url: "http://localhost:9095"
       api_key: "<your-shared-key>"
       enabled: true
       default_model: "<a-sonnet-class-model>"
   ```

   The model name is how your LLM endpoint names the model. ContextMatrix's
   [configuration guide](https://github.com/mhersson/contextmatrix/blob/main/docs/configuration.md)
   and [remote execution guide](https://github.com/mhersson/contextmatrix/blob/main/docs/remote-execution.md)
   list every `backends` setting, and the LLM endpoint's own block
   (`llm_endpoint`).

5. Start the architect with
   `./contextmatrix-architect serve --config ~/.config/contextmatrix-architect/config.yaml`.
   To keep it
   running in the background, the architect's [README](../README.md) shows how
   to run it as a service or in Docker, and lists every other setting.

## Check that it works

Open ContextMatrix in your browser, go to the chat panel and start a new
chat. When ContextMatrix knows about the architect, the New chat window
offers two tiles, Chat and **Blueprint**, and picking Blueprint turns its title
into **New blueprint**. Without the Blueprint tile, ContextMatrix has not been
told about the architect: check that the `backends.architect` block has a
`url`, an `api_key` and is not `enabled: false`, then restart ContextMatrix.

The tile only shows that ContextMatrix has been given the architect's address.
To see that the architect itself answers, create a blueprint and send your first
message: the interview starts.

## Where to go next

- [Architect reference](reference.md) for what you can type, the modes, the cost
  cap and what to do when a blueprint stops.
- [Models](reference.md#models) for more on choosing a model.
- [design.md](design.md) for how the architect works inside.
