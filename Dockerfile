FROM golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /architect ./cmd/architect
# An empty state dir owned by the nonroot user, so a fresh volume mounted over
# it inherits that ownership.
RUN mkdir -p /state

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /architect /architect
COPY --from=build --chown=65532:65532 /state /var/lib/contextmatrix-architect
VOLUME ["/var/lib/contextmatrix-architect"]
EXPOSE 9094
ENTRYPOINT ["/architect", "serve", "--config", "/etc/contextmatrix-architect/config.yaml"]
