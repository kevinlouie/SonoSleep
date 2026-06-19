# Build the static-ish Go binary, then ship it on a slim runtime that has ffmpeg
# (the PCM->MP3 encoder the stream handler pipes through).
FROM golang:1.22-bookworm AS build
WORKDIR /src
# Cache module downloads. (No deps yet; go.sum may be absent.)
COPY go.mod ./
COPY go.su[m] ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/hwnsonos ./cmd/hwnsonos

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/hwnsonos /usr/local/bin/hwnsonos
EXPOSE 8099
ENTRYPOINT ["/usr/local/bin/hwnsonos"]
