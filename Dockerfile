FROM golang:1.27.3-trixie@sha256:604106102d33b1084e37ca08adf98ac0813624ae8595bd0d7f3965ecebded09c AS build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
WORKDIR /src
# An empty state dir owned by the nonroot user, so a fresh volume mounted over
# it inherits that ownership.
RUN mkdir -p /state
# The harness grep and glob tools exec ripgrep; the static musl build runs on
# distroless. Pinned by version and digest per architecture (14.1.1 ships no
# aarch64 musl build, so the pin is 15.x). It sits above the sources, so a
# rebuild, which always changes the stamped build layer, still reuses it.
ARG TARGETARCH
ARG RIPGREP_VERSION=15.2.0
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) arch=x86_64; sum=33e15bcf1624b25cdd2a55813a47a2f95dbe126268203e76aa6a585d1e7b149c ;; \
      arm64) arch=aarch64; sum=800b1e7206afe799dfb5a6901f23147cfaabe0e52210538100f61e86e1740915 ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /tmp/rg.tgz "https://github.com/BurntSushi/ripgrep/releases/download/${RIPGREP_VERSION}/ripgrep-${RIPGREP_VERSION}-${arch}-unknown-linux-musl.tar.gz"; \
    echo "${sum}  /tmp/rg.tgz" | sha256sum -c -; \
    tar -xzf /tmp/rg.tgz -C /tmp; \
    install -m 0755 "/tmp/ripgrep-${RIPGREP_VERSION}-${arch}-unknown-linux-musl/rg" /usr/local/bin/rg
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# make docker and CI's image job set these; they are empty in a plain docker
# build.
ARG VERSION=""
ARG GIT_COMMIT=""
ARG BUILD_TIME=""
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-X github.com/mhersson/contextmatrix-architect/internal/buildinfo.version=${VERSION} -X github.com/mhersson/contextmatrix-architect/internal/buildinfo.commit=${GIT_COMMIT} -X github.com/mhersson/contextmatrix-architect/internal/buildinfo.buildTime=${BUILD_TIME}" \
      -o /contextmatrix-architect ./cmd/contextmatrix-architect

FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
# The default state_dir is $XDG_STATE_HOME/contextmatrix-architect; this keeps
# it on the volume below.
ENV XDG_STATE_HOME=/var/lib
COPY --from=build /contextmatrix-architect /contextmatrix-architect
COPY --from=build /usr/local/bin/rg /usr/local/bin/rg
COPY --from=build --chown=65532:65532 /state /var/lib/contextmatrix-architect
VOLUME ["/var/lib/contextmatrix-architect"]
EXPOSE 9095
ENTRYPOINT ["/contextmatrix-architect", "serve", "--config", "/etc/contextmatrix-architect/config.yaml"]
