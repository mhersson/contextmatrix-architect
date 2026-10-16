FROM golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
WORKDIR /src
# An empty state dir owned by the nonroot user, so a fresh volume mounted over
# it inherits that ownership.
RUN mkdir -p /state
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /contextmatrix-architect ./cmd/contextmatrix-architect
# The harness grep and glob tools exec ripgrep; the static musl build runs on
# distroless. Pinned by version and digest per architecture (14.1.1 ships no
# aarch64 musl build, so the pin is 15.x).
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /contextmatrix-architect /contextmatrix-architect
COPY --from=build /usr/local/bin/rg /usr/local/bin/rg
COPY --from=build --chown=65532:65532 /state /var/lib/contextmatrix-architect
VOLUME ["/var/lib/contextmatrix-architect"]
EXPOSE 9095
ENTRYPOINT ["/contextmatrix-architect", "serve", "--config", "/etc/contextmatrix-architect/config.yaml"]
